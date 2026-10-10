package workflows

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/template"
)

// jobFinish records how a workflow finished its job.
type jobFinish struct {
	mu             sync.Mutex
	finalized      []activities.FinalizeJobInput
	exportsStarted int
	exports        int
	exportFailures []string

	finalizeErr error
	exportErr   error
}

func registerJobFinish(env *testsuite.TestWorkflowEnvironment) *jobFinish {
	f := &jobFinish{}
	env.RegisterActivityWithOptions(func(_ context.Context, in activities.FinalizeJobInput) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.finalized = append(f.finalized, in)
		return f.finalizeErr
	}, activities.RegisterOptions("FinalizeJob"))
	env.RegisterActivityWithOptions(func(_ context.Context, _ activities.ExportJobResultsInput) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.exportsStarted++
		return nil
	}, activities.RegisterOptions("RecordExportStarted"))
	env.RegisterActivityWithOptions(func(_ context.Context, _ activities.ExportJobResultsInput) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.exports++
		return f.exportErr
	}, activities.RegisterOptions("ExportJobResults"))
	env.RegisterActivityWithOptions(func(_ context.Context, in activities.RecordExportFailureInput) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.exportFailures = append(f.exportFailures, in.Error)
		return nil
	}, activities.RegisterOptions("RecordExportFailure"))
	return f
}

func (f *jobFinish) last(t *testing.T) activities.FinalizeJobInput {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.finalized, "the job was never finalized")
	return f.finalized[len(f.finalized)-1]
}

// lifecycleEnv is a migration workflow environment whose Ingest is supplied
// by the test.
func lifecycleEnv(t *testing.T, ingest func(context.Context, activities.IngestInput) (activities.IngestOutput, error)) (*testsuite.TestWorkflowEnvironment, *jobFinish) {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	stub := &stubActivities{tpl: template.RuleTemplate{
		ID: "rt_t", Version: 1, Name: "t",
		Source:      template.Source{Type: "csv"},
		Mapping:     map[string]any{"payload": map[string]any{"id": map[string]any{"$from": "id"}}},
		Destination: template.Destination{Type: "http", Method: "POST", URL: "http://x/y"},
	}}
	env.RegisterActivityWithOptions(stub.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(stub.CallEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))
	return env, registerJobFinish(env)
}

func ingestOneRow(context.Context, activities.IngestInput) (activities.IngestOutput, error) {
	return activities.IngestOutput{
		Rows:      []connectors.Row{{Index: 0, Data: map[string]any{"id": "a"}}},
		NewOffset: connectors.Offset{Items: 1}, Done: true,
	}, nil
}

func TestIngestFailureFinalizesTheJobAsFailed(t *testing.T) {
	env, finish := lifecycleEnv(t, func(context.Context, activities.IngestInput) (activities.IngestOutput, error) {
		return activities.IngestOutput{}, temporal.NewNonRetryableApplicationError("source object vanished", "SourceMissing", nil)
	})
	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 7, RuleTemplateID: 1})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(), "the workflow still reports the failure")
	got := finish.last(t)
	require.Equal(t, int64(7), got.JobID)
	require.False(t, got.Cancelled)
	require.Contains(t, got.Error, "ingest")
	require.Contains(t, got.Error, "source object vanished")
	require.Equal(t, 1, finish.exports, "results of the rows that ran are still exported")
}

func TestCancelledWorkflowStillFinalizesOnADisconnectedContext(t *testing.T) {
	env, finish := lifecycleEnv(t, func(context.Context, activities.IngestInput) (activities.IngestOutput, error) {
		return activities.IngestOutput{Pending: true, NewOffset: connectors.Offset{}}, nil
	})
	env.RegisterDelayedCallback(env.CancelWorkflow, 30*time.Second)
	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 8, RuleTemplateID: 1})

	require.True(t, env.IsWorkflowCompleted())
	require.True(t, temporal.IsCanceledError(env.GetWorkflowError()))
	got := finish.last(t)
	require.True(t, got.Cancelled)
	require.Empty(t, got.Error)
	require.Equal(t, 1, finish.exports)
}

func TestFinalizeFailureFailsTheWorkflow(t *testing.T) {
	env, finish := lifecycleEnv(t, ingestOneRow)
	finish.finalizeErr = temporal.NewNonRetryableApplicationError("db down", "DB", nil)
	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 9, RuleTemplateID: 1})

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err, "an unrecorded outcome must not look like success")
	require.Contains(t, err.Error(), "finalize job")
	require.Zero(t, finish.exports, "nothing is exported for an unfinalized job")
}

func TestExportFailureIsRecordedWithoutFailingTheJob(t *testing.T) {
	env, finish := lifecycleEnv(t, ingestOneRow)
	finish.exportErr = temporal.NewNonRetryableApplicationError("bucket unavailable", "S3", nil)
	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 10, RuleTemplateID: 1})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Empty(t, finish.last(t).Error, "the migration outcome is unchanged")
	require.Len(t, finish.exportFailures, 1)
	require.Contains(t, finish.exportFailures[0], "bucket unavailable")
}

func TestExportResultsWorkflowOnlyExports(t *testing.T) {
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	finish := registerJobFinish(env)
	// No destination activity is registered: any call would fail the test.
	env.ExecuteWorkflow(ExportResultsWorkflow, ExportResultsInput{JobID: 11})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, finish.exportsStarted)
	require.Equal(t, 1, finish.exports)
	require.Empty(t, finish.finalized, "an export retry does not change the job outcome")
}

func TestJobOutcomeMessages(t *testing.T) {
	long := errors.New(strings.Repeat("x", maxJobErrorLen+10))
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		require.Empty(t, jobOutcome(ctx, 1, nil).Error)
		require.Len(t, jobOutcome(ctx, 1, long).Error, maxJobErrorLen)
		require.True(t, jobOutcome(ctx, 1, temporal.NewCanceledError()).Cancelled)
		return nil
	})
	require.NoError(t, env.GetWorkflowError())
}
