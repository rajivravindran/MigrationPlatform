package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/template"
)

// failedRowStore mimics ListFailedRows' SQL contract over an in-memory set of
// failed row indexes and records which rows were retried.
type failedRowStore struct {
	mu      sync.Mutex
	failed  []int64
	retried []int64
	cursors []*int64
}

func (s *failedRowStore) ListFailedRows(_ context.Context, in activities.ListFailedRowsInput) (activities.ListFailedRowsOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors = append(s.cursors, in.After)
	out := activities.ListFailedRowsOutput{}
	for _, idx := range s.failed {
		if in.After != nil && idx <= *in.After {
			continue
		}
		out.RowIndexes = append(out.RowIndexes, idx)
		if len(out.RowIndexes) == in.Limit {
			break
		}
	}
	out.Done = len(out.RowIndexes) < in.Limit
	return out, nil
}

func (s *failedRowStore) LoadRowState(_ context.Context, in activities.LoadRowStateInput) (activities.LoadRowStateOutput, error) {
	s.mu.Lock()
	s.retried = append(s.retried, in.RowIndex)
	s.mu.Unlock()
	return activities.LoadRowStateOutput{Found: true, Row: map[string]any{"id": in.RowIndex}}, nil
}

func newRetryEnv(t *testing.T, store *failedRowStore) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	ts := &testsuite.WorkflowTestSuite{}
	env := ts.NewTestWorkflowEnvironment()
	env.SetTestTimeout(60 * time.Second)
	tpl := template.RuleTemplate{
		ID: "rt_t", Version: 1, Name: "t", Source: template.Source{Type: "csv"},
		Mapping:     map[string]any{"payload": map[string]any{"id": map[string]any{"$from": "id"}}},
		Destination: template.Destination{Type: "http", Method: "POST", URL: "http://x"},
	}
	stub := &stubActivities{tpl: tpl}
	registerStub(env, stub)
	env.RegisterActivityWithOptions(store.ListFailedRows, activities.RegisterOptions("ListFailedRows"))
	env.RegisterActivityWithOptions(store.LoadRowState, activities.RegisterOptions("LoadRowState"))
	return env
}

func seq(from, to int64) []int64 {
	out := make([]int64, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

func TestRetryFailedRowsIncludesRowZero(t *testing.T) {
	store := &failedRowStore{failed: []int64{0, 3}}
	env := newRetryEnv(t, store)

	env.ExecuteWorkflow(RetryFailedRowsWorkflow, RetryFailedRowsInput{OrgID: 1, JobID: 2, RuleTemplateID: 3})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []int64{0, 3}, store.retried)
	require.NotNil(t, store.cursors[0])
	require.EqualValues(t, -1, *store.cursors[0], "first page must start before row zero")
}

// A worker built before MP-09 decodes ListFailedRowsInput.After as a plain
// int64, where an omitted cursor means 0 and skips row zero. The first page
// request must keep meaning "before row zero" for that decoder too.
func TestRetryFailedRowsFirstPageIsSafeForPreMP09Workers(t *testing.T) {
	store := &failedRowStore{failed: []int64{0}}
	env := newRetryEnv(t, store)
	env.ExecuteWorkflow(RetryFailedRowsWorkflow, RetryFailedRowsInput{OrgID: 1, JobID: 2, RuleTemplateID: 3})
	require.NoError(t, env.GetWorkflowError())

	raw, err := json.Marshal(activities.ListFailedRowsInput{JobID: 2, After: store.cursors[0], Limit: 200})
	require.NoError(t, err)
	var legacy struct {
		After int64 `json:"after"`
	}
	require.NoError(t, json.Unmarshal(raw, &legacy))
	require.Less(t, legacy.After, int64(0), "legacy SQL is row_index > after")
}

func TestRetryFailedRowsMultiplePagesRetryEachRowOnce(t *testing.T) {
	store := &failedRowStore{failed: seq(0, 450)}
	env := newRetryEnv(t, store)

	env.ExecuteWorkflow(RetryFailedRowsWorkflow, RetryFailedRowsInput{OrgID: 1, JobID: 2, RuleTemplateID: 3})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	got := append([]int64(nil), store.retried...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	require.Equal(t, seq(0, 450), got)
	var result MigrationWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.EqualValues(t, 450, result.Processed)
}

func TestRetryFailedRowsEmptyJob(t *testing.T) {
	store := &failedRowStore{}
	env := newRetryEnv(t, store)

	env.ExecuteWorkflow(RetryFailedRowsWorkflow, RetryFailedRowsInput{OrgID: 1, JobID: 2, RuleTemplateID: 3})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Empty(t, store.retried)
	require.Len(t, store.cursors, 1)
}

func TestRetryFailedRowsContinueAsNewResumesAfterCursor(t *testing.T) {
	store := &failedRowStore{failed: seq(0, 2100)}
	env := newRetryEnv(t, store)

	env.ExecuteWorkflow(RetryFailedRowsWorkflow, RetryFailedRowsInput{OrgID: 1, JobID: 2, RuleTemplateID: 3})

	require.True(t, env.IsWorkflowCompleted())
	var can *workflow.ContinueAsNewError
	require.True(t, errors.As(env.GetWorkflowError(), &can), "expected continue-as-new, got %v", env.GetWorkflowError())
	require.Len(t, store.retried, 2000)

	var next RetryFailedRowsInput
	require.NoError(t, converter.GetDefaultDataConverter().FromPayloads(can.Input, &next))
	require.NotNil(t, next.After)
	require.EqualValues(t, 1999, *next.After)

	env2 := newRetryEnv(t, store)
	env2.ExecuteWorkflow(RetryFailedRowsWorkflow, next)
	require.True(t, env2.IsWorkflowCompleted())
	require.NoError(t, env2.GetWorkflowError())
	require.Equal(t, seq(0, 2100), store.retried, "rows must be retried exactly once across runs")
}
