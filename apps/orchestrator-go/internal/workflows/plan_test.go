package workflows

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
)

// planStub fakes the plan activities and records what the workflow asked for.
type planStub struct {
	mu           sync.Mutex
	gates        []activities.CheckPlanRunGateOutput // returned in order; last repeats
	gateCalls    int
	started      int
	finalized    []activities.FinalizePlanRunInput
	cancelled    []int64
	childStatus  string
	childErr     error
	createdRuns  []activities.PlanRunRef
	overridesGot map[string]activities.StepOverride
}

func (s *planStub) CheckPlanRunGate(_ context.Context, _ activities.CheckPlanRunGateInput) (activities.CheckPlanRunGateOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.gateCalls
	if i >= len(s.gates) {
		i = len(s.gates) - 1
	}
	s.gateCalls++
	return s.gates[i], nil
}

func (s *planStub) StartPlanRunJob(_ context.Context, in activities.StartPlanRunJobInput) (activities.StartPlanRunJobOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started++
	return activities.StartPlanRunJobOutput{
		JobID: 77, WorkflowID: "plan-run-job-1", OrgID: 1, RuleTemplateID: 5, RuleTemplateVersion: 2,
		SourceRef: map[string]any{"type": "minio", "bucket": "b", "key": "k", "filename": "orders.csv"},
	}, nil
}

func (s *planStub) FinalizePlanRun(_ context.Context, in activities.FinalizePlanRunInput) (activities.FinalizePlanRunOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = append(s.finalized, in)
	if in.ExecError != "" || in.Cancelled {
		return activities.FinalizePlanRunOutput{Status: "failed", Reason: "x"}, nil
	}
	return activities.FinalizePlanRunOutput{Status: "completed", RuleMet: true}, nil
}

func (s *planStub) CancelPlanRun(_ context.Context, in activities.CancelPlanRunInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = append(s.cancelled, in.PlanRunID)
	return nil
}

func (s *planStub) MarkJobRunning(context.Context, activities.MarkJobRunningInput) error { return nil }
func (s *planStub) RequireLicensed(context.Context) error                                { return nil }

func (s *planStub) StagePlanArchive(_ context.Context, in activities.StagePlanArchiveInput) (activities.StagePlanArchiveOutput, error) {
	return activities.StagePlanArchiveOutput{Files: []activities.StagedFile{
		{StageKey: "accounts", File: "a.csv", Filename: "a.csv", Key: "plan-intakes/9/files/accounts/a.csv"},
		{StageKey: "orders", File: "o.csv", Filename: "o.csv", Key: "plan-intakes/9/files/orders/o.csv"},
	}}, nil
}

func (s *planStub) CreatePlanIntakeRuns(_ context.Context, in activities.CreatePlanIntakeRunsInput) (activities.CreatePlanIntakeRunsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overridesGot = in.Overrides
	return activities.CreatePlanIntakeRunsOutput{Runs: s.createdRuns, Unmatched: 1}, nil
}

func (s *planStub) FailPlanIntake(context.Context, activities.FailPlanIntakeInput) error { return nil }

func (s *planStub) fakeMigration(ctx workflow.Context, in MigrationWorkflowInput) (*MigrationWorkflowResult, error) {
	if s.childErr != nil {
		return nil, s.childErr
	}
	return &MigrationWorkflowResult{Processed: 97, Failed: 3, Status: s.childStatus}, nil
}

func newPlanEnv(t *testing.T, stub *planStub) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)
	env.RegisterWorkflow(PlanRunWorkflow)
	env.RegisterWorkflow(PlanArchiveWorkflow)
	env.RegisterWorkflowWithOptions(stub.fakeMigration, workflow.RegisterOptions{Name: "MigrationWorkflow"})
	for name, fn := range map[string]any{
		"CheckPlanRunGate": stub.CheckPlanRunGate, "StartPlanRunJob": stub.StartPlanRunJob,
		"FinalizePlanRun": stub.FinalizePlanRun, "CancelPlanRun": stub.CancelPlanRun,
		"MarkJobRunning": stub.MarkJobRunning, "RequireLicensed": stub.RequireLicensed,
		"StagePlanArchive": stub.StagePlanArchive, "CreatePlanIntakeRuns": stub.CreatePlanIntakeRuns,
		"FailPlanIntake": stub.FailPlanIntake,
	} {
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	return env
}

func TestPlanRunWaitsForPrerequisitesThenRuns(t *testing.T) {
	stub := &planStub{
		gates: []activities.CheckPlanRunGateOutput{
			{Reason: "waiting for accounts to complete"},
			{Blocked: true, Reason: "blocked: accounts (12% failed)"},
			{Ready: true},
		},
		childStatus: "failed", // rows failed, but the step's tolerance rule decides
	}
	env := newPlanEnv(t, stub)
	env.ExecuteWorkflow(PlanRunWorkflow, PlanRunWorkflowInput{OrgID: 1, PlanRunID: 1})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var res PlanRunWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&res))
	require.Equal(t, 3, stub.gateCalls, "the run re-checked until its prerequisites were met")
	require.Equal(t, 1, stub.started, "the job starts exactly once, after the gate opens")
	require.Len(t, stub.finalized, 1)
	require.Equal(t, int64(77), stub.finalized[0].JobID)
	require.Empty(t, stub.finalized[0].ExecError, "a job with failed rows is judged by the rule, not as an execution error")
	require.Equal(t, "completed", res.Status)
}

func TestPlanRunNeverStartsWhenNoLongerWaiting(t *testing.T) {
	stub := &planStub{gates: []activities.CheckPlanRunGateOutput{{Stop: true, Reason: "plan run is cancelled"}}}
	env := newPlanEnv(t, stub)
	env.ExecuteWorkflow(PlanRunWorkflow, PlanRunWorkflowInput{OrgID: 1, PlanRunID: 1})
	require.NoError(t, env.GetWorkflowError())
	require.Zero(t, stub.started)
	require.Empty(t, stub.finalized)
}

func TestPlanRunCancelledWhileWaiting(t *testing.T) {
	stub := &planStub{gates: []activities.CheckPlanRunGateOutput{{Reason: "waiting for accounts to complete"}}}
	env := newPlanEnv(t, stub)
	env.RegisterDelayedCallback(env.CancelWorkflow, PlanGatePollInterval*3+time.Second)
	env.ExecuteWorkflow(PlanRunWorkflow, PlanRunWorkflowInput{OrgID: 1, PlanRunID: 4})
	require.True(t, env.IsWorkflowCompleted())
	require.Equal(t, []int64{4}, stub.cancelled, "the waiting run is recorded as cancelled")
	require.Zero(t, stub.started)
}

func TestPlanRunRecordsChildExecutionError(t *testing.T) {
	stub := &planStub{gates: []activities.CheckPlanRunGateOutput{{Ready: true}}, childErr: errors.New("worker lost")}
	env := newPlanEnv(t, stub)
	env.ExecuteWorkflow(PlanRunWorkflow, PlanRunWorkflowInput{OrgID: 1, PlanRunID: 1})
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, stub.finalized, 1)
	require.Contains(t, stub.finalized[0].ExecError, "worker lost")
}

func TestPlanArchiveStartsARunPerBoundFile(t *testing.T) {
	stub := &planStub{
		gates: []activities.CheckPlanRunGateOutput{{Stop: true}},
		createdRuns: []activities.PlanRunRef{
			{PlanRunID: 11, WorkflowID: activities.PlanRunWorkflowID(11)},
			{PlanRunID: 12, WorkflowID: activities.PlanRunWorkflowID(12)},
		},
	}
	env := newPlanEnv(t, stub)
	var startedChildren []string
	env.SetOnChildWorkflowStartedListener(func(info *workflow.Info, _ workflow.Context, _ converter.EncodedValues) {
		startedChildren = append(startedChildren, info.WorkflowExecution.ID)
	})
	overrides := map[string]activities.StepOverride{"orders": {TemplateKey: "orders-v2"}}
	env.ExecuteWorkflow(PlanArchiveWorkflow, PlanArchiveWorkflowInput{OrgID: 1, IntakeID: 9, Bucket: "b", Key: "uploads/1/ab/x", Filename: "wave.tar.gz", Overrides: overrides})
	require.NoError(t, env.GetWorkflowError())
	var res PlanArchiveWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&res))
	require.Equal(t, 2, res.Runs)
	require.Equal(t, 1, res.Unmatched)
	require.ElementsMatch(t, []string{"plan-run-11", "plan-run-12"}, startedChildren)
	require.Equal(t, overrides, stub.overridesGot, "upload-time mapping reaches the binding activity")
}
