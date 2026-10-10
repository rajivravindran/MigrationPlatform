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
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/lifecycle"
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
	sources   *sourceAuthorizer
	ledger    operationLedger
	secrets   secretResolver
}

func NewActivities(d Deps) *Activities {
	return &Activities{
		d:         d,
		sftpSlots: make(chan struct{}, 8),
		sources:   newSourceAuthorizer(d.DB, d.SecretsMasterKey),
		ledger:    pgOperationLedger{db: d.DB},
		secrets:   pgSecretResolver{db: d.DB, masterKey: d.SecretsMasterKey},
	}
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
	// Version is the rule_templates.version row that was loaded.
	Version int32 `json:"version,omitempty"`
}

func (a *Activities) LoadTemplate(ctx context.Context, in LoadTemplateInput) (LoadTemplateOutput, error) {
	var raw []byte
	var version int32
	var err error
	if in.TemplateVersion > 0 {
		err = a.d.DB.QueryRow(ctx,
			`SELECT schema_json, version FROM rule_templates WHERE id = $1 AND org_id = $2 AND version = $3`,
			in.RuleTemplateID, in.OrgID, in.TemplateVersion,
		).Scan(&raw, &version)
	} else {
		err = a.d.DB.QueryRow(ctx,
			`SELECT schema_json, version FROM rule_templates WHERE id = $1 AND org_id = $2 ORDER BY version DESC LIMIT 1`,
			in.RuleTemplateID, in.OrgID,
		).Scan(&raw, &version)
	}
	if err != nil {
		return LoadTemplateOutput{}, fmt.Errorf("load template: %w", err)
	}
	var tpl template.RuleTemplate
	if err := json.Unmarshal(raw, &tpl); err != nil {
		return LoadTemplateOutput{}, fmt.Errorf("parse template: %w", err)
	}
	return LoadTemplateOutput{Template: tpl, Version: version}, nil
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
	JobID           int64          `json:"jobId"`
	OrgID           int64          `json:"orgId"`
	ConnectorKind   string         `json:"connectorKind"`
	ConnectorConfig map[string]any `json:"connectorConfig"`
	// ConnectorSecret is never honoured; a non-empty value (from workflows
	// started before ADR 0001) fails authorization.
	ConnectorSecret string `json:"connectorSecret,omitempty"`
	// SourceID names the registered object; ConnectorID a stored connector.
	SourceID    int64           `json:"sourceId,omitempty"`
	ConnectorID int64           `json:"connectorId,omitempty"`
	Offset      json.RawMessage `json:"offset"`
	BatchSize   int             `json:"batchSize"`
	// ExpectedSourceVersion is the version earlier batches of this job read;
	// empty on the first batch or for sources without a version.
	ExpectedSourceVersion string `json:"expectedSourceVersion,omitempty"`
}

type IngestOutput struct {
	Rows      []connectors.Row  `json:"rows"`
	NewOffset connectors.Offset `json:"newOffset"`
	Done      bool              `json:"done"`
	// Pending means the source has no rows yet (a Salesforce query still
	// running); call again later with NewOffset.
	Pending       bool   `json:"pending,omitempty"`
	SourceVersion string `json:"sourceVersion,omitempty"`
}

// ErrTypeInvalidCheckpoint marks a resume position that cannot belong to the
// source; resuming from it would skip or repeat rows.
const ErrTypeInvalidCheckpoint = "InvalidCheckpoint"

func invalidCheckpoint(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeInvalidCheckpoint, err)
}

func (a *Activities) Ingest(ctx context.Context, in IngestInput) (IngestOutput, error) {
	offset, err := connectors.UnmarshalOffset(in.Offset)
	if err != nil {
		return IngestOutput{}, invalidCheckpoint(fmt.Errorf("decode ingest offset: %w", err))
	}
	cfg, err := a.sources.authorize(ctx, in)
	if err != nil {
		activity.GetLogger(ctx).Warn("ingest source refused", "jobId", in.JobID, "orgId", in.OrgID,
			"sourceId", in.SourceID, "connectorId", in.ConnectorID, "error", err)
		return IngestOutput{}, err
	}
	if in.ExpectedSourceVersion != "" && cfg.Version != in.ExpectedSourceVersion {
		return IngestOutput{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("source changed during the job (read version %s, now %s)", in.ExpectedSourceVersion, cfg.Version),
			ErrTypeSourceChanged, nil)
	}
	cfg.HTTP = a.d.HTTP
	conn, err := connectors.Factory{}.Build(cfg)
	if err != nil {
		return IngestOutput{}, err
	}
	iter, err := conn.Open(ctx, offset)
	if errors.Is(err, connectors.ErrInvalidOffset) {
		return IngestOutput{}, invalidCheckpoint(err)
	}
	if err != nil {
		return IngestOutput{}, err
	}
	defer iter.Close()

	if in.BatchSize <= 0 {
		in.BatchSize = 500
	}
	out := IngestOutput{Rows: make([]connectors.Row, 0, in.BatchSize), NewOffset: offset, SourceVersion: cfg.Version}
	for len(out.Rows) < in.BatchSize {
		activity.RecordHeartbeat(ctx, len(out.Rows))
		row, newOffset, more, err := iter.Next(ctx)
		if errors.Is(err, connectors.ErrSourceNotReady) && len(out.Rows) == 0 {
			out.NewOffset, out.Pending = newOffset, true
			return out, nil
		}
		if errors.Is(err, connectors.ErrInvalidOffset) {
			return out, invalidCheckpoint(err)
		}
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
	// AttemptID identifies this processing of the row; a redelivery with the
	// same ID does not count as another attempt.
	AttemptID string `json:"attemptId,omitempty"`
	// Row is the raw (preprocessed) source row; persisted so single-row
	// retries never need to re-read the source file.
	Row      map[string]any  `json:"row,omitempty"`
	Payload  map[string]any  `json:"payload,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// PersistOutcome upserts the row's final state for this processing attempt.
// `attempts` counts distinct processing attempts: it is incremented
// server-side when AttemptID changes, so a redelivered call is not counted
// twice and retries accumulate no matter which path dispatched them.
func (a *Activities) PersistOutcome(ctx context.Context, in PersistOutcomeInput) error {
	payload, _ := json.Marshal(in.Payload)
	rowJSON, _ := json.Marshal(in.Row)
	_, err := a.d.DB.Exec(ctx, `
        INSERT INTO job_rows (job_id, row_index, status, attempts, last_error, idempotency_key, row_json, payload_json, response_json, last_attempt_id)
        VALUES ($1, $2, $3::row_status, 1, $4, $5, $6, $7, $8, $9)
        ON CONFLICT (job_id, row_index) DO UPDATE SET
            status = EXCLUDED.status,
            attempts = job_rows.attempts + CASE
                WHEN EXCLUDED.last_attempt_id IS NOT NULL AND EXCLUDED.last_attempt_id = job_rows.last_attempt_id THEN 0
                ELSE 1 END,
            last_attempt_id = EXCLUDED.last_attempt_id,
            last_error = EXCLUDED.last_error,
            row_json = COALESCE(EXCLUDED.row_json, job_rows.row_json),
            payload_json = EXCLUDED.payload_json,
            response_json = EXCLUDED.response_json,
            updated_at = now()`,
		in.JobID, in.RowIndex, in.Status,
		nullIf(in.LastError), nullIf(in.IdempotencyKey), nullIfJSON(rowJSON), payload, safeJSON(in.Response), nullIf(in.AttemptID))
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
	AttemptID      string          `json:"attemptId,omitempty"`
}

// PersistStepOutcome records one step of a multi-step chain. Only invoked for
// chain templates (single-step templates keep their outcome on job_rows
// alone), so 10M-row single-call jobs pay no extra write cost.
func (a *Activities) PersistStepOutcome(ctx context.Context, in PersistStepOutcomeInput) error {
	reqJSON, _ := json.Marshal(in.Request)
	_, err := a.d.DB.Exec(ctx, `
        INSERT INTO job_row_steps (job_id, row_index, step_index, step_name, status, attempts, request_url, request_json, response_status, response_json, last_error, last_attempt_id)
        VALUES ($1, $2, $3, $4, $5::row_status, 1, $6, $7, $8, $9, $10, $11)
        ON CONFLICT (job_id, row_index, step_index) DO UPDATE SET
            step_name = EXCLUDED.step_name,
            status = EXCLUDED.status,
            attempts = job_row_steps.attempts + CASE
                WHEN EXCLUDED.last_attempt_id IS NOT NULL AND EXCLUDED.last_attempt_id = job_row_steps.last_attempt_id THEN 0
                ELSE 1 END,
            last_attempt_id = EXCLUDED.last_attempt_id,
            request_url = EXCLUDED.request_url,
            request_json = EXCLUDED.request_json,
            response_status = EXCLUDED.response_status,
            response_json = EXCLUDED.response_json,
            last_error = EXCLUDED.last_error,
            updated_at = now()`,
		in.JobID, in.RowIndex, in.StepIndex, in.StepName, in.Status,
		nullIf(in.RequestURL), reqJSON, zeroToNil(in.ResponseStatus), safeJSON(in.Response), nullIf(in.LastError), nullIf(in.AttemptID))
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
	// After is an exclusive row_index cursor; nil starts before row zero.
	After *int64 `json:"after,omitempty"`
	Limit int    `json:"limit"`
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
        WHERE job_id = $1 AND status = 'failed'::row_status
          AND ($2::bigint IS NULL OR row_index > $2::bigint)
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
	// Error records an execution failure outside a row; the job is failed.
	Error string `json:"error,omitempty"`
}

// FinalizeJob recomputes totals from job_rows and writes the terminal status
// (see lifecycle.Finalize for the rules).
func (a *Activities) FinalizeJob(ctx context.Context, in FinalizeJobInput) error {
	_, err := lifecycle.Finalize(ctx, a.d.DB, in.JobID, lifecycle.Outcome{Cancelled: in.Cancelled, Error: in.Error})
	return err
}

func (a *Activities) syncJobTotals(ctx context.Context, jobID int64) error {
	t, err := lifecycle.CountRows(ctx, a.d.DB, jobID)
	if err != nil {
		return err
	}
	totals, err := json.Marshal(t)
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
