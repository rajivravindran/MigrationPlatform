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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/migration-platform/orchestrator/internal/connectors"
	"github.com/migration-platform/orchestrator/internal/metrics"
	"github.com/migration-platform/orchestrator/internal/security"
)

// ---------------- LoadConnector ----------------

type LoadConnectorInput struct {
	OrgID       int64 `json:"orgId"`
	ConnectorID int64 `json:"connectorId"`
}

type LoadConnectorOutput struct {
	ID     int64          `json:"id"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Config map[string]any `json:"config"`
}

func (a *Activities) LoadConnector(ctx context.Context, in LoadConnectorInput) (LoadConnectorOutput, error) {
	var kind, name string
	var raw []byte
	err := a.d.DB.QueryRow(ctx, `
		SELECT connector_kind::text, name, config_json
		FROM connectors WHERE id = $1 AND org_id = $2`,
		in.ConnectorID, in.OrgID,
	).Scan(&kind, &name, &raw)
	if err != nil {
		return LoadConnectorOutput{}, fmt.Errorf("load connector: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return LoadConnectorOutput{}, fmt.Errorf("parse connector config: %w", err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return LoadConnectorOutput{ID: in.ConnectorID, Kind: kind, Name: name, Config: cfg}, nil
}

// ---------------- ListPrefixObjects ----------------

type ListPrefixObjectsInput struct {
	OrgID  int64  `json:"orgId"`
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
	Glob   string `json:"glob"`
	Sort   string `json:"sort"`
}

type ListPrefixObjectsOutput struct {
	Objects []connectors.ObjectInfo `json:"objects"`
}

func (a *Activities) ListPrefixObjects(ctx context.Context, in ListPrefixObjectsInput) (ListPrefixObjectsOutput, error) {
	if err := a.sources.checkOrgLocation(in.OrgID, in.Bucket, in.Prefix); err != nil {
		return ListPrefixObjectsOutput{}, err
	}
	objs, err := connectors.ListPrefixObjects(ctx, in.Bucket, in.Prefix, in.Glob, in.Sort)
	if err != nil {
		return ListPrefixObjectsOutput{}, err
	}
	observeWatchLag("object_store", objs)
	return ListPrefixObjectsOutput{Objects: objs}, nil
}

// ---------------- ListSftpObjects ----------------

type ListSftpObjectsInput struct {
	OrgID       int64 `json:"orgId"`
	ConnectorID int64 `json:"connectorId"`
}

type ListSftpObjectsOutput struct {
	Objects       []connectors.ObjectInfo `json:"objects"`
	StagingBucket string                  `json:"stagingBucket"`
	StagingPrefix string                  `json:"stagingPrefix"`
}

// ListSftpObjects loads connector config + decrypted secret, then lists remote files.
// Credentials never enter workflow history — only this activity sees them.
func (a *Activities) ListSftpObjects(ctx context.Context, in ListSftpObjectsInput) (ListSftpObjectsOutput, error) {
	release, err := a.acquireSFTP(ctx)
	if err != nil {
		return ListSftpObjectsOutput{}, err
	}
	defer release()
	cfg, auth, err := a.loadSftpConnector(ctx, in.OrgID, in.ConnectorID)
	if err != nil {
		return ListSftpObjectsOutput{}, err
	}
	objs, err := connectors.ListSftpObjects(ctx, cfg, auth)
	if err != nil {
		metrics.SFTPFailures.WithLabelValues("list").Inc()
		return ListSftpObjectsOutput{}, err
	}
	observeWatchLag("sftp", objs)
	return ListSftpObjectsOutput{
		Objects: objs, StagingBucket: cfg.StagingBucket, StagingPrefix: cfg.StagingPrefix,
	}, nil
}

func observeWatchLag(kind string, objs []connectors.ObjectInfo) {
	if len(objs) == 0 {
		return
	}
	oldest := objs[0].LastModified
	for _, obj := range objs[1:] {
		if obj.LastModified.Before(oldest) {
			oldest = obj.LastModified
		}
	}
	if lag := time.Since(oldest).Seconds(); lag >= 0 {
		metrics.WatchLag.WithLabelValues(kind).Observe(lag)
	}
}

// ---------------- StageSftpObject ----------------

type StageSftpObjectInput struct {
	OrgID       int64  `json:"orgId"`
	ConnectorID int64  `json:"connectorId"`
	RemoteKey   string `json:"remoteKey"`
	ETag        string `json:"etag"`
	Size        int64  `json:"size"`
}

type StageSftpObjectOutput struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
	// ObjectETag is the staged object's ETag in the object store (the content
	// version); ETag above is the SFTP fingerprint used by the discovery cursor.
	ObjectETag string `json:"objectEtag,omitempty"`
}

// StageSftpObject downloads one remote file into the MinIO staging bucket so
// MigrationWorkflow / BatchWorkflow can reuse the existing minio source_ref path.
func (a *Activities) StageSftpObject(ctx context.Context, in StageSftpObjectInput) (StageSftpObjectOutput, error) {
	release, err := a.acquireSFTP(ctx)
	if err != nil {
		return StageSftpObjectOutput{}, err
	}
	defer release()
	cfg, auth, err := a.loadSftpConnector(ctx, in.OrgID, in.ConnectorID)
	if err != nil {
		return StageSftpObjectOutput{}, err
	}
	stagingKey := connectors.StagingObjectKey(cfg.StagingPrefix, in.ConnectorID, in.RemoteKey)
	if err := a.sources.checkOrgLocation(in.OrgID, cfg.StagingBucket, stagingKey); err != nil {
		return StageSftpObjectOutput{}, err
	}
	tmp, err := os.MkdirTemp("", "sftp-stage-*")
	if err != nil {
		return StageSftpObjectOutput{}, err
	}
	defer os.RemoveAll(tmp)

	local := filepath.Join(tmp, path.Base(in.RemoteKey))
	n, err := connectors.DownloadSftpFile(ctx, cfg, auth, in.RemoteKey, local, in.ETag, in.Size)
	if err != nil {
		metrics.SFTPFailures.WithLabelValues("download").Inc()
		return StageSftpObjectOutput{}, err
	}
	size := in.Size
	if size <= 0 {
		size = n
	}
	stored, err := connectors.PutObject(ctx, cfg.StagingBucket, stagingKey, local, "")
	if err != nil {
		return StageSftpObjectOutput{}, fmt.Errorf("stage to object store: %w", err)
	}
	// Keep the SFTP fingerprint as etag so cursor diffs stay consistent; the
	// staged object's own ETag is what the source registry and conditional
	// reads are bound to (F2/F3).
	etag := in.ETag
	if etag == "" {
		etag = connectors.SftpFingerprint(time.Now().UTC(), size)
	}
	return StageSftpObjectOutput{
		Bucket: cfg.StagingBucket, Key: stagingKey, ETag: etag, Size: size, ObjectETag: stored.ETag,
	}, nil
}

func (a *Activities) loadSftpConnector(ctx context.Context, orgID, connectorID int64) (connectors.SftpConfig, connectors.SftpAuth, error) {
	var kind string
	var raw []byte
	var secretID *int64
	err := a.d.DB.QueryRow(ctx, `
		SELECT connector_kind::text, config_json, secret_id
		FROM connectors WHERE id = $1 AND org_id = $2`,
		connectorID, orgID,
	).Scan(&kind, &raw, &secretID)
	if err != nil {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, fmt.Errorf("load sftp connector: %w", err)
	}
	kindLower := strings.ToLower(kind)
	if kindLower != "watched_sftp" && kindLower != "sftp" {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, fmt.Errorf("connector %d kind %q is not watched_sftp", connectorID, kind)
	}
	var cfgMap map[string]any
	if err := json.Unmarshal(raw, &cfgMap); err != nil {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, err
	}
	cfg, err := connectors.ParseSftpConfig(cfgMap)
	if err != nil {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, err
	}
	if p := strings.Trim(cfg.StagingPrefix, "/"); p == "" || p == "sftp-landing" {
		cfg.StagingPrefix = OrgObjectPrefix(orgID) + "sftp-landing/"
	}
	if err := a.sources.checkOrgLocation(orgID, cfg.StagingBucket, cfg.StagingPrefix); err != nil {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, err
	}
	if secretID == nil {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, fmt.Errorf("watched_sftp connector %d has no secret_id", connectorID)
	}
	var ct, nonce []byte
	err = a.d.DB.QueryRow(ctx, `
		SELECT ciphertext, nonce FROM secrets WHERE id = $1 AND org_id = $2`,
		*secretID, orgID,
	).Scan(&ct, &nonce)
	if err != nil {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, fmt.Errorf("load sftp secret: %w", err)
	}
	plain, err := security.DecryptSecret(a.d.SecretsMasterKey, ct, nonce)
	if err != nil {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, fmt.Errorf("decrypt sftp secret: %w", err)
	}
	auth, err := connectors.ParseSftpAuth(string(plain))
	if err != nil {
		return connectors.SftpConfig{}, connectors.SftpAuth{}, err
	}
	return cfg, auth, nil
}

// TestSFTPConnector performs a real authenticated handshake and bounded remote
// listing using the same host-key and egress controls as scheduled execution.
func (a *Activities) TestSFTPConnector(ctx context.Context, orgID, connectorID int64) error {
	release, err := a.acquireSFTP(ctx)
	if err != nil {
		return err
	}
	defer release()
	cfg, auth, err := a.loadSftpConnector(ctx, orgID, connectorID)
	if err != nil {
		return err
	}
	_, err = connectors.ListSftpObjects(ctx, cfg, auth)
	if err != nil {
		metrics.SFTPFailures.WithLabelValues("test").Inc()
		return fmt.Errorf("SFTP validation failed: %w", err)
	}
	return nil
}

// ---------------- AdvanceConnectorCursor ----------------

type AdvanceConnectorCursorInput struct {
	OrgID       int64 `json:"orgId"`
	ConnectorID int64 `json:"connectorId"`
	// ScheduleID scopes the discovery cursor to the schedule (the execution
	// intent) that processed these objects. Zero writes the legacy
	// connector-wide cursor, shared by every schedule on the connector.
	ScheduleID int64             `json:"scheduleId,omitempty"`
	Seen       map[string]string `json:"seen"` // key -> etag
}

// Cursor layout inside connectors.config_json (review A4):
//
//	cursor.seen                       legacy, connector-wide: key -> etag
//	cursor.bySchedule.<scheduleId>.seen per execution intent: key -> etag
//	cursor.updatedAt
//
// SeenObjects merges the legacy connector-wide cursor with the schedule's own
// cursor. The legacy entries count as seen for every schedule: they were
// recorded when one cursor served all schedules, and reprocessing them
// would repeat external effects. Objects discovered after the per-schedule
// cursors exist are deduplicated per schedule only, so two schedules over
// the same source each execute once. A later template version within the
// same schedule does not reprocess source versions that schedule has seen;
// replaying them is an explicit operator action, not a side effect of a
// version change.
func SeenObjects(config map[string]any, scheduleID int64) map[string]string {
	seen := map[string]string{}
	cursor, _ := config["cursor"].(map[string]any)
	if cursor == nil {
		return seen
	}
	copySeen := func(src map[string]any) {
		for k, v := range src {
			if s, ok := v.(string); ok {
				seen[k] = s
			}
		}
	}
	if legacy, ok := cursor["seen"].(map[string]any); ok {
		copySeen(legacy)
	}
	if scheduleID > 0 {
		if by, ok := cursor["bySchedule"].(map[string]any); ok {
			if own, ok := by[strconv.FormatInt(scheduleID, 10)].(map[string]any); ok {
				if s, ok := own["seen"].(map[string]any); ok {
					copySeen(s)
				}
			}
		}
	}
	return seen
}

// advanceSeen merges newly seen objects into the cursor map for one intent
// (the legacy connector-wide map when scheduleID is zero) and bounds its
// size. It returns the modified cursor.
func advanceSeen(cursor map[string]any, scheduleID int64, newly map[string]string, now time.Time) map[string]any {
	if cursor == nil {
		cursor = map[string]any{}
	}
	owner := cursor
	if scheduleID > 0 {
		by, _ := cursor["bySchedule"].(map[string]any)
		if by == nil {
			by = map[string]any{}
		}
		key := strconv.FormatInt(scheduleID, 10)
		own, _ := by[key].(map[string]any)
		if own == nil {
			own = map[string]any{}
		}
		by[key] = own
		cursor["bySchedule"] = by
		owner = own
	}
	seen, _ := owner["seen"].(map[string]any)
	if seen == nil {
		seen = map[string]any{}
	}
	for k, etag := range newly {
		seen[k] = etag
	}
	const maxCursorEntries = 10_000
	if len(seen) > maxCursorEntries {
		keys := make([]string, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[:len(keys)-maxCursorEntries] {
			delete(seen, k)
		}
	}
	owner["seen"] = seen
	owner["updatedAt"] = now.UTC().Format(time.RFC3339)
	cursor["updatedAt"] = now.UTC().Format(time.RFC3339)
	return cursor
}

func (a *Activities) AdvanceConnectorCursor(ctx context.Context, in AdvanceConnectorCursorInput) error {
	if len(in.Seen) == 0 {
		return nil
	}
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var raw []byte
	err = tx.QueryRow(ctx, `
		SELECT config_json FROM connectors WHERE id = $1 AND org_id = $2 FOR UPDATE`,
		in.ConnectorID, in.OrgID,
	).Scan(&raw)
	if err != nil {
		return fmt.Errorf("lock connector: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	cursor, _ := cfg["cursor"].(map[string]any)
	cfg["cursor"] = advanceSeen(cursor, in.ScheduleID, in.Seen, time.Now())

	next, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE connectors SET config_json = $1, updated_at = now() WHERE id = $2 AND org_id = $3`,
		next, in.ConnectorID, in.OrgID,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------------- StartWatchObjectJob ----------------

type StartWatchObjectJobInput struct {
	OrgID               int64     `json:"orgId"`
	ScheduleID          int64     `json:"scheduleId"`
	RuleTemplateID      int64     `json:"ruleTemplateId"`
	RuleTemplateVersion int32     `json:"ruleTemplateVersion"`
	ConnectorID         int64     `json:"connectorId"`
	Bucket              string    `json:"bucket"`
	Key                 string    `json:"key"`
	ETag                string    `json:"etag"`
	Size                int64     `json:"size"`
	ScheduledTime       time.Time `json:"scheduledTime"`
	// ContentETag is the object store's ETag for the object when ETag is a
	// discovery fingerprint rather than a content version (SFTP staging).
	// The source registry records the content version so reads can be bound
	// to it; identity hashing keeps using ETag.
	ContentETag string `json:"contentEtag,omitempty"`
}

type StartWatchObjectJobOutput struct {
	JobID      int64  `json:"jobId"`
	WorkflowID string `json:"workflowId"`
	Created    bool   `json:"created"` // false if idempotent conflict (already exists)
	SourceID   int64  `json:"sourceId"`
}

// WatchWorkflowID is the legacy workflow id for a watched object: it names
// the source version only (organization, bucket, key, ETag). Two schedules
// over the same object resolved to the same id, so the second was skipped as
// already started (review A4). Kept for recognising jobs created before the
// intent-scoped id.
func WatchWorkflowID(orgID int64, bucket, key, etag string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", orgID, bucket, key, etag)))
	return fmt.Sprintf("watch-%d-%s", orgID, hex.EncodeToString(sum[:16]))
}

// WatchExecutionWorkflowID names an execution intent: the source version
// together with the schedule that binds it to a template and destination.
// Distinct schedules over the same object get distinct ids and each executes
// once; redelivery of the same schedule still deduplicates. Without a
// schedule the legacy source-only id is used.
func WatchExecutionWorkflowID(orgID, scheduleID int64, bucket, key, etag string) string {
	if scheduleID <= 0 {
		return WatchWorkflowID(orgID, bucket, key, etag)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|s%d|%s|%s|%s", orgID, scheduleID, bucket, key, etag)))
	return fmt.Sprintf("watch-%d-%s", orgID, hex.EncodeToString(sum[:16]))
}

// StartWatchObjectJob inserts a jobs (+ schedule_runs) row for one discovered
// object. Idempotent on temporal_workflow_id so redeliveries reuse the same
// job. A job this schedule already created under the legacy source-only id
// is reused rather than duplicated.
func (a *Activities) StartWatchObjectJob(ctx context.Context, in StartWatchObjectJobInput) (StartWatchObjectJobOutput, error) {
	if err := a.sources.checkOrgLocation(in.OrgID, in.Bucket, in.Key); err != nil {
		return StartWatchObjectJobOutput{}, err
	}
	filename := path.Base(in.Key)
	workflowID := WatchExecutionWorkflowID(in.OrgID, in.ScheduleID, in.Bucket, in.Key, in.ETag)
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return StartWatchObjectJobOutput{}, err
	}
	defer tx.Rollback(ctx)

	if legacyID := WatchWorkflowID(in.OrgID, in.Bucket, in.Key, in.ETag); legacyID != workflowID {
		var legacyJobID, legacySourceID int64
		err := tx.QueryRow(ctx, `
			SELECT j.id, COALESCE((j.source_ref->>'source_id')::bigint, 0)
			FROM jobs j
			WHERE j.temporal_workflow_id = $1 AND j.org_id = $2 AND j.schedule_id = $3`,
			legacyID, in.OrgID, in.ScheduleID).Scan(&legacyJobID, &legacySourceID)
		switch {
		case err == nil:
			return StartWatchObjectJobOutput{JobID: legacyJobID, WorkflowID: legacyID, Created: false, SourceID: legacySourceID}, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return StartWatchObjectJobOutput{}, fmt.Errorf("look up legacy watch job: %w", err)
		}
	}

	contentETag := in.ContentETag
	if contentETag == "" {
		contentETag = in.ETag
	}
	sourceID, err := registerSource(ctx, tx, newSource{
		OrgID: in.OrgID, Bucket: in.Bucket, Key: in.Key, Filename: filename,
		ETag: contentETag, Size: in.Size, Provenance: "watched",
	})
	if err != nil {
		return StartWatchObjectJobOutput{}, err
	}
	srcJSON, err := json.Marshal(map[string]any{
		"type":      "minio",
		"source_id": sourceID,
		"bucket":    in.Bucket,
		"key":       in.Key,
		"filename":  filename,
		"size":      in.Size,
		"etag":      contentETag,
	})
	if err != nil {
		return StartWatchObjectJobOutput{}, err
	}

	var jobID int64
	existed := false
	err = tx.QueryRow(ctx, `
			INSERT INTO jobs (org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status, temporal_workflow_id, started_at)
			VALUES ($1, $2, $3, $4, $5, 'pending'::job_status, $6, now())
			ON CONFLICT (temporal_workflow_id) DO NOTHING
			RETURNING id`,
		in.OrgID, in.RuleTemplateID, in.RuleTemplateVersion, in.ScheduleID, srcJSON, workflowID,
	).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		existed = true
		err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE temporal_workflow_id = $1`, workflowID).Scan(&jobID)
	}
	if err != nil {
		return StartWatchObjectJobOutput{}, fmt.Errorf("insert job: %w", err)
	}
	if !existed {
		if _, err = tx.Exec(ctx, `
			INSERT INTO schedule_runs (schedule_id, job_id, scheduled_time, actual_start_time, status)
			VALUES ($1, $2, $3, now(), 'running'::job_status)`,
			in.ScheduleID, jobID, in.ScheduledTime,
		); err != nil {
			return StartWatchObjectJobOutput{}, fmt.Errorf("insert schedule_run: %w", err)
		}
		if _, err = tx.Exec(ctx, `
			UPDATE schedules SET last_run_at = $1, updated_at = now() WHERE id = $2`,
			in.ScheduledTime, in.ScheduleID,
		); err != nil {
			return StartWatchObjectJobOutput{}, err
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return StartWatchObjectJobOutput{}, err
	}
	return StartWatchObjectJobOutput{JobID: jobID, WorkflowID: workflowID, Created: !existed, SourceID: sourceID}, nil
}

// MarkJobRunningInput marks a pending watch job as running after child start.
type MarkJobRunningInput struct {
	JobID int64  `json:"jobId"`
	RunID string `json:"runId"`
}

// MarkJobRunning records the run id after the child MigrationWorkflow has
// started. Only a pending job becomes running: a fast child may already have
// finalized it.
func (a *Activities) MarkJobRunning(ctx context.Context, in MarkJobRunningInput) error {
	_, err := a.d.DB.Exec(ctx, `
		UPDATE jobs SET
			status = CASE WHEN status = 'pending'::job_status THEN 'running'::job_status ELSE status END,
			temporal_run_id = $1, started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = $2`, in.RunID, in.JobID)
	return err
}
