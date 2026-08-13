package activities

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"

	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/export"
	"github.com/migration-platform/orchestrator/internal/template"
)

const exportPageSize = 1000

type ExportJobResultsInput struct {
	JobID int64 `json:"jobId"`
}

// ExportJobResults streams job_rows into CSV objects on MinIO and stores
// results_ref on the job. Safe to retry: objects are overwritten.
func (a *Activities) ExportJobResults(ctx context.Context, in ExportJobResultsInput) error {
	bucket := os.Getenv("MINIO_BUCKET")
	if bucket == "" {
		bucket = "migration"
	}

	var orgID, tplID int64
	var tplVer int32
	if err := a.d.DB.QueryRow(ctx,
		`SELECT org_id, rule_template_id, rule_template_version FROM jobs WHERE id = $1`,
		in.JobID,
	).Scan(&orgID, &tplID, &tplVer); err != nil {
		return fmt.Errorf("load job for export: %w", err)
	}

	var cols []template.ExportColumn
	if tplOut, err := a.LoadTemplate(ctx, LoadTemplateInput{
		OrgID: orgID, RuleTemplateID: tplID, TemplateVersion: tplVer,
	}); err == nil && tplOut.Template.Export != nil {
		cols = tplOut.Template.Export.Columns
	}

	allKey := fmt.Sprintf("jobs/%d/results.csv", in.JobID)
	failedKey := fmt.Sprintf("jobs/%d/results-failed.csv", in.JobID)

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
	})
	if err != nil {
		return err
	}
	_, err = a.d.DB.Exec(ctx, `
		UPDATE jobs SET results_ref = $1, updated_at = now() WHERE id = $2`, ref, in.JobID)
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

	if err := connectors.PutObject(ctx, bucket, key, tmpPath, "text/csv"); err != nil {
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
		q += ` AND status = 'failed'::row_status`
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
