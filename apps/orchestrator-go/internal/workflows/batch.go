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
	"github.com/migration-platform/orchestrator/internal/lifecycle"
)

// BatchWorkflowInput is started for each watched (or manually submitted) archive package.
type BatchWorkflowInput struct {
	OrgID       int64  `json:"orgId"`
	BatchID     int64  `json:"batchId"`
	ScheduleID  int64  `json:"scheduleId,omitempty"`
	ConnectorID int64  `json:"connectorId,omitempty"`
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	// Filename is the archive's original name. Uploaded sources are stored
	// under content-hash keys without an extension, so the format comes from
	// here when set; watched and scheduled archives fall back to Key.
	Filename      string    `json:"filename,omitempty"`
	ETag          string    `json:"etag,omitempty"`
	Size          int64     `json:"size,omitempty"`
	ScheduledTime time.Time `json:"scheduledTime,omitempty"`
}

// archiveName is the name that carries the archive format.
func (in BatchWorkflowInput) archiveName() string {
	if in.Filename != "" {
		return in.Filename
	}
	return in.Key
}

// BatchWorkflowResult summarises a batch run. Status/Reason are the verdict
// FinalizeBatch persisted (lifecycle.BatchVerdict); StagesOK/StagesFailed
// are this run's counters and are informational.
type BatchWorkflowResult struct {
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	StagesTotal  int    `json:"stagesTotal"`
	StagesOK     int    `json:"stagesOk"`
	StagesFailed int    `json:"stagesFailed"`
	Quarantined  bool   `json:"quarantined,omitempty"`
}

// batchFinalizeOnExitVersion gates, via workflow.GetVersion, finalizing the
// batch on every exit from its persisted stage outcomes (lifecycle.FinalizeBatch)
// on a disconnected context. Histories recorded before it replay the old
// path, which computed the final status from in-workflow counters and left
// the batch running when a stage-persistence error ended the workflow.
const (
	batchFinalizeOnExitChange  = "batch-finalize-on-exit"
	batchFinalizeOnExitVersion = 1
)

// BatchWorkflow unpacks an archive with manifest.json, then runs child
// MigrationWorkflows per stage. Without dependsOn: ordered sequential (P2).
// With any dependsOn: topological DAG; ready stages run in parallel (P4a).
// onStageFailure stop|continue controls whether further stages start after a failure.
//
// Every exit except a quarantine (which persists its own status) goes
// through finishBatch, so the persisted batch status converges to the
// persisted stage outcomes however the workflow ended (review A2).
func BatchWorkflow(ctx workflow.Context, in BatchWorkflowInput) (*BatchWorkflowResult, error) {
	log := workflow.GetLogger(ctx)
	log.Info("BatchWorkflow start", "batchId", in.BatchID, "key", in.Key)

	ao := defaultActivityOptions(nil)
	ctx = workflow.WithActivityOptions(ctx, ao)
	finalizeOnExit := workflow.GetVersion(ctx, batchFinalizeOnExitChange, workflow.DefaultVersion,
		batchFinalizeOnExitVersion) >= batchFinalizeOnExitVersion

	result := &BatchWorkflowResult{Status: "failed"}
	runErr := runBatch(ctx, in, result)
	if result.Quarantined {
		return result, runErr
	}
	if !finalizeOnExit {
		if runErr != nil {
			return result, runErr
		}
		return result, legacyFinalizeBatch(ctx, in, result)
	}
	return result, finishBatch(ctx, in.BatchID, result, runErr)
}

// runBatch does the work of one batch run: license gate, admission, unpack,
// stage materialization and stage execution. It returns the error that
// stopped the batch; BatchWorkflow records the outcome either way.
func runBatch(ctx workflow.Context, in BatchWorkflowInput, result *BatchWorkflowResult) error {
	log := workflow.GetLogger(ctx)
	if err := requireLicensed(ctx); err != nil {
		return err
	}

	if !batch.IsArchive(in.archiveName()) {
		return fmt.Errorf("not an archive package: %s", in.archiveName())
	}

	if err := workflow.ExecuteActivity(ctx, "MarkBatchRunning", activities.MarkBatchRunningInput{
		BatchID: in.BatchID,
		RunID:   workflow.GetInfo(ctx).WorkflowExecution.RunID,
	}).Get(ctx, nil); err != nil {
		return fmt.Errorf("mark batch running: %w", err)
	}

	var unpacked activities.UnpackAndStageArchiveOutput
	err := workflow.ExecuteActivity(ctx, "UnpackAndStageArchive", activities.UnpackAndStageArchiveInput{
		OrgID: in.OrgID, BatchID: in.BatchID, Bucket: in.Bucket, Key: in.Key, Filename: in.Filename,
	}).Get(ctx, &unpacked)
	if err != nil {
		log.Error("unpack failed; quarantining", "err", err)
		if qErr := workflow.ExecuteActivity(ctx, "QuarantineArchive", activities.QuarantineArchiveInput{
			OrgID: in.OrgID, BatchID: in.BatchID, Bucket: in.Bucket, Key: in.Key, Reason: err.Error(),
		}).Get(ctx, nil); qErr != nil {
			return fmt.Errorf("unpack failed (%v); quarantine persistence failed: %w", err, qErr)
		}
		result.Status = "quarantined"
		result.Quarantined = true
		return nil
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
			return fmt.Errorf("materialize failed (%v); quarantine persistence failed: %w", err, qErr)
		}
		result.Status = "quarantined"
		result.Quarantined = true
		return nil
	}

	result.StagesTotal = len(resolved.Stages)
	nodes := stageNodes(resolved.Stages)
	if batch.UsesDependsOnNodes(nodes) {
		log.Info("BatchWorkflow DAG mode (dependsOn)")
		return runStagesDAG(ctx, in, resolved.Stages, result)
	}
	log.Info("BatchWorkflow sequential mode")
	return runStagesSequential(ctx, in, resolved.Stages, result)
}

// legacyFinalizeBatch is the pre-"batch-finalize-on-exit" finalization,
// replayed for old histories: the status is computed from this run's
// counters and written unconditionally.
func legacyFinalizeBatch(ctx workflow.Context, in BatchWorkflowInput, result *BatchWorkflowResult) error {
	final := counterStatus(result)
	result.Status = final
	if err := workflow.ExecuteActivity(ctx, "FinalizeBatch", activities.FinalizeBatchInput{
		BatchID: in.BatchID, Status: final,
	}).Get(ctx, nil); err != nil {
		return fmt.Errorf("finalize batch: %w", err)
	}
	workflow.GetLogger(ctx).Info("BatchWorkflow done", "status", final, "ok", result.StagesOK, "failed", result.StagesFailed)
	return nil
}

// counterStatus is the pre-verdict batch status computed from this run's
// stage counters: succeeded, partial or failed.
func counterStatus(result *BatchWorkflowResult) string {
	switch {
	case result.StagesFailed > 0 && result.StagesOK > 0:
		return "partial"
	case result.StagesFailed > 0:
		return "failed"
	default:
		return "succeeded"
	}
}

// finishBatch records the batch outcome once execution has ended, however it
// ended. The status is derived by the lifecycle module from the persisted
// stage outcomes plus the execution outcome (cancelled or an error outside
// a stage), never from this run's counters, so the batch, its stages and
// the UI agree after a crash or a persistence error. It runs on a
// disconnected context so a cancelled workflow still records its outcome.
// A FinalizeBatch failure is returned: the workflow fails and the
// reconciler repairs the batch. runErr is returned unchanged when finalizing
// succeeds.
func finishBatch(ctx workflow.Context, batchID int64, result *BatchWorkflowResult, runErr error) error {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	// Status carries the counter-derived value for workers that predate
	// DeriveFromStages; a current worker ignores it and derives the verdict.
	in := activities.FinalizeBatchInput{BatchID: batchID, Status: counterStatus(result), DeriveFromStages: true}
	if runErr != nil {
		in.Status = "failed"
	}
	switch {
	case runErr == nil:
	case temporal.IsCanceledError(runErr) || errors.Is(ctx.Err(), workflow.ErrCanceled):
		in.Cancelled = true
	default:
		in.Error = failureMessage(runErr)
	}
	fctx := workflow.WithActivityOptions(dctx, finishActivityOptions())
	var verdict *activities.FinalizeBatchOutput
	if err := workflow.ExecuteActivity(fctx, "FinalizeBatch", in).Get(fctx, &verdict); err != nil {
		workflow.GetLogger(ctx).Error("FinalizeBatch failed; the reconciler will repair the batch", "batchId", batchID, "err", err)
		if runErr != nil {
			return fmt.Errorf("%w (and finalize batch failed: %v)", runErr, err)
		}
		return fmt.Errorf("finalize batch: %w", err)
	}
	if verdict != nil && verdict.Status != "" {
		result.Status = verdict.Status
		result.Reason = verdict.Reason
	}
	workflow.GetLogger(ctx).Info("BatchWorkflow done", "status", result.Status, "reason", result.Reason,
		"ok", result.StagesOK, "failed", result.StagesFailed, "err", runErr)
	return runErr
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
				StageKey: st.StageKey, ETag: st.ETag,
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
					"type":      "minio",
					"source_id": started.SourceID,
					"bucket":    st.Bucket,
					"key":       st.Key,
					"filename":  st.Filename,
					"size":      st.Size,
					"etag":      st.ETag,
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
			verdict := stageVerdict(childResult, childErr)
			if verdict.ok {
				result.StagesOK++
				status[item.st.StageKey] = batch.StageSucceeded
			} else {
				result.StagesFailed++
				status[item.st.StageKey] = batch.StageFailed
				if item.st.OnFailure != "continue" {
					stopEarly = true
				}
			}
			if err := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
				StageRowID: item.st.StageRowID, JobID: item.jobID, Status: verdict.status, Error: verdict.reason,
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
		StageKey: st.StageKey, ETag: st.ETag,
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
			"type":      "minio",
			"source_id": started.SourceID,
			"bucket":    st.Bucket,
			"key":       st.Key,
			"filename":  st.Filename,
			"size":      st.Size,
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
	verdict := stageVerdict(childResult, childErr)
	if verdict.ok {
		result.StagesOK++
	} else {
		result.StagesFailed++
	}
	if persistErr := finalizeBatchStage(ctx, activities.FinalizeBatchStageInput{
		StageRowID: st.StageRowID, JobID: started.JobID, Status: verdict.status, Error: verdict.reason,
	}); persistErr != nil {
		return false, false, persistErr
	}
	if verdict.ok {
		return true, false, nil
	}
	return false, st.OnFailure != "continue", nil
}

// stageOutcome is what the batch records for a stage once its child job has
// ended.
type stageOutcome struct {
	ok     bool
	status string // persisted batch_stages status: succeeded | failed
	reason string // operator-facing explanation; empty on success
}

// stageVerdict decides a stage's outcome from its child MigrationWorkflow.
// The authority is the verdict the child persisted on its job
// (MigrationWorkflowResult.Status/Reason, written by FinalizeJob through
// lifecycle.Finalize). The batch never reconstructs success from row
// counters: an empty job, a cancelled job and a job whose execution error
// was recorded are all failed by the lifecycle module and must be failed
// here too, so dependent stages cannot run on them (ADR 0004, review A1).
//
// Two distinct failure kinds are kept apart in the reason: an execution
// error (childErr: the workflow failed, timed out or could not record its
// outcome) versus a completed execution whose business outcome is not
// success. A child that completed without reporting a verdict (legacy
// history, or a worker predating verdicts) has not established success and
// is failed with a reason saying so; the reconciler, not this parent, is
// the place that may repair such a job.
func stageVerdict(res MigrationWorkflowResult, childErr error) stageOutcome {
	switch {
	case childErr != nil:
		return stageOutcome{status: "failed", reason: "execution error: " + childErr.Error()}
	case res.Status == "":
		return stageOutcome{status: "failed", reason: fmt.Sprintf(
			"job completed without a persisted verdict (processed=%d failed=%d unresolved=%d cancelled=%v); success not established",
			res.Processed, res.Failed, res.Unresolved, res.Cancelled)}
	case res.Status == lifecycle.StatusSucceeded:
		return stageOutcome{ok: true, status: "succeeded"}
	default:
		reason := res.Reason
		if reason == "" {
			reason = fmt.Sprintf("job finished with failed=%d unresolved=%d cancelled=%v", res.Failed, res.Unresolved, res.Cancelled)
		}
		return stageOutcome{status: "failed", reason: fmt.Sprintf("job %s: %s", res.Status, reason)}
	}
}

func finalizeBatchStage(ctx workflow.Context, in activities.FinalizeBatchStageInput) error {
	if err := workflow.ExecuteActivity(ctx, "FinalizeBatchStage", in).Get(ctx, nil); err != nil {
		return fmt.Errorf("finalize batch stage %d: %w", in.StageRowID, err)
	}
	return nil
}
