package workflows

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/migration-platform/orchestrator/internal/activities"
	"github.com/migration-platform/orchestrator/internal/batch"
)

// BatchWorkflowInput is started for each watched (or manually submitted) archive package.
type BatchWorkflowInput struct {
	OrgID         int64     `json:"orgId"`
	BatchID       int64     `json:"batchId"`
	ScheduleID    int64     `json:"scheduleId,omitempty"`
	ConnectorID   int64     `json:"connectorId,omitempty"`
	Bucket        string    `json:"bucket"`
	Key           string    `json:"key"`
	ETag          string    `json:"etag,omitempty"`
	Size          int64     `json:"size,omitempty"`
	ScheduledTime time.Time `json:"scheduledTime,omitempty"`
}

type BatchWorkflowResult struct {
	Status       string `json:"status"`
	StagesTotal  int    `json:"stagesTotal"`
	StagesOK     int    `json:"stagesOk"`
	StagesFailed int    `json:"stagesFailed"`
	Quarantined  bool   `json:"quarantined,omitempty"`
}

// BatchWorkflow unpacks an archive with manifest.json, then runs child
// MigrationWorkflows per stage. Without dependsOn: ordered sequential (P2).
// With any dependsOn: topological DAG; ready stages run in parallel (P4a).
// onStageFailure stop|continue controls whether further stages start after a failure.
func BatchWorkflow(ctx workflow.Context, in BatchWorkflowInput) (*BatchWorkflowResult, error) {
	log := workflow.GetLogger(ctx)
	log.Info("BatchWorkflow start", "batchId", in.BatchID, "key", in.Key)

	ao := defaultActivityOptions(nil)
	ctx = workflow.WithActivityOptions(ctx, ao)
	if err := requireLicensed(ctx); err != nil {
		return nil, err
	}

	result := &BatchWorkflowResult{Status: "failed"}

	if !batch.IsArchive(in.Key) {
		return result, fmt.Errorf("not an archive package: %s", in.Key)
	}

	if err := workflow.ExecuteActivity(ctx, "MarkBatchRunning", activities.MarkBatchRunningInput{
		BatchID: in.BatchID,
		RunID:   workflow.GetInfo(ctx).WorkflowExecution.RunID,
	}).Get(ctx, nil); err != nil {
		return result, fmt.Errorf("mark batch running: %w", err)
	}

	var unpacked activities.UnpackAndStageArchiveOutput
	err := workflow.ExecuteActivity(ctx, "UnpackAndStageArchive", activities.UnpackAndStageArchiveInput{
		OrgID: in.OrgID, BatchID: in.BatchID, Bucket: in.Bucket, Key: in.Key,
	}).Get(ctx, &unpacked)
	if err != nil {
		log.Error("unpack failed; quarantining", "err", err)
		if qErr := workflow.ExecuteActivity(ctx, "QuarantineArchive", activities.QuarantineArchiveInput{
			OrgID: in.OrgID, BatchID: in.BatchID, Bucket: in.Bucket, Key: in.Key, Reason: err.Error(),
		}).Get(ctx, nil); qErr != nil {
			return result, fmt.Errorf("unpack failed (%v); quarantine persistence failed: %w", err, qErr)
		}
		result.Status = "quarantined"
		result.Quarantined = true
		return result, nil
	}

	var resolved activities.MaterializeBatchStagesOutput
	err = workflow.ExecuteActivity(ctx, "MaterializeBatchStages", activities.MaterializeBatchStagesInput{
		OrgID: in.OrgID, BatchID: in.BatchID,
		OnStageFailure: unpacked.OnStageFailure, Stages: unpacked.Stages,
	}).Get(ctx, &resolved)
	if err != nil {
		log.Error("materialize stages failed; quarantining", "err", err)
		if qErr := workflow.ExecuteActivity(ctx, "QuarantineArchive", activities.QuarantineArchiveInput{
			OrgID: in.OrgID, BatchID: in.BatchID, Bucket: in.Bucket, Key: in.Key, Reason: err.Error(),
		}).Get(ctx, nil); qErr != nil {
			return result, fmt.Errorf("materialize failed (%v); quarantine persistence failed: %w", err, qErr)
		}
		result.Status = "quarantined"
		result.Quarantined = true
		return result, nil
	}

	result.StagesTotal = len(resolved.Stages)
	nodes := stageNodes(resolved.Stages)
	if batch.UsesDependsOnNodes(nodes) {
		log.Info("BatchWorkflow DAG mode (dependsOn)")
		if err := runStagesDAG(ctx, in, resolved.Stages, result); err != nil {
			return result, err
		}
	} else {
		log.Info("BatchWorkflow sequential mode")
		if err := runStagesSequential(ctx, in, resolved.Stages, result); err != nil {
			return result, err
		}
	}

	final := "succeeded"
	if result.StagesFailed > 0 && result.StagesOK > 0 {
		final = "partial"
	} else if result.StagesFailed > 0 {
		final = "failed"
	}
	result.Status = final
	if err := workflow.ExecuteActivity(ctx, "FinalizeBatch", activities.FinalizeBatchInput{
		BatchID: in.BatchID, Status: final,
	}).Get(ctx, nil); err != nil {
		return result, fmt.Errorf("finalize batch: %w", err)
	}

	log.Info("BatchWorkflow done", "status", final, "ok", result.StagesOK, "failed", result.StagesFailed)
	return result, nil
}

func stageNodes(stages []activities.ResolvedStage) []batch.StageNode {
	nodes := make([]batch.StageNode, len(stages))
	for i, st := range stages {
		nodes[i] = batch.StageNode{ID: st.StageKey, DependsOn: st.DependsOn}
	}
	return nodes
}

func runStagesSequential(ctx workflow.Context, in BatchWorkflowInput, stages []activities.ResolvedStage, result *BatchWorkflowResult) error {
	stopEarly := false
	for _, st := range stages {
		if stopEarly {
			if err := skipStage(ctx, st, "skipped due to prior stage failure", result); err != nil {
				return err
			}
			continue
		}
		ok, stop, err := runOneStage(ctx, in, st, result)
		if err != nil {
			return err
		}
		if !ok && stop {
			stopEarly = true
		}
	}
	return nil
}

func runStagesDAG(ctx workflow.Context, in BatchWorkflowInput, stages []activities.ResolvedStage, result *BatchWorkflowResult) error {
	log := workflow.GetLogger(ctx)
	nodes := stageNodes(stages)
	byKey := map[string]activities.ResolvedStage{}
	status := map[string]batch.StageRunStatus{}
	for _, st := range stages {
		byKey[st.StageKey] = st
		status[st.StageKey] = batch.StagePending
	}

	stopEarly := false
	for {
		// Skip pending stages blocked by failed/skipped deps, or global stop.
		progress := false
		for _, n := range nodes {
			if status[n.ID] != batch.StagePending {
				continue
			}
			if stopEarly {
				if err := skipStage(ctx, byKey[n.ID], "skipped due to prior stage failure", result); err != nil {
					return err
				}
				status[n.ID] = batch.StageSkipped
				progress = true
				continue
			}
			if batch.DepsBlocking(n, status) {
				if err := skipStage(ctx, byKey[n.ID], "skipped due to failed dependency", result); err != nil {
					return err
				}
				status[n.ID] = batch.StageSkipped
				progress = true
			}
		}

		readyIDs := batch.ReadyStageIDs(nodes, status)
		if len(readyIDs) == 0 {
			// Any remaining pending is a scheduling deadlock (should not happen after Validate).
			for _, n := range nodes {
				if status[n.ID] == batch.StagePending {
					if err := skipStage(ctx, byKey[n.ID], "skipped: dependencies never satisfied", result); err != nil {
						return err
					}
					status[n.ID] = batch.StageSkipped
				}
			}
			if !progress {
				break
			}
			continue
		}

		type waveItem struct {
			st      activities.ResolvedStage
			jobID   int64
			child   workflow.ChildWorkflowFuture
			startOK bool
		}
		wave := make([]waveItem, 0, len(readyIDs))

		for _, id := range readyIDs {
			if stopEarly {
				if err := skipStage(ctx, byKey[id], "skipped due to prior stage failure", result); err != nil {
					return err
				}
				status[id] = batch.StageSkipped
				continue
			}
			st := byKey[id]
			item := waveItem{st: st}
			var started activities.StartBatchStageJobOutput
			err := workflow.ExecuteActivity(ctx, "StartBatchStageJob", activities.StartBatchStageJobInput{
				OrgID: in.OrgID, BatchID: in.BatchID, StageRowID: st.StageRowID,
				ScheduleID: in.ScheduleID, RuleTemplateID: st.RuleTemplateID,
				RuleTemplateVersion: st.RuleTemplateVersion,
				Bucket:              st.Bucket, Key: st.Key, Filename: st.Filename, Size: st.Size,
				StageKey: st.StageKey,
			}).Get(ctx, &started)
			if err != nil {
				if persistErr := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
					StageRowID: st.StageRowID, Status: "failed", Error: err.Error(),
				}); persistErr != nil {
					return persistErr
				}
				result.StagesFailed++
				status[st.StageKey] = batch.StageFailed
				if st.OnFailure != "continue" {
					stopEarly = true
				}
				continue
			}
			item.jobID = started.JobID
			item.startOK = true

			cwo := workflow.ChildWorkflowOptions{
				WorkflowID:               started.WorkflowID,
				TaskQueue:                "migration",
				WorkflowExecutionTimeout: 24 * time.Hour,
				WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
				RetryPolicy:              &temporal.RetryPolicy{MaximumAttempts: 1},
			}
			childCtx := workflow.WithChildOptions(ctx, cwo)
			item.child = workflow.ExecuteChildWorkflow(childCtx, MigrationWorkflow, MigrationWorkflowInput{
				OrgID:               in.OrgID,
				JobID:               started.JobID,
				RuleTemplateID:      st.RuleTemplateID,
				RuleTemplateVersion: st.RuleTemplateVersion,
				ScheduleID:          in.ScheduleID,
				ScheduledTime:       in.ScheduledTime,
				SourceRef: map[string]any{
					"type":     "minio",
					"bucket":   st.Bucket,
					"key":      st.Key,
					"filename": st.Filename,
					"size":     st.Size,
				},
			})

			var exec workflow.Execution
			if err := item.child.GetChildWorkflowExecution().Get(ctx, &exec); err != nil {
				var already *temporal.ChildWorkflowExecutionAlreadyStartedError
				if temporal.IsWorkflowExecutionAlreadyStartedError(err) || errors.As(err, &already) ||
					strings.Contains(err.Error(), "already started") || strings.Contains(err.Error(), "AlreadyStarted") {
					log.Info("stage child already started", "stage", st.StageKey)
				} else {
					if persistErr := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
						StageRowID: st.StageRowID, JobID: started.JobID, Status: "failed", Error: err.Error(),
					}); persistErr != nil {
						return persistErr
					}
					result.StagesFailed++
					status[st.StageKey] = batch.StageFailed
					item.startOK = false
					if st.OnFailure != "continue" {
						stopEarly = true
					}
					continue
				}
			} else {
				if err := workflow.ExecuteActivity(ctx, "MarkJobRunning", activities.MarkJobRunningInput{
					JobID: started.JobID, RunID: exec.RunID,
				}).Get(ctx, nil); err != nil {
					return fmt.Errorf("mark stage job running: %w", err)
				}
			}
			wave = append(wave, item)
		}

		// Wait for the parallel wave.
		for _, item := range wave {
			if !item.startOK || item.child == nil {
				continue
			}
			var childResult MigrationWorkflowResult
			childErr := item.child.Get(ctx, &childResult)
			stageStatus := "succeeded"
			stageErr := ""
			if childErr != nil {
				stageStatus = "failed"
				stageErr = childErr.Error()
				result.StagesFailed++
				status[item.st.StageKey] = batch.StageFailed
				if item.st.OnFailure != "continue" {
					stopEarly = true
				}
			} else if childResult.Failed > 0 || childResult.Cancelled {
				stageStatus = "failed"
				stageErr = fmt.Sprintf("job finished with failed=%d cancelled=%v", childResult.Failed, childResult.Cancelled)
				result.StagesFailed++
				status[item.st.StageKey] = batch.StageFailed
				if item.st.OnFailure != "continue" {
					stopEarly = true
				}
			} else {
				result.StagesOK++
				status[item.st.StageKey] = batch.StageSucceeded
			}
			if err := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
				StageRowID: item.st.StageRowID, JobID: item.jobID, Status: stageStatus, Error: stageErr,
			}); err != nil {
				return err
			}
		}

		pendingLeft := false
		for _, st := range stages {
			if status[st.StageKey] == batch.StagePending {
				pendingLeft = true
				break
			}
		}
		if !pendingLeft {
			break
		}
	}
	return nil
}

func skipStage(ctx workflow.Context, st activities.ResolvedStage, reason string, result *BatchWorkflowResult) error {
	if err := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
		StageRowID: st.StageRowID, Status: "failed", Error: reason,
	}); err != nil {
		return fmt.Errorf("persist skipped stage %s: %w", st.StageKey, err)
	}
	result.StagesFailed++
	return nil
}

// runOneStage starts and waits for a single stage child. Returns (succeeded, shouldStop).
func runOneStage(ctx workflow.Context, in BatchWorkflowInput, st activities.ResolvedStage, result *BatchWorkflowResult) (ok bool, shouldStop bool, persistErr error) {
	log := workflow.GetLogger(ctx)

	var started activities.StartBatchStageJobOutput
	err := workflow.ExecuteActivity(ctx, "StartBatchStageJob", activities.StartBatchStageJobInput{
		OrgID: in.OrgID, BatchID: in.BatchID, StageRowID: st.StageRowID,
		ScheduleID: in.ScheduleID, RuleTemplateID: st.RuleTemplateID,
		RuleTemplateVersion: st.RuleTemplateVersion,
		Bucket:              st.Bucket, Key: st.Key, Filename: st.Filename, Size: st.Size,
		StageKey: st.StageKey,
	}).Get(ctx, &started)
	if err != nil {
		if persistErr := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
			StageRowID: st.StageRowID, Status: "failed", Error: err.Error(),
		}); persistErr != nil {
			return false, false, persistErr
		}
		result.StagesFailed++
		return false, st.OnFailure != "continue", nil
	}

	cwo := workflow.ChildWorkflowOptions{
		WorkflowID:               started.WorkflowID,
		TaskQueue:                "migration",
		WorkflowExecutionTimeout: 24 * time.Hour,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		RetryPolicy:              &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	childCtx := workflow.WithChildOptions(ctx, cwo)

	child := workflow.ExecuteChildWorkflow(childCtx, MigrationWorkflow, MigrationWorkflowInput{
		OrgID:               in.OrgID,
		JobID:               started.JobID,
		RuleTemplateID:      st.RuleTemplateID,
		RuleTemplateVersion: st.RuleTemplateVersion,
		ScheduleID:          in.ScheduleID,
		ScheduledTime:       in.ScheduledTime,
		SourceRef: map[string]any{
			"type":     "minio",
			"bucket":   st.Bucket,
			"key":      st.Key,
			"filename": st.Filename,
			"size":     st.Size,
		},
	})

	var exec workflow.Execution
	if err := child.GetChildWorkflowExecution().Get(ctx, &exec); err != nil {
		var already *temporal.ChildWorkflowExecutionAlreadyStartedError
		if temporal.IsWorkflowExecutionAlreadyStartedError(err) || errors.As(err, &already) ||
			strings.Contains(err.Error(), "already started") || strings.Contains(err.Error(), "AlreadyStarted") {
			log.Info("stage child already started", "stage", st.StageKey)
		} else {
			if persistErr := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
				StageRowID: st.StageRowID, JobID: started.JobID, Status: "failed", Error: err.Error(),
			}); persistErr != nil {
				return false, false, persistErr
			}
			result.StagesFailed++
			return false, st.OnFailure != "continue", nil
		}
	} else {
		if err := workflow.ExecuteActivity(ctx, "MarkJobRunning", activities.MarkJobRunningInput{
			JobID: started.JobID, RunID: exec.RunID,
		}).Get(ctx, nil); err != nil {
			return false, false, fmt.Errorf("mark stage job running: %w", err)
		}
	}

	var childResult MigrationWorkflowResult
	childErr := child.Get(ctx, &childResult)
	stageStatus := "succeeded"
	stageErr := ""
	if childErr != nil {
		stageStatus = "failed"
		stageErr = childErr.Error()
		result.StagesFailed++
		if persistErr := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
			StageRowID: st.StageRowID, JobID: started.JobID, Status: stageStatus, Error: stageErr,
		}); persistErr != nil {
			return false, false, persistErr
		}
		return false, st.OnFailure != "continue", nil
	}
	if childResult.Failed > 0 || childResult.Cancelled {
		stageStatus = "failed"
		stageErr = fmt.Sprintf("job finished with failed=%d cancelled=%v", childResult.Failed, childResult.Cancelled)
		result.StagesFailed++
		if persistErr := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
			StageRowID: st.StageRowID, JobID: started.JobID, Status: stageStatus, Error: stageErr,
		}); persistErr != nil {
			return false, false, persistErr
		}
		return false, st.OnFailure != "continue", nil
	}
	result.StagesOK++
	if persistErr := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
		StageRowID: st.StageRowID, JobID: started.JobID, Status: stageStatus, Error: stageErr,
	}); persistErr != nil {
		return false, false, persistErr
	}
	return true, false, nil
}

func finalizeBatchStage(ctx workflow.Context, in activities.FinalizeBatchStageInput) error {
	if err := workflow.ExecuteActivity(ctx, "FinalizeBatchStage", in).Get(ctx, nil); err != nil {
		return fmt.Errorf("finalize batch stage %d: %w", in.StageRowID, err)
	}
	return nil
}
