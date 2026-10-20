package workflows

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/migration-platform/orchestrator/internal/activities"
)

// These tests cover architecture review A1: the batch parent consumes the
// verdict the child job persisted instead of reconstructing success from
// counters, so an empty, cancelled or errored child cannot unblock dependent
// stages.

func dependentStages() []activities.StagedFile {
	return []activities.StagedFile{
		{StageKey: "customers", File: "c.csv", TemplateKey: "t", OnFailure: "stop", Bucket: "b", Key: "kc", Filename: "c.csv", Size: 1},
		{StageKey: "subscriptions", File: "s.csv", TemplateKey: "t", OnFailure: "stop", DependsOn: []string{"customers"},
			Bucket: "b", Key: "ks", Filename: "s.csv", Size: 1},
	}
}

func runBatchTest(t *testing.T, stub *batchStub, configure func(*jobFinish)) (BatchWorkflowResult, error) {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)
	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	finish := registerBatchStub(env, stub)
	if configure != nil {
		configure(finish)
	}
	env.ExecuteWorkflow(BatchWorkflow, BatchWorkflowInput{OrgID: 1, BatchID: 21, Bucket: "migration", Key: "incoming/wave.tar.gz"})
	require.True(t, env.IsWorkflowCompleted())
	var result BatchWorkflowResult
	if err := env.GetWorkflowError(); err != nil {
		return result, err
	}
	require.NoError(t, env.GetWorkflowResult(&result))
	return result, nil
}

func TestEmptyChildJobFailsItsStageAndBlocksDependents(t *testing.T) {
	stub := &batchStub{stages: dependentStages()}
	result, err := runBatchTest(t, stub, func(f *jobFinish) { f.verdict = emptyJobVerdict })
	require.NoError(t, err)

	require.Equal(t, "failed", result.Status)
	require.Equal(t, 0, result.StagesOK, "a job that read no rows is failed by the lifecycle module and must not count as a succeeded stage")
	require.Equal(t, 2, result.StagesFailed)
	require.Equal(t, 1, stub.startedJobs, "the dependent stage never starts on a failed parent")
	require.Equal(t, "failed", stub.stageOutcomes["customers"].Status)
	require.Contains(t, stub.stageOutcomes["customers"].Error, "no source rows were read")
	require.Contains(t, stub.stageOutcomes["subscriptions"].Error, "skipped")
	require.Equal(t, "failed", stub.finalStatus)
}

func TestCancelledChildJobFailsItsStage(t *testing.T) {
	stub := &batchStub{stages: dependentStages()}
	result, err := runBatchTest(t, stub, func(f *jobFinish) {
		// lifecycle.Finalize keeps a job cancelled by the operator even when
		// the workflow exits normally; the stage must consume that verdict.
		f.verdict = func(in activities.FinalizeJobInput) *activities.FinalizeJobOutput {
			return &activities.FinalizeJobOutput{Status: "cancelled", Reason: "execution was cancelled"}
		}
	})
	require.NoError(t, err)
	require.Equal(t, 0, result.StagesOK)
	require.Equal(t, 2, result.StagesFailed)
	require.Equal(t, 1, stub.startedJobs)
	require.Contains(t, stub.stageOutcomes["customers"].Error, "job cancelled: execution was cancelled")
}

func TestChildExecutionErrorIsKeptDistinctFromABusinessFailure(t *testing.T) {
	stub := &batchStub{stages: dependentStages(),
		ingestErr: temporal.NewNonRetryableApplicationError("source object vanished", "SourceMissing", nil)}
	result, err := runBatchTest(t, stub, nil)
	require.NoError(t, err)
	require.Equal(t, 0, result.StagesOK)
	require.Equal(t, 2, result.StagesFailed)
	require.Equal(t, 1, stub.startedJobs)
	require.Contains(t, stub.stageOutcomes["customers"].Error, "execution error:")
	require.Contains(t, stub.stageOutcomes["customers"].Error, "source object vanished")
}

func TestChildWithoutAPersistedVerdictDoesNotCountAsSuccess(t *testing.T) {
	stub := &batchStub{stages: dependentStages()}
	result, err := runBatchTest(t, stub, func(f *jobFinish) {
		// A FinalizeJob worker built before verdicts returns nothing.
		f.verdict = func(activities.FinalizeJobInput) *activities.FinalizeJobOutput { return nil }
	})
	require.NoError(t, err)
	require.Equal(t, 0, result.StagesOK, "zero counters without a verdict are not evidence of success")
	require.Equal(t, 2, result.StagesFailed)
	require.Contains(t, stub.stageOutcomes["customers"].Error, "without a persisted verdict")
}

func TestVerifiedSucceededChildUnblocksDependents(t *testing.T) {
	stub := &batchStub{stages: dependentStages()}
	result, err := runBatchTest(t, stub, nil)
	require.NoError(t, err)
	require.Equal(t, "succeeded", result.Status)
	require.Equal(t, 2, result.StagesOK)
	require.Equal(t, 2, stub.startedJobs)
	require.Equal(t, "succeeded", stub.stageOutcomes["subscriptions"].Status)
	require.Empty(t, stub.stageOutcomes["subscriptions"].Error)
}

func TestStageVerdictTable(t *testing.T) {
	cases := []struct {
		name   string
		res    MigrationWorkflowResult
		err    error
		ok     bool
		reason string
	}{
		{"persisted success", MigrationWorkflowResult{Processed: 3, Status: "succeeded", Reason: "all 3 rows succeeded"}, nil, true, ""},
		{"persisted failure", MigrationWorkflowResult{Processed: 2, Failed: 1, Status: "failed", Reason: "1 of 3 rows failed, 0 unresolved"}, nil, false, "job failed: 1 of 3 rows failed"},
		{"persisted cancellation", MigrationWorkflowResult{Cancelled: true, Status: "cancelled", Reason: "execution was cancelled"}, nil, false, "job cancelled"},
		{"empty job verdict", MigrationWorkflowResult{Status: "failed", Reason: "no source rows were read"}, nil, false, "no source rows were read"},
		{"clean counters but no verdict", MigrationWorkflowResult{}, nil, false, "without a persisted verdict"},
		{"execution error wins", MigrationWorkflowResult{Status: "succeeded"}, errors.New("workflow timed out"), false, "execution error: workflow timed out"},
		{"failed status without reason still explains", MigrationWorkflowResult{Failed: 2, Status: "failed"}, nil, false, "failed=2"},
	}
	for _, c := range cases {
		got := stageVerdict(c.res, c.err)
		require.Equal(t, c.ok, got.ok, c.name)
		if c.ok {
			require.Equal(t, "succeeded", got.status, c.name)
			require.Empty(t, got.reason, c.name)
		} else {
			require.Equal(t, "failed", got.status, c.name)
			require.Contains(t, got.reason, c.reason, c.name)
		}
	}
}

// Review A2: the batch is finalized from its persisted stages on every exit.

func TestStagePersistenceErrorStillFinalizesTheBatch(t *testing.T) {
	stub := &batchStub{stages: dependentStages(),
		stagePersistErr: map[string]error{"customers": temporal.NewNonRetryableApplicationError("db down", "DB", nil)}}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)
	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerBatchStub(env, stub)
	env.ExecuteWorkflow(BatchWorkflow, BatchWorkflowInput{OrgID: 1, BatchID: 22, Bucket: "migration", Key: "incoming/wave.tar.gz"})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err, "the workflow still reports the persistence failure")
	require.Contains(t, err.Error(), "finalize batch stage")
	require.Len(t, stub.finalizeInputs, 1, "the batch is finalized exactly once even though a stage write failed")
	in := stub.finalizeInputs[0]
	require.True(t, in.DeriveFromStages, "the status is derived from persisted stages, not supplied by the workflow")
	require.Equal(t, "failed", in.Status, "an old worker still gets the counter-derived status it understands")
	require.Contains(t, in.Error, "db down")
	require.Equal(t, "failed", stub.finalStatus, "a batch that ended on an execution error is failed, not left running")
}

func TestCancelledBatchIsFinalizedOnADisconnectedContext(t *testing.T) {
	stub := &batchStub{stages: dependentStages()}
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)
	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerBatchStub(env, stub)
	env.RegisterDelayedCallback(env.CancelWorkflow, 0)
	env.ExecuteWorkflow(BatchWorkflow, BatchWorkflowInput{OrgID: 1, BatchID: 23, Bucket: "migration", Key: "incoming/wave.tar.gz"})

	require.True(t, env.IsWorkflowCompleted())
	require.True(t, temporal.IsCanceledError(env.GetWorkflowError()), "got %v", env.GetWorkflowError())
	require.Len(t, stub.finalizeInputs, 1)
	require.True(t, stub.finalizeInputs[0].Cancelled)
	require.Equal(t, "failed", stub.finalStatus)
}

func TestBatchVerdictComesFromPersistedStages(t *testing.T) {
	stub := &batchStub{stages: dependentStages()}
	result, err := runBatchTest(t, stub, nil)
	require.NoError(t, err)
	require.Len(t, stub.finalizeInputs, 1)
	require.True(t, stub.finalizeInputs[0].DeriveFromStages)
	require.Equal(t, "succeeded", stub.finalizeInputs[0].Status, "mixed-version hint matches the counters")
	require.Equal(t, "succeeded", result.Status)
	require.Equal(t, "all 2 stages succeeded", result.Reason)
}

// An uploaded archive is stored under a content-hash key with no extension;
// POST /batches passes the original filename, which must select the format.
func TestUploadedArchiveUsesOriginalFilename(t *testing.T) {
	run := func(filename string) (*batchStub, error) {
		stub := &batchStub{stages: dependentStages()}
		env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
		env.SetTestTimeout(30 * time.Second)
		env.RegisterWorkflow(MigrationWorkflow)
		env.RegisterWorkflow(BatchWorkflow)
		registerBatchStub(env, stub)
		env.ExecuteWorkflow(BatchWorkflow, BatchWorkflowInput{
			OrgID: 1, BatchID: 24, Bucket: "migration", Key: "uploads/1/2e/2e19deadbeef", Filename: filename,
		})
		require.True(t, env.IsWorkflowCompleted())
		return stub, env.GetWorkflowError()
	}

	stub, err := run("cutover.zip")
	require.NoError(t, err)
	require.Equal(t, "succeeded", stub.finalStatus)
	require.Equal(t, 2, stub.startedJobs)
	require.Equal(t, "cutover.zip", stub.unpackInput.Filename, "the unpack activity needs the name to pick zip vs tar")

	stub, _ = run("")
	require.Equal(t, 0, stub.startedJobs, "without a filename an extensionless key is not an archive")
	require.Equal(t, "failed", stub.finalStatus)
}
