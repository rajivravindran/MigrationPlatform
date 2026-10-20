package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	"github.com/migration-platform/orchestrator/internal/control"
	"github.com/migration-platform/orchestrator/internal/metrics"
)

// WorkflowDescriber is the part of the Temporal client the reconciler uses.
type WorkflowDescriber interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// WorkflowController adds what control-command reconciliation needs: reading
// the workflow's pause state and re-sending an operator command whose
// delivery was never confirmed (architecture review A3). client.Client
// satisfies it.
type WorkflowController interface {
	WorkflowDescriber
	QueryWorkflow(ctx context.Context, workflowID, runID, queryType string, args ...interface{}) (converter.EncodedValue, error)
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

// Repair is what the reconciler does with a stale non-terminal job.
type Repair struct {
	// Finalize is false when the workflow is still running and owns the job.
	Finalize bool
	Outcome  Outcome
}

// RepairFor decides a repair from the state of the job's workflow. found is
// false when Temporal has no execution with the job's workflow ID: it never
// started, or its history expired past retention.
func RepairFor(status enums.WorkflowExecutionStatus, found bool) Repair {
	if !found {
		return Repair{Finalize: true, Outcome: Outcome{Error: "workflow not found: it never started or its history has expired"}}
	}
	switch status {
	case enums.WORKFLOW_EXECUTION_STATUS_RUNNING, enums.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return Repair{}
	case enums.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		// Completed without finalizing (older builds ignored FinalizeJob
		// errors): the rows are the outcome.
		return Repair{Finalize: true}
	case enums.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return Repair{Finalize: true, Outcome: Outcome{Cancelled: true}}
	case enums.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return Repair{Finalize: true, Outcome: Outcome{Error: "workflow was terminated before recording its outcome"}}
	case enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return Repair{Finalize: true, Outcome: Outcome{Error: "workflow timed out before recording its outcome"}}
	default:
		return Repair{Finalize: true, Outcome: Outcome{Error: "workflow failed before recording its outcome"}}
	}
}

// reconcileLockKey serialises passes across orchestrator replicas.
const reconcileLockKey = 7_301_202_605

const reconcilePageSize = 200

// Reconciler repairs jobs left pending, running or paused, and batches left
// pending or running, by a workflow that ended without finalizing them
// (worker crash, DB outage during finalize, termination, or a start that
// never happened). Batch repair uses the same stage-derived rule as the
// workflow (lifecycle.FinalizeBatch), so the two cannot disagree (review A2).
type Reconciler struct {
	DB         *pgxpool.Pool
	Temporal   WorkflowController
	Interval   time.Duration
	StaleAfter time.Duration
	Log        *slog.Logger
}

// Run reconciles every Interval until ctx is done.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		if _, err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			metrics.ReconcileErrors.Inc()
			r.Log.Error("job reconcile pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass checks every stale non-terminal job once and returns how many it
// repaired. Only one replica runs a pass at a time.
func (r *Reconciler) Pass(ctx context.Context) (int, error) {
	conn, err := r.DB.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, reconcileLockKey).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, nil
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, reconcileLockKey); err != nil {
			r.Log.Warn("job reconcile unlock failed", "err", err)
		}
	}()

	repaired := 0
	var after int64
	for {
		jobs, err := r.staleJobs(ctx, after)
		if err != nil {
			return repaired, err
		}
		for _, j := range jobs {
			after = j.id
			ok, err := r.reconcile(ctx, j)
			if err != nil {
				if ctx.Err() != nil {
					return repaired, ctx.Err()
				}
				metrics.ReconcileErrors.Inc()
				r.Log.Error("job reconcile failed", "jobId", j.id, "workflowId", j.workflowID, "err", err)
				continue
			}
			if ok {
				repaired++
			}
		}
		if len(jobs) < reconcilePageSize {
			break
		}
	}
	after = 0
	for {
		cmds, err := r.pendingControls(ctx, after)
		if err != nil {
			return repaired, err
		}
		for _, c := range cmds {
			after = c.id
			ok, err := r.reconcileControl(ctx, c)
			if err != nil {
				if ctx.Err() != nil {
					return repaired, ctx.Err()
				}
				metrics.ReconcileErrors.Inc()
				r.Log.Error("control reconcile failed", "jobId", c.id, "workflowId", c.workflowID, "err", err)
				continue
			}
			if ok {
				repaired++
			}
		}
		if len(cmds) < reconcilePageSize {
			break
		}
	}
	after = 0
	for {
		batches, err := r.staleBatches(ctx, after)
		if err != nil {
			return repaired, err
		}
		for _, b := range batches {
			after = b.id
			ok, err := r.reconcileBatch(ctx, b)
			if err != nil {
				if ctx.Err() != nil {
					return repaired, ctx.Err()
				}
				metrics.ReconcileErrors.Inc()
				r.Log.Error("batch reconcile failed", "batchId", b.id, "workflowId", b.workflowID, "err", err)
				continue
			}
			if ok {
				repaired++
			}
		}
		if len(batches) < reconcilePageSize {
			return repaired, nil
		}
	}
}

// staleBatch mirrors staleJob for archive packages.
type staleBatch = staleJob

func (r *Reconciler) staleBatches(ctx context.Context, after int64) ([]staleBatch, error) {
	rows, err := r.DB.Query(ctx, `
        SELECT id, status::text, COALESCE(temporal_workflow_id, '')
        FROM batches
        WHERE status IN ('pending'::batch_status, 'running'::batch_status)
          AND updated_at < now() - make_interval(secs => $1)
          AND id > $2
        ORDER BY id
        LIMIT $3`, r.StaleAfter.Seconds(), after, reconcilePageSize)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (staleBatch, error) {
		var b staleBatch
		err := row.Scan(&b.id, &b.status, &b.workflowID)
		return b, err
	})
}

// describe reports the state of a workflow execution; found is false when
// Temporal has none with that ID.
func (r *Reconciler) describe(ctx context.Context, workflowID string) (enums.WorkflowExecutionStatus, bool, error) {
	if workflowID == "" {
		return enums.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, false, nil
	}
	desc, err := r.Temporal.DescribeWorkflowExecution(ctx, workflowID, "")
	var notFound *serviceerror.NotFound
	switch {
	case errors.As(err, &notFound):
		return enums.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, false, nil
	case err != nil:
		return enums.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, false, fmt.Errorf("describe workflow: %w", err)
	}
	return desc.GetWorkflowExecutionInfo().GetStatus(), true, nil
}

// reconcileBatch converges a stale non-terminal batch to the outcome its
// workflow actually reached. A running workflow owns the batch (a pending
// batch whose start was never recorded becomes running); otherwise the
// batch is finalized from its persisted stages, with stages the workflow
// abandoned recorded as failed. It never restarts stage work: child jobs are
// repaired by the job pass, and uncertain external effects stay visible.
func (r *Reconciler) reconcileBatch(ctx context.Context, b staleBatch) (bool, error) {
	wfStatus, found, err := r.describe(ctx, b.workflowID)
	if err != nil {
		return false, err
	}
	repair := RepairFor(wfStatus, found)
	if !repair.Finalize {
		if b.status != "pending" {
			return false, nil
		}
		tag, err := r.DB.Exec(ctx, `
            UPDATE batches SET status = 'running'::batch_status, started_at = COALESCE(started_at, now()), updated_at = now()
            WHERE id = $1 AND status = 'pending'::batch_status`, b.id)
		if err != nil || tag.RowsAffected() == 0 {
			return false, err
		}
		metrics.BatchesReconciled.WithLabelValues(wfStatus.String(), "running").Inc()
		r.Log.Warn("batch reconciled", "batchId", b.id, "previousStatus", b.status, "workflowId", b.workflowID, "status", "running")
		return true, nil
	}
	verdict, err := finalizeBatch(ctx, r.DB, b.id, repair.Outcome, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	workflowStatus := "not_found"
	if found {
		workflowStatus = wfStatus.String()
	}
	metrics.BatchesReconciled.WithLabelValues(workflowStatus, verdict.Status).Inc()
	r.Log.Warn("batch reconciled", "batchId", b.id, "previousStatus", b.status, "workflowId", b.workflowID,
		"workflowStatus", workflowStatus, "status", verdict.Status, "reason", verdict.Reason, "error", repair.Outcome.Error)
	return true, nil
}

type staleJob struct {
	id         int64
	status     string
	workflowID string
}

func (r *Reconciler) staleJobs(ctx context.Context, after int64) ([]staleJob, error) {
	rows, err := r.DB.Query(ctx, `
        SELECT id, status::text, COALESCE(temporal_workflow_id, '')
        FROM jobs
        WHERE status IN ('pending'::job_status, 'running'::job_status, 'paused'::job_status)
          AND updated_at < now() - make_interval(secs => $1)
          AND id > $2
        ORDER BY id
        LIMIT $3`, r.StaleAfter.Seconds(), after, reconcilePageSize)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (staleJob, error) {
		var j staleJob
		err := row.Scan(&j.id, &j.status, &j.workflowID)
		return j, err
	})
}

func (r *Reconciler) reconcile(ctx context.Context, j staleJob) (bool, error) {
	wfStatus, found := enums.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, false
	if j.workflowID != "" {
		desc, err := r.Temporal.DescribeWorkflowExecution(ctx, j.workflowID, "")
		var notFound *serviceerror.NotFound
		switch {
		case errors.As(err, &notFound):
		case err != nil:
			return false, fmt.Errorf("describe workflow: %w", err)
		default:
			found = true
			wfStatus = desc.GetWorkflowExecutionInfo().GetStatus()
		}
	}
	repair := RepairFor(wfStatus, found)
	if !repair.Finalize {
		if j.status != "pending" || wfStatus != enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
			return false, nil
		}
		// The start succeeded but its caller never recorded it.
		tag, err := r.DB.Exec(ctx, `
            UPDATE jobs SET status = 'running'::job_status, started_at = COALESCE(started_at, now()), updated_at = now()
            WHERE id = $1 AND status = 'pending'::job_status`, j.id)
		if err != nil || tag.RowsAffected() == 0 {
			return false, err
		}
		metrics.JobsReconciled.WithLabelValues(wfStatus.String(), "running").Inc()
		r.Log.Warn("job reconciled", "jobId", j.id, "previousStatus", j.status, "workflowId", j.workflowID, "status", "running")
		return true, nil
	}
	verdict, err := finalize(ctx, r.DB, j.id, repair.Outcome, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	workflowStatus := "not_found"
	if found {
		workflowStatus = wfStatus.String()
	}
	metrics.JobsReconciled.WithLabelValues(workflowStatus, verdict.Status).Inc()
	r.Log.Warn("job reconciled", "jobId", j.id, "previousStatus", j.status, "workflowId", j.workflowID,
		"workflowStatus", workflowStatus, "status", verdict.Status, "reason", verdict.Reason, "error", repair.Outcome.Error)
	return true, nil
}

// pendingControl is a job whose last pause/resume request was recorded but
// never confirmed against the workflow (the route failed between the signal
// and the status write, or before the signal).
type pendingControl struct {
	id         int64
	status     string
	workflowID string
	requested  string
	commandID  string
}

func (r *Reconciler) pendingControls(ctx context.Context, after int64) ([]pendingControl, error) {
	rows, err := r.DB.Query(ctx, `
        SELECT id, status::text, COALESCE(temporal_workflow_id, ''), control_requested, COALESCE(control_command_id, '')
        FROM jobs
        WHERE control_requested IS NOT NULL
          AND control_requested_at < now() - make_interval(secs => $1)
          AND id > $2
        ORDER BY id
        LIMIT $3`, r.StaleAfter.Seconds(), after, reconcilePageSize)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (pendingControl, error) {
		var c pendingControl
		err := row.Scan(&c.id, &c.status, &c.workflowID, &c.requested, &c.commandID)
		return c, err
	})
}

// reconcileControl converges a job with an unconfirmed pause/resume request.
// The workflow's own answer decides: a pause it has applied makes the job
// paused; a resume it has applied makes the job running. A request the
// workflow never received is re-sent (it is the operator's recorded intent
// and the signal is idempotent); a request against an ended workflow is
// dropped, since the terminal verdict supersedes it.
func (r *Reconciler) reconcileControl(ctx context.Context, c pendingControl) (bool, error) {
	wfStatus, found, err := r.describe(ctx, c.workflowID)
	if err != nil {
		return false, err
	}
	if !found || (wfStatus != enums.WORKFLOW_EXECUTION_STATUS_RUNNING && wfStatus != enums.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW) {
		return r.clearControl(ctx, c, "workflow is not running; the terminal verdict supersedes the request")
	}
	val, err := r.Temporal.QueryWorkflow(ctx, c.workflowID, "", control.QueryPauseState)
	if err != nil {
		return false, fmt.Errorf("query pause state: %w", err)
	}
	var state control.PauseState
	if err := val.Get(&state); err != nil {
		return false, fmt.Errorf("decode pause state: %w", err)
	}
	wantPaused := c.requested == control.RequestPause
	applied := state.LastCommandID != "" && state.LastCommandID == c.commandID
	if !applied && state.Paused != wantPaused {
		// The signal never arrived: deliver the recorded intent, confirm on a later pass.
		signal := control.SignalResume
		if wantPaused {
			signal = control.SignalPause
		}
		if err := r.Temporal.SignalWorkflow(ctx, c.workflowID, "", signal, control.Command{CommandID: c.commandID}); err != nil {
			return false, fmt.Errorf("re-send %s: %w", signal, err)
		}
		r.Log.Warn("control command re-sent", "jobId", c.id, "workflowId", c.workflowID, "command", c.requested, "commandId", c.commandID)
		return false, nil
	}
	if state.Paused != wantPaused {
		// A later command superseded this one inside the workflow; the
		// persisted status follows the workflow.
		r.Log.Warn("control request superseded by workflow state", "jobId", c.id, "requested", c.requested, "paused", state.Paused)
	}
	newStatus := "running"
	if state.Paused {
		newStatus = "paused"
	}
	tag, err := r.DB.Exec(ctx, `
        UPDATE jobs SET status = $2::job_status,
            paused_at = CASE WHEN $2 = 'paused' THEN COALESCE(paused_at, now()) ELSE paused_at END,
            control_requested = NULL, control_command_id = NULL, control_requested_at = NULL, updated_at = now()
        WHERE id = $1 AND status IN ('running'::job_status, 'paused'::job_status) AND control_command_id = $3`,
		c.id, newStatus, c.commandID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	metrics.JobsReconciled.WithLabelValues("control_"+c.requested, newStatus).Inc()
	r.Log.Warn("job control reconciled", "jobId", c.id, "previousStatus", c.status, "status", newStatus, "commandId", c.commandID)
	return true, nil
}

func (r *Reconciler) clearControl(ctx context.Context, c pendingControl, why string) (bool, error) {
	tag, err := r.DB.Exec(ctx, `
        UPDATE jobs SET control_requested = NULL, control_command_id = NULL, control_requested_at = NULL, updated_at = now()
        WHERE id = $1 AND control_command_id = $2`, c.id, c.commandID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	r.Log.Warn("job control request dropped", "jobId", c.id, "command", c.requested, "reason", why)
	return true, nil
}
