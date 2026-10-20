package workflows

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
)

// Architecture review A3: a pause is acknowledged by the workflow and can be
// read back, so the API and the reconciler converge jobs.status with the
// workflow's own state rather than with a write made before the signal.
func TestPauseIsAcknowledgedAndQueryable(t *testing.T) {
	var polls atomic.Int32
	env, finish := lifecycleEnv(t, func(context.Context, activities.IngestInput) (activities.IngestOutput, error) {
		n := polls.Add(1)
		if n < 3 {
			return activities.IngestOutput{Pending: true, NewOffset: connectors.Offset{}}, nil
		}
		return activities.IngestOutput{
			Rows:      []connectors.Row{{Index: 0, Data: map[string]any{"id": "a"}}},
			NewOffset: connectors.Offset{Items: 1}, Done: true,
		}, nil
	})

	var acknowledged PauseState
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalPause, ControlCommand{CommandID: "cmd-1"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		v, err := env.QueryWorkflow(QueryPauseState)
		require.NoError(t, err)
		require.NoError(t, v.Get(&acknowledged))
	}, 20*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalResume, ControlCommand{CommandID: "cmd-2"})
	}, 30*time.Second)

	env.ExecuteWorkflow(MigrationWorkflow, MigrationWorkflowInput{OrgID: 1, JobID: 12, RuleTemplateID: 1})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	require.True(t, acknowledged.Paused, "the pause was recorded by the workflow")
	require.True(t, acknowledged.Acknowledged, "no new work is dispatched once acknowledged")
	require.Equal(t, "cmd-1", acknowledged.LastCommandID, "the command id travels with the signal")

	v, err := env.QueryWorkflow(QueryPauseState)
	require.NoError(t, err)
	var final PauseState
	require.NoError(t, v.Get(&final))
	require.False(t, final.Paused)
	require.Equal(t, "cmd-2", final.LastCommandID)
	require.Empty(t, finish.last(t).Error, "the run completed after resume")
}
