package workflows

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/worker"
)

// Histories recorded by the worker before the checkpointed-ingest change
// (harness runs on synthetic data) must replay unchanged, or in-flight jobs
// would fail with a nondeterminism error after a deploy.
func TestPreCheckpointHistoriesReplay(t *testing.T) {
	for _, f := range []string{"testdata/pre_r03_migration.json", "testdata/pre_r03_migration_python.json"} {
		replayer := worker.NewWorkflowReplayer()
		replayer.RegisterWorkflow(MigrationWorkflow)
		require.NoError(t, replayer.ReplayWorkflowHistoryFromJSONFile(nil, f), f)
	}
}
