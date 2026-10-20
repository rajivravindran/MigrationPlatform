package workflows

import (
	"encoding/json"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/connectors"
)

// checkpointVersion is the only Checkpoint layout this code reads. A change
// to the layout bumps it and keeps a reader for the previous value until no
// run can still carry it (ADR 0002).
const checkpointVersion = 1

// checkpointedIngestVersion gates, via workflow.GetVersion, the behaviour that
// changes command order for in-flight runs: history-aware continue-as-new,
// row-index continuity checks and durable waits for pending sources.
const (
	checkpointedIngestChange  = "checkpointed-ingest"
	checkpointedIngestVersion = 1
)

// Checkpoint is the ingest position a MigrationWorkflow run hands to its
// continue-as-new successor (ADR 0002).
type Checkpoint struct {
	Version int `json:"v"`
	// SourceVersion is the content version the job has been reading (an
	// object ETag); a later batch that sees another version fails.
	SourceVersion string            `json:"sourceVersion,omitempty"`
	Offset        connectors.Offset `json:"offset"`
	// NextRowIndex is the absolute index of the next row to ingest.
	NextRowIndex int64 `json:"nextRowIndex"`
	Processed    int64 `json:"processed"`
	Failed       int64 `json:"failed"`
	Unresolved   int64 `json:"unresolved,omitempty"`
}

func (c Checkpoint) validate() error {
	switch {
	case c.Version != checkpointVersion:
		return fmt.Errorf("checkpoint version %d is not supported (want %d)", c.Version, checkpointVersion)
	case c.NextRowIndex < 0 || c.Processed < 0 || c.Failed < 0 || c.Unresolved < 0:
		return fmt.Errorf("checkpoint has negative position or totals: %+v", c)
	case c.Offset.Items != c.NextRowIndex:
		return fmt.Errorf("checkpoint offset is at row %d but next row index is %d", c.Offset.Items, c.NextRowIndex)
	case c.Processed+c.Failed+c.Unresolved > c.NextRowIndex:
		return fmt.Errorf("checkpoint counts %d outcomes for %d rows", c.Processed+c.Failed+c.Unresolved, c.NextRowIndex)
	}
	return nil
}

func invalidCheckpointError(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), activities.ErrTypeInvalidCheckpoint, nil)
}

// legacyOffsetKey is where runs before ADR 0002 stored the offset when they
// continued as new (and then ignored it).
const legacyOffsetKey = "_offset"

// startingCheckpoint returns where this run starts reading. It never falls
// back to row zero for input that names a position it cannot read.
func startingCheckpoint(in MigrationWorkflowInput) (Checkpoint, error) {
	if in.Checkpoint != nil {
		if err := in.Checkpoint.validate(); err != nil {
			return Checkpoint{}, invalidCheckpointError(err)
		}
		return *in.Checkpoint, nil
	}
	cp := Checkpoint{Version: checkpointVersion}
	raw, ok := in.SourceRef[legacyOffsetKey]
	if !ok {
		return cp, nil
	}
	s, isString := raw.(string)
	if !isString {
		return Checkpoint{}, invalidCheckpointError(fmt.Errorf("legacy offset is %T, not a string", raw))
	}
	var off connectors.Offset
	if err := json.Unmarshal([]byte(s), &off); err != nil {
		return Checkpoint{}, invalidCheckpointError(fmt.Errorf("legacy offset: %w", err))
	}
	// Totals were not carried; FinalizeJob recomputes them from job_rows.
	cp.Offset, cp.NextRowIndex = off, off.Items
	return cp, nil
}

// checkRowContinuity verifies a batch starts at the checkpoint, has no gaps
// and makes progress, so a connector regression fails the run instead of
// losing rows or looping.
func checkRowContinuity(next int64, rows []connectors.Row, newOffset connectors.Offset, done bool) error {
	if len(rows) == 0 && !done {
		return invalidCheckpointError(fmt.Errorf("ingest returned no rows at row %d without finishing", next))
	}
	for i, r := range rows {
		if want := next + int64(i); r.Index != want {
			return invalidCheckpointError(fmt.Errorf("ingest returned row %d where row %d was expected", r.Index, want))
		}
	}
	if want := next + int64(len(rows)); newOffset.Items != want {
		return invalidCheckpointError(fmt.Errorf("ingest offset is at row %d after delivering through row %d", newOffset.Items, want-1))
	}
	return nil
}

// Temporal terminates a run at 51,200 events or 50 MB of history and warns at
// a fifth of that. Stay well under the warning so one more batch always fits.
const (
	historyEventBudget = 10_000
	historyByteBudget  = 8 << 20
)

// historySnapshot is what shouldContinueAsNew needs from WorkflowInfo.
type historySnapshot struct {
	Events, Bytes int
	Suggested     bool
	// LastEvents/LastBytes are what the previous batch added.
	LastEvents, LastBytes int
}

// shouldContinueAsNew reports whether the run must hand off before ingesting
// another batch: the server suggests it, or the next batch, sized like the
// last one, would cross the budget.
func shouldContinueAsNew(h historySnapshot) bool {
	if h.Suggested {
		return true
	}
	return h.Events+h.LastEvents > historyEventBudget || h.Bytes+h.LastBytes > historyByteBudget
}

// Batch sizing (principal review F4). History cost per row grows with the
// number of chained steps (two activities per step plus one per row), so the
// rows ingested per batch are derived from that cost instead of being fixed.
const (
	maxBatchSize = 500
	// minBatchSize is the smallest batch worth ingesting; below it the run
	// continues as new instead so the next batch starts on a fresh history.
	minBatchSize = 10
	// eventsPerActivity is the history events one awaited activity costs
	// (scheduled, started, completed, plus a share of workflow-task events).
	eventsPerActivity = 5
)

// initialBatchSize sizes the first batch of a run from the template's step
// count so even the maximum supported chain stays inside the budget before
// its cost has been measured (a single-step template keeps the historical
// 500-row batch).
func initialBatchSize(steps int) int {
	if steps < 1 {
		steps = 1
	}
	perRow := (2*steps + 1) * eventsPerActivity
	size := historyEventBudget * 3 / 4 / perRow
	if size > maxBatchSize {
		size = maxBatchSize
	}
	if size < minBatchSize {
		size = minBatchSize
	}
	return size
}

type batchPlanInput struct {
	Events, Bytes         int
	LastEvents, LastBytes int
	LastRows              int
	Suggested             bool
}

type batchPlan struct {
	ContinueAsNew bool
	BatchSize     int
}

// planNextBatch decides, after a batch, whether the run continues as new or
// ingests another batch, and how many rows that batch may hold: the rows
// that fit in the remaining event and byte budget at the per-row cost just
// measured. A projected batch smaller than minBatchSize hands off instead.
func planNextBatch(in batchPlanInput) batchPlan {
	if in.Suggested {
		return batchPlan{ContinueAsNew: true}
	}
	if in.LastRows <= 0 || in.LastEvents <= 0 {
		// Nothing measured (an empty or pending batch): keep the server
		// suggestion and the fixed-cost check as the only guards.
		return batchPlan{ContinueAsNew: shouldContinueAsNew(historySnapshot{
			Events: in.Events, Bytes: in.Bytes, LastEvents: in.LastEvents, LastBytes: in.LastBytes,
		}), BatchSize: maxBatchSize}
	}
	perRowEvents := (in.LastEvents + in.LastRows - 1) / in.LastRows
	perRowBytes := (in.LastBytes + in.LastRows - 1) / in.LastRows
	size := (historyEventBudget - in.Events) / perRowEvents
	if perRowBytes > 0 {
		if byBytes := (historyByteBudget - in.Bytes) / perRowBytes; byBytes < size {
			size = byBytes
		}
	}
	if size < minBatchSize {
		return batchPlan{ContinueAsNew: true}
	}
	if size > maxBatchSize {
		size = maxBatchSize
	}
	return batchPlan{BatchSize: size}
}

// pendingBackoff is the wait before polling a source that reported Pending
// for the n-th consecutive time (n >= 1).
func pendingBackoff(n int) time.Duration {
	d := 2 * time.Second
	for i := 1; i < n && d < 30*time.Second; i++ {
		d *= 2
	}
	return min(d, 30*time.Second)
}
