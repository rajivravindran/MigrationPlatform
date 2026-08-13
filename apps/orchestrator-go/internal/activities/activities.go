// Package activities implements every IO-bound side effect used by the
// migration workflows. All activities are idempotent and safe to retry.
package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"

	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/metrics"
	"github.com/migration-platform/orchestrator/internal/redisbus"
	"github.com/migration-platform/orchestrator/internal/security"
	"github.com/migration-platform/orchestrator/internal/template"
)

type Deps struct {
	DB               *pgxpool.Pool
	Redis            *redisbus.Client
	HTTP             *http.Client
	LicenseHTTP      *http.Client
	SecretsMasterKey string
	// RateLimiter throttles outbound calls per destination host (nil = off).
	RateLimiter *security.HostLimiter
	// MaxResponseBytes caps how much of a response body is read/stored.
	MaxResponseBytes int64
	// LicenseCheckURL is the API's authenticated runtime authorization endpoint.
	LicenseCheckURL string
	BridgeToken     string
}

type Activities struct {
	d         Deps
	sftpSlots chan struct{}
}

func NewActivities(d Deps) *Activities {
	return &Activities{d: d, sftpSlots: make(chan struct{}, 8)}
}

func (a *Activities) acquireSFTP(ctx context.Context) (func(), error) {
	select {
	case a.sftpSlots <- struct{}{}:
		return func() { <-a.sftpSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RequireLicensed fails closed before a workflow creates or processes work.
// The API remains the single authority for signature, expiry, and dev-mode policy.
func (a *Activities) RequireLicensed(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.d.LicenseCheckURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.d.BridgeToken)
	resp, err := a.d.LicenseHTTP.Do(req)
	if err != nil {
		metrics.LicenseDenied.Inc()
		return fmt.Errorf("license authorization unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		metrics.LicenseDenied.Inc()
		return fmt.Errorf("license authorization denied (status %d)", resp.StatusCode)
	}
	return nil
}

// ---------------- LoadTemplate ----------------

type LoadTemplateInput struct {
	OrgID           int64 `json:"orgId"`
	RuleTemplateID  int64 `json:"ruleTemplateId"`
	TemplateVersion int32 `json:"templateVersion,omitempty"`
}

type LoadTemplateOutput struct {
	Template template.RuleTemplate `json:"template"`
}

func (a *Activities) LoadTemplate(ctx context.Context, in LoadTemplateInput) (LoadTemplateOutput, error) {
	var raw []byte
	var err error
	if in.TemplateVersion > 0 {
		err = a.d.DB.QueryRow(ctx,
			`SELECT schema_json FROM rule_templates WHERE id = $1 AND org_id = $2 AND version = $3`,
			in.RuleTemplateID, in.OrgID, in.TemplateVersion,
		).Scan(&raw)
	} else {
		err = a.d.DB.QueryRow(ctx,
			`SELECT schema_json FROM rule_templates WHERE id = $1 AND org_id = $2 ORDER BY version DESC LIMIT 1`,
			in.RuleTemplateID, in.OrgID,
		).Scan(&raw)
	}
	if err != nil {
		return LoadTemplateOutput{}, fmt.Errorf("load template: %w", err)
	}
	var tpl template.RuleTemplate
	if err := json.Unmarshal(raw, &tpl); err != nil {
		return LoadTemplateOutput{}, fmt.Errorf("parse template: %w", err)
	}
	return LoadTemplateOutput{Template: tpl}, nil
}

// ---------------- BootstrapScheduledJob ----------------

type BootstrapScheduledJobInput struct {
	OrgID               int64          `json:"orgId"`
	ScheduleID          int64          `json:"scheduleId"`
	RuleTemplateID      int64          `json:"ruleTemplateId"`
	RuleTemplateVersion int32          `json:"ruleTemplateVersion"`
	ConnectorID         int64          `json:"connectorId"`
	ScheduledTime       time.Time      `json:"scheduledTime"`
	TemporalWorkflowID  string         `json:"temporalWorkflowId"`
	TemporalRunID       string         `json:"temporalRunId"`
	ExtraSourceConfig   map[string]any `json:"extraSourceConfig,omitempty"`
}

type BootstrapScheduledJobOutput struct {
	JobID int64 `json:"jobId"`
}

// BootstrapScheduledJob inserts a `jobs` row for a schedule-triggered run and
// also upserts into schedule_runs. Idempotent on (temporal_workflow_id).
func (a *Activities) BootstrapScheduledJob(ctx context.Context, in BootstrapScheduledJobInput) (BootstrapScheduledJobOutput, error) {
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return BootstrapScheduledJobOutput{}, err
	}
	defer tx.Rollback(ctx)

	source := map[string]any{"type": "connector", "connectorId": in.ConnectorID, "extra": in.ExtraSourceConfig}
	srcJSON, _ := json.Marshal(source)

	var jobID int64
	err = tx.QueryRow(ctx, `
        INSERT INTO jobs (org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status, temporal_workflow_id, temporal_run_id, started_at)
        VALUES ($1, $2, $3, $4, $5, 'running'::job_status, $6, $7, now())
        ON CONFLICT (temporal_workflow_id) DO UPDATE SET started_at = jobs.started_at
        RETURNING id`,
		in.OrgID, in.RuleTemplateID, in.RuleTemplateVersion, in.ScheduleID, srcJSON, in.TemporalWorkflowID, in.TemporalRunID,
	).Scan(&jobID)
	if err != nil {
		return BootstrapScheduledJobOutput{}, fmt.Errorf("insert job: %w", err)
	}

	if _, err = tx.Exec(ctx, `
        INSERT INTO schedule_runs (schedule_id, job_id, scheduled_time, actual_start_time, status)
        VALUES ($1, $2, $3, now(), 'running'::job_status)`,
		in.ScheduleID, jobID, in.ScheduledTime,
	); err != nil {
		return BootstrapScheduledJobOutput{}, fmt.Errorf("insert schedule_run: %w", err)
	}

	if _, err = tx.Exec(ctx, `UPDATE schedules SET last_run_at = $1, updated_at = now() WHERE id = $2`,
		in.ScheduledTime, in.ScheduleID); err != nil {
		return BootstrapScheduledJobOutput{}, err
	}

	if err = tx.Commit(ctx); err != nil {
		return BootstrapScheduledJobOutput{}, err
	}
	return BootstrapScheduledJobOutput{JobID: jobID}, nil
}

// ---------------- Ingest ----------------

type IngestInput struct {
	JobID           int64           `json:"jobId"`
	OrgID           int64           `json:"orgId"`
	ConnectorKind   string          `json:"connectorKind"`
	ConnectorConfig map[string]any  `json:"connectorConfig"`
	ConnectorSecret string          `json:"connectorSecret"`
	Offset          json.RawMessage `json:"offset"`
	BatchSize       int             `json:"batchSize"`
}

type IngestOutput struct {
	Rows      []connectors.Row  `json:"rows"`
	NewOffset connectors.Offset `json:"newOffset"`
	Done      bool              `json:"done"`
}

func (a *Activities) Ingest(ctx context.Context, in IngestInput) (IngestOutput, error) {
	offset, _ := connectors.UnmarshalOffset(in.Offset)
	factory := connectors.Factory{}
	conn, err := factory.Build(connectors.Config{
		Kind:   in.ConnectorKind,
		Config: in.ConnectorConfig,
		Secret: in.ConnectorSecret,
	})
	if err != nil {
		return IngestOutput{}, err
	}
	iter, err := conn.Open(ctx, offset)
	if err != nil {
		return IngestOutput{}, err
	}
	defer iter.Close()

	if in.BatchSize <= 0 {
		in.BatchSize = 500
	}
	out := IngestOutput{Rows: make([]connectors.Row, 0, in.BatchSize)}
	for len(out.Rows) < in.BatchSize {
		activity.RecordHeartbeat(ctx, len(out.Rows))
		row, newOffset, more, err := iter.Next(ctx)
		if err != nil {
			return out, err
		}
		out.NewOffset = newOffset
		if !more {
			out.Done = true
			break
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// ---------------- CallEndpoint ----------------

// CallEndpointInput carries the per-row, fully-rendered HTTP request. URL,
// Headers and IdempotencyKey are computed by the workflow (see
// template.RenderDestination) so the activity is purely an IO sender.
type CallEndpointInput struct {
	JobID             int64             `json:"jobId"`
	RowIndex          int64             `json:"rowIndex"`
	StepName          string            `json:"stepName,omitempty"`
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers,omitempty"`
	Payload           map[string]any    `json:"payload"`
	HasBody           bool              `json:"hasBody,omitempty"`
	IdempotencyKey    string            `json:"idempotencyKey,omitempty"`
	IdempotencyHeader string            `json:"idempotencyHeader,omitempty"`
	RetryAttempts     int               `json:"retryAttempts,omitempty"`
}

type CallEndpointOutput struct {
	Status    int             `json:"status"`
	Body      json.RawMessage `json:"body"`
	Succeeded bool            `json:"succeeded"`
	Error     string          `json:"error,omitempty"`
	LatencyMs int64           `json:"latencyMs"`
}

func (a *Activities) CallEndpoint(ctx context.Context, in CallEndpointInput) (CallEndpointOutput, error) {
	started := time.Now()

	var bodyReader io.Reader
	// GET/DELETE steps typically carry no body; sending "{}" to strict APIs
	// causes spurious 400s, so only attach a body when the step mapped one.
	sendBody := in.HasBody || len(in.Payload) > 0
	if sendBody {
		body, err := json.Marshal(in.Payload)
		if err != nil {
			return CallEndpointOutput{Error: err.Error()}, err
		}
		bodyReader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, in.Method, in.URL, bodyReader)
	if err != nil {
		return CallEndpointOutput{Error: err.Error()}, err
	}
	if sendBody {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range in.Headers {
		req.Header.Set(k, v)
	}
	if in.IdempotencyHeader != "" && in.IdempotencyKey != "" {
		req.Header.Set(in.IdempotencyHeader, in.IdempotencyKey)
	}

	if a.d.RateLimiter != nil {
		if err := a.d.RateLimiter.Wait(ctx, req.URL.Host); err != nil {
			return CallEndpointOutput{Error: err.Error()}, err
		}
	}

	resp, err := a.d.HTTP.Do(req)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		metrics.EndpointLatency.WithLabelValues("error").Observe(float64(latency) / 1000)
		return CallEndpointOutput{Error: err.Error(), LatencyMs: latency}, err
	}
	defer resp.Body.Close()
	maxBytes := a.d.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes))

	statusLabel := fmt.Sprintf("%d", resp.StatusCode/100*100)
	metrics.EndpointLatency.WithLabelValues(statusLabel).Observe(float64(latency) / 1000)

	safeBody := safeJSON(respBody)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return CallEndpointOutput{
			Status: resp.StatusCode, Body: safeBody, LatencyMs: latency,
			Error: fmt.Sprintf("retryable %d: %s", resp.StatusCode, excerpt(respBody, 300)),
		}, fmt.Errorf("retryable status %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return CallEndpointOutput{
			Status: resp.StatusCode, Body: safeBody, LatencyMs: latency, Succeeded: false,
			Error: fmt.Sprintf("status %d: %s", resp.StatusCode, excerpt(respBody, 300)),
		}, nil
	}

	return CallEndpointOutput{
		Status: resp.StatusCode, Body: safeBody, Succeeded: true, LatencyMs: latency,
	}, nil
}

// safeJSON returns raw unchanged when it is valid JSON, otherwise wraps it in
// a JSON string so it survives Temporal payload marshaling and JSONB inserts.
// Empty bodies become JSON null.
func safeJSON(raw []byte) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return json.RawMessage("null")
	}
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	wrapped, err := json.Marshal(trimmed)
	if err != nil {
		return json.RawMessage("null")
	}
	return wrapped
}

// excerpt renders a short single-line preview of a response body for error
// messages surfaced in the UI.
func excerpt(raw []byte, max int) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// ---------------- PersistOutcome ----------------

type PersistOutcomeInput struct {
	JobID          int64  `json:"jobId"`
	RowIndex       int64  `json:"rowIndex"`
	Status         string `json:"status"`
	LastError      string `json:"lastError,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Row is the raw (preprocessed) source row; persisted so single-row
	// retries never need to re-read the source file.
	Row      map[string]any  `json:"row,omitempty"`
	Payload  map[string]any  `json:"payload,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// PersistOutcome upserts the row's final state for this processing attempt.
// `attempts` counts processing attempts and is incremented server-side so
// retries accumulate correctly no matter which path dispatched them.
func (a *Activities) PersistOutcome(ctx context.Context, in PersistOutcomeInput) error {
	payload, _ := json.Marshal(in.Payload)
	rowJSON, _ := json.Marshal(in.Row)
	_, err := a.d.DB.Exec(ctx, `
        INSERT INTO job_rows (job_id, row_index, status, attempts, last_error, idempotency_key, row_json, payload_json, response_json)
        VALUES ($1, $2, $3::row_status, 1, $4, $5, $6, $7, $8)
        ON CONFLICT (job_id, row_index) DO UPDATE SET
            status = EXCLUDED.status,
            attempts = job_rows.attempts + 1,
            last_error = EXCLUDED.last_error,
            row_json = COALESCE(EXCLUDED.row_json, job_rows.row_json),
            payload_json = EXCLUDED.payload_json,
            response_json = EXCLUDED.response_json,
            updated_at = now()`,
		in.JobID, in.RowIndex, in.Status,
		nullIf(in.LastError), nullIf(in.IdempotencyKey), nullIfJSON(rowJSON), payload, safeJSON(in.Response))
	if err != nil {
		return err
	}
	switch in.Status {
	case "succeeded":
		metrics.RowsProcessed.WithLabelValues(fmt.Sprintf("%d", in.JobID)).Inc()
	case "failed":
		metrics.RowsFailed.WithLabelValues(fmt.Sprintf("%d", in.JobID)).Inc()
	}
	return nil
}

// ---------------- PersistStepOutcome ----------------

type PersistStepOutcomeInput struct {
	JobID          int64           `json:"jobId"`
	RowIndex       int64           `json:"rowIndex"`
	StepIndex      int             `json:"stepIndex"`
	StepName       string          `json:"stepName"`
	Status         string          `json:"status"`
	RequestURL     string          `json:"requestUrl,omitempty"`
	Request        map[string]any  `json:"request,omitempty"`
	ResponseStatus int             `json:"responseStatus,omitempty"`
	Response       json.RawMessage `json:"response,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
}

// PersistStepOutcome records one step of a multi-step chain. Only invoked for
// chain templates (single-step templates keep their outcome on job_rows
// alone), so 10M-row single-call jobs pay no extra write cost.
func (a *Activities) PersistStepOutcome(ctx context.Context, in PersistStepOutcomeInput) error {
	reqJSON, _ := json.Marshal(in.Request)
	_, err := a.d.DB.Exec(ctx, `
        INSERT INTO job_row_steps (job_id, row_index, step_index, step_name, status, attempts, request_url, request_json, response_status, response_json, last_error)
        VALUES ($1, $2, $3, $4, $5::row_status, 1, $6, $7, $8, $9, $10)
        ON CONFLICT (job_id, row_index, step_index) DO UPDATE SET
            step_name = EXCLUDED.step_name,
            status = EXCLUDED.status,
            attempts = job_row_steps.attempts + 1,
            request_url = EXCLUDED.request_url,
            request_json = EXCLUDED.request_json,
            response_status = EXCLUDED.response_status,
            response_json = EXCLUDED.response_json,
            last_error = EXCLUDED.last_error,
            updated_at = now()`,
		in.JobID, in.RowIndex, in.StepIndex, in.StepName, in.Status,
		nullIf(in.RequestURL), reqJSON, zeroToNil(in.ResponseStatus), safeJSON(in.Response), nullIf(in.LastError))
	return err
}

// ---------------- LoadRowState ----------------

type LoadRowStateInput struct {
	JobID    int64 `json:"jobId"`
	RowIndex int64 `json:"rowIndex"`
}

type PersistedStep struct {
	StepIndex int             `json:"stepIndex"`
	StepName  string          `json:"stepName"`
	Status    string          `json:"status"`
	Response  json.RawMessage `json:"response,omitempty"`
}

type LoadRowStateOutput struct {
	Found bool            `json:"found"`
	Row   map[string]any  `json:"row,omitempty"`
	Steps []PersistedStep `json:"steps,omitempty"`
}

// LoadRowState fetches the persisted raw row plus any per-step outcomes so a
// single-row retry can (a) re-run without re-ingesting the source and (b)
// resume at the first non-succeeded step, reusing earlier responses.
func (a *Activities) LoadRowState(ctx context.Context, in LoadRowStateInput) (LoadRowStateOutput, error) {
	out := LoadRowStateOutput{}
	var rowJSON []byte
	err := a.d.DB.QueryRow(ctx,
		`SELECT row_json FROM job_rows WHERE job_id = $1 AND row_index = $2`,
		in.JobID, in.RowIndex,
	).Scan(&rowJSON)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return out, nil
		}
		return out, err
	}
	out.Found = true
	if len(rowJSON) > 0 {
		if err := json.Unmarshal(rowJSON, &out.Row); err != nil {
			return out, fmt.Errorf("parse persisted row: %w", err)
		}
	}

	rows, err := a.d.DB.Query(ctx, `
        SELECT step_index, step_name, status::text, response_json
        FROM job_row_steps WHERE job_id = $1 AND row_index = $2 ORDER BY step_index ASC`,
		in.JobID, in.RowIndex)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var s PersistedStep
		var resp []byte
		if err := rows.Scan(&s.StepIndex, &s.StepName, &s.Status, &resp); err != nil {
			return out, err
		}
		s.Response = json.RawMessage(resp)
		out.Steps = append(out.Steps, s)
	}
	return out, rows.Err()
}

// ---------------- ListFailedRows ----------------

type ListFailedRowsInput struct {
	JobID int64 `json:"jobId"`
	After int64 `json:"after"` // exclusive row_index cursor
	Limit int   `json:"limit"`
}

type ListFailedRowsOutput struct {
	RowIndexes []int64 `json:"rowIndexes"`
	Done       bool    `json:"done"`
}

// ListFailedRows pages over the failed rows of a job so the retry-failed
// workflow can fan out without loading millions of indexes at once.
func (a *Activities) ListFailedRows(ctx context.Context, in ListFailedRowsInput) (ListFailedRowsOutput, error) {
	if in.Limit <= 0 {
		in.Limit = 500
	}
	rows, err := a.d.DB.Query(ctx, `
        SELECT row_index FROM job_rows
        WHERE job_id = $1 AND status = 'failed'::row_status AND row_index > $2
        ORDER BY row_index ASC LIMIT $3`,
		in.JobID, in.After, in.Limit)
	if err != nil {
		return ListFailedRowsOutput{}, err
	}
	defer rows.Close()
	out := ListFailedRowsOutput{}
	for rows.Next() {
		var idx int64
		if err := rows.Scan(&idx); err != nil {
			return out, err
		}
		out.RowIndexes = append(out.RowIndexes, idx)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.Done = len(out.RowIndexes) < in.Limit
	return out, nil
}

// ---------------- PublishProgress ----------------

type PublishProgressInput struct {
	JobID      int64 `json:"jobId"`
	Processed  int64 `json:"processed"`
	Failed     int64 `json:"failed"`
	Pending    int64 `json:"pending"`
	ETASeconds int64 `json:"etaSeconds"`
	LastRow    int64 `json:"lastRow"`
}

func (a *Activities) PublishProgress(ctx context.Context, in PublishProgressInput) error {
	if err := a.d.Redis.PublishProgress(ctx, in.JobID, in); err != nil {
		return err
	}
	return a.syncJobTotals(ctx, in.JobID)
}

// ---------------- FinalizeJob ----------------

type FinalizeJobInput struct {
	JobID     int64 `json:"jobId"`
	Cancelled bool  `json:"cancelled,omitempty"`
}

// FinalizeJob marks a job complete: recomputes totals from job_rows (source of
// truth) and sets status + finished_at. Skips jobs already cancelled.
func (a *Activities) FinalizeJob(ctx context.Context, in FinalizeJobInput) error {
	var processed, failed, total int64
	err := a.d.DB.QueryRow(ctx, `
        SELECT
            count(*) FILTER (WHERE status = 'succeeded'::row_status),
            count(*) FILTER (WHERE status = 'failed'::row_status),
            count(*)
        FROM job_rows WHERE job_id = $1`, in.JobID).Scan(&processed, &failed, &total)
	if err != nil {
		return err
	}

	status := "succeeded"
	switch {
	case in.Cancelled:
		status = "cancelled"
	case failed > 0:
		status = "failed"
	case total == 0:
		status = "failed"
	}

	totals, err := json.Marshal(map[string]any{
		"processed": processed,
		"failed":    failed,
		"total":     total,
	})
	if err != nil {
		return err
	}

	_, err = a.d.DB.Exec(ctx, `
        UPDATE jobs SET
            status = $1::job_status,
            totals_json = $2,
            finished_at = now(),
            updated_at = now()
        WHERE id = $3 AND status <> 'cancelled'::job_status`,
		status, totals, in.JobID)
	return err
}

func (a *Activities) syncJobTotals(ctx context.Context, jobID int64) error {
	var processed, failed, total int64
	err := a.d.DB.QueryRow(ctx, `
        SELECT
            count(*) FILTER (WHERE status = 'succeeded'::row_status),
            count(*) FILTER (WHERE status = 'failed'::row_status),
            count(*)
        FROM job_rows WHERE job_id = $1`, jobID).Scan(&processed, &failed, &total)
	if err != nil {
		return err
	}
	totals, err := json.Marshal(map[string]any{
		"processed": processed,
		"failed":    failed,
		"total":     total,
	})
	if err != nil {
		return err
	}
	_, err = a.d.DB.Exec(ctx, `
        UPDATE jobs SET totals_json = $1, updated_at = now()
        WHERE id = $2 AND status = 'running'::job_status`,
		totals, jobID)
	return err
}

// ---------------- util ----------------

func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" || string(b) == "{}" {
		return nil
	}
	return b
}

func zeroToNil(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// ComputeIdempotencyKey derives a stable key from job/row/step identity plus
// the rendered payload, so each step of a chain gets its own key and retries
// of the same step reuse it.
func ComputeIdempotencyKey(jobID, rowIndex int64, step string, payload map[string]any) string {
	b, err := json.Marshal(payload)
	if err != nil {
		b = nil
	}
	seed := fmt.Sprintf("%d-%d-%s-", jobID, rowIndex, step)
	h := sha256.Sum256(append([]byte(seed), b...))
	return hex.EncodeToString(h[:16])
}

var _ = errors.New
