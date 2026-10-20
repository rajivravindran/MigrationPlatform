// Package lifecycle owns a job's terminal transition. The workflow finalizes
// its own job; the reconciler finalizes jobs whose workflow ended without
// doing so. Both go through Finalize so they cannot disagree, and both get
// back the one persisted Verdict, which is the only thing a parent workflow
// (a batch stage) may consult when deciding whether dependent work can run
// (ADR 0004, architecture review A1).
package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"

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

// Terminal job statuses as persisted in jobs.status.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Verdict is the persisted terminal decision about a job: its status, the
// reason it was reached in operator-facing words, and the row totals it was
// derived from. It is returned by Finalize so that callers consume the
// decision rather than recomputing it from counters.
type Verdict struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	Totals Totals `json:"totals"`
}

// Succeeded reports whether the verdict allows dependent work to proceed.
// Only a persisted "succeeded" does; failed, cancelled and unknown do not.
func (v Verdict) Succeeded() bool { return v.Status == StatusSucceeded }

// TerminalStatus derives the job status from its rows and the execution
// outcome.
func TerminalStatus(t Totals, o Outcome) string {
	switch {
	case o.Cancelled:
		return StatusCancelled
	case o.Error != "":
		return StatusFailed
	case t.Failed > 0 || t.Unresolved > 0 || t.Total == 0:
		return StatusFailed
	default:
		return StatusSucceeded
	}
}

// Explain gives the reason for a terminal status in operator-facing words.
// It covers the status derived from this execution (TerminalStatus) as well
// as the two sticky cases Finalize preserves: an earlier cancellation and an
// earlier execution error carried in jobs.last_error.
func Explain(status string, t Totals, o Outcome, priorError string) string {
	switch {
	case status == StatusCancelled:
		return "execution was cancelled"
	case o.Error != "":
		return "execution error: " + o.Error
	case status == StatusFailed && priorError != "":
		return "earlier execution error kept: " + priorError
	case t.Total == 0:
		return "no source rows were read"
	case t.Failed > 0 || t.Unresolved > 0:
		return fmt.Sprintf("%d of %d rows failed, %d unresolved", t.Failed, t.Total, t.Unresolved)
	case status == StatusSucceeded:
		return fmt.Sprintf("all %d rows succeeded", t.Total)
	default:
		return "status " + status
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
// records that were never read. Returns the job's persisted verdict.
func Finalize(ctx context.Context, db *pgxpool.Pool, jobID int64, o Outcome) (Verdict, error) {
	return finalize(ctx, db, jobID, o, false)
}

// finalize with activeOnly leaves jobs that already reached a terminal state
// untouched and returns pgx.ErrNoRows for them.
func finalize(ctx context.Context, db *pgxpool.Pool, jobID int64, o Outcome, activeOnly bool) (Verdict, error) {
	t, err := CountRows(ctx, db, jobID)
	if err != nil {
		return Verdict{}, err
	}
	totals, err := json.Marshal(t)
	if err != nil {
		return Verdict{}, err
	}
	var lastError *string
	if o.Error != "" {
		lastError = &o.Error
	}
	rowStatus := TerminalStatus(t, o)
	var status string
	var persistedError *string
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
            control_requested = NULL, control_command_id = NULL, control_requested_at = NULL,
            finished_at = now(),
            updated_at = now()
        WHERE id = $4
          AND (NOT $5 OR status IN ('pending'::job_status, 'running'::job_status, 'paused'::job_status))
        RETURNING status::text, last_error`,
		rowStatus, totals, lastError, jobID, activeOnly).Scan(&status, &persistedError)
	if err != nil {
		return Verdict{}, err
	}
	prior := ""
	if persistedError != nil {
		prior = *persistedError
	}
	return Verdict{Status: status, Reason: Explain(status, t, o, prior), Totals: t}, nil
}
