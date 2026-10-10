package workflows

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/template"
)

func TestShouldContinueAsNew(t *testing.T) {
	cases := []struct {
		name string
		h    historySnapshot
		want bool
	}{
		{"fresh run", historySnapshot{Events: 20, Bytes: 4 << 10, LastEvents: 20}, false},
		{"server suggests", historySnapshot{Events: 20, Suggested: true}, true},
		{"next batch fits", historySnapshot{Events: 5_000, LastEvents: 4_000}, false},
		{"next batch would cross events", historySnapshot{Events: 6_500, LastEvents: 4_000}, true},
		{"next batch would cross bytes", historySnapshot{Events: 100, Bytes: 6 << 20, LastBytes: 3 << 20}, true},
	}
	for _, c := range cases {
		require.Equal(t, c.want, shouldContinueAsNew(c.h), c.name)
	}
}

func TestCheckpointValidate(t *testing.T) {
	ok := Checkpoint{Version: 1, Offset: connectors.Offset{Items: 500}, NextRowIndex: 500, Processed: 498, Failed: 2}
	require.NoError(t, ok.validate())
	bad := map[string]Checkpoint{
		"unknown version":     {Version: 2, Offset: connectors.Offset{Items: 1}, NextRowIndex: 1},
		"missing version":     {Offset: connectors.Offset{Items: 1}, NextRowIndex: 1},
		"offset/index differ": {Version: 1, Offset: connectors.Offset{Items: 0}, NextRowIndex: 500},
		"more outcomes":       {Version: 1, Offset: connectors.Offset{Items: 5}, NextRowIndex: 5, Processed: 6},
		"negative":            {Version: 1, Offset: connectors.Offset{Items: -1}, NextRowIndex: -1},
	}
	for name, cp := range bad {
		require.Error(t, cp.validate(), name)
	}
}

func TestStartingCheckpointReadsLegacyOffset(t *testing.T) {
	cp, err := startingCheckpoint(MigrationWorkflowInput{SourceRef: map[string]any{"_offset": `{"items":50000}`}})
	require.NoError(t, err)
	require.EqualValues(t, 50000, cp.NextRowIndex)
	require.EqualValues(t, 50000, cp.Offset.Items)

	_, err = startingCheckpoint(MigrationWorkflowInput{SourceRef: map[string]any{"_offset": `{"items":`}})
	requireAppErrorType(t, err, activities.ErrTypeInvalidCheckpoint)
	_, err = startingCheckpoint(MigrationWorkflowInput{SourceRef: map[string]any{"_offset": 7.0}})
	requireAppErrorType(t, err, activities.ErrTypeInvalidCheckpoint)
}

func TestCheckRowContinuity(t *testing.T) {
	rows := func(idx ...int64) []connectors.Row {
		out := make([]connectors.Row, len(idx))
		for i, x := range idx {
			out[i] = connectors.Row{Index: x}
		}
		return out
	}
	require.NoError(t, checkRowContinuity(500, rows(500, 501), connectors.Offset{Items: 502}, false))
	require.NoError(t, checkRowContinuity(500, nil, connectors.Offset{Items: 500}, true))
	require.Error(t, checkRowContinuity(500, nil, connectors.Offset{Items: 500}, false), "no progress")
	require.Error(t, checkRowContinuity(500, rows(0, 1), connectors.Offset{Items: 2}, false), "restarted at zero")
	require.Error(t, checkRowContinuity(500, rows(500, 502), connectors.Offset{Items: 503}, false), "gap")
	require.Error(t, checkRowContinuity(500, rows(500, 501), connectors.Offset{Items: 2000}, false), "offset ran ahead")
}

func requireAppErrorType(t *testing.T, err error, want string) {
	t.Helper()
	var app *temporal.ApplicationError
	require.True(t, errors.As(err, &app), "want ApplicationError, got %v", err)
	require.Equal(t, want, app.Type())
}

// pagedSource serves rows [0, total) from Offset.Items, like the file
// connectors, and records what the workflow asked for.
type pagedSource struct {
	stubActivities
	total   int64
	version string
	pending int // Ingest calls answered Pending before rows flow

	mu      sync.Mutex
	ingests []activities.IngestInput
	called  []int64
	tplIn   []activities.LoadTemplateInput
}

func (p *pagedSource) LoadTemplate(_ context.Context, in activities.LoadTemplateInput) (activities.LoadTemplateOutput, error) {
	p.mu.Lock()
	p.tplIn = append(p.tplIn, in)
	p.mu.Unlock()
	return activities.LoadTemplateOutput{Template: p.tpl, Version: 7}, nil
}

func (p *pagedSource) Ingest(_ context.Context, in activities.IngestInput) (activities.IngestOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ingests = append(p.ingests, in)
	off, err := connectors.UnmarshalOffset(in.Offset)
	if err != nil {
		return activities.IngestOutput{}, err
	}
	if p.pending > 0 {
		p.pending--
		return activities.IngestOutput{Pending: true, NewOffset: connectors.Offset{SalesforceJobID: "J1"}}, nil
	}
	end := min(off.Items+int64(in.BatchSize), p.total)
	out := activities.IngestOutput{SourceVersion: p.version, NewOffset: connectors.Offset{Items: end}, Done: end == p.total}
	for i := off.Items; i < end; i++ {
		out.Rows = append(out.Rows, connectors.Row{Index: i, Data: map[string]any{"id": i}})
	}
	return out, nil
}

func (p *pagedSource) CallEndpoint(_ context.Context, in activities.CallEndpointInput) (activities.CallEndpointOutput, error) {
	p.mu.Lock()
	p.called = append(p.called, in.RowIndex)
	p.mu.Unlock()
	return activities.CallEndpointOutput{Status: 201, Succeeded: true}, nil
}

func pagedTemplate() template.RuleTemplate {
	return template.RuleTemplate{ID: "rt_p", Version: 1, Name: "paged", Source: template.Source{Type: "csv"},
		Destination: template.Destination{Type: "http", Method: "POST", URL: "https://dest.example/items"}}
}

func newPagedEnv(t *testing.T, src *pagedSource, suggestCAN bool) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(MigrationWorkflow)
	registerPaged(env, src, src.Ingest)
	env.SetContinueAsNewSuggested(suggestCAN)
	return env
}

// registerPaged registers src's own activities; they take context.Context so
// the test environment hands them the decoded input.
func registerPaged(env *testsuite.TestWorkflowEnvironment, src *pagedSource,
	ingest func(context.Context, activities.IngestInput) (activities.IngestOutput, error)) {
	stub := &src.stubActivities
	env.RegisterActivityWithOptions(src.LoadTemplate, activities.RegisterOptions("LoadTemplate"))
	env.RegisterActivityWithOptions(ingest, activities.RegisterOptions("Ingest"))
	env.RegisterActivityWithOptions(src.CallEndpoint, activities.RegisterOptions("CallEndpoint"))
	env.RegisterActivityWithOptions(stub.RequireLicensed, activities.RegisterOptions("RequireLicensed"))
	env.RegisterActivityWithOptions(stub.PersistOutcome, activities.RegisterOptions("PersistOutcome"))
	env.RegisterActivityWithOptions(stub.PersistStepOutcome, activities.RegisterOptions("PersistStepOutcome"))
	env.RegisterActivityWithOptions(stub.PublishProgress, activities.RegisterOptions("PublishProgress"))
	registerJobFinish(env)
}

func continuedInput(t *testing.T, env *testsuite.TestWorkflowEnvironment) MigrationWorkflowInput {
	t.Helper()
	var can *workflow.ContinueAsNewError
	require.True(t, errors.As(env.GetWorkflowError(), &can), "expected continue-as-new, got %v", env.GetWorkflowError())
	var next MigrationWorkflowInput
	require.NoError(t, converter.GetDefaultDataConverter().FromPayloads(can.Input, &next))
	return next
}

func TestMigrationWorkflowContinuesAsNewFromCheckpoint(t *testing.T) {
	src := &pagedSource{stubActivities: stubActivities{tpl: pagedTemplate()}, total: 1250, version: "etag-1"}
	in := MigrationWorkflowInput{OrgID: 1, JobID: 2, RuleTemplateID: 3,
		SourceRef: map[string]any{"type": "minio", "source_id": 9.0, "bucket": "b", "key": "k", "filename": "f.csv"}}

	env := newPagedEnv(t, src, true)
	env.ExecuteWorkflow(MigrationWorkflow, in)
	next := continuedInput(t, env)
	require.NotNil(t, next.Checkpoint)
	require.Equal(t, Checkpoint{Version: 1, SourceVersion: "etag-1", Offset: connectors.Offset{Items: 500},
		NextRowIndex: 500, Processed: 500}, *next.Checkpoint)
	require.EqualValues(t, 7, next.RuleTemplateVersion, "the loaded template version is pinned")

	env2 := newPagedEnv(t, src, false)
	env2.ExecuteWorkflow(MigrationWorkflow, next)
	require.NoError(t, env2.GetWorkflowError())
	var res MigrationWorkflowResult
	require.NoError(t, env2.GetWorkflowResult(&res))
	require.EqualValues(t, 1250, res.Processed, "totals carry across runs")

	want := make([]int64, 1250)
	for i := range want {
		want[i] = int64(i)
	}
	require.Equal(t, want, src.called, "every row is sent exactly once across runs")
	require.Equal(t, "", src.ingests[0].ExpectedSourceVersion)
	for _, call := range src.ingests[1:] {
		require.Equal(t, "etag-1", call.ExpectedSourceVersion, "later batches must read the same version")
	}
	require.EqualValues(t, 7, src.tplIn[len(src.tplIn)-1].TemplateVersion)
}

func TestMigrationWorkflowFailsOnMalformedCheckpoint(t *testing.T) {
	src := &pagedSource{stubActivities: stubActivities{tpl: pagedTemplate()}, total: 10}
	env := newPagedEnv(t, src, false)
	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 2, RuleTemplateID: 3,
		Checkpoint: &Checkpoint{Version: 1, NextRowIndex: 500}})
	requireAppErrorType(t, env.GetWorkflowError(), activities.ErrTypeInvalidCheckpoint)
	require.Empty(t, src.ingests, "a bad checkpoint must not restart the read")
	require.Empty(t, src.called)
}

func TestMigrationWorkflowWaitsForPendingSource(t *testing.T) {
	src := &pagedSource{stubActivities: stubActivities{tpl: pagedTemplate()}, total: 3, pending: 2}
	env := newPagedEnv(t, src, false)
	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 2, RuleTemplateID: 3})
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, src.ingests, 3)
	off, err := connectors.UnmarshalOffset(src.ingests[2].Offset)
	require.NoError(t, err)
	require.Equal(t, "J1", off.SalesforceJobID, "the pending query is resumed, not recreated")
	require.Equal(t, []int64{0, 1, 2}, src.called)
}

func TestMigrationWorkflowRejectsConnectorThatRestartsAtZero(t *testing.T) {
	src := &restartingSource{pagedSource: pagedSource{stubActivities: stubActivities{tpl: pagedTemplate()}, total: 1000}}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(MigrationWorkflow)
	registerPaged(env, &src.pagedSource, src.Ingest)
	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 2, RuleTemplateID: 3})
	requireAppErrorType(t, env.GetWorkflowError(), activities.ErrTypeInvalidCheckpoint)
	require.Len(t, src.called, 500, "only the first batch is sent")
}

// restartingSource reproduces MP-03: every batch restarts at row zero.
type restartingSource struct{ pagedSource }

func (r *restartingSource) Ingest(_ context.Context, in activities.IngestInput) (activities.IngestOutput, error) {
	rows := make([]connectors.Row, in.BatchSize)
	for i := range rows {
		rows[i] = connectors.Row{Index: int64(i), Data: map[string]any{"id": i}}
	}
	return activities.IngestOutput{Rows: rows, NewOffset: connectors.Offset{Items: int64(in.BatchSize)}}, nil
}
