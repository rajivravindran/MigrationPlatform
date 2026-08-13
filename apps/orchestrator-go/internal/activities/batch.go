package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/migration-platform/orchestrator/internal/batch"
	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/metrics"
)

// ---------------- StartBatch ----------------

type StartBatchInput struct {
	OrgID         int64     `json:"orgId"`
	ScheduleID    int64     `json:"scheduleId"`
	ConnectorID   int64     `json:"connectorId"`
	Bucket        string    `json:"bucket"`
	Key           string    `json:"key"`
	ETag          string    `json:"etag"`
	Size          int64     `json:"size"`
	ScheduledTime time.Time `json:"scheduledTime"`
}

type StartBatchOutput struct {
	BatchID    int64  `json:"batchId"`
	WorkflowID string `json:"workflowId"`
	Created    bool   `json:"created"`
}

// BatchWorkflowID builds a stable Temporal workflow id for an archive package.
func BatchWorkflowID(orgID int64, bucket, key, etag string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", orgID, bucket, key, etag)))
	return fmt.Sprintf("batch-%d-%s", orgID, hex.EncodeToString(sum[:16]))
}

// StartBatch inserts a batches row for a watched archive. Idempotent on workflow id.
func (a *Activities) StartBatch(ctx context.Context, in StartBatchInput) (StartBatchOutput, error) {
	workflowID := BatchWorkflowID(in.OrgID, in.Bucket, in.Key, in.ETag)
	source := map[string]any{
		"type":     "minio",
		"bucket":   in.Bucket,
		"key":      in.Key,
		"filename": path.Base(in.Key),
		"size":     in.Size,
		"etag":     in.ETag,
	}
	srcJSON, err := json.Marshal(source)
	if err != nil {
		return StartBatchOutput{}, err
	}

	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return StartBatchOutput{}, err
	}
	defer tx.Rollback(ctx)

	var batchID int64
	existed := false
	err = tx.QueryRow(ctx, `
		INSERT INTO batches (org_id, schedule_id, connector_id, source_ref, status, temporal_workflow_id, started_at)
		VALUES ($1, $2, $3, $4, 'pending'::batch_status, $5, now())
		ON CONFLICT (temporal_workflow_id) DO NOTHING
		RETURNING id`,
		in.OrgID, nullIfZero(in.ScheduleID), nullIfZero(in.ConnectorID), srcJSON, workflowID,
	).Scan(&batchID)
	if errors.Is(err, pgx.ErrNoRows) {
		existed = true
		err = tx.QueryRow(ctx, `SELECT id FROM batches WHERE temporal_workflow_id = $1`, workflowID).Scan(&batchID)
	}
	if err != nil {
		return StartBatchOutput{}, fmt.Errorf("insert batch: %w", err)
	}
	if !existed {
		if in.ScheduleID > 0 {
			if _, err = tx.Exec(ctx, `
				UPDATE schedules SET last_run_at = $1, updated_at = now() WHERE id = $2`,
				in.ScheduledTime, in.ScheduleID,
			); err != nil {
				return StartBatchOutput{}, err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return StartBatchOutput{}, err
	}
	return StartBatchOutput{BatchID: batchID, WorkflowID: workflowID, Created: !existed}, nil
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ---------------- MarkBatchRunning ----------------

type MarkBatchRunningInput struct {
	BatchID int64  `json:"batchId"`
	RunID   string `json:"runId"`
}

func (a *Activities) MarkBatchRunning(ctx context.Context, in MarkBatchRunningInput) error {
	_, err := a.d.DB.Exec(ctx, `
		UPDATE batches SET status = 'running'::batch_status, temporal_run_id = $1,
			started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = $2`, in.RunID, in.BatchID)
	return err
}

// ---------------- UnpackAndStageArchive ----------------

type UnpackAndStageArchiveInput struct {
	OrgID   int64  `json:"orgId"`
	BatchID int64  `json:"batchId"`
	Bucket  string `json:"bucket"`
	Key     string `json:"key"`
}

type StagedFile struct {
	StageKey    string   `json:"stageKey"`
	File        string   `json:"file"`
	TemplateKey string   `json:"templateKey"`
	OnFailure   string   `json:"onFailure"`
	DependsOn   []string `json:"dependsOn,omitempty"`
	Bucket      string   `json:"bucket"`
	Key         string   `json:"key"`
	Filename    string   `json:"filename"`
	Size        int64    `json:"size"`
}

type UnpackAndStageArchiveOutput struct {
	ManifestJSON   json.RawMessage `json:"manifestJson"`
	OnStageFailure string          `json:"onStageFailure"`
	BatchKey       string          `json:"batchKey,omitempty"`
	Stages         []StagedFile    `json:"stages"`
}

func unpackLimitsFromEnv() batch.UnpackLimits {
	lim := batch.DefaultUnpackLimits()
	if v := os.Getenv("MAX_ARCHIVE_COMPRESSED_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			lim.MaxCompressedBytes = n
		}
	}
	if v := os.Getenv("MAX_ARCHIVE_UNCOMPRESSED_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			lim.MaxUncompressedBytes = n
		}
	}
	if v := os.Getenv("MAX_ARCHIVE_ENTRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lim.MaxEntries = n
		}
	}
	return lim
}

// UnpackAndStageArchive downloads, safely unpacks, validates manifest.json, and
// re-uploads each stage file under batches/{batchId}/stages/...
func (a *Activities) UnpackAndStageArchive(ctx context.Context, in UnpackAndStageArchiveInput) (UnpackAndStageArchiveOutput, error) {
	lim := unpackLimitsFromEnv()
	tmpRoot, err := os.MkdirTemp("", fmt.Sprintf("batch-%d-*", in.BatchID))
	if err != nil {
		return UnpackAndStageArchiveOutput{}, err
	}
	defer os.RemoveAll(tmpRoot)

	archivePath := filepath.Join(tmpRoot, path.Base(in.Key))
	if _, err := connectors.DownloadObject(ctx, in.Bucket, in.Key, archivePath, lim.MaxCompressedBytes); err != nil {
		return UnpackAndStageArchiveOutput{}, fmt.Errorf("download archive: %w", err)
	}

	extractDir := filepath.Join(tmpRoot, "extract")
	unpacked, err := batch.UnpackArchive(archivePath, extractDir, lim)
	if err != nil {
		return UnpackAndStageArchiveOutput{}, fmt.Errorf("unpack: %w", err)
	}

	manifestPath, ok := unpacked.Files["manifest.json"]
	if !ok {
		return UnpackAndStageArchiveOutput{}, fmt.Errorf("manifest.json missing at archive root")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return UnpackAndStageArchiveOutput{}, err
	}
	manifest, err := batch.ParseManifest(raw)
	if err != nil {
		return UnpackAndStageArchiveOutput{}, err
	}

	out := UnpackAndStageArchiveOutput{
		ManifestJSON:   raw,
		OnStageFailure: manifest.OnStageFailure,
		BatchKey:       manifest.BatchID,
		Stages:         make([]StagedFile, 0, len(manifest.Stages)),
	}

	for _, st := range manifest.Stages {
		abs, ok := unpacked.Files[st.File]
		if !ok {
			// Also try clean slash form.
			abs, ok = unpacked.Files[path.Clean(st.File)]
		}
		if !ok {
			return UnpackAndStageArchiveOutput{}, fmt.Errorf("stage %q file %q not found in archive", st.ID, st.File)
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return UnpackAndStageArchiveOutput{}, err
		}
		destKey := path.Join("batches", fmt.Sprintf("%d", in.BatchID), "stages", st.ID, path.Base(st.File))
		if err := connectors.PutObject(ctx, in.Bucket, destKey, abs, "application/octet-stream"); err != nil {
			return UnpackAndStageArchiveOutput{}, fmt.Errorf("upload stage %s: %w", st.ID, err)
		}
		out.Stages = append(out.Stages, StagedFile{
			StageKey:    st.ID,
			File:        st.File,
			TemplateKey: st.TemplateKey,
			OnFailure:   manifest.EffectiveOnFailure(st),
			DependsOn:   append([]string(nil), st.DependsOn...),
			Bucket:      in.Bucket,
			Key:         destKey,
			Filename:    path.Base(st.File),
			Size:        fi.Size(),
		})
	}

	if _, err := a.d.DB.Exec(ctx, `
		UPDATE batches SET batch_key = COALESCE(NULLIF($1, ''), batch_key),
			manifest_json = $2, on_stage_failure = $3, updated_at = now()
		WHERE id = $4`,
		manifest.BatchID, raw, manifest.OnStageFailure, in.BatchID,
	); err != nil {
		return UnpackAndStageArchiveOutput{}, fmt.Errorf("persist manifest: %w", err)
	}
	return out, nil
}

// ---------------- QuarantineArchive ----------------

type QuarantineArchiveInput struct {
	OrgID   int64  `json:"orgId"`
	BatchID int64  `json:"batchId"`
	Bucket  string `json:"bucket"`
	Key     string `json:"key"`
	Reason  string `json:"reason"`
}

type QuarantineArchiveOutput struct {
	QuarantineKey string `json:"quarantineKey"`
}

// QuarantineArchive copies the bad package under prefix/failed/yyyy/mm/dd/ and audits.
func (a *Activities) QuarantineArchive(ctx context.Context, in QuarantineArchiveInput) (QuarantineArchiveOutput, error) {
	dir := path.Dir(in.Key)
	if dir == "." {
		dir = ""
	}
	now := time.Now().UTC()
	qKey := path.Join(dir, "failed", now.Format("2006"), now.Format("01"), now.Format("02"), path.Base(in.Key))
	if err := connectors.CopyObject(ctx, in.Bucket, in.Key, qKey); err != nil {
		return QuarantineArchiveOutput{}, fmt.Errorf("quarantine copy: %w", err)
	}
	qRef, _ := json.Marshal(map[string]any{
		"type": "minio", "bucket": in.Bucket, "key": qKey, "reason": in.Reason,
	})
	_, err := a.d.DB.Exec(ctx, `
		UPDATE batches SET status = 'quarantined'::batch_status, quarantine_ref = $1,
			error_message = $2, finished_at = now(), updated_at = now()
		WHERE id = $3`,
		qRef, truncateErr(in.Reason, 2000), in.BatchID,
	)
	if err != nil {
		return QuarantineArchiveOutput{}, err
	}
	if _, err := a.d.DB.Exec(ctx, `
		INSERT INTO audit_log (org_id, actor, entity, entity_id, action, after_json)
		VALUES ($1, 'system', 'batch', $2, 'quarantine', $3)`,
		in.OrgID, fmt.Sprintf("%d", in.BatchID), qRef,
	); err != nil {
		return QuarantineArchiveOutput{}, fmt.Errorf("persist quarantine audit: %w", err)
	}
	metrics.QuarantinedBatches.Inc()
	return QuarantineArchiveOutput{QuarantineKey: qKey}, nil
}

func truncateErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------- MaterializeBatchStages ----------------

type MaterializeBatchStagesInput struct {
	OrgID          int64        `json:"orgId"`
	BatchID        int64        `json:"batchId"`
	OnStageFailure string       `json:"onStageFailure"`
	Stages         []StagedFile `json:"stages"`
}

type ResolvedStage struct {
	StageRowID          int64    `json:"stageRowId"`
	StageIndex          int      `json:"stageIndex"`
	StageKey            string   `json:"stageKey"`
	TemplateKey         string   `json:"templateKey"`
	RuleTemplateID      int64    `json:"ruleTemplateId"`
	RuleTemplateVersion int32    `json:"ruleTemplateVersion"`
	OnFailure           string   `json:"onFailure"`
	DependsOn           []string `json:"dependsOn,omitempty"`
	Bucket              string   `json:"bucket"`
	Key                 string   `json:"key"`
	Filename            string   `json:"filename"`
	Size                int64    `json:"size"`
}

type MaterializeBatchStagesOutput struct {
	Stages []ResolvedStage `json:"stages"`
}

// MaterializeBatchStages resolves templateKey → published template and inserts batch_stages rows.
func (a *Activities) MaterializeBatchStages(ctx context.Context, in MaterializeBatchStagesInput) (MaterializeBatchStagesOutput, error) {
	out := MaterializeBatchStagesOutput{Stages: make([]ResolvedStage, 0, len(in.Stages))}
	hasDependencies := false
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)

	for i, st := range in.Stages {
		hasDependencies = hasDependencies || len(st.DependsOn) > 0
		var tplID int64
		var tplVer int32
		err := tx.QueryRow(ctx, `
			SELECT id, version FROM rule_templates
			WHERE org_id = $1 AND template_key = $2 AND published = true
			ORDER BY version DESC LIMIT 1`,
			in.OrgID, st.TemplateKey,
		).Scan(&tplID, &tplVer)
		if err != nil {
			return out, fmt.Errorf("resolve templateKey %q: %w", st.TemplateKey, err)
		}
		onFail := st.OnFailure
		if onFail == "" {
			onFail = in.OnStageFailure
		}
		if onFail == "" {
			onFail = "stop"
		}
		deps := st.DependsOn
		if deps == nil {
			deps = []string{}
		}
		var stageRowID int64
		err = tx.QueryRow(ctx, `
			INSERT INTO batch_stages (
				batch_id, stage_index, stage_key, file_path, template_key,
				rule_template_id, rule_template_version, status, on_stage_failure, depends_on
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending'::batch_status, $8, $9)
			ON CONFLICT (batch_id, stage_index) DO UPDATE SET
				stage_key = EXCLUDED.stage_key,
				file_path = EXCLUDED.file_path,
				template_key = EXCLUDED.template_key,
				rule_template_id = EXCLUDED.rule_template_id,
				rule_template_version = EXCLUDED.rule_template_version,
				on_stage_failure = EXCLUDED.on_stage_failure,
				depends_on = EXCLUDED.depends_on,
				updated_at = now()
			RETURNING id`,
			in.BatchID, i, st.StageKey, st.File, st.TemplateKey, tplID, tplVer, onFail, deps,
		).Scan(&stageRowID)
		if err != nil {
			return out, fmt.Errorf("insert batch_stage: %w", err)
		}
		out.Stages = append(out.Stages, ResolvedStage{
			StageRowID: stageRowID, StageIndex: i, StageKey: st.StageKey,
			TemplateKey: st.TemplateKey, RuleTemplateID: tplID, RuleTemplateVersion: tplVer,
			OnFailure: onFail, DependsOn: append([]string(nil), st.DependsOn...),
			Bucket: st.Bucket, Key: st.Key, Filename: st.Filename, Size: st.Size,
		})
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	if hasDependencies {
		metrics.DAGBatches.Inc()
	}
	return out, nil
}

// ---------------- StartBatchStageJob ----------------

type StartBatchStageJobInput struct {
	OrgID               int64  `json:"orgId"`
	BatchID             int64  `json:"batchId"`
	StageRowID          int64  `json:"stageRowId"`
	ScheduleID          int64  `json:"scheduleId"`
	RuleTemplateID      int64  `json:"ruleTemplateId"`
	RuleTemplateVersion int32  `json:"ruleTemplateVersion"`
	Bucket              string `json:"bucket"`
	Key                 string `json:"key"`
	Filename            string `json:"filename"`
	Size                int64  `json:"size"`
	StageKey            string `json:"stageKey"`
}

type StartBatchStageJobOutput struct {
	JobID      int64  `json:"jobId"`
	WorkflowID string `json:"workflowId"`
	Created    bool   `json:"created"`
}

// StageWorkflowID is the Temporal child id for one batch stage job.
func StageWorkflowID(batchID int64, stageKey string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", batchID, stageKey)))
	return fmt.Sprintf("batch-stage-%d-%s", batchID, hex.EncodeToString(sum[:12]))
}

// StartBatchStageJob creates a jobs row linked to the batch stage.
func (a *Activities) StartBatchStageJob(ctx context.Context, in StartBatchStageJobInput) (StartBatchStageJobOutput, error) {
	workflowID := StageWorkflowID(in.BatchID, in.StageKey)
	source := map[string]any{
		"type":     "minio",
		"bucket":   in.Bucket,
		"key":      in.Key,
		"filename": in.Filename,
		"size":     in.Size,
		"batchId":  in.BatchID,
		"stageKey": in.StageKey,
	}
	srcJSON, err := json.Marshal(source)
	if err != nil {
		return StartBatchStageJobOutput{}, err
	}

	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return StartBatchStageJobOutput{}, err
	}
	defer tx.Rollback(ctx)

	var jobID int64
	existed := false
	err = tx.QueryRow(ctx, `
		INSERT INTO jobs (
			org_id, rule_template_id, rule_template_version, schedule_id,
			source_ref, status, temporal_workflow_id, started_at, batch_id, batch_stage_id
		) VALUES ($1, $2, $3, $4, $5, 'pending'::job_status, $6, now(), $7, $8)
		ON CONFLICT (temporal_workflow_id) DO NOTHING
		RETURNING id`,
		in.OrgID, in.RuleTemplateID, in.RuleTemplateVersion, nullIfZero(in.ScheduleID),
		srcJSON, workflowID, in.BatchID, in.StageRowID,
	).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		existed = true
		err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE temporal_workflow_id = $1`, workflowID).Scan(&jobID)
	}
	if err != nil {
		return StartBatchStageJobOutput{}, fmt.Errorf("insert stage job: %w", err)
	}
	if !existed {
		if _, err = tx.Exec(ctx, `
			UPDATE batch_stages SET job_id = $1, status = 'running'::batch_status, updated_at = now()
			WHERE id = $2`, jobID, in.StageRowID,
		); err != nil {
			return StartBatchStageJobOutput{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return StartBatchStageJobOutput{}, err
	}
	return StartBatchStageJobOutput{JobID: jobID, WorkflowID: workflowID, Created: !existed}, nil
}

// ---------------- FinalizeBatchStage ----------------

type FinalizeBatchStageInput struct {
	StageRowID int64  `json:"stageRowId"`
	JobID      int64  `json:"jobId"`
	Status     string `json:"status"` // succeeded | failed
	Error      string `json:"error,omitempty"`
}

func (a *Activities) FinalizeBatchStage(ctx context.Context, in FinalizeBatchStageInput) error {
	status := strings.ToLower(in.Status)
	if status != "succeeded" && status != "failed" {
		status = "failed"
	}
	_, err := a.d.DB.Exec(ctx, `
		UPDATE batch_stages SET status = $1::batch_status, error_message = NULLIF($2, ''),
			job_id = COALESCE(job_id, $3), updated_at = now()
		WHERE id = $4`,
		status, truncateErr(in.Error, 2000), nullIfZero(in.JobID), in.StageRowID,
	)
	return err
}

// ---------------- FinalizeBatch ----------------

type FinalizeBatchInput struct {
	BatchID int64  `json:"batchId"`
	Status  string `json:"status"` // succeeded | failed | partial
	Error   string `json:"error,omitempty"`
}

func (a *Activities) FinalizeBatch(ctx context.Context, in FinalizeBatchInput) error {
	status := strings.ToLower(in.Status)
	switch status {
	case "succeeded", "failed", "partial", "quarantined":
	default:
		status = "failed"
	}
	_, err := a.d.DB.Exec(ctx, `
		UPDATE batches SET status = $1::batch_status, error_message = NULLIF($2, ''),
			finished_at = now(), updated_at = now()
		WHERE id = $3`,
		status, truncateErr(in.Error, 2000), in.BatchID,
	)
	return err
}
