package workflows

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
)

// Architecture review A4: a watched object's identity is not an execution
// intent. Two schedules over the same source must each execute once, and
// discovery progress belongs to the schedule.

func sharedConnectorWithScheduleOneCursor() activities.LoadConnectorOutput {
	return activities.LoadConnectorOutput{
		ID: 9, Kind: "watched_prefix", Name: "incoming",
		Config: map[string]any{
			"bucket": "migration", "prefix": "incoming/", "glob": "*", "sort": "lexical",
			"cursor": map[string]any{
				// Recorded before per-schedule cursors existed: seen for everyone.
				"seen": map[string]any{"incoming/ancient.csv": "etag-ancient"},
				// Schedule 1 already processed new.csv.
				"bySchedule": map[string]any{
					"1": map[string]any{"seen": map[string]any{"incoming/new.csv": "etag-new"}},
				},
			},
		},
	}
}

func runWatch(t *testing.T, scheduleID int64, stub *watchStub) (WatchPrefixWorkflowResult, *watchStub) {
	t.Helper()
	env := (&testsuite.WorkflowTestSuite{}).NewTestWorkflowEnvironment()
	env.SetTestTimeout(30 * time.Second)
	env.RegisterWorkflow(MigrationWorkflow)
	env.RegisterWorkflow(BatchWorkflow)
	registerWatchStub(env, stub)
	env.ExecuteWorkflow(WatchPrefixWorkflow, WatchPrefixWorkflowInput{
		OrgID: 1, ScheduleID: scheduleID, RuleTemplateID: 7, RuleTemplateVersion: 1, ConnectorID: 9,
		ScheduledTime: time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC),
	})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var result WatchPrefixWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	return result, stub
}

func TestTwoSchedulesOverTheSameSourceEachExecuteOnce(t *testing.T) {
	objects := []connectors.ObjectInfo{
		{Key: "incoming/ancient.csv", ETag: "etag-ancient", Size: 1},
		{Key: "incoming/new.csv", ETag: "etag-new", Size: 20},
	}

	// Schedule 1 has already processed new.csv: nothing to do.
	r1, s1 := runWatch(t, 1, &watchStub{conn: sharedConnectorWithScheduleOneCursor(), objects: objects})
	require.Equal(t, 0, r1.Started)
	require.Equal(t, 2, r1.Skipped)
	require.Empty(t, s1.jobs)

	// Schedule 2 (a different template/destination) must still process it,
	// under its own execution identity, and record progress for itself only.
	r2, s2 := runWatch(t, 2, &watchStub{conn: sharedConnectorWithScheduleOneCursor(), objects: objects})
	require.Equal(t, 1, r2.Started)
	require.Equal(t, 1, r2.Skipped, "the pre-upgrade connector-wide cursor still counts as seen")
	require.Len(t, s2.jobs, 1)
	require.Equal(t, "incoming/new.csv", s2.jobs[0].Key)
	require.EqualValues(t, 2, s2.jobs[0].ScheduleID)
	require.EqualValues(t, 2, s2.cursorScheduleID)
	require.Equal(t, map[string]string{"incoming/new.csv": "etag-new"}, s2.cursor)

	require.NotEqual(t,
		activities.WatchExecutionWorkflowID(1, 1, "migration", "incoming/new.csv", "etag-new"),
		activities.WatchExecutionWorkflowID(1, 2, "migration", "incoming/new.csv", "etag-new"),
		"distinct intents over one source version get distinct workflow ids")
}

func TestExecutionWorkflowIDs(t *testing.T) {
	// Redelivery of the same intent deduplicates.
	require.Equal(t,
		activities.WatchExecutionWorkflowID(1, 5, "b", "k", "e"),
		activities.WatchExecutionWorkflowID(1, 5, "b", "k", "e"))
	// A new source version is a new execution.
	require.NotEqual(t,
		activities.WatchExecutionWorkflowID(1, 5, "b", "k", "e"),
		activities.WatchExecutionWorkflowID(1, 5, "b", "k", "e2"))
	// Without a schedule the legacy source-only id is kept.
	require.Equal(t, activities.WatchWorkflowID(1, "b", "k", "e"), activities.WatchExecutionWorkflowID(1, 0, "b", "k", "e"))
	require.NotEqual(t, activities.WatchWorkflowID(1, "b", "k", "e"), activities.WatchExecutionWorkflowID(1, 5, "b", "k", "e"))
	require.Contains(t, activities.WatchExecutionWorkflowID(1, 5, "b", "k", "e"), "watch-1-")

	require.Equal(t, activities.BatchWorkflowID(1, "b", "k.tar.gz", "e"), activities.BatchExecutionWorkflowID(1, 0, "b", "k.tar.gz", "e"))
	require.NotEqual(t,
		activities.BatchExecutionWorkflowID(1, 1, "b", "k.tar.gz", "e"),
		activities.BatchExecutionWorkflowID(1, 2, "b", "k.tar.gz", "e"))
	require.Contains(t, activities.BatchExecutionWorkflowID(1, 2, "b", "k.tar.gz", "e"), "batch-1-")
}
