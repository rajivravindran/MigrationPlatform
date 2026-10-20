package activities

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"

	"go.temporal.io/sdk/temporal"

	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/export"
	"github.com/migration-platform/orchestrator/internal/template"
)

const exportPageSize = 1000

type ExportJobResultsInput struct {
	JobID int64 `json:"jobId"`
}

// ErrTypeExportStale is returned when row outcomes changed while an export
// was being written: the files describe an older revision and are not
// published (principal review F5). Non-retryable; a new export reads the
// current revision.
const ErrTypeExportStale = "ExportStale"

// ExportJobResults streams job_rows into CSV objects on MinIO and publishes
// results_ref on the job. Every export is fenced by the job's outcome
// revision (bumped by every job_rows write, see migration 0011): the files
// are written under a revision-specific prefix and results_ref is replaced
// only if the revision is still current and not older than the one already
// published. Two exports racing a row retry can therefore never leave stale
// evidence labelled as current, whichever finishes last.
func (a *Activities) ExportJobResults(ctx context.Context, in ExportJobResultsInput) error {
	bucket := os.Getenv("MINIO_BUCKET")
	if bucket == "" {
		bucket = "migration"
	}

	var orgID, tplID, revision int64
	var tplVer int32
	if err := a.d.DB.QueryRow(ctx,
		`SELECT org_id, rule_template_id, rule_template_version, outcome_revision FROM jobs WHERE id = $1`,
		in.JobID,
	).Scan(&orgID, &tplID, &tplVer, &revision); err != nil {
		return fmt.Errorf("load job for export: %w", err)
	}

	var cols []template.ExportColumn
	if tplOut, err := a.LoadTemplate(ctx, LoadTemplateInput{
		OrgID: orgID, RuleTemplateID: tplID, TemplateVersion: tplVer,
	}); err == nil && tplOut.Template.Export != nil {
		cols = tplOut.Template.Export.Columns
	}

	allKey := fmt.Sprintf("jobs/%d/exports/%d/results.csv", in.JobID, revision)
	failedKey := fmt.Sprintf("jobs/%d/exports/%d/results-failed.csv", in.JobID, revision)

	allCount, err := a.writeResultsCSV(ctx, in.JobID, cols, allKey, bucket, false)
	if err != nil {
		return err
	}
	failedCount, err := a.writeResultsCSV(ctx, in.JobID, cols, failedKey, bucket, true)
	if err != nil {
		return err
	}

	ref, err := json.Marshal(map[string]any{
		"type":        "minio",
		"bucket":      bucket,
		"key":         allKey,
		"failed_key":  failedKey,
		"rows":        allCount,
		"failed_rows": failedCount,
		"revision":    revision,
	})
	if err != nil {
		return err
	}
	tag, err := a.d.DB.Exec(ctx, `
		UPDATE jobs SET results_ref = $1, export_status = 'succeeded', export_error = NULL, updated_at = now()
		WHERE id = $2
		  AND outcome_revision = $3
		  AND COALESCE((results_ref->>'revision')::bigint, -1) <= $3`, ref, in.JobID, revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var current int64
		_ = a.d.DB.QueryRow(ctx, `SELECT outcome_revision FROM jobs WHERE id = $1`, in.JobID).Scan(&current)
		msg := fmt.Sprintf("row outcomes changed while exporting (revision %d, now %d); the files were not published, export again", revision, current)
		return temporal.NewNonRetryableApplicationError(msg, ErrTypeExportStale, nil)
	}
	return nil
}

// RecordExportStarted marks the export in progress. The export workflow owns
// export_status, so its own ordering keeps pending before the outcome.
func (a *Activities) RecordExportStarted(ctx context.Context, in ExportJobResultsInput) error {
	_, err := a.d.DB.Exec(ctx, `
		UPDATE jobs SET export_status = 'pending', export_error = NULL, updated_at = now() WHERE id = $1`,
		in.JobID)
	return err
}

type RecordExportFailureInput struct {
	JobID int64  `json:"jobId"`
	Error string `json:"error"`
}

// RecordExportFailure makes a failed export visible without changing the
// migration outcome. A previous results_ref stays but may be stale until an
// export succeeds.
func (a *Activities) RecordExportFailure(ctx context.Context, in RecordExportFailureInput) error {
	_, err := a.d.DB.Exec(ctx, `
		UPDATE jobs SET export_status = 'failed', export_error = $1, updated_at = now() WHERE id = $2`,
		in.Error, in.JobID)
	return err
}

func (a *Activities) writeResultsCSV(ctx context.Context, jobID int64, cols []template.ExportColumn, key, bucket string, failedOnly bool) (int64, error) {
	tmp, err := os.CreateTemp("", "job-results-*.csv")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	w := csv.NewWriter(tmp)
	if err := w.Write(export.Headers(cols)); err != nil {
		return 0, err
	}

	var written int64
	var after int64 = -1
	for {
		page, err := a.loadExportPage(ctx, jobID, after, failedOnly)
		if err != nil {
			return 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			if err := w.Write(export.Project(row, cols)); err != nil {
				return 0, err
			}
			written++
			after = row.Index
		}
		if len(page) < exportPageSize {
			break
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}

	if _, err := connectors.PutObject(ctx, bucket, key, tmpPath, "text/csv"); err != nil {
		return 0, fmt.Errorf("put results %s: %w", key, err)
	}
	return written, nil
}

type exportRowScan struct {
	Index    int64
	Status   string
	Error    *string
	Source   []byte
	Response []byte
}

func (a *Activities) loadExportPage(ctx context.Context, jobID, after int64, failedOnly bool) ([]export.Row, error) {
	q := `
		SELECT row_index, status::text, last_error, row_json, response_json
		FROM job_rows
		WHERE job_id = $1 AND row_index > $2`
	if failedOnly {
		q += ` AND status IN ('failed'::row_status, 'unresolved'::row_status)`
	}
	q += ` ORDER BY row_index ASC LIMIT $3`

	rows, err := a.d.DB.Query(ctx, q, jobID, after, exportPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var page []export.Row
	var minIdx, maxIdx int64
	for rows.Next() {
		var s exportRowScan
		if err := rows.Scan(&s.Index, &s.Status, &s.Error, &s.Source, &s.Response); err != nil {
			return nil, err
		}
		row := export.Row{Index: s.Index, Status: s.Status, Responses: map[string]json.RawMessage{}}
		if s.Error != nil {
			row.Error = *s.Error
		}
		if len(s.Source) > 0 {
			_ = json.Unmarshal(s.Source, &row.Source)
		}
		if len(s.Response) > 0 {
			row.Responses["main"] = json.RawMessage(s.Response)
		}
		if len(page) == 0 {
			minIdx = s.Index
		}
		maxIdx = s.Index
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page) == 0 {
		return page, nil
	}

	stepRows, err := a.d.DB.Query(ctx, `
		SELECT row_index, step_name, response_json
		FROM job_row_steps
		WHERE job_id = $1 AND row_index >= $2 AND row_index <= $3`,
		jobID, minIdx, maxIdx)
	if err != nil {
		return nil, err
	}
	defer stepRows.Close()
	byIndex := make(map[int64]*export.Row, len(page))
	for i := range page {
		byIndex[page[i].Index] = &page[i]
	}
	for stepRows.Next() {
		var idx int64
		var name string
		var body []byte
		if err := stepRows.Scan(&idx, &name, &body); err != nil {
			return nil, err
		}
		row, ok := byIndex[idx]
		if !ok || len(body) == 0 {
			continue
		}
		row.Responses[name] = json.RawMessage(body)
	}
	return page, stepRows.Err()
}
