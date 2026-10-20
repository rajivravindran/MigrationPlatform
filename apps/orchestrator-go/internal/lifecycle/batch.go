package lifecycle

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Batch (archive package) lifecycle. A batch's terminal status is derived
// from its persisted stage outcomes the same way a job's is derived from
// job_rows: one rule, applied by the workflow on every exit and by the
// reconciler when the workflow can no longer do it (architecture review A2).

// Batch terminal statuses as persisted in batches.status.
const (
	BatchSucceeded   = "succeeded"
	BatchFailed      = "failed"
	BatchPartial     = "partial"
	BatchQuarantined = "quarantined"
)

// StageTotals are stage outcomes counted from batch_stages.
type StageTotals struct {
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	// Unfinished stages are still pending or running when the batch is
	// finalized: the batch ended before they reached a terminal state.
	Unfinished int64 `json:"unfinished"`
	Total      int64 `json:"total"`
}

// BatchVerdict is the persisted terminal decision about a batch.
type BatchVerdict struct {
	Status string      `json:"status"`
	Reason string      `json:"reason"`
	Stages StageTotals `json:"stages"`
}

// BatchTerminalStatus derives the batch status from its stages and the
// execution outcome. Cancellation of a batch is recorded as failed: the
// batch_status enum has no cancelled member and stages may have run.
func BatchTerminalStatus(t StageTotals, o Outcome) string {
	switch {
	case o.Cancelled || o.Error != "":
		return BatchFailed
	case t.Unfinished > 0:
		return BatchFailed
	case t.Total == 0:
		return BatchFailed
	case t.Failed > 0 && t.Succeeded > 0:
		return BatchPartial
	case t.Failed > 0:
		return BatchFailed
	default:
		return BatchSucceeded
	}
}

// ExplainBatch gives the reason for a batch's terminal status.
func ExplainBatch(status string, t StageTotals, o Outcome) string {
	switch {
	case o.Cancelled:
		return "execution was cancelled"
	case o.Error != "":
		return "execution error: " + o.Error
	case t.Unfinished > 0:
		return fmt.Sprintf("%d of %d stages never reached a terminal state", t.Unfinished, t.Total)
	case t.Total == 0:
		return "no stages were materialized"
	case status == BatchPartial:
		return fmt.Sprintf("%d of %d stages failed, %d succeeded", t.Failed, t.Total, t.Succeeded)
	case t.Failed > 0:
		return fmt.Sprintf("all %d stages failed or were skipped", t.Total)
	case status == BatchSucceeded:
		return fmt.Sprintf("all %d stages succeeded", t.Total)
	default:
		return "status " + status
	}
}

// CountStages counts stage outcomes for a batch.
func CountStages(ctx context.Context, db *pgxpool.Pool, batchID int64) (StageTotals, error) {
	var t StageTotals
	err := db.QueryRow(ctx, `
        SELECT
            count(*) FILTER (WHERE status = 'succeeded'::batch_status),
            count(*) FILTER (WHERE status IN ('failed'::batch_status, 'partial'::batch_status, 'quarantined'::batch_status)),
            count(*) FILTER (WHERE status IN ('pending'::batch_status, 'running'::batch_status)),
            count(*)
        FROM batch_stages WHERE batch_id = $1`, batchID).Scan(&t.Succeeded, &t.Failed, &t.Unfinished, &t.Total)
	return t, err
}

// FinalizeBatch derives and writes the batch's terminal status from its
// persisted stages. A quarantined batch stays quarantined; its stages are
// not consulted. Stages still pending or running are first recorded as
// failed with the given reason, so a batch whose workflow ended early cannot
// leave stages that look runnable. Returns the persisted verdict.
func FinalizeBatch(ctx context.Context, db *pgxpool.Pool, batchID int64, o Outcome) (BatchVerdict, error) {
	return finalizeBatch(ctx, db, batchID, o, false)
}

// finalizeBatch with activeOnly leaves batches already terminal untouched
// and returns pgx.ErrNoRows for them.
func finalizeBatch(ctx context.Context, db *pgxpool.Pool, batchID int64, o Outcome, activeOnly bool) (BatchVerdict, error) {
	var current string
	if err := db.QueryRow(ctx, `SELECT status::text FROM batches WHERE id = $1`, batchID).Scan(&current); err != nil {
		return BatchVerdict{}, err
	}
	if current == BatchQuarantined {
		if activeOnly {
			return BatchVerdict{}, pgx.ErrNoRows
		}
		t, err := CountStages(ctx, db, batchID)
		if err != nil {
			return BatchVerdict{}, err
		}
		return BatchVerdict{Status: BatchQuarantined, Reason: "archive was quarantined", Stages: t}, nil
	}
	if activeOnly && current != "pending" && current != "running" {
		return BatchVerdict{}, pgx.ErrNoRows
	}

	// Count before the sweep so the verdict can say how many stages the
	// batch abandoned; the sweep then leaves no stage that looks runnable.
	t, err := CountStages(ctx, db, batchID)
	if err != nil {
		return BatchVerdict{}, err
	}
	if t.Unfinished > 0 {
		unfinishedReason := "batch ended before this stage reached a terminal state"
		if o.Error != "" {
			unfinishedReason += ": " + o.Error
		} else if o.Cancelled {
			unfinishedReason += ": execution was cancelled"
		}
		if _, err := db.Exec(ctx, `
        UPDATE batch_stages SET status = 'failed'::batch_status,
            error_message = COALESCE(error_message, $2), updated_at = now()
        WHERE batch_id = $1 AND status IN ('pending'::batch_status, 'running'::batch_status)`,
			batchID, unfinishedReason); err != nil {
			return BatchVerdict{}, err
		}
	}
	status := BatchTerminalStatus(t, o)
	reason := ExplainBatch(status, t, o)
	var errMsg *string
	switch {
	case o.Error != "":
		errMsg = &o.Error
	case o.Cancelled, t.Unfinished > 0:
		errMsg = &reason
	}
	var persisted string
	err = db.QueryRow(ctx, `
        UPDATE batches SET
            status = $1::batch_status,
            error_message = COALESCE($2, error_message),
            finished_at = now(),
            updated_at = now()
        WHERE id = $3
          AND status <> 'quarantined'::batch_status
          AND (NOT $4 OR status IN ('pending'::batch_status, 'running'::batch_status))
        RETURNING status::text`, status, errMsg, batchID, activeOnly).Scan(&persisted)
	if err != nil {
		return BatchVerdict{}, err
	}
	return BatchVerdict{Status: persisted, Reason: reason, Stages: t}, nil
}
