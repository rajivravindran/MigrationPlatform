package workflows

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
)

// finalizeOnExitVersion gates, via workflow.GetVersion, finalizing the job on
// every exit (failure and cancellation included) on a disconnected context,
// propagating FinalizeJob errors and recording export failures. Histories
// recorded before it replay the old success-only, errors-ignored path.
const (
	finalizeOnExitChange  = "finalize-on-exit"
	finalizeOnExitVersion = 1
)

const maxJobErrorLen = 2000

// jobOutcome turns how execution ended into the outcome FinalizeJob records.
func jobOutcome(ctx workflow.Context, jobID int64, runErr error) activities.FinalizeJobInput {
	in := activities.FinalizeJobInput{JobID: jobID}
	switch {
	case runErr == nil:
	case temporal.IsCanceledError(runErr) || errors.Is(ctx.Err(), workflow.ErrCanceled):
		in.Cancelled = true
	default:
		in.Error = failureMessage(runErr)
	}
	return in
}

// failureMessage is the operator-facing text for an execution failure.
func failureMessage(err error) string {
	msg := err.Error()
	if len(msg) > maxJobErrorLen {
		msg = msg[:maxJobErrorLen]
	}
	return msg
}

// finishActivityOptions keep trying to record the outcome for longer than a
// row activity would: a job left unfinalized needs the reconciler.
func finishActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToCloseTimeout: 15 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
		},
	}
}

// exportActivityOptions bound the results upload, whose failure is recorded
// rather than failing the job.
func exportActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    5 * time.Minute,
		HeartbeatTimeout:       time.Minute,
		ScheduleToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
}

// finishJob records the job outcome once execution has ended, however it
// ended, and then exports results. It runs on a disconnected context so a
// cancelled workflow still records its outcome. A FinalizeJob failure is
// returned (the workflow fails and the reconciler repairs the job); an
// export failure is recorded on the job and does not change its outcome.
// runErr is returned unchanged when finalizing succeeds.
//
// The returned verdict is the one FinalizeJob persisted (status, reason,
// totals). It is nil when the outcome was not recorded by this run: the
// legacy pre-"finalize-on-exit" history path, a FinalizeJob failure, or a
// FinalizeJob worker built before the activity returned a verdict. Callers
// surface it in their result so parents consume the persisted decision
// instead of reconstructing one from counters (ADR 0004, review A1).
func finishJob(ctx workflow.Context, jobID int64, runErr error) (*activities.FinalizeJobOutput, error) {
	if workflow.GetVersion(ctx, finalizeOnExitChange, workflow.DefaultVersion, finalizeOnExitVersion) == workflow.DefaultVersion {
		if runErr == nil {
			legacyFinalizeAndExport(ctx, jobID)
		}
		return nil, runErr
	}

	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	outcome := jobOutcome(ctx, jobID, runErr)
	fctx := workflow.WithActivityOptions(dctx, finishActivityOptions())
	var verdict *activities.FinalizeJobOutput
	if err := workflow.ExecuteActivity(fctx, "FinalizeJob", outcome).Get(fctx, &verdict); err != nil {
		workflow.GetLogger(ctx).Error("FinalizeJob failed; the reconciler will repair the job", "jobId", jobID, "err", err)
		if runErr != nil {
			return nil, fmt.Errorf("%w (and finalize job failed: %v)", runErr, err)
		}
		return nil, fmt.Errorf("finalize job: %w", err)
	}
	if verdict != nil && verdict.Status == "" {
		// An older FinalizeJob worker returned nothing: no persisted verdict
		// is known to this run.
		verdict = nil
	}
	if err := exportResults(dctx, jobID); err != nil {
		if runErr != nil {
			return verdict, runErr
		}
		return verdict, err
	}
	return verdict, runErr
}

// applyVerdict copies the persisted verdict onto the workflow result.
func applyVerdict(result *MigrationWorkflowResult, verdict *activities.FinalizeJobOutput) {
	if result == nil || verdict == nil {
		return
	}
	result.Status = verdict.Status
	result.Reason = verdict.Reason
}

// exportResults uploads the results files and records a failure on the job
// when the upload cannot complete. It makes no destination calls, so it is
// also the whole of ExportResultsWorkflow.
func exportResults(ctx workflow.Context, jobID int64) error {
	in := activities.ExportJobResultsInput{JobID: jobID}
	fctx := workflow.WithActivityOptions(ctx, finishActivityOptions())
	if err := workflow.ExecuteActivity(fctx, "RecordExportStarted", in).Get(fctx, nil); err != nil {
		return fmt.Errorf("record export started: %w", err)
	}
	ectx := workflow.WithActivityOptions(ctx, exportActivityOptions())
	exportErr := workflow.ExecuteActivity(ectx, "ExportJobResults", in).Get(ectx, nil)
	if exportErr == nil {
		return nil
	}
	workflow.GetLogger(ctx).Warn("results export failed", "jobId", jobID, "err", exportErr)
	if err := workflow.ExecuteActivity(fctx, "RecordExportFailure", activities.RecordExportFailureInput{
		JobID: jobID, Error: failureMessage(exportErr),
	}).Get(fctx, nil); err != nil {
		return fmt.Errorf("record export failure: %w", err)
	}
	return nil
}

func legacyFinalizeAndExport(ctx workflow.Context, jobID int64) {
	_ = workflow.ExecuteActivity(ctx, "FinalizeJob", activities.FinalizeJobInput{JobID: jobID}).Get(ctx, nil)
	_ = workflow.ExecuteActivity(ctx, "ExportJobResults", activities.ExportJobResultsInput{JobID: jobID}).Get(ctx, nil)
}

// ExportResultsInput drives ExportResultsWorkflow.
type ExportResultsInput struct {
	JobID int64 `json:"jobId"`
}

// ExportResultsWorkflow re-runs only the results export for a finished job.
// It touches no destination, so retrying an export cannot repeat a write.
func ExportResultsWorkflow(ctx workflow.Context, in ExportResultsInput) error {
	if in.JobID <= 0 {
		return temporal.NewNonRetryableApplicationError("jobId is required", "InvalidInput", nil)
	}
	return exportResults(ctx, in.JobID)
}
