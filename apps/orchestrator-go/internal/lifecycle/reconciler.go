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

	"github.com/migration-platform/orchestrator/internal/metrics"
)

// WorkflowDescriber is the part of the Temporal client the reconciler uses.
type WorkflowDescriber interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
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

// Reconciler repairs jobs left pending, running or paused by a workflow that
// ended without finalizing them (worker crash, DB outage during finalize,
// termination, or a start that never happened).
type Reconciler struct {
	DB         *pgxpool.Pool
	Temporal   WorkflowDescriber
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
			return repaired, nil
		}
	}
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
	status, err := finalize(ctx, r.DB, j.id, repair.Outcome, true)
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
	metrics.JobsReconciled.WithLabelValues(workflowStatus, status).Inc()
	r.Log.Warn("job reconciled", "jobId", j.id, "previousStatus", j.status, "workflowId", j.workflowID,
		"workflowStatus", workflowStatus, "status", status, "error", repair.Outcome.Error)
	return true, nil
}
