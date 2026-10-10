// Package lifecycle owns a job's terminal transition. The workflow finalizes
// its own job; the reconciler finalizes jobs whose workflow ended without
// doing so. Both go through Finalize so they cannot disagree.
package lifecycle

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Outcome is what the execution concluded about a job.
type Outcome struct {
	Cancelled bool
	// Error is set when execution stopped outside a row (ingest, license,
	// persistence, termination). Such a job is failed whatever its rows say.
	Error string
}

// Totals are row outcomes counted from job_rows, the source of truth.
type Totals struct {
	Processed  int64 `json:"processed"`
	Failed     int64 `json:"failed"`
	Unresolved int64 `json:"unresolved"`
	Total      int64 `json:"total"`
}

// TerminalStatus derives the job status from its rows and the execution
// outcome.
func TerminalStatus(t Totals, o Outcome) string {
	switch {
	case o.Cancelled:
		return "cancelled"
	case o.Error != "":
		return "failed"
	case t.Failed > 0 || t.Unresolved > 0 || t.Total == 0:
		return "failed"
	default:
		return "succeeded"
	}
}

// CountRows counts row outcomes for a job.
func CountRows(ctx context.Context, db *pgxpool.Pool, jobID int64) (Totals, error) {
	var t Totals
	err := db.QueryRow(ctx, `
        SELECT
            count(*) FILTER (WHERE status = 'succeeded'::row_status),
            count(*) FILTER (WHERE status = 'failed'::row_status),
            count(*) FILTER (WHERE status = 'unresolved'::row_status),
            count(*)
        FROM job_rows WHERE job_id = $1`, jobID).Scan(&t.Processed, &t.Failed, &t.Unresolved, &t.Total)
	return t, err
}

// Finalize writes the terminal status and totals. A cancelled job stays
// cancelled (only its totals are refreshed), and an execution error already
// on the job is kept, so a later row retry cannot report success for source
// records that were never read. Returns the job's resulting status.
func Finalize(ctx context.Context, db *pgxpool.Pool, jobID int64, o Outcome) (string, error) {
	return finalize(ctx, db, jobID, o, false)
}

// finalize with activeOnly leaves jobs that already reached a terminal state
// untouched and returns pgx.ErrNoRows for them.
func finalize(ctx context.Context, db *pgxpool.Pool, jobID int64, o Outcome, activeOnly bool) (string, error) {
	t, err := CountRows(ctx, db, jobID)
	if err != nil {
		return "", err
	}
	totals, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	var lastError *string
	if o.Error != "" {
		lastError = &o.Error
	}
	rowStatus := TerminalStatus(t, o)
	var status string
	err = db.QueryRow(ctx, `
        UPDATE jobs SET
            status = CASE
                WHEN status = 'cancelled'::job_status THEN status
                WHEN $1 = 'cancelled' THEN 'cancelled'::job_status
                WHEN COALESCE($3, last_error) IS NOT NULL THEN 'failed'::job_status
                ELSE $1::job_status
            END,
            totals_json = $2,
            last_error = COALESCE($3, last_error),
            finished_at = now(),
            updated_at = now()
        WHERE id = $4
          AND (NOT $5 OR status IN ('pending'::job_status, 'running'::job_status, 'paused'::job_status))
        RETURNING status::text`,
		rowStatus, totals, lastError, jobID, activeOnly).Scan(&status)
	return status, err
}
