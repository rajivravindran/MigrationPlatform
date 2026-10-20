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
	"github.com/migration-platform/orchestrator/internal/lifecycle"
	"github.com/migration-platform/orchestrator/internal/metrics"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
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

// BatchWorkflowID is the legacy workflow id for an archive package, naming the
// source version only. See WatchWorkflowID for why it is kept.
func BatchWorkflowID(orgID int64, bucket, key, etag string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", orgID, bucket, key, etag)))
	return fmt.Sprintf("batch-%d-%s", orgID, hex.EncodeToString(sum[:16]))
}

// BatchExecutionWorkflowID names the execution intent for a watched archive:
// the source version plus the schedule that discovered it (review A4).
// Without a schedule the legacy source-only id is used.
func BatchExecutionWorkflowID(orgID, scheduleID int64, bucket, key, etag string) string {
	if scheduleID <= 0 {
		return BatchWorkflowID(orgID, bucket, key, etag)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|s%d|%s|%s|%s", orgID, scheduleID, bucket, key, etag)))
	return fmt.Sprintf("batch-%d-%s", orgID, hex.EncodeToString(sum[:16]))
}

// StartBatch inserts a batches row for a watched archive. Idempotent on
// workflow id; a batch this schedule already created under the legacy
// source-only id is reused rather than duplicated.
func (a *Activities) StartBatch(ctx context.Context, in StartBatchInput) (StartBatchOutput, error) {
	if err := a.sources.checkOrgLocation(in.OrgID, in.Bucket, in.Key); err != nil {
		return StartBatchOutput{}, err
	}
	workflowID := BatchExecutionWorkflowID(in.OrgID, in.ScheduleID, in.Bucket, in.Key, in.ETag)
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return StartBatchOutput{}, err
	}
	defer tx.Rollback(ctx)

	if legacyID := BatchWorkflowID(in.OrgID, in.Bucket, in.Key, in.ETag); legacyID != workflowID {
		var legacyBatchID int64
		err := tx.QueryRow(ctx, `
			SELECT id FROM batches WHERE temporal_workflow_id = $1 AND org_id = $2 AND schedule_id = $3`,
			legacyID, in.OrgID, in.ScheduleID).Scan(&legacyBatchID)
		switch {
		case err == nil:
			return StartBatchOutput{BatchID: legacyBatchID, WorkflowID: legacyID, Created: false}, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return StartBatchOutput{}, fmt.Errorf("look up legacy batch: %w", err)
		}
	}

	sourceID, err := registerSource(ctx, tx, newSource{
		OrgID: in.OrgID, Bucket: in.Bucket, Key: in.Key, Filename: path.Base(in.Key),
		ETag: in.ETag, Size: in.Size, Provenance: "watched",
	})
	if err != nil {
		return StartBatchOutput{}, err
	}
	srcJSON, err := json.Marshal(map[string]any{
		"type":      "minio",
		"source_id": sourceID,
		"bucket":    in.Bucket,
		"key":       in.Key,
		"filename":  path.Base(in.Key),
		"size":      in.Size,
		"etag":      in.ETag,
	})
	if err != nil {
		return StartBatchOutput{}, err
	}

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

// MarkBatchRunning records that the workflow owns the batch. It is
// conditional: a batch that already reached a terminal state (a fast run
// finalized before a redelivered start was recorded, or a quarantine) is
// never put back to running (ADR 0004 transition rules, review A2).
func (a *Activities) MarkBatchRunning(ctx context.Context, in MarkBatchRunningInput) error {
	_, err := a.d.DB.Exec(ctx, `
		UPDATE batches SET status = 'running'::batch_status, temporal_run_id = $1,
			started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = $2 AND status IN ('pending'::batch_status, 'running'::batch_status)`, in.RunID, in.BatchID)
	return err
}

// ---------------- UnpackAndStageArchive ----------------

type UnpackAndStageArchiveInput struct {
	OrgID   int64  `json:"orgId"`
	BatchID int64  `json:"batchId"`
	Bucket  string `json:"bucket"`
	Key     string `json:"key"`
	// Filename is the original archive name when Key has no extension
	// (uploaded sources); it selects the format only, never a path.
	Filename string `json:"filename,omitempty"`
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
	// ETag is the staged object's content version, recorded at upload (F3).
	ETag string `json:"etag,omitempty"`
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

// openedArchive is a registered archive that was downloaded at its recorded
// version, safely unpacked and whose manifest.json validated. The caller
// removes tmpRoot.
type openedArchive struct {
	tmpRoot  string
	files    map[string]string
	raw      []byte
	manifest *batch.Manifest
}

// openArchive is the shared front half of every archive intake (batches and
// plan archives): authorize, bind to the registered version, unpack, parse.
// filename carries the format when key has no extension (uploads).
func (a *Activities) openArchive(ctx context.Context, orgID int64, bucket, key, filename, label string) (*openedArchive, error) {
	src, err := a.sources.store.SourceByObject(ctx, orgID, bucket, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notAuthorized("archive %s/%s is not registered for org %d", bucket, key, orgID)
		}
		return nil, fmt.Errorf("load archive source: %w", err)
	}
	// The download is bound to the registered archive version (F3). An
	// archive registered before versions were recorded cannot be verified
	// against its registration; it is bound to the version seen now and that
	// version is recorded, so later attempts and stages cannot drift.
	archiveETag := src.ETag
	if archiveETag == "" {
		info, err := a.sources.stat(ctx, bucket, key)
		if err != nil {
			return nil, fmt.Errorf("stat archive: %w", err)
		}
		archiveETag = info.ETag
		activity.GetLogger(ctx).Warn("archive was registered without a version; binding to the current object version",
			"archive", label, "key", key, "etag", archiveETag)
		if _, err := registerSource(ctx, a.d.DB, newSource{
			OrgID: orgID, Bucket: bucket, Key: key, Filename: path.Base(key),
			ETag: archiveETag, Size: info.Size, Provenance: "watched",
		}); err != nil {
			return nil, err
		}
	}
	lim := unpackLimitsFromEnv()
	tmpRoot, err := os.MkdirTemp("", label+"-*")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*openedArchive, error) {
		os.RemoveAll(tmpRoot)
		return nil, err
	}

	name := filename
	if name == "" {
		name = key
	}
	ext := batch.ArchiveExt(name)
	if ext == "" {
		return fail(temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("not an archive package: %s", name), "NotAnArchive", nil))
	}
	archivePath := filepath.Join(tmpRoot, "archive"+ext)
	if _, err := connectors.DownloadObject(ctx, bucket, key, archivePath, lim.MaxCompressedBytes, archiveETag); err != nil {
		if errors.Is(err, connectors.ErrSourceChanged) {
			return fail(temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("archive %s/%s changed since registration: %v", bucket, key, err), ErrTypeSourceChanged, err))
		}
		return fail(fmt.Errorf("download archive: %w", err))
	}

	unpacked, err := batch.UnpackArchive(archivePath, filepath.Join(tmpRoot, "extract"), lim)
	if err != nil {
		return fail(fmt.Errorf("unpack: %w", err))
	}
	manifestPath, ok := unpacked.Files["manifest.json"]
	if !ok {
		return fail(fmt.Errorf("manifest.json missing at archive root"))
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fail(err)
	}
	manifest, err := batch.ParseManifest(raw)
	if err != nil {
		return fail(err)
	}
	return &openedArchive{tmpRoot: tmpRoot, files: unpacked.Files, raw: raw, manifest: manifest}, nil
}

// stageFile uploads one manifest stage's file under destPrefix/<stage id>/.
func (o *openedArchive) stageFile(ctx context.Context, bucket, destPrefix string, st batch.ManifestStage) (StagedFile, error) {
	abs, ok := o.files[st.File]
	if !ok {
		// Also try clean slash form.
		abs, ok = o.files[path.Clean(st.File)]
	}
	if !ok {
		return StagedFile{}, fmt.Errorf("stage %q file %q not found in archive", st.ID, st.File)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return StagedFile{}, err
	}
	destKey := path.Join(destPrefix, st.ID, path.Base(st.File))
	stored, err := connectors.PutObject(ctx, bucket, destKey, abs, "application/octet-stream")
	if err != nil {
		return StagedFile{}, fmt.Errorf("upload stage %s: %w", st.ID, err)
	}
	return StagedFile{
		StageKey:    st.ID,
		File:        st.File,
		TemplateKey: st.TemplateKey,
		OnFailure:   o.manifest.EffectiveOnFailure(st),
		DependsOn:   append([]string(nil), st.DependsOn...),
		Bucket:      bucket,
		Key:         destKey,
		Filename:    path.Base(st.File),
		Size:        fi.Size(),
		ETag:        stored.ETag,
	}, nil
}

// UnpackAndStageArchive downloads, safely unpacks, validates manifest.json, and
// re-uploads each stage file under batches/{batchId}/stages/...
func (a *Activities) UnpackAndStageArchive(ctx context.Context, in UnpackAndStageArchiveInput) (UnpackAndStageArchiveOutput, error) {
	arc, err := a.openArchive(ctx, in.OrgID, in.Bucket, in.Key, in.Filename, fmt.Sprintf("batch-%d", in.BatchID))
	if err != nil {
		return UnpackAndStageArchiveOutput{}, err
	}
	defer os.RemoveAll(arc.tmpRoot)
	raw, manifest := arc.raw, arc.manifest

	out := UnpackAndStageArchiveOutput{
		ManifestJSON:   raw,
		OnStageFailure: manifest.OnStageFailure,
		BatchKey:       manifest.BatchID,
		Stages:         make([]StagedFile, 0, len(manifest.Stages)),
	}

	prefix := path.Join("batches", fmt.Sprintf("%d", in.BatchID), "stages")
	for _, st := range manifest.Stages {
		staged, err := arc.stageFile(ctx, in.Bucket, prefix, st)
		if err != nil {
			return UnpackAndStageArchiveOutput{}, err
		}
		out.Stages = append(out.Stages, staged)
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
	ETag                string   `json:"etag,omitempty"`
}

type MaterializeBatchStagesOutput struct {
	Stages []ResolvedStage `json:"stages"`
}

// MaterializeBatchStages resolves templateKey → published template and inserts batch_stages rows.
// MaterializeBatchStages binds every manifest stage to a published template
// version and persists the binding. The binding is resolved once per batch:
// on redelivery (a lost activity acknowledgment, a worker crash after commit)
// an existing stage row keeps the template version it was first bound to,
// even if a newer version was published in between, so the plan a batch runs
// cannot change between attempts (R08 plan binding; architecture review,
// "future plan binding"). Stage key, file and template key are fixed by the
// archive's manifest; a row that disagrees with the manifest is a conflict,
// not something to overwrite.
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

		// Latest published version, used only when this stage has no binding yet.
		var latestID int64
		var latestVer int32
		err := tx.QueryRow(ctx, `
			SELECT id, version FROM rule_templates
			WHERE org_id = $1 AND template_key = $2 AND published = true
			ORDER BY version DESC LIMIT 1`,
			in.OrgID, st.TemplateKey,
		).Scan(&latestID, &latestVer)
		if err != nil {
			return out, fmt.Errorf("resolve templateKey %q: %w", st.TemplateKey, err)
		}

		var stageRowID int64
		var boundID int64
		var boundVer int32
		var boundStageKey, boundTemplateKey, boundFile string
		err = tx.QueryRow(ctx, `
			INSERT INTO batch_stages (
				batch_id, stage_index, stage_key, file_path, template_key,
				rule_template_id, rule_template_version, status, on_stage_failure, depends_on
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending'::batch_status, $8, $9)
			ON CONFLICT (batch_id, stage_index) DO UPDATE SET
				rule_template_id = COALESCE(batch_stages.rule_template_id, EXCLUDED.rule_template_id),
				rule_template_version = COALESCE(batch_stages.rule_template_version, EXCLUDED.rule_template_version),
				on_stage_failure = EXCLUDED.on_stage_failure,
				depends_on = EXCLUDED.depends_on,
				updated_at = now()
			RETURNING id, rule_template_id, rule_template_version, stage_key, template_key, file_path`,
			in.BatchID, i, st.StageKey, st.File, st.TemplateKey, latestID, latestVer, onFail, deps,
		).Scan(&stageRowID, &boundID, &boundVer, &boundStageKey, &boundTemplateKey, &boundFile)
		if err != nil {
			return out, fmt.Errorf("insert batch_stage: %w", err)
		}
		if boundStageKey != st.StageKey || boundTemplateKey != st.TemplateKey || boundFile != st.File {
			return out, temporal.NewNonRetryableApplicationError(fmt.Sprintf(
				"batch %d stage %d is already bound to %s/%s (%s) but the manifest names %s/%s (%s)",
				in.BatchID, i, boundStageKey, boundTemplateKey, boundFile, st.StageKey, st.TemplateKey, st.File),
				"StageBindingConflict", nil)
		}
		out.Stages = append(out.Stages, ResolvedStage{
			StageRowID: stageRowID, StageIndex: i, StageKey: st.StageKey,
			TemplateKey: st.TemplateKey, RuleTemplateID: boundID, RuleTemplateVersion: boundVer,
			OnFailure: onFail, DependsOn: append([]string(nil), st.DependsOn...),
			Bucket: st.Bucket, Key: st.Key, Filename: st.Filename, Size: st.Size, ETag: st.ETag,
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
	// ETag is the staged object's content version; empty for stages staged
	// before versions were recorded, which then rely on size alone.
	ETag string `json:"etag,omitempty"`
}

type StartBatchStageJobOutput struct {
	JobID      int64  `json:"jobId"`
	WorkflowID string `json:"workflowId"`
	Created    bool   `json:"created"`
	SourceID   int64  `json:"sourceId"`
}

// StageWorkflowID is the Temporal child id for one batch stage job.
func StageWorkflowID(batchID int64, stageKey string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", batchID, stageKey)))
	return fmt.Sprintf("batch-stage-%d-%s", batchID, hex.EncodeToString(sum[:12]))
}

// StartBatchStageJob creates a jobs row linked to the batch stage.
func (a *Activities) StartBatchStageJob(ctx context.Context, in StartBatchStageJobInput) (StartBatchStageJobOutput, error) {
	workflowID := StageWorkflowID(in.BatchID, in.StageKey)
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return StartBatchStageJobOutput{}, err
	}
	defer tx.Rollback(ctx)

	var batchOrg int64
	if err := tx.QueryRow(ctx, `SELECT org_id FROM batches WHERE id = $1`, in.BatchID).Scan(&batchOrg); err != nil {
		return StartBatchStageJobOutput{}, fmt.Errorf("load batch %d: %w", in.BatchID, err)
	}
	if batchOrg != in.OrgID || !strings.HasPrefix(in.Key, fmt.Sprintf("batches/%d/stages/", in.BatchID)) {
		return StartBatchStageJobOutput{}, notAuthorized("stage object %s is not part of batch %d for org %d", in.Key, in.BatchID, in.OrgID)
	}
	sourceID, err := registerSource(ctx, tx, newSource{
		OrgID: in.OrgID, Bucket: in.Bucket, Key: in.Key, Filename: in.Filename,
		ETag: in.ETag, Size: in.Size, Provenance: "batch_stage",
	})
	if err != nil {
		return StartBatchStageJobOutput{}, err
	}
	srcJSON, err := json.Marshal(map[string]any{
		"type":      "minio",
		"source_id": sourceID,
		"bucket":    in.Bucket,
		"key":       in.Key,
		"filename":  in.Filename,
		"size":      in.Size,
		"etag":      in.ETag,
		"batchId":   in.BatchID,
		"stageKey":  in.StageKey,
	})
	if err != nil {
		return StartBatchStageJobOutput{}, err
	}

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
	return StartBatchStageJobOutput{JobID: jobID, WorkflowID: workflowID, Created: !existed, SourceID: sourceID}, nil
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
	BatchID int64 `json:"batchId"`
	// Status is the counter-derived status the workflow computed. Workers
	// built before DeriveFromStages existed write it as-is (their original
	// behaviour), so a mixed-version rollout never records "failed" for a
	// batch the old worker would have called succeeded.
	Status string `json:"status,omitempty"`
	// DeriveFromStages asks this worker to ignore Status and derive the
	// verdict from the persisted stage outcomes (lifecycle.FinalizeBatch).
	// BatchWorkflow sets it on every finalize-on-exit call.
	DeriveFromStages bool `json:"deriveFromStages,omitempty"`
	// Error records an execution failure outside a stage; the batch is failed.
	Error string `json:"error,omitempty"`
	// Cancelled records that the batch workflow was cancelled.
	Cancelled bool `json:"cancelled,omitempty"`
}

// FinalizeBatchOutput is the persisted batch verdict.
type FinalizeBatchOutput struct {
	Status string                `json:"status"`
	Reason string                `json:"reason"`
	Stages lifecycle.StageTotals `json:"stages"`
}

// FinalizeBatch writes the batch's terminal status. With DeriveFromStages
// (or an empty Status) the verdict is derived from batch_stages, one rule
// shared with the reconciler; otherwise the caller-supplied Status is written
// as legacy histories recorded it.
func (a *Activities) FinalizeBatch(ctx context.Context, in FinalizeBatchInput) (*FinalizeBatchOutput, error) {
	if in.DeriveFromStages || in.Status == "" {
		v, err := lifecycle.FinalizeBatch(ctx, a.d.DB, in.BatchID, lifecycle.Outcome{Cancelled: in.Cancelled, Error: truncateErr(in.Error, 2000)})
		if err != nil {
			return nil, err
		}
		return &FinalizeBatchOutput{Status: v.Status, Reason: v.Reason, Stages: v.Stages}, nil
	}
	status := strings.ToLower(in.Status)
	switch status {
	case "succeeded", "failed", "partial", "quarantined":
	default:
		status = "failed"
	}
	_, err := a.d.DB.Exec(ctx, `
		UPDATE batches SET status = $1::batch_status, error_message = NULLIF($2, ''),
			finished_at = now(), updated_at = now()
		WHERE id = $3 AND status <> 'quarantined'::batch_status`,
		status, truncateErr(in.Error, 2000), in.BatchID,
	)
	if err != nil {
		return nil, err
	}
	return &FinalizeBatchOutput{Status: status, Reason: in.Error}, nil
}
