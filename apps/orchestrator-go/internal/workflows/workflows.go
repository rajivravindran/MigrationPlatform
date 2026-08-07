// Package workflows contains the Temporal workflow definitions that drive
// every migration job.
package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/template"
)

const (
	SignalPause  = "pause"
	SignalResume = "resume"

	// TaskQueueTransforms hosts the Python preprocess/$py sandbox activities.
	TaskQueueTransforms = "transforms"
)

// MigrationWorkflowInput is the single input shape for the top-level workflow.
// `RetryRow` is set when the workflow is just a single-row retry harness.
type MigrationWorkflowInput struct {
	OrgID               int64          `json:"orgId"`
	JobID               int64          `json:"jobId"`
	RuleTemplateID      int64          `json:"ruleTemplateId"`
	RuleTemplateVersion int32          `json:"ruleTemplateVersion"`
	SourceRef           map[string]any `json:"sourceRef"`
	ScheduleID          int64          `json:"scheduleId,omitempty"`
	ScheduledTime       time.Time      `json:"scheduledTime,omitempty"`
	RetryRow            *RetryRowRef   `json:"retryRow,omitempty"`
	Shards              int            `json:"shards,omitempty"`
}

type RetryRowRef struct {
	JobID    int64 `json:"jobId"`
	RowIndex int64 `json:"rowIndex"`
	// FromStart forces the whole chain to re-run even when earlier steps
	// succeeded previously. Default (false) resumes at the first failed step
	// so non-idempotent creates are not repeated.
	FromStart bool `json:"fromStart,omitempty"`
}

// Result summarises the workflow outcome.
type MigrationWorkflowResult struct {
	Processed int64 `json:"processed"`
	Failed    int64 `json:"failed"`
	Cancelled bool  `json:"cancelled"`
}

// MigrationWorkflow is the entrypoint workflow for a migration job. It loads
// the rule template, ingests rows in batches (continue-as-new every N batches
// to keep history bounded), applies preprocessing (Python sandbox tier),
// renders + executes every step of the template chain per row, and writes
// row/step outcomes back to Postgres.
func MigrationWorkflow(ctx workflow.Context, in MigrationWorkflowInput) (*MigrationWorkflowResult, error) {
	log := workflow.GetLogger(ctx)
	log.Info("MigrationWorkflow start", "jobId", in.JobID, "orgId", in.OrgID)

	ao := defaultActivityOptions(nil)
	ctx = workflow.WithActivityOptions(ctx, ao)

	if in.RetryRow != nil {
		return runSingleRowRetry(ctx, in)
	}

	var tplOut activities.LoadTemplateOutput
	if err := workflow.ExecuteActivity(ctx, "LoadTemplate", activities.LoadTemplateInput{
		OrgID: in.OrgID, RuleTemplateID: in.RuleTemplateID, TemplateVersion: in.RuleTemplateVersion,
	}).Get(ctx, &tplOut); err != nil {
		return nil, fmt.Errorf("load template: %w", err)
	}
	// Honor template.retry for subsequent activities (CallEndpoint, Persist*, …).
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions(tplOut.Template.Retry))

	paused := false
	pauseCh := workflow.GetSignalChannel(ctx, SignalPause)
	resumeCh := workflow.GetSignalChannel(ctx, SignalResume)

	// Background goroutine listening for pause/resume.
	workflow.Go(ctx, func(gCtx workflow.Context) {
		for gCtx.Err() == nil {
			sel := workflow.NewSelector(gCtx)
			sel.AddReceive(pauseCh, func(c workflow.ReceiveChannel, _ bool) {
				c.Receive(gCtx, nil)
				paused = true
				log.Info("workflow paused")
			})
			sel.AddReceive(resumeCh, func(c workflow.ReceiveChannel, _ bool) {
				c.Receive(gCtx, nil)
				paused = false
				log.Info("workflow resumed")
			})
			sel.Select(gCtx)
		}
	})

	result := &MigrationWorkflowResult{}
	var offsetRaw []byte = []byte("{}")
	const batchSize = 500
	batchIdx := 0

	connCfg, secret := extractConnector(in.SourceRef)

	for {
		// Cooperative pause: spin with a short sleep while paused.
		for paused {
			if err := workflow.Sleep(ctx, 2*time.Second); err != nil {
				return result, err
			}
		}

		var ingest activities.IngestOutput
		err := workflow.ExecuteActivity(ctx, "Ingest", activities.IngestInput{
			JobID:           in.JobID,
			OrgID:           in.OrgID,
			ConnectorKind:   connCfg.Kind,
			ConnectorConfig: connCfg.Config,
			ConnectorSecret: secret,
			Offset:          offsetRaw,
			BatchSize:       batchSize,
		}).Get(ctx, &ingest)
		if err != nil {
			return result, fmt.Errorf("ingest: %w", err)
		}

		rows, err := preprocessRows(ctx, tplOut.Template, ingest.Rows)
		if err != nil {
			return result, fmt.Errorf("preprocess: %w", err)
		}

		for _, row := range rows {
			if err := processOneRow(ctx, in.JobID, tplOut.Template, row, nil, result); err != nil {
				return result, err
			}
		}

		// Publish aggregate progress once per batch.
		_ = workflow.ExecuteActivity(ctx, "PublishProgress", activities.PublishProgressInput{
			JobID: in.JobID, Processed: result.Processed, Failed: result.Failed,
			Pending: 0, LastRow: int64(len(ingest.Rows)),
		}).Get(ctx, nil)

		offsetRaw, _ = ingest.NewOffset.Marshal()
		batchIdx++
		if ingest.Done {
			break
		}

		// Bound history via continue-as-new every 100 batches.
		if batchIdx%100 == 0 {
			nextIn := in
			nextIn.SourceRef["_offset"] = string(offsetRaw)
			return nil, workflow.NewContinueAsNewError(ctx, MigrationWorkflow, nextIn)
		}
	}

	log.Info("MigrationWorkflow done", "processed", result.Processed, "failed", result.Failed)
	_ = workflow.ExecuteActivity(ctx, "FinalizeJob", activities.FinalizeJobInput{
		JobID: in.JobID, Cancelled: result.Cancelled,
	}).Get(ctx, nil)
	return result, nil
}

// preprocessRows runs the template's preprocess steps on the Python sandbox
// tier (task queue "transforms"). Rows pass through untouched when the
// template has no preprocess steps.
func preprocessRows(ctx workflow.Context, tpl template.RuleTemplate, rows []connectors.Row) ([]connectors.Row, error) {
	if len(tpl.Preprocess) == 0 || len(rows) == 0 {
		return rows, nil
	}
	data := make([]map[string]any, len(rows))
	for i, r := range rows {
		data[i] = r.Data
	}
	pyCtx := workflow.WithTaskQueue(ctx, TaskQueueTransforms)
	var out []map[string]any
	if err := workflow.ExecuteActivity(pyCtx, "preprocess_batch", map[string]any{
		"rows":  data,
		"steps": tpl.Preprocess,
	}).Get(ctx, &out); err != nil {
		return nil, err
	}
	if len(out) != len(rows) {
		return nil, fmt.Errorf("preprocess returned %d rows for %d inputs", len(out), len(rows))
	}
	result := make([]connectors.Row, len(rows))
	for i, r := range rows {
		result[i] = connectors.Row{Index: r.Index, Data: out[i]}
	}
	return result, nil
}

// stepOutcome captures what happened to one step for row-level reporting.
type stepOutcome struct {
	failed   bool
	err      string
	payload  map[string]any
	response json.RawMessage
}

// processOneRow executes the template's step chain for one row.
//
// resume, when non-nil, maps step names to persisted responses of previously
// succeeded steps: those steps are skipped and their responses seeded into the
// render context so `$fromResponse` bindings still resolve. This is how a
// retry avoids re-firing non-idempotent earlier calls.
func processOneRow(
	ctx workflow.Context,
	jobID int64,
	tpl template.RuleTemplate,
	row connectors.Row,
	resume map[string]json.RawMessage,
	result *MigrationWorkflowResult,
) error {
	steps := tpl.ExecutionSteps()
	multiStep := len(steps) > 1
	rctx := template.NewContext(row.Data)

	var last stepOutcome
	anyFailed := false
	for i, step := range steps {
		if resume != nil {
			if resp, ok := resume[step.Name]; ok {
				rctx.AddResponseJSON(step.Name, resp)
				continue
			}
		}
		last = executeStep(ctx, jobID, row, i, step, multiStep, rctx)
		if last.failed {
			anyFailed = true
			if !step.ContinuesOnFailure() {
				break
			}
		}
	}

	status := "succeeded"
	if anyFailed {
		status = "failed"
		result.Failed++
	} else {
		result.Processed++
	}

	persistIn := activities.PersistOutcomeInput{
		JobID: jobID, RowIndex: row.Index, Status: status,
		IdempotencyKey: activities.ComputeIdempotencyKey(jobID, row.Index, "row", row.Data),
		Row:            row.Data,
		Payload:        last.payload,
		Response:       last.response,
		LastError:      last.err,
	}
	return workflow.ExecuteActivity(ctx, "PersistOutcome", persistIn).Get(ctx, nil)
}

// executeStep renders and fires one HTTP call of the chain, persisting the
// step outcome when the template is multi-step.
func executeStep(
	ctx workflow.Context,
	jobID int64,
	row connectors.Row,
	stepIndex int,
	step template.Step,
	multiStep bool,
	rctx *template.Context,
) stepOutcome {
	out := stepOutcome{}

	fail := func(msg string, requestURL string, payload map[string]any, respStatus int, respBody json.RawMessage) stepOutcome {
		out.failed = true
		if multiStep {
			out.err = fmt.Sprintf("step %q: %s", step.Name, msg)
		} else {
			out.err = msg
		}
		out.payload = payload
		out.response = respBody
		if multiStep {
			_ = workflow.ExecuteActivity(ctx, "PersistStepOutcome", activities.PersistStepOutcomeInput{
				JobID: jobID, RowIndex: row.Index, StepIndex: stepIndex, StepName: step.Name,
				Status: "failed", RequestURL: requestURL, Request: payload,
				ResponseStatus: respStatus, Response: respBody, LastError: msg,
			}).Get(ctx, nil)
		}
		return out
	}

	payload, err := renderStepPayload(ctx, step, rctx)
	if err != nil {
		// Render failures are deterministic for the row: no retry budget spent.
		return fail(err.Error(), "", nil, 0, nil)
	}

	rendered, err := template.RenderDestination(step.Destination, rctx)
	if err != nil {
		return fail(err.Error(), "", payload, 0, nil)
	}

	idem := activities.ComputeIdempotencyKey(jobID, row.Index, step.Name, payload)
	var call activities.CallEndpointOutput
	callErr := workflow.ExecuteActivity(ctx, "CallEndpoint", activities.CallEndpointInput{
		JobID:             jobID,
		RowIndex:          row.Index,
		StepName:          step.Name,
		Method:            step.Destination.Method,
		URL:               rendered.URL,
		Headers:           rendered.Headers,
		Payload:           payload,
		HasBody:           step.PayloadTemplate() != nil,
		IdempotencyKey:    idem,
		IdempotencyHeader: step.Destination.IdempotencyHeader,
	}).Get(ctx, &call)

	if callErr != nil {
		msg := call.Error
		if msg == "" {
			msg = callErr.Error()
		}
		return fail(msg, rendered.URL, payload, call.Status, call.Body)
	}
	if !call.Succeeded {
		msg := call.Error
		if msg == "" {
			msg = fmt.Sprintf("status %d", call.Status)
		}
		return fail(msg, rendered.URL, payload, call.Status, call.Body)
	}

	if multiStep {
		_ = workflow.ExecuteActivity(ctx, "PersistStepOutcome", activities.PersistStepOutcomeInput{
			JobID: jobID, RowIndex: row.Index, StepIndex: stepIndex, StepName: step.Name,
			Status: "succeeded", RequestURL: rendered.URL, Request: payload,
			ResponseStatus: call.Status, Response: call.Body,
		}).Get(ctx, nil)
	}

	rctx.AddResponse(step.Name, []byte(call.Body))
	out.payload = payload
	out.response = call.Body
	return out
}

// renderStepPayload renders a step's payload template: $from/$fromResponse/
// $literal resolve in Go; any remaining $py expressions are evaluated on the
// Python sandbox tier with the row in scope.
func renderStepPayload(ctx workflow.Context, step template.Step, rctx *template.Context) (map[string]any, error) {
	tplPayload := step.PayloadTemplate()
	if tplPayload == nil {
		return map[string]any{}, nil
	}
	partial, err := template.RenderPayload(tplPayload, rctx)
	if err != nil {
		return nil, err
	}
	if !containsPy(partial) {
		return partial, nil
	}
	pyCtx := workflow.WithTaskQueue(ctx, TaskQueueTransforms)
	var out []map[string]any
	if err := workflow.ExecuteActivity(pyCtx, "render_payload_batch", map[string]any{
		"rows":    []map[string]any{rctx.Row},
		"payload": partial,
	}).Get(ctx, &out); err != nil {
		return nil, fmt.Errorf("python payload eval: %w", err)
	}
	if len(out) != 1 {
		return nil, fmt.Errorf("python payload eval returned %d results", len(out))
	}
	return out[0], nil
}

func containsPy(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["$py"]; ok {
			return true
		}
		for _, vv := range x {
			if containsPy(vv) {
				return true
			}
		}
	case []any:
		for _, vv := range x {
			if containsPy(vv) {
				return true
			}
		}
	}
	return false
}

// runSingleRowRetry re-processes exactly one row using its persisted state:
// the preprocessed row data saved on job_rows plus (unless FromStart) the
// responses of previously-succeeded steps so the chain resumes at the first
// failed step.
func runSingleRowRetry(ctx workflow.Context, in MigrationWorkflowInput) (*MigrationWorkflowResult, error) {
	if in.RetryRow == nil {
		return nil, errors.New("retry row spec missing")
	}
	var tplOut activities.LoadTemplateOutput
	if err := workflow.ExecuteActivity(ctx, "LoadTemplate", activities.LoadTemplateInput{
		OrgID: in.OrgID, RuleTemplateID: in.RuleTemplateID, TemplateVersion: in.RuleTemplateVersion,
	}).Get(ctx, &tplOut); err != nil {
		return nil, err
	}
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions(tplOut.Template.Retry))

	result := &MigrationWorkflowResult{}
	if err := retryPersistedRow(ctx, in.RetryRow.JobID, tplOut.Template, in.RetryRow.RowIndex, in.RetryRow.FromStart, result); err != nil {
		return result, err
	}
	_ = workflow.ExecuteActivity(ctx, "PublishProgress", activities.PublishProgressInput{
		JobID: in.RetryRow.JobID, Processed: result.Processed, Failed: result.Failed,
	}).Get(ctx, nil)
	_ = workflow.ExecuteActivity(ctx, "FinalizeJob", activities.FinalizeJobInput{
		JobID: in.RetryRow.JobID,
	}).Get(ctx, nil)
	return result, nil
}

// RetryFailedRowsInput drives RetryFailedRowsWorkflow.
type RetryFailedRowsInput struct {
	OrgID               int64 `json:"orgId"`
	JobID               int64 `json:"jobId"`
	RuleTemplateID      int64 `json:"ruleTemplateId"`
	RuleTemplateVersion int32 `json:"ruleTemplateVersion"`
	// After is the row_index cursor for continue-as-new.
	After     int64 `json:"after,omitempty"`
	FromStart bool  `json:"fromStart,omitempty"`
}

// RetryFailedRowsWorkflow re-processes every currently-failed row of a job
// using persisted row state (resume-at-failed-step semantics, same as a
// single-row retry). Pages through failed rows and continue-as-news every few
// thousand to keep history bounded.
func RetryFailedRowsWorkflow(ctx workflow.Context, in RetryFailedRowsInput) (*MigrationWorkflowResult, error) {
	ao := defaultActivityOptions(nil)
	ctx = workflow.WithActivityOptions(ctx, ao)

	var tplOut activities.LoadTemplateOutput
	if err := workflow.ExecuteActivity(ctx, "LoadTemplate", activities.LoadTemplateInput{
		OrgID: in.OrgID, RuleTemplateID: in.RuleTemplateID, TemplateVersion: in.RuleTemplateVersion,
	}).Get(ctx, &tplOut); err != nil {
		return nil, fmt.Errorf("load template: %w", err)
	}
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions(tplOut.Template.Retry))

	result := &MigrationWorkflowResult{}
	const rowsPerRun = 2000
	processedThisRun := 0
	cursor := in.After

	for {
		var page activities.ListFailedRowsOutput
		if err := workflow.ExecuteActivity(ctx, "ListFailedRows", activities.ListFailedRowsInput{
			JobID: in.JobID, After: cursor, Limit: 200,
		}).Get(ctx, &page); err != nil {
			return result, fmt.Errorf("list failed rows: %w", err)
		}
		if len(page.RowIndexes) == 0 {
			break
		}
		for _, rowIndex := range page.RowIndexes {
			cursor = rowIndex
			if err := retryPersistedRow(ctx, in.JobID, tplOut.Template, rowIndex, in.FromStart, result); err != nil {
				return result, err
			}
			processedThisRun++
		}
		_ = workflow.ExecuteActivity(ctx, "PublishProgress", activities.PublishProgressInput{
			JobID: in.JobID, Processed: result.Processed, Failed: result.Failed,
		}).Get(ctx, nil)

		if page.Done {
			break
		}
		if processedThisRun >= rowsPerRun {
			next := in
			next.After = cursor
			return nil, workflow.NewContinueAsNewError(ctx, RetryFailedRowsWorkflow, next)
		}
	}
	_ = workflow.ExecuteActivity(ctx, "FinalizeJob", activities.FinalizeJobInput{
		JobID: in.JobID,
	}).Get(ctx, nil)
	return result, nil
}

// retryPersistedRow re-runs one row from its persisted state (shared by the
// single-row retry and the retry-all-failed fan-out).
func retryPersistedRow(
	ctx workflow.Context,
	jobID int64,
	tpl template.RuleTemplate,
	rowIndex int64,
	fromStart bool,
	result *MigrationWorkflowResult,
) error {
	var state activities.LoadRowStateOutput
	if err := workflow.ExecuteActivity(ctx, "LoadRowState", activities.LoadRowStateInput{
		JobID: jobID, RowIndex: rowIndex,
	}).Get(ctx, &state); err != nil {
		return err
	}
	if !state.Found || state.Row == nil {
		msg := fmt.Sprintf("cannot retry row %d: no persisted row data found", rowIndex)
		if err := workflow.ExecuteActivity(ctx, "PersistOutcome", activities.PersistOutcomeInput{
			JobID: jobID, RowIndex: rowIndex, Status: "failed", LastError: msg,
		}).Get(ctx, nil); err != nil {
			return err
		}
		result.Failed++
		return nil
	}
	var resume map[string]json.RawMessage
	if !fromStart {
		resume = map[string]json.RawMessage{}
		for _, s := range state.Steps {
			if s.Status == "succeeded" {
				resume[s.StepName] = s.Response
			}
		}
	}
	row := connectors.Row{Index: rowIndex, Data: state.Row}
	return processOneRow(ctx, jobID, tpl, row, resume, result)
}

// ProcessShardWorkflow lets large jobs run several MigrationWorkflow shards in
// parallel (each scoped to a hash range over the row index).
func ProcessShardWorkflow(ctx workflow.Context, in MigrationWorkflowInput) (*MigrationWorkflowResult, error) {
	return MigrationWorkflow(ctx, in)
}

// RetryRowWorkflow is a thin wrapper that just delegates to MigrationWorkflow
// with a RetryRow input set. Kept as a distinct workflow type so the API can
// dispatch unambiguously.
func RetryRowWorkflow(ctx workflow.Context, in MigrationWorkflowInput) (*MigrationWorkflowResult, error) {
	return MigrationWorkflow(ctx, in)
}

// --------- helpers ---------

// defaultActivityOptions builds Temporal activity options. When retry is nil,
// uses platform defaults (5 attempts, exponential from 1s). Template retry
// overrides maxAttempts / initial interval / backoff coefficient.
func defaultActivityOptions(r *template.Retry) workflow.ActivityOptions {
	maxAttempts := int32(5)
	initial := time.Second
	backoff := 2.0
	if r != nil {
		if r.MaxAttempts > 0 {
			maxAttempts = int32(r.MaxAttempts)
		}
		if r.InitialIntervalMs > 0 {
			initial = time.Duration(r.InitialIntervalMs) * time.Millisecond
		}
		if r.Backoff == "fixed" {
			backoff = 1.0
		}
	}
	return workflow.ActivityOptions{
		StartToCloseTimeout:    2 * time.Minute,
		HeartbeatTimeout:       30 * time.Second,
		ScheduleToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    initial,
			MaximumInterval:    30 * time.Second,
			BackoffCoefficient: backoff,
			MaximumAttempts:    maxAttempts,
		},
	}
}

func extractConnector(sourceRef map[string]any) (connectors.Config, string) {
	cfg := connectors.Config{Kind: "csv", Config: map[string]any{}}
	if sourceRef == nil {
		return cfg, ""
	}
	if kind, ok := sourceRef["type"].(string); ok {
		cfg.Kind = kind
	}
	// Browser uploads land in MinIO with type=minio. Map to csv/json/xml and
	// pass bucket+key so the ingest connector can stream the object.
	if cfg.Kind == "minio" {
		bucket, _ := sourceRef["bucket"].(string)
		key, _ := sourceRef["key"].(string)
		filename, _ := sourceRef["filename"].(string)
		cfg.Kind = connectors.FormatFromFilename(filename)
		cfg.Config["s3_bucket"] = bucket
		cfg.Config["s3_key"] = key
		if cfg.Kind == "csv" {
			cfg.Config["header"] = true
			cfg.Config["delimiter"] = ","
		}
		secret, _ := sourceRef["secret"].(string)
		return cfg, secret
	}
	if extra, ok := sourceRef["extra"].(map[string]any); ok {
		for k, v := range extra {
			cfg.Config[k] = v
		}
	}
	if key, ok := sourceRef["key"].(string); ok {
		cfg.Config["url"] = key
	}
	if url, ok := sourceRef["url"].(string); ok {
		cfg.Config["url"] = url
	}
	if path, ok := sourceRef["path"].(string); ok {
		cfg.Config["path"] = path
	}
	secret, _ := sourceRef["secret"].(string)
	return cfg, secret
}
