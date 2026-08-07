package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/migration-platform/orchestrator/internal/connectors"
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
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
	Glob   string `json:"glob"`
	Sort   string `json:"sort"`
}

type ListPrefixObjectsOutput struct {
	Objects []connectors.ObjectInfo `json:"objects"`
}

func (a *Activities) ListPrefixObjects(ctx context.Context, in ListPrefixObjectsInput) (ListPrefixObjectsOutput, error) {
	objs, err := connectors.ListPrefixObjects(ctx, in.Bucket, in.Prefix, in.Glob, in.Sort)
	if err != nil {
		return ListPrefixObjectsOutput{}, err
	}
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
	cfg, auth, err := a.loadSftpConnector(ctx, in.OrgID, in.ConnectorID)
	if err != nil {
		return ListSftpObjectsOutput{}, err
	}
	objs, err := connectors.ListSftpObjects(ctx, cfg, auth)
	if err != nil {
		return ListSftpObjectsOutput{}, err
	}
	return ListSftpObjectsOutput{
		Objects: objs, StagingBucket: cfg.StagingBucket, StagingPrefix: cfg.StagingPrefix,
	}, nil
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
}

// StageSftpObject downloads one remote file into the MinIO staging bucket so
// MigrationWorkflow / BatchWorkflow can reuse the existing minio source_ref path.
func (a *Activities) StageSftpObject(ctx context.Context, in StageSftpObjectInput) (StageSftpObjectOutput, error) {
	cfg, auth, err := a.loadSftpConnector(ctx, in.OrgID, in.ConnectorID)
	if err != nil {
		return StageSftpObjectOutput{}, err
	}
	stagingKey := connectors.StagingObjectKey(cfg.StagingPrefix, in.ConnectorID, in.RemoteKey)
	tmp, err := os.MkdirTemp("", "sftp-stage-*")
	if err != nil {
		return StageSftpObjectOutput{}, err
	}
	defer os.RemoveAll(tmp)

	local := filepath.Join(tmp, path.Base(in.RemoteKey))
	n, err := connectors.DownloadSftpFile(ctx, cfg, auth, in.RemoteKey, local, 0)
	if err != nil {
		return StageSftpObjectOutput{}, err
	}
	size := in.Size
	if size <= 0 {
		size = n
	}
	if err := connectors.PutObject(ctx, cfg.StagingBucket, stagingKey, local, ""); err != nil {
		return StageSftpObjectOutput{}, fmt.Errorf("stage to object store: %w", err)
	}
	// Keep the SFTP fingerprint as etag so cursor diffs stay consistent.
	etag := in.ETag
	if etag == "" {
		etag = connectors.SftpFingerprint(time.Now().UTC(), size)
	}
	return StageSftpObjectOutput{
		Bucket: cfg.StagingBucket, Key: stagingKey, ETag: etag, Size: size,
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

// ---------------- AdvanceConnectorCursor ----------------

type AdvanceConnectorCursorInput struct {
	OrgID       int64             `json:"orgId"`
	ConnectorID int64             `json:"connectorId"`
	Seen        map[string]string `json:"seen"` // key -> etag
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
	if cursor == nil {
		cursor = map[string]any{}
	}
	seen, _ := cursor["seen"].(map[string]any)
	if seen == nil {
		seen = map[string]any{}
	}
	for k, etag := range in.Seen {
		seen[k] = etag
	}
	cursor["seen"] = seen
	cursor["updatedAt"] = time.Now().UTC().Format(time.RFC3339)
	cfg["cursor"] = cursor

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
}

type StartWatchObjectJobOutput struct {
	JobID      int64  `json:"jobId"`
	WorkflowID string `json:"workflowId"`
	Created    bool   `json:"created"` // false if idempotent conflict (already exists)
}

// WatchWorkflowID builds a stable Temporal workflow id for a watched object.
func WatchWorkflowID(orgID int64, bucket, key, etag string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", orgID, bucket, key, etag)))
	return fmt.Sprintf("watch-%d-%s", orgID, hex.EncodeToString(sum[:16]))
}

// StartWatchObjectJob inserts a jobs (+ schedule_runs) row for one discovered
// object. Idempotent on temporal_workflow_id so redeliveries reuse the same job.
func (a *Activities) StartWatchObjectJob(ctx context.Context, in StartWatchObjectJobInput) (StartWatchObjectJobOutput, error) {
	filename := path.Base(in.Key)
	workflowID := WatchWorkflowID(in.OrgID, in.Bucket, in.Key, in.ETag)
	source := map[string]any{
		"type":     "minio",
		"bucket":   in.Bucket,
		"key":      in.Key,
		"filename": filename,
		"size":     in.Size,
		"etag":     in.ETag,
	}
	srcJSON, err := json.Marshal(source)
	if err != nil {
		return StartWatchObjectJobOutput{}, err
	}

	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return StartWatchObjectJobOutput{}, err
	}
	defer tx.Rollback(ctx)

	var jobID int64
	var existed bool
	err = tx.QueryRow(ctx, `
		SELECT id FROM jobs WHERE temporal_workflow_id = $1`, workflowID).Scan(&jobID)
	if err == nil {
		existed = true
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO jobs (org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status, temporal_workflow_id, started_at)
			VALUES ($1, $2, $3, $4, $5, 'pending'::job_status, $6, now())
			RETURNING id`,
			in.OrgID, in.RuleTemplateID, in.RuleTemplateVersion, in.ScheduleID, srcJSON, workflowID,
		).Scan(&jobID)
		if err != nil {
			return StartWatchObjectJobOutput{}, fmt.Errorf("insert job: %w", err)
		}
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
	return StartWatchObjectJobOutput{JobID: jobID, WorkflowID: workflowID, Created: !existed}, nil
}

// MarkJobRunningInput marks a pending watch job as running after child start.
type MarkJobRunningInput struct {
	JobID int64  `json:"jobId"`
	RunID string `json:"runId"`
}

// MarkJobRunning sets status/run id after the child MigrationWorkflow has started.
func (a *Activities) MarkJobRunning(ctx context.Context, in MarkJobRunningInput) error {
	_, err := a.d.DB.Exec(ctx, `
		UPDATE jobs SET status = 'running'::job_status, temporal_run_id = $1, started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = $2`, in.RunID, in.JobID)
	return err
}
