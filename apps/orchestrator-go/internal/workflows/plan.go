package workflows

import (
	"errors"
	"fmt"
	"strings"
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
)

// PlanGatePollInterval is how often a waiting plan run re-checks its
// prerequisites. Prerequisite completion is a database fact written by other
// workflows, so polling keeps this workflow independent of them.
var PlanGatePollInterval = 15 * time.Second

// planGatePollsPerRun bounds history: a run that waits for days continues as
// new instead of accumulating timer events.
const planGatePollsPerRun = 400

type PlanRunWorkflowInput struct {
	OrgID     int64 `json:"orgId"`
	PlanRunID int64 `json:"planRunId"`
}

type PlanRunWorkflowResult struct {
	Status    string  `json:"status"`
	RuleMet   bool    `json:"ruleMet"`
	FailedPct float64 `json:"failedPct"`
	Reason    string  `json:"reason,omitempty"`
	JobID     int64   `json:"jobId,omitempty"`
}

// PlanRunWorkflow holds a plan run until every step it depends on has a run
// that met its completion rule, then runs the file as an ordinary migration
// job and records whether this run met its own step's rule.
func PlanRunWorkflow(ctx workflow.Context, in PlanRunWorkflowInput) (*PlanRunWorkflowResult, error) {
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions(nil))
	if err := requireLicensed(ctx); err != nil {
		return nil, err
	}

	for polls := 0; ; polls++ {
		var gate activities.CheckPlanRunGateOutput
		if err := workflow.ExecuteActivity(ctx, "CheckPlanRunGate", activities.CheckPlanRunGateInput{PlanRunID: in.PlanRunID}).Get(ctx, &gate); err != nil {
			return nil, fmt.Errorf("check plan run gate: %w", err)
		}
		if gate.Stop {
			return &PlanRunWorkflowResult{Status: "stopped", Reason: gate.Reason}, nil
		}
		if gate.Ready {
			break
		}
		if polls >= planGatePollsPerRun {
			return nil, workflow.NewContinueAsNewError(ctx, PlanRunWorkflow, in)
		}
		if err := workflow.Sleep(ctx, PlanGatePollInterval); err != nil {
			if temporal.IsCanceledError(err) {
				return cancelWaitingPlanRun(ctx, in.PlanRunID)
			}
			return nil, err
		}
	}

	var started activities.StartPlanRunJobOutput
	if err := workflow.ExecuteActivity(ctx, "StartPlanRunJob", activities.StartPlanRunJobInput{PlanRunID: in.PlanRunID}).Get(ctx, &started); err != nil {
		return nil, finalizePlanRunAfter(ctx, in.PlanRunID, 0, err)
	}
	if started.Stop {
		return &PlanRunWorkflowResult{Status: "stopped", Reason: "plan run left waiting before it started"}, nil
	}

	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:               started.WorkflowID,
		TaskQueue:                "migration",
		WorkflowExecutionTimeout: 24 * time.Hour,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		RetryPolicy:              &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	child := workflow.ExecuteChildWorkflow(childCtx, MigrationWorkflow, MigrationWorkflowInput{
		OrgID:               started.OrgID,
		JobID:               started.JobID,
		RuleTemplateID:      started.RuleTemplateID,
		RuleTemplateVersion: started.RuleTemplateVersion,
		ScheduleID:          started.ScheduleID,
		SourceRef:           started.SourceRef,
	})
	var exec workflow.Execution
	if err := child.GetChildWorkflowExecution().Get(ctx, &exec); err != nil {
		var already *temporal.ChildWorkflowExecutionAlreadyStartedError
		if !(temporal.IsWorkflowExecutionAlreadyStartedError(err) || errors.As(err, &already) ||
			strings.Contains(err.Error(), "already started")) {
			return nil, finalizePlanRunAfter(ctx, in.PlanRunID, started.JobID, err)
		}
	} else if err := workflow.ExecuteActivity(ctx, "MarkJobRunning", activities.MarkJobRunningInput{
		JobID: started.JobID, RunID: exec.RunID,
	}).Get(ctx, nil); err != nil {
		return nil, fmt.Errorf("mark plan run job running: %w", err)
	}

	var childResult MigrationWorkflowResult
	childErr := child.Get(ctx, &childResult)

	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	fin := activities.FinalizePlanRunInput{PlanRunID: in.PlanRunID, JobID: started.JobID}
	switch {
	case childErr == nil:
	case temporal.IsCanceledError(childErr) || errors.Is(ctx.Err(), workflow.ErrCanceled):
		fin.Cancelled = true
	default:
		fin.ExecError = failureMessage(childErr)
	}
	var out activities.FinalizePlanRunOutput
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(dctx, finishActivityOptions()), "FinalizePlanRun", fin).Get(dctx, &out); err != nil {
		return nil, fmt.Errorf("finalize plan run %d: %w", in.PlanRunID, err)
	}
	return &PlanRunWorkflowResult{Status: out.Status, RuleMet: out.RuleMet, FailedPct: out.FailedPct, Reason: out.Reason, JobID: started.JobID}, nil
}

func cancelWaitingPlanRun(ctx workflow.Context, planRunID int64) (*PlanRunWorkflowResult, error) {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(dctx, finishActivityOptions()), "CancelPlanRun",
		activities.CancelPlanRunInput{PlanRunID: planRunID}).Get(dctx, nil)
	if err != nil {
		return nil, fmt.Errorf("record plan run %d cancelled: %w", planRunID, err)
	}
	return &PlanRunWorkflowResult{Status: "cancelled", Reason: "cancelled before it started"}, nil
}

// finalizePlanRunAfter records a run that could not start its job as failed
// and returns the original error.
func finalizePlanRunAfter(ctx workflow.Context, planRunID, jobID int64, cause error) error {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	in := activities.FinalizePlanRunInput{PlanRunID: planRunID, JobID: jobID, ExecError: failureMessage(cause)}
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(dctx, finishActivityOptions()), "FinalizePlanRun", in).Get(dctx, nil); err != nil {
		return fmt.Errorf("%w (and recording the failure: %v)", cause, err)
	}
	return cause
}

type PlanArchiveWorkflowInput struct {
	OrgID     int64                              `json:"orgId"`
	IntakeID  int64                              `json:"intakeId"`
	Bucket    string                             `json:"bucket"`
	Key       string                             `json:"key"`
	Filename  string                             `json:"filename,omitempty"`
	Overrides map[string]activities.StepOverride `json:"overrides,omitempty"`
}

type PlanArchiveWorkflowResult struct {
	Runs      int `json:"runs"`
	Unmatched int `json:"unmatched"`
}

// PlanArchiveWorkflow unpacks an archive that arrived for a plan, binds each
// file to a step and template, and starts a PlanRunWorkflow per file. The
// runs gate on the plan's dependencies, not on manifest order, and outlive
// this workflow.
func PlanArchiveWorkflow(ctx workflow.Context, in PlanArchiveWorkflowInput) (*PlanArchiveWorkflowResult, error) {
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions(nil))
	if err := requireLicensed(ctx); err != nil {
		return nil, err
	}
	fail := func(err error) (*PlanArchiveWorkflowResult, error) {
		dctx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		_ = workflow.ExecuteActivity(workflow.WithActivityOptions(dctx, finishActivityOptions()), "FailPlanIntake",
			activities.FailPlanIntakeInput{IntakeID: in.IntakeID, Error: failureMessage(err)}).Get(dctx, nil)
		return nil, err
	}

	var staged activities.StagePlanArchiveOutput
	if err := workflow.ExecuteActivity(ctx, "StagePlanArchive", activities.StagePlanArchiveInput{
		OrgID: in.OrgID, IntakeID: in.IntakeID, Bucket: in.Bucket, Key: in.Key, Filename: in.Filename,
	}).Get(ctx, &staged); err != nil {
		return fail(fmt.Errorf("unpack: %w", err))
	}
	var created activities.CreatePlanIntakeRunsOutput
	if err := workflow.ExecuteActivity(ctx, "CreatePlanIntakeRuns", activities.CreatePlanIntakeRunsInput{
		OrgID: in.OrgID, IntakeID: in.IntakeID, Files: staged.Files, Overrides: in.Overrides,
	}).Get(ctx, &created); err != nil {
		return fail(fmt.Errorf("bind files to steps: %w", err))
	}
	for _, run := range created.Runs {
		if err := startPlanRunChild(ctx, in.OrgID, run); err != nil {
			return nil, err
		}
	}
	return &PlanArchiveWorkflowResult{Runs: len(created.Runs), Unmatched: created.Unmatched}, nil
}

// startPlanRunChild starts a detached PlanRunWorkflow and waits only for it to start.
func startPlanRunChild(ctx workflow.Context, orgID int64, run activities.PlanRunRef) error {
	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:        run.WorkflowID,
		TaskQueue:         "migration",
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
		// A run may wait for its prerequisites for a long time; continue-as-new keeps history small.
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		RetryPolicy:           &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	child := workflow.ExecuteChildWorkflow(childCtx, PlanRunWorkflow, PlanRunWorkflowInput{OrgID: orgID, PlanRunID: run.PlanRunID})
	if err := child.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
		var already *temporal.ChildWorkflowExecutionAlreadyStartedError
		if temporal.IsWorkflowExecutionAlreadyStartedError(err) || errors.As(err, &already) ||
			strings.Contains(err.Error(), "already started") {
			return nil
		}
		return fmt.Errorf("start plan run %d: %w", run.PlanRunID, err)
	}
	return nil
}
