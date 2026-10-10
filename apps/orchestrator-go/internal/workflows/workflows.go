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
	// Checkpoint is set by continue-as-new; nil starts at the first row.
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
}

type RetryRowRef struct {
	JobID    int64 `json:"jobId"`
	RowIndex int64 `json:"rowIndex"`
	// FromStart forces the whole chain to re-run even when earlier steps
	// succeeded previously. Default (false) resumes at the first failed step
	// so non-idempotent creates are not repeated.
	FromStart bool `json:"fromStart,omitempty"`
	// ConfirmNotApplied records that an operator checked the destination and
	// found no effect from an unresolved operation, so it may be sent again.
	ConfirmNotApplied bool `json:"confirmNotApplied,omitempty"`
}

// Result summarises the workflow outcome.
type MigrationWorkflowResult struct {
	Processed int64 `json:"processed"`
	Failed    int64 `json:"failed"`
	// Unresolved rows stopped at an operation whose effect is unknown.
	Unresolved int64 `json:"unresolved,omitempty"`
	Cancelled  bool  `json:"cancelled"`
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
		if err := requireLicensed(ctx); err != nil {
			return nil, err
		}
		return runSingleRowRetry(ctx, in)
	}

	result, err := runMigration(ctx, in)
	var continued *workflow.ContinueAsNewError
	if errors.As(err, &continued) {
		return nil, err
	}
	if err == nil {
		log.Info("MigrationWorkflow done", "processed", result.Processed, "failed", result.Failed, "unresolved", result.Unresolved)
	}
	return result, finishJob(ctx, in.JobID, err)
}

// runMigration does the work of one run. It returns a ContinueAsNewError to
// hand over, or the error that stopped the job; MigrationWorkflow records the
// outcome either way.
func runMigration(ctx workflow.Context, in MigrationWorkflowInput) (*MigrationWorkflowResult, error) {
	log := workflow.GetLogger(ctx)
	if err := requireLicensed(ctx); err != nil {
		return nil, err
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

	cp, err := startingCheckpoint(in)
	if err != nil {
		return nil, err
	}
	result := &MigrationWorkflowResult{Processed: cp.Processed, Failed: cp.Failed, Unresolved: cp.Unresolved}
	const batchSize = 500
	batchIdx := 0
	pendingPolls := 0
	checkpointed := workflow.GetVersion(ctx, checkpointedIngestChange, workflow.DefaultVersion,
		checkpointedIngestVersion) >= checkpointedIngestVersion
	info := workflow.GetInfo(ctx)
	source := extractConnector(in.SourceRef)

	for {
		// Cooperative pause: spin with a short sleep while paused.
		for paused {
			if err := workflow.Sleep(ctx, 2*time.Second); err != nil {
				return result, err
			}
		}
		startEvents, startBytes := info.GetCurrentHistoryLength(), info.GetCurrentHistorySize()
		continueAsNew := func() error {
			if !checkpointed {
				return nil
			}
			if !shouldContinueAsNew(historySnapshot{
				Events: info.GetCurrentHistoryLength(), Bytes: info.GetCurrentHistorySize(),
				Suggested:  info.GetContinueAsNewSuggested(),
				LastEvents: info.GetCurrentHistoryLength() - startEvents,
				LastBytes:  info.GetCurrentHistorySize() - startBytes,
			}) {
				return nil
			}
			return continueMigrationAsNew(ctx, in, cp, tplOut.Version)
		}

		offsetRaw, err := json.Marshal(cp.Offset)
		if err != nil {
			return result, err
		}
		var ingest activities.IngestOutput
		err = workflow.ExecuteActivity(ctx, "Ingest", activities.IngestInput{
			JobID:                 in.JobID,
			OrgID:                 in.OrgID,
			ConnectorKind:         source.Kind,
			ConnectorConfig:       source.Config,
			SourceID:              source.SourceID,
			ConnectorID:           source.ConnectorID,
			Offset:                offsetRaw,
			BatchSize:             batchSize,
			ExpectedSourceVersion: cp.SourceVersion,
		}).Get(ctx, &ingest)
		if err != nil {
			return result, fmt.Errorf("ingest: %w", err)
		}

		if ingest.Pending {
			pendingPolls++
			cp.Offset = ingest.NewOffset
			if err := workflow.Sleep(ctx, pendingBackoff(pendingPolls)); err != nil {
				return result, err
			}
			if err := continueAsNew(); err != nil {
				return nil, err
			}
			continue
		}
		pendingPolls = 0
		if checkpointed {
			if err := checkRowContinuity(cp.NextRowIndex, ingest.Rows, ingest.NewOffset, ingest.Done); err != nil {
				return result, err
			}
		}

		rows, err := preprocessRows(ctx, tplOut.Template, ingest.Rows)
		if err != nil {
			return result, fmt.Errorf("preprocess: %w", err)
		}

		for _, row := range rows {
			if err := processOneRow(ctx, in.JobID, tplOut.Template, row, rowRun{}, result); err != nil {
				return result, err
			}
		}

		cp.Offset, cp.NextRowIndex = ingest.NewOffset, ingest.NewOffset.Items
		cp.Processed, cp.Failed, cp.Unresolved = result.Processed, result.Failed, result.Unresolved
		if cp.SourceVersion == "" {
			cp.SourceVersion = ingest.SourceVersion
		}

		// Publish aggregate progress once per batch.
		_ = workflow.ExecuteActivity(ctx, "PublishProgress", activities.PublishProgressInput{
			JobID: in.JobID, Processed: result.Processed, Failed: result.Failed + result.Unresolved,
			Pending: 0, LastRow: cp.NextRowIndex,
		}).Get(ctx, nil)

		batchIdx++
		if ingest.Done {
			break
		}
		if checkpointed {
			if err := continueAsNew(); err != nil {
				return nil, err
			}
		} else if batchIdx%100 == 0 {
			return nil, continueMigrationAsNew(ctx, in, cp, tplOut.Version)
		}
	}

	return result, nil
}

// continueMigrationAsNew hands the checkpoint to a fresh run. The template
// version is pinned so a run started on "latest" keeps the same rules.
func continueMigrationAsNew(ctx workflow.Context, in MigrationWorkflowInput, cp Checkpoint, templateVersion int32) error {
	next := in
	next.Checkpoint = &cp
	next.SourceRef = make(map[string]any, len(in.SourceRef))
	for k, v := range in.SourceRef {
		if k != legacyOffsetKey {
			next.SourceRef[k] = v
		}
	}
	if next.RuleTemplateVersion == 0 && templateVersion > 0 {
		next.RuleTemplateVersion = templateVersion
	}
	return workflow.NewContinueAsNewError(ctx, MigrationWorkflow, next)
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
	failed bool
	// unresolved: the step may have been applied; the chain stops (ADR 0003).
	unresolved bool
	err        string
	payload    map[string]any
	response   json.RawMessage
}

// rowRun says how a row is (re-)processed.
//
// resume, when non-nil, maps step names to persisted responses of previously
// succeeded steps: those steps are skipped and their responses seeded into the
// render context so `$fromResponse` bindings still resolve. This is how a
// retry avoids re-firing non-idempotent earlier calls. resend re-applies
// operations that already succeeded (a from-start retry); confirmNotApplied
// lets an unresolved operation be sent.
type rowRun struct {
	resume            map[string]json.RawMessage
	resend            bool
	confirmNotApplied bool
}

// processOneRow executes the template's step chain for one row.
func processOneRow(
	ctx workflow.Context,
	jobID int64,
	tpl template.RuleTemplate,
	row connectors.Row,
	run rowRun,
	result *MigrationWorkflowResult,
) error {
	steps := tpl.ExecutionSteps()
	multiStep := len(steps) > 1
	rctx := template.NewContext(row.Data)
	attemptID := fmt.Sprintf("%s:%d", workflow.GetInfo(ctx).WorkflowExecution.RunID, row.Index)

	var last stepOutcome
	anyFailed, unresolved := false, false
	for i, step := range steps {
		if run.resume != nil {
			if resp, ok := run.resume[step.Name]; ok {
				rctx.AddResponseJSON(step.Name, resp)
				continue
			}
		}
		var err error
		last, err = executeStep(ctx, jobID, row, i, step, multiStep, rctx, run, attemptID)
		if err != nil {
			return err
		}
		if last.unresolved {
			unresolved = true
			break
		}
		if last.failed {
			anyFailed = true
			if !step.ContinuesOnFailure() {
				break
			}
		}
	}

	status := "succeeded"
	switch {
	case unresolved:
		status = "unresolved"
		result.Unresolved++
	case anyFailed:
		status = "failed"
		result.Failed++
	default:
		result.Processed++
	}

	persistIn := activities.PersistOutcomeInput{
		JobID: jobID, RowIndex: row.Index, Status: status,
		IdempotencyKey: activities.ComputeIdempotencyKey(jobID, row.Index, "row", row.Data),
		AttemptID:      attemptID,
		Row:            row.Data,
		Payload:        last.payload,
		Response:       last.response,
		LastError:      last.err,
	}
	if err := workflow.ExecuteActivity(ctx, "PersistOutcome", persistIn).Get(ctx, nil); err != nil {
		return fmt.Errorf("persist row %d outcome: %w", row.Index, err)
	}
	return nil
}

// executeStep renders and fires one HTTP call of the chain, persisting the
// step outcome when the template is multi-step. A returned error means the
// outcome could not be recorded; the row must not continue.
func executeStep(
	ctx workflow.Context,
	jobID int64,
	row connectors.Row,
	stepIndex int,
	step template.Step,
	multiStep bool,
	rctx *template.Context,
	run rowRun,
	rowAttemptID string,
) (stepOutcome, error) {
	out := stepOutcome{}
	persistStep := func(status, requestURL string, payload map[string]any, respStatus int, respBody json.RawMessage, msg string) error {
		if !multiStep {
			return nil
		}
		err := workflow.ExecuteActivity(ctx, "PersistStepOutcome", activities.PersistStepOutcomeInput{
			JobID: jobID, RowIndex: row.Index, StepIndex: stepIndex, StepName: step.Name,
			Status: status, RequestURL: requestURL, Request: payload,
			ResponseStatus: respStatus, Response: respBody, LastError: msg,
			AttemptID: fmt.Sprintf("%s:%d", rowAttemptID, stepIndex),
		}).Get(ctx, nil)
		if err != nil {
			return fmt.Errorf("persist step %q outcome for row %d: %w", step.Name, row.Index, err)
		}
		return nil
	}
	stop := func(status, msg, requestURL string, payload map[string]any, respStatus int, respBody json.RawMessage) (stepOutcome, error) {
		out.failed = status == "failed"
		out.unresolved = status == "unresolved"
		if multiStep {
			out.err = fmt.Sprintf("step %q: %s", step.Name, msg)
		} else {
			out.err = msg
		}
		out.payload = payload
		out.response = respBody
		return out, persistStep(status, requestURL, payload, respStatus, respBody, msg)
	}

	payload, err := renderStepPayload(ctx, step, rctx)
	if err != nil {
		// Render failures are deterministic for the row: no retry budget spent.
		return stop("failed", err.Error(), "", nil, 0, nil)
	}

	rendered, err := template.RenderDestination(step.Destination, rctx)
	if err != nil {
		return stop("failed", err.Error(), "", payload, 0, nil)
	}

	in := activities.CallEndpointInput{
		JobID:             jobID,
		RowIndex:          row.Index,
		StepIndex:         stepIndex,
		StepName:          step.Name,
		Method:            step.Destination.Method,
		URL:               rendered.URL,
		Headers:           rendered.Headers,
		Payload:           payload,
		HasBody:           step.PayloadTemplate() != nil,
		IdempotencyKey:    activities.ComputeIdempotencyKey(jobID, row.Index, step.Name, payload),
		IdempotencyHeader: step.Destination.IdempotencyHeader,
		DeliveryMode:      step.Destination.EffectiveDeliveryMode(),
		Resend:            run.resend,
		ConfirmNotApplied: run.confirmNotApplied,
	}
	if d := step.Destination.Delivery; d != nil {
		in.RetentionHours = d.RetentionHours()
		if d.Lookup != nil {
			lookup, err := template.RenderLookup(*d.Lookup, rctx)
			if err != nil {
				return stop("failed", "delivery.lookup: "+err.Error(), rendered.URL, payload, 0, nil)
			}
			in.Lookup = &activities.RenderedLookup{
				Method: d.Lookup.Method, URL: lookup.URL, Headers: lookup.Headers, FoundPath: d.Lookup.FoundPath,
			}
		}
	}
	if a := step.Destination.Auth; a != nil {
		in.Auth = &activities.DestinationAuth{Type: a.Type, SecretRef: a.SecretRef, HeaderName: a.HeaderName}
	}

	var call activities.CallEndpointOutput
	callErr := workflow.ExecuteActivity(ctx, "CallEndpoint", in).Get(ctx, &call)
	if callErr != nil {
		msg := call.Error
		if msg == "" {
			msg = callErr.Error()
		}
		if effectMayExist(callErr) {
			return stop("unresolved", msg, rendered.URL, payload, call.Status, call.Body)
		}
		return stop("failed", msg, rendered.URL, payload, call.Status, call.Body)
	}
	switch {
	case call.Outcome == activities.CallUnresolved:
		return stop("unresolved", call.Error, rendered.URL, payload, call.Status, call.Body)
	case !call.Succeeded:
		msg := call.Error
		if msg == "" {
			msg = fmt.Sprintf("status %d", call.Status)
		}
		return stop("failed", msg, rendered.URL, payload, call.Status, call.Body)
	}

	if err := persistStep("succeeded", rendered.URL, payload, call.Status, call.Body, ""); err != nil {
		return out, err
	}
	rctx.AddResponse(step.Name, []byte(call.Body))
	out.payload = payload
	out.response = call.Body
	return out, nil
}

// effectMayExist reports whether a failed CallEndpoint may still have been
// applied by the destination: its last attempt saw an unknown outcome, could
// not take the operation's lease, or timed out while possibly sending.
func effectMayExist(err error) bool {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		switch appErr.Type() {
		case activities.ErrTypeOutcomeUnknown, activities.ErrTypeOperationBusy:
			return true
		}
		return false
	}
	var timeoutErr *temporal.TimeoutError
	return errors.As(err, &timeoutErr)
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
	if err := retryPersistedRow(ctx, in.RetryRow.JobID, tplOut.Template, in.RetryRow.RowIndex,
		in.RetryRow.FromStart, in.RetryRow.ConfirmNotApplied, result); err != nil {
		return result, err
	}
	_ = workflow.ExecuteActivity(ctx, "PublishProgress", activities.PublishProgressInput{
		JobID: in.RetryRow.JobID, Processed: result.Processed, Failed: result.Failed + result.Unresolved,
	}).Get(ctx, nil)
	return result, finishJob(ctx, in.RetryRow.JobID, nil)
}

// RetryFailedRowsInput drives RetryFailedRowsWorkflow.
type RetryFailedRowsInput struct {
	OrgID               int64 `json:"orgId"`
	JobID               int64 `json:"jobId"`
	RuleTemplateID      int64 `json:"ruleTemplateId"`
	RuleTemplateVersion int32 `json:"ruleTemplateVersion"`
	// After is the exclusive row_index cursor carried across continue-as-new.
	// nil means "start before the first row"; row indexes are zero-based, so a
	// zero value cannot double as the initial cursor.
	After     *int64 `json:"after,omitempty"`
	FromStart bool   `json:"fromStart,omitempty"`
}

// RetryFailedRowsWorkflow re-processes every currently-failed row of a job
// using persisted row state (resume-at-failed-step semantics, same as a
// single-row retry). Pages through failed rows and continue-as-news every few
// thousand to keep history bounded.
func RetryFailedRowsWorkflow(ctx workflow.Context, in RetryFailedRowsInput) (*MigrationWorkflowResult, error) {
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions(nil))
	if err := requireLicensed(ctx); err != nil {
		return nil, err
	}
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
	// Send an explicit -1 rather than omitting the cursor: activity workers
	// built before MP-09 decode a missing cursor as 0 and would skip row zero.
	cursor := in.After
	if cursor == nil {
		start := int64(-1)
		cursor = &start
	}

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
			cursor = &rowIndex
			if err := retryPersistedRow(ctx, in.JobID, tplOut.Template, rowIndex, in.FromStart, false, result); err != nil {
				return result, err
			}
			processedThisRun++
		}
		_ = workflow.ExecuteActivity(ctx, "PublishProgress", activities.PublishProgressInput{
			JobID: in.JobID, Processed: result.Processed, Failed: result.Failed + result.Unresolved,
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
	return result, finishJob(ctx, in.JobID, nil)
}

// retryPersistedRow re-runs one row from its persisted state (shared by the
// single-row retry and the retry-all-failed fan-out).
func retryPersistedRow(
	ctx workflow.Context,
	jobID int64,
	tpl template.RuleTemplate,
	rowIndex int64,
	fromStart, confirmNotApplied bool,
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
	run := rowRun{resend: fromStart, confirmNotApplied: confirmNotApplied}
	if !fromStart {
		run.resume = map[string]json.RawMessage{}
		for _, s := range state.Steps {
			if s.Status == "succeeded" {
				run.resume[s.StepName] = s.Response
			}
		}
	}
	row := connectors.Row{Index: rowIndex, Data: state.Row}
	return processOneRow(ctx, jobID, tpl, row, run, result)
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
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions(nil))
	if err := requireLicensed(ctx); err != nil {
		return nil, err
	}
	return MigrationWorkflow(ctx, in)
}

func requireLicensed(ctx workflow.Context) error {
	if err := workflow.ExecuteActivity(ctx, "RequireLicensed").Get(ctx, nil); err != nil {
		return fmt.Errorf("runtime license gate: %w", err)
	}
	return nil
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

// ingestSource is what the workflow forwards to Ingest. It names a source;
// the activity re-authorizes it and loads any credentials (ADR 0001).
type ingestSource struct {
	Kind        string
	Config      map[string]any
	SourceID    int64
	ConnectorID int64
}

func extractConnector(sourceRef map[string]any) ingestSource {
	src := ingestSource{Kind: "csv", Config: map[string]any{}}
	if sourceRef == nil {
		return src
	}
	kind, _ := sourceRef["type"].(string)
	src.SourceID = int64Field(sourceRef, "source_id")
	switch kind {
	case "minio":
		bucket, _ := sourceRef["bucket"].(string)
		key, _ := sourceRef["key"].(string)
		filename, _ := sourceRef["filename"].(string)
		src.Kind = connectors.FormatFromFilename(filename)
		src.Config["s3_bucket"] = bucket
		src.Config["s3_key"] = key
		if src.Kind == "csv" {
			src.Config["header"] = true
			src.Config["delimiter"] = ","
		}
		return src
	case "connector", "salesforce":
		src.Kind = "salesforce"
		src.ConnectorID = int64Field(sourceRef, "connector_id")
		if src.ConnectorID == 0 {
			src.ConnectorID = int64Field(sourceRef, "connectorId")
		}
	default:
		// Legacy shapes are passed through so Ingest can refuse them with a
		// clear, non-retryable error instead of the workflow guessing.
		if kind != "" {
			src.Kind = kind
		}
		for _, k := range []string{"url", "path"} {
			if v, ok := sourceRef[k].(string); ok {
				src.Config[k] = v
			}
		}
	}
	if extra, ok := sourceRef["extra"].(map[string]any); ok {
		for k, v := range extra {
			src.Config[k] = v
		}
	}
	return src
}

// int64Field reads a JSON number that may have decoded as float64.
func int64Field(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}
