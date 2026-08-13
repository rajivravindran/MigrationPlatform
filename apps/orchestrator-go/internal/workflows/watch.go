package workflows

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/batch"
	"github.com/migration-platform/orchestrator/internal/connectors"
)

// WatchPrefixWorkflowInput is fired by Temporal Schedules bound to a
// watched_prefix or watched_sftp connector. It lists new objects and starts one
// MigrationWorkflow child per plain file, or one BatchWorkflow child per archive.
type WatchPrefixWorkflowInput struct {
	OrgID               int64     `json:"orgId"`
	ScheduleID          int64     `json:"scheduleId"`
	RuleTemplateID      int64     `json:"ruleTemplateId"`
	RuleTemplateVersion int32     `json:"ruleTemplateVersion"`
	ConnectorID         int64     `json:"connectorId"`
	ScheduledTime       time.Time `json:"scheduledTime,omitempty"`
}

type WatchPrefixWorkflowResult struct {
	Listed  int `json:"listed"`
	Started int `json:"started"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// WatchPrefixWorkflow discovers new objects under a connector's source
// (MinIO/S3 prefix or SFTP path) and starts an idempotent child workflow per object.
func WatchPrefixWorkflow(ctx workflow.Context, in WatchPrefixWorkflowInput) (*WatchPrefixWorkflowResult, error) {
	log := workflow.GetLogger(ctx)
	log.Info("WatchPrefixWorkflow start",
		"scheduleId", in.ScheduleID, "connectorId", in.ConnectorID, "orgId", in.OrgID)

	ao := defaultActivityOptions(nil)
	ctx = workflow.WithActivityOptions(ctx, ao)
	if err := requireLicensed(ctx); err != nil {
		return nil, err
	}

	result := &WatchPrefixWorkflowResult{}
	scheduled := in.ScheduledTime
	if scheduled.IsZero() {
		scheduled = workflow.Now(ctx)
	}

	var conn activities.LoadConnectorOutput
	if err := workflow.ExecuteActivity(ctx, "LoadConnector", activities.LoadConnectorInput{
		OrgID: in.OrgID, ConnectorID: in.ConnectorID,
	}).Get(ctx, &conn); err != nil {
		return result, fmt.Errorf("load connector: %w", err)
	}

	kind := strings.ToLower(conn.Kind)
	var listedObjs []connectors.ObjectInfo
	var workBucket string
	stageFromSFTP := false

	switch kind {
	case "watched_prefix", "watched-prefix":
		bucket, _ := conn.Config["bucket"].(string)
		prefix, _ := conn.Config["prefix"].(string)
		glob, _ := conn.Config["glob"].(string)
		sortBy, _ := conn.Config["sort"].(string)
		if bucket == "" {
			if def, ok := conn.Config["s3_bucket"].(string); ok {
				bucket = def
			}
		}
		if glob == "" {
			glob = "*"
		}
		if sortBy == "" {
			sortBy = "lexical"
		}
		var listed activities.ListPrefixObjectsOutput
		if err := workflow.ExecuteActivity(ctx, "ListPrefixObjects", activities.ListPrefixObjectsInput{
			Bucket: bucket, Prefix: prefix, Glob: glob, Sort: sortBy,
		}).Get(ctx, &listed); err != nil {
			return result, fmt.Errorf("list objects: %w", err)
		}
		listedObjs = listed.Objects
		workBucket = bucket

	case "watched_sftp", "sftp":
		var listed activities.ListSftpObjectsOutput
		if err := workflow.ExecuteActivity(ctx, "ListSftpObjects", activities.ListSftpObjectsInput{
			OrgID: in.OrgID, ConnectorID: in.ConnectorID,
		}).Get(ctx, &listed); err != nil {
			return result, fmt.Errorf("list sftp objects: %w", err)
		}
		listedObjs = listed.Objects
		workBucket = listed.StagingBucket
		stageFromSFTP = true

	default:
		return result, fmt.Errorf("connector %d kind %q is not a watched arrival connector", in.ConnectorID, conn.Kind)
	}

	result.Listed = len(listedObjs)

	seen := map[string]string{}
	if cursor, ok := conn.Config["cursor"].(map[string]any); ok {
		if prev, ok := cursor["seen"].(map[string]any); ok {
			for k, v := range prev {
				if s, ok := v.(string); ok {
					seen[k] = s
				}
			}
		}
	}

	type workItem struct {
		remoteKey string // cursor key (remote path or object key)
		bucket    string
		key       string // object-store key used by child workflows
		etag      string
		size      int64
	}

	var newObjs []workItem
	var newArchives []workItem
	for _, obj := range listedObjs {
		// Quarantine copies live under .../failed/yyyy/mm/dd/ — never re-ingest them.
		if strings.Contains(obj.Key, "/failed/") || strings.HasPrefix(obj.Key, "failed/") {
			result.Skipped++
			continue
		}
		if etag, ok := seen[obj.Key]; ok && etag == obj.ETag {
			result.Skipped++
			continue
		}
		item := workItem{
			remoteKey: obj.Key,
			bucket:    workBucket,
			key:       obj.Key,
			etag:      obj.ETag,
			size:      obj.Size,
		}
		if stageFromSFTP {
			var staged activities.StageSftpObjectOutput
			err := workflow.ExecuteActivity(ctx, "StageSftpObject", activities.StageSftpObjectInput{
				OrgID: in.OrgID, ConnectorID: in.ConnectorID,
				RemoteKey: obj.Key, ETag: obj.ETag, Size: obj.Size,
			}).Get(ctx, &staged)
			if err != nil {
				log.Error("StageSftpObject failed", "key", obj.Key, "err", err)
				result.Failed++
				continue
			}
			item.bucket = staged.Bucket
			item.key = staged.Key
			item.etag = staged.ETag
			item.size = staged.Size
		}
		if batch.IsArchive(obj.Key) {
			newArchives = append(newArchives, item)
			continue
		}
		newObjs = append(newObjs, item)
	}

	advanced := map[string]string{}
	for _, obj := range newArchives {
		var started activities.StartBatchOutput
		err := workflow.ExecuteActivity(ctx, "StartBatch", activities.StartBatchInput{
			OrgID: in.OrgID, ScheduleID: in.ScheduleID, ConnectorID: in.ConnectorID,
			Bucket: obj.bucket, Key: obj.key, ETag: obj.etag, Size: obj.size, ScheduledTime: scheduled,
		}).Get(ctx, &started)
		if err != nil {
			log.Error("StartBatch failed", "key", obj.remoteKey, "err", err)
			result.Failed++
			continue
		}

		cwo := workflow.ChildWorkflowOptions{
			WorkflowID:               started.WorkflowID,
			TaskQueue:                "migration",
			WorkflowExecutionTimeout: 48 * time.Hour,
			WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
			// WatchPrefix only waits for child *start*, then completes. Default
			// ParentClosePolicy TERMINATE would kill BatchWorkflow mid-unpack.
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
			RetryPolicy:       &temporal.RetryPolicy{MaximumAttempts: 1},
		}
		childCtx := workflow.WithChildOptions(ctx, cwo)
		child := workflow.ExecuteChildWorkflow(childCtx, BatchWorkflow, BatchWorkflowInput{
			OrgID: in.OrgID, BatchID: started.BatchID, ScheduleID: in.ScheduleID,
			ConnectorID: in.ConnectorID, Bucket: obj.bucket, Key: obj.key, ETag: obj.etag,
			Size: obj.size, ScheduledTime: scheduled,
		})
		var exec workflow.Execution
		if err := child.GetChildWorkflowExecution().Get(ctx, &exec); err != nil {
			var already *temporal.ChildWorkflowExecutionAlreadyStartedError
			if temporal.IsWorkflowExecutionAlreadyStartedError(err) || errors.As(err, &already) ||
				strings.Contains(err.Error(), "already started") || strings.Contains(err.Error(), "AlreadyStarted") {
				result.Skipped++
				advanced[obj.remoteKey] = obj.etag
				continue
			}
			log.Error("batch child start failed", "key", obj.remoteKey, "err", err)
			result.Failed++
			continue
		}
		if err := workflow.ExecuteActivity(ctx, "MarkBatchRunning", activities.MarkBatchRunningInput{
			BatchID: started.BatchID, RunID: exec.RunID,
		}).Get(ctx, nil); err != nil {
			return result, fmt.Errorf("mark batch running: %w", err)
		}
		result.Started++
		advanced[obj.remoteKey] = obj.etag
	}

	for _, obj := range newObjs {
		var started activities.StartWatchObjectJobOutput
		err := workflow.ExecuteActivity(ctx, "StartWatchObjectJob", activities.StartWatchObjectJobInput{
			OrgID: in.OrgID, ScheduleID: in.ScheduleID,
			RuleTemplateID: in.RuleTemplateID, RuleTemplateVersion: in.RuleTemplateVersion,
			ConnectorID: in.ConnectorID, Bucket: obj.bucket,
			Key: obj.key, ETag: obj.etag, Size: obj.size, ScheduledTime: scheduled,
		}).Get(ctx, &started)
		if err != nil {
			log.Error("StartWatchObjectJob failed", "key", obj.remoteKey, "err", err)
			result.Failed++
			continue
		}

		cwo := workflow.ChildWorkflowOptions{
			WorkflowID:               started.WorkflowID,
			TaskQueue:                "migration",
			WorkflowExecutionTimeout: 24 * time.Hour,
			WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
			// Same as batch children: parent returns after start; must not terminate.
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 1,
			},
		}
		childCtx := workflow.WithChildOptions(ctx, cwo)

		child := workflow.ExecuteChildWorkflow(childCtx, MigrationWorkflow, MigrationWorkflowInput{
			OrgID:               in.OrgID,
			JobID:               started.JobID,
			RuleTemplateID:      in.RuleTemplateID,
			RuleTemplateVersion: in.RuleTemplateVersion,
			ScheduleID:          in.ScheduleID,
			ScheduledTime:       scheduled,
			SourceRef: map[string]any{
				"type":     "minio",
				"bucket":   obj.bucket,
				"key":      obj.key,
				"filename": path.Base(obj.remoteKey),
				"size":     obj.size,
				"etag":     obj.etag,
			},
		})

		var exec workflow.Execution
		if err := child.GetChildWorkflowExecution().Get(ctx, &exec); err != nil {
			var already *temporal.ChildWorkflowExecutionAlreadyStartedError
			if temporal.IsWorkflowExecutionAlreadyStartedError(err) || errors.As(err, &already) {
				result.Skipped++
				advanced[obj.remoteKey] = obj.etag
				continue
			}
			// Fallback string match — SDK error types vary by version.
			if strings.Contains(err.Error(), "already started") || strings.Contains(err.Error(), "AlreadyStarted") {
				result.Skipped++
				advanced[obj.remoteKey] = obj.etag
				continue
			}
			log.Error("child start failed", "key", obj.remoteKey, "err", err)
			result.Failed++
			continue
		}
		if err := workflow.ExecuteActivity(ctx, "MarkJobRunning", activities.MarkJobRunningInput{
			JobID: started.JobID, RunID: exec.RunID,
		}).Get(ctx, nil); err != nil {
			return result, fmt.Errorf("mark watch job running: %w", err)
		}

		result.Started++
		advanced[obj.remoteKey] = obj.etag
	}

	if len(advanced) > 0 {
		if err := workflow.ExecuteActivity(ctx, "AdvanceConnectorCursor", activities.AdvanceConnectorCursorInput{
			OrgID: in.OrgID, ConnectorID: in.ConnectorID, Seen: advanced,
		}).Get(ctx, nil); err != nil {
			return result, fmt.Errorf("advance cursor: %w", err)
		}
	}

	log.Info("WatchPrefixWorkflow done",
		"listed", result.Listed, "started", result.Started, "skipped", result.Skipped, "failed", result.Failed)
	return result, nil
}
