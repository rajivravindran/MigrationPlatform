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
	"strings"

	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"

	"github.com/migration-platform/orchestrator/internal/plan"
)

// Plan runs: a file bound to a step of a migration plan. A run waits until
// every step it depends on has a run that met that step's completion rule,
// then starts an ordinary migration job (see workflows.PlanRunWorkflow).

// PlanRunWorkflowID is the Temporal id of the workflow that gates and runs a plan run.
func PlanRunWorkflowID(planRunID int64) string { return fmt.Sprintf("plan-run-%d", planRunID) }

// planRunJobWorkflowID is the id of the migration job a plan run starts.
func planRunJobWorkflowID(planRunID int64) string { return fmt.Sprintf("plan-run-job-%d", planRunID) }

// ---------------- CheckPlanRunGate ----------------

type CheckPlanRunGateInput struct {
	PlanRunID int64 `json:"planRunId"`
}

type CheckPlanRunGateOutput struct {
	// Stop: the run is no longer waiting (cancelled or already started).
	Stop    bool   `json:"stop,omitempty"`
	Ready   bool   `json:"ready,omitempty"`
	Blocked bool   `json:"blocked,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// CheckPlanRunGate evaluates a waiting run's prerequisites and records why it
// is still waiting (or blocked) on the run, so the plan view can say so.
func (a *Activities) CheckPlanRunGate(ctx context.Context, in CheckPlanRunGateInput) (CheckPlanRunGateOutput, error) {
	var (
		orgID   int64
		planKey string
		status  string
		deps    []string
	)
	err := a.d.DB.QueryRow(ctx, `
		SELECT r.org_id, p.plan_key, r.status, COALESCE(s.depends_on, '{}')
		FROM plan_runs r
		JOIN plans p ON p.id = r.plan_id
		LEFT JOIN plan_steps s ON s.plan_id = r.plan_id AND s.step_key = r.step_key
		WHERE r.id = $1`, in.PlanRunID,
	).Scan(&orgID, &planKey, &status, &deps)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckPlanRunGateOutput{Stop: true, Reason: "plan run no longer exists"}, nil
	}
	if err != nil {
		return CheckPlanRunGateOutput{}, err
	}
	if status != "waiting" {
		return CheckPlanRunGateOutput{Stop: true, Reason: "plan run is " + status}, nil
	}

	prereqs := make([]plan.Prerequisite, 0, len(deps))
	for _, dep := range deps {
		p := plan.Prerequisite{Step: dep}
		var latestStatus, latestReason *string
		err := a.d.DB.QueryRow(ctx, `
			SELECT
			  EXISTS (SELECT 1 FROM plan_runs r JOIN plans p ON p.id = r.plan_id
			          WHERE r.org_id = $1 AND p.plan_key = $2 AND r.step_key = $3 AND r.status = 'completed'),
			  EXISTS (SELECT 1 FROM plan_runs r JOIN plans p ON p.id = r.plan_id
			          WHERE r.org_id = $1 AND p.plan_key = $2 AND r.step_key = $3 AND r.status IN ('waiting', 'running')),
			  (SELECT r.status FROM plan_runs r JOIN plans p ON p.id = r.plan_id
			   WHERE r.org_id = $1 AND p.plan_key = $2 AND r.step_key = $3 AND r.status IN ('completed', 'failed')
			   ORDER BY r.finished_at DESC NULLS LAST, r.id DESC LIMIT 1),
			  (SELECT r.reason FROM plan_runs r JOIN plans p ON p.id = r.plan_id
			   WHERE r.org_id = $1 AND p.plan_key = $2 AND r.step_key = $3 AND r.status IN ('completed', 'failed')
			   ORDER BY r.finished_at DESC NULLS LAST, r.id DESC LIMIT 1)`,
			orgID, planKey, dep,
		).Scan(&p.Completed, &p.Running, &latestStatus, &latestReason)
		if err != nil {
			return CheckPlanRunGateOutput{}, fmt.Errorf("prerequisite %s: %w", dep, err)
		}
		if latestStatus != nil && *latestStatus == "failed" {
			p.FailedReason = "its last run did not meet its completion rule"
			if latestReason != nil && *latestReason != "" {
				p.FailedReason = *latestReason
			}
		}
		prereqs = append(prereqs, p)
	}
	d := plan.Gate(prereqs)
	if !d.Ready {
		if _, err := a.d.DB.Exec(ctx, `
			UPDATE plan_runs SET blocked = $2, reason = $3, updated_at = now()
			WHERE id = $1 AND status = 'waiting' AND (blocked IS DISTINCT FROM $2 OR reason IS DISTINCT FROM $3)`,
			in.PlanRunID, d.Blocked, d.Reason,
		); err != nil {
			return CheckPlanRunGateOutput{}, err
		}
	}
	return CheckPlanRunGateOutput{Ready: d.Ready, Blocked: d.Blocked, Reason: d.Reason}, nil
}

// ---------------- StartPlanRunJob ----------------

type StartPlanRunJobInput struct {
	PlanRunID int64 `json:"planRunId"`
}

type StartPlanRunJobOutput struct {
	JobID               int64          `json:"jobId"`
	WorkflowID          string         `json:"workflowId"`
	OrgID               int64          `json:"orgId"`
	ScheduleID          int64          `json:"scheduleId,omitempty"`
	RuleTemplateID      int64          `json:"ruleTemplateId"`
	RuleTemplateVersion int32          `json:"ruleTemplateVersion"`
	SourceRef           map[string]any `json:"sourceRef"`
	// Stop: the run left 'waiting' before it could start (e.g. cancelled).
	Stop bool `json:"stop,omitempty"`
}

// StartPlanRunJob creates the migration job for a ready run and marks the run
// running. Idempotent: a retry finds the job it already created.
func (a *Activities) StartPlanRunJob(ctx context.Context, in StartPlanRunJobInput) (StartPlanRunJobOutput, error) {
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return StartPlanRunJobOutput{}, err
	}
	defer tx.Rollback(ctx)

	var (
		out        StartPlanRunJobOutput
		status     string
		scheduleID *int64
		templateID *int64
		version    *int32
		srcJSON    []byte
		jobID      *int64
	)
	err = tx.QueryRow(ctx, `
		SELECT org_id, status, schedule_id, rule_template_id, rule_template_version, source_ref, job_id
		FROM plan_runs WHERE id = $1 FOR UPDATE`, in.PlanRunID,
	).Scan(&out.OrgID, &status, &scheduleID, &templateID, &version, &srcJSON, &jobID)
	if err != nil {
		return out, fmt.Errorf("load plan run %d: %w", in.PlanRunID, err)
	}
	out.WorkflowID = planRunJobWorkflowID(in.PlanRunID)
	if templateID == nil || version == nil || len(srcJSON) == 0 {
		return out, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("plan run %d has no template or source", in.PlanRunID), "PlanRunIncomplete", nil)
	}
	out.RuleTemplateID, out.RuleTemplateVersion = *templateID, *version
	if scheduleID != nil {
		out.ScheduleID = *scheduleID
	}
	if err := json.Unmarshal(srcJSON, &out.SourceRef); err != nil {
		return out, fmt.Errorf("plan run %d source_ref: %w", in.PlanRunID, err)
	}
	if status == "running" && jobID != nil {
		out.JobID = *jobID
		return out, tx.Commit(ctx)
	}
	if status != "waiting" {
		out.Stop = true
		return out, nil
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO jobs (
			org_id, rule_template_id, rule_template_version, schedule_id,
			source_ref, status, temporal_workflow_id, started_at
		) VALUES ($1, $2, $3, $4, $5, 'pending'::job_status, $6, now())
		ON CONFLICT (temporal_workflow_id) DO UPDATE SET updated_at = jobs.updated_at
		RETURNING id`,
		out.OrgID, out.RuleTemplateID, out.RuleTemplateVersion, scheduleID, srcJSON, out.WorkflowID,
	).Scan(&out.JobID)
	if err != nil {
		return out, fmt.Errorf("insert plan run job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE plan_runs SET status = 'running', job_id = $2, blocked = false, reason = NULL,
			started_at = now(), updated_at = now()
		WHERE id = $1`, in.PlanRunID, out.JobID,
	); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// ---------------- FinalizePlanRun ----------------

type FinalizePlanRunInput struct {
	PlanRunID int64  `json:"planRunId"`
	JobID     int64  `json:"jobId"`
	ExecError string `json:"execError,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

type FinalizePlanRunOutput struct {
	Status    string  `json:"status"`
	RuleMet   bool    `json:"ruleMet"`
	FailedPct float64 `json:"failedPct"`
	Reason    string  `json:"reason"`
}

// FinalizePlanRun applies the step's completion rule to the job's persisted
// outcome. The job row (status, totals) is the authority, as for batches.
func (a *Activities) FinalizePlanRun(ctx context.Context, in FinalizePlanRunInput) (FinalizePlanRunOutput, error) {
	var (
		ruleKind   string
		maxPct     *float64
		jobStatus  *string
		totalsJSON []byte
	)
	err := a.d.DB.QueryRow(ctx, `
		SELECT COALESCE(s.completion_rule, 'strict'), s.max_failed_pct::float8, j.status::text, j.totals_json
		FROM plan_runs r
		LEFT JOIN plan_steps s ON s.plan_id = r.plan_id AND s.step_key = r.step_key
		LEFT JOIN jobs j ON j.id = $2
		WHERE r.id = $1`, in.PlanRunID, in.JobID,
	).Scan(&ruleKind, &maxPct, &jobStatus, &totalsJSON)
	if err != nil {
		return FinalizePlanRunOutput{}, fmt.Errorf("load plan run %d: %w", in.PlanRunID, err)
	}
	rule := plan.Rule{Kind: ruleKind}
	if maxPct != nil {
		rule.MaxFailedPct = *maxPct
	}
	job := plan.JobOutcome{ExecError: in.ExecError}
	if jobStatus != nil {
		job.Status = *jobStatus
	}
	if in.Cancelled {
		job.Status = "cancelled"
	}
	var totals struct {
		Total      int64 `json:"total"`
		Processed  int64 `json:"processed"`
		Failed     int64 `json:"failed"`
		Unresolved int64 `json:"unresolved"`
	}
	if len(totalsJSON) > 0 {
		_ = json.Unmarshal(totalsJSON, &totals)
	}
	job.Total, job.Processed, job.Failed, job.Unresolved = totals.Total, totals.Processed, totals.Failed, totals.Unresolved

	v := plan.Evaluate(rule, job)
	status := "failed"
	switch {
	case v.Met:
		status = "completed"
	case in.Cancelled:
		status = "cancelled"
	}
	if _, err := a.d.DB.Exec(ctx, `
		UPDATE plan_runs SET status = $2, rule_met = $3, failed_pct = $4, reason = $5,
			job_id = COALESCE(job_id, $6), blocked = false, finished_at = now(), updated_at = now()
		WHERE id = $1`,
		in.PlanRunID, status, v.Met, v.FailedPct, truncateErr(v.Reason, 2000), nullIfZero(in.JobID),
	); err != nil {
		return FinalizePlanRunOutput{}, err
	}
	return FinalizePlanRunOutput{Status: status, RuleMet: v.Met, FailedPct: v.FailedPct, Reason: v.Reason}, nil
}

// ---------------- CancelPlanRun ----------------

type CancelPlanRunInput struct {
	PlanRunID int64  `json:"planRunId"`
	Reason    string `json:"reason,omitempty"`
}

// CancelPlanRun records that a waiting run was cancelled before it started.
func (a *Activities) CancelPlanRun(ctx context.Context, in CancelPlanRunInput) error {
	reason := in.Reason
	if reason == "" {
		reason = "cancelled before it started"
	}
	_, err := a.d.DB.Exec(ctx, `
		UPDATE plan_runs SET status = 'cancelled', blocked = false, reason = $2, finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'waiting'`, in.PlanRunID, reason)
	return err
}

// ---------------- plan archives ----------------

// StepOverride is the mapping chosen for one archive file before the run
// started (e.g. by the person uploading it). Empty fields fall back.
type StepOverride struct {
	StepKey     string `json:"stepKey,omitempty"`
	TemplateKey string `json:"templateKey,omitempty"`
}

type StagePlanArchiveInput struct {
	OrgID    int64  `json:"orgId"`
	IntakeID int64  `json:"intakeId"`
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
	Filename string `json:"filename,omitempty"`
}

type StagePlanArchiveOutput struct {
	Files []StagedFile `json:"files"`
}

func planIntakePrefix(intakeID int64) string {
	return path.Join("plan-intakes", fmt.Sprintf("%d", intakeID), "files")
}

// StagePlanArchive unpacks an archive that arrived for a plan and stages each
// manifest file under plan-intakes/<intake>/files/<stage id>/.
func (a *Activities) StagePlanArchive(ctx context.Context, in StagePlanArchiveInput) (StagePlanArchiveOutput, error) {
	arc, err := a.openArchive(ctx, in.OrgID, in.Bucket, in.Key, in.Filename, fmt.Sprintf("plan-intake-%d", in.IntakeID))
	if err != nil {
		return StagePlanArchiveOutput{}, err
	}
	defer os.RemoveAll(arc.tmpRoot)
	out := StagePlanArchiveOutput{Files: make([]StagedFile, 0, len(arc.manifest.Stages))}
	for _, st := range arc.manifest.Stages {
		staged, err := arc.stageFile(ctx, in.Bucket, planIntakePrefix(in.IntakeID), st)
		if err != nil {
			return out, err
		}
		out.Files = append(out.Files, staged)
	}
	if _, err := a.d.DB.Exec(ctx, `UPDATE plan_intakes SET manifest_json = $2, updated_at = now() WHERE id = $1`,
		in.IntakeID, arc.raw); err != nil {
		return out, fmt.Errorf("persist manifest: %w", err)
	}
	return out, nil
}

type CreatePlanIntakeRunsInput struct {
	OrgID     int64                   `json:"orgId"`
	IntakeID  int64                   `json:"intakeId"`
	Files     []StagedFile            `json:"files"`
	Overrides map[string]StepOverride `json:"overrides,omitempty"` // by manifest stage id
}

type PlanRunRef struct {
	PlanRunID  int64  `json:"planRunId"`
	WorkflowID string `json:"workflowId"`
}

type CreatePlanIntakeRunsOutput struct {
	Runs      []PlanRunRef `json:"runs"`
	Unmatched int          `json:"unmatched"`
}

// CreatePlanIntakeRuns binds each staged archive file to a plan step and
// template and records a waiting run for it. The step comes from an override,
// else a step whose key equals the manifest stage id, else the step file
// patterns; the template from an override, else the manifest, else the step
// default. Files that cannot be bound are recorded as failed runs saying why.
// Idempotent per intake: a retry returns the runs already created.
func (a *Activities) CreatePlanIntakeRuns(ctx context.Context, in CreatePlanIntakeRunsInput) (CreatePlanIntakeRunsOutput, error) {
	out := CreatePlanIntakeRunsOutput{}
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)

	var planID int64
	var scheduleID *int64
	var createdBy *int64
	if err := tx.QueryRow(ctx, `SELECT plan_id, schedule_id, created_by FROM plan_intakes WHERE id = $1 AND org_id = $2 FOR UPDATE`,
		in.IntakeID, in.OrgID).Scan(&planID, &scheduleID, &createdBy); err != nil {
		return out, fmt.Errorf("load intake %d: %w", in.IntakeID, err)
	}

	existing, err := tx.Query(ctx, `SELECT id, status FROM plan_runs WHERE intake_id = $1 ORDER BY id`, in.IntakeID)
	if err != nil {
		return out, err
	}
	for existing.Next() {
		var id int64
		var status string
		if err := existing.Scan(&id, &status); err != nil {
			existing.Close()
			return out, err
		}
		if status == "waiting" {
			out.Runs = append(out.Runs, PlanRunRef{PlanRunID: id, WorkflowID: PlanRunWorkflowID(id)})
		}
	}
	existing.Close()
	if len(out.Runs) > 0 {
		return out, tx.Commit(ctx)
	}

	steps, err := loadPlanSteps(ctx, tx, planID)
	if err != nil {
		return out, err
	}
	for _, f := range in.Files {
		if !strings.HasPrefix(f.Key, planIntakePrefix(in.IntakeID)+"/") {
			return out, notAuthorized("staged file %s is not part of plan intake %d", f.Key, in.IntakeID)
		}
		ov := in.Overrides[f.StageKey]
		stepKey, step, ok := bindStep(steps, ov.StepKey, f.StageKey, f.Filename)
		bind := planRunBinding{OrgID: in.OrgID, PlanID: planID, IntakeID: in.IntakeID, ScheduleID: scheduleID, CreatedBy: createdBy, Filename: f.Filename}
		if !ok {
			bind.Failure = fmt.Sprintf("no plan step matches %s (manifest stage %q)", f.File, f.StageKey)
			if ov.StepKey != "" {
				bind.Failure = fmt.Sprintf("plan has no step %q", ov.StepKey)
			}
			out.Unmatched++
		} else {
			bind.StepKey = stepKey
			tplKey := firstNonEmpty(ov.TemplateKey, f.TemplateKey, step.defaultTemplate)
			if err := bind.resolveTemplate(ctx, tx, tplKey, f.Filename); err != nil {
				return out, err
			}
		}
		if bind.Failure == "" {
			sourceID, err := registerSource(ctx, tx, newSource{
				OrgID: in.OrgID, Bucket: f.Bucket, Key: f.Key, Filename: f.Filename,
				ETag: f.ETag, Size: f.Size, Provenance: "plan_intake",
			})
			if err != nil {
				return out, err
			}
			bind.SourceID = sourceID
			bind.SourceRef = map[string]any{
				"type": "minio", "source_id": sourceID, "bucket": f.Bucket, "key": f.Key,
				"filename": f.Filename, "size": f.Size, "etag": f.ETag,
			}
		}
		id, err := bind.insert(ctx, tx)
		if err != nil {
			return out, err
		}
		if bind.Failure == "" {
			out.Runs = append(out.Runs, PlanRunRef{PlanRunID: id, WorkflowID: PlanRunWorkflowID(id)})
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE plan_intakes SET status = 'dispatched', updated_at = now() WHERE id = $1`, in.IntakeID); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

type FailPlanIntakeInput struct {
	IntakeID int64  `json:"intakeId"`
	Error    string `json:"error"`
}

func (a *Activities) FailPlanIntake(ctx context.Context, in FailPlanIntakeInput) error {
	_, err := a.d.DB.Exec(ctx, `UPDATE plan_intakes SET status = 'failed', error_message = $2, updated_at = now() WHERE id = $1`,
		in.IntakeID, truncateErr(in.Error, 2000))
	return err
}

// ---------------- watched folders ----------------

type CreatePlanIntakeInput struct {
	OrgID      int64  `json:"orgId"`
	PlanID     int64  `json:"planId"`
	ScheduleID int64  `json:"scheduleId"`
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
}

type CreatePlanIntakeOutput struct {
	IntakeID   int64  `json:"intakeId"`
	WorkflowID string `json:"workflowId"`
	Filename   string `json:"filename"`
}

// PlanArchiveWorkflowID is deterministic per watched object version so a
// re-listed archive is not unpacked twice.
func PlanArchiveWorkflowID(scheduleID int64, key, etag string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", scheduleID, key, etag)))
	return "plan-intake-" + hex.EncodeToString(sum[:12])
}

// CreatePlanIntake records an archive found in a watched folder routed to a plan.
func (a *Activities) CreatePlanIntake(ctx context.Context, in CreatePlanIntakeInput) (CreatePlanIntakeOutput, error) {
	out := CreatePlanIntakeOutput{WorkflowID: PlanArchiveWorkflowID(in.ScheduleID, in.Key, in.ETag), Filename: path.Base(in.Key)}
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	sourceID, err := registerSource(ctx, tx, newSource{
		OrgID: in.OrgID, Bucket: in.Bucket, Key: in.Key, Filename: out.Filename,
		ETag: in.ETag, Size: in.Size, Provenance: "watched",
	})
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO plan_intakes (org_id, plan_id, source_id, filename, schedule_id, temporal_workflow_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (temporal_workflow_id) DO UPDATE SET updated_at = plan_intakes.updated_at
		RETURNING id`,
		in.OrgID, in.PlanID, sourceID, out.Filename, nullIfZero(in.ScheduleID), out.WorkflowID,
	).Scan(&out.IntakeID)
	if err != nil {
		return out, fmt.Errorf("insert plan intake: %w", err)
	}
	return out, tx.Commit(ctx)
}

type RoutePlanFileInput struct {
	OrgID      int64  `json:"orgId"`
	PlanID     int64  `json:"planId"`
	ScheduleID int64  `json:"scheduleId"`
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	Filename   string `json:"filename"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
	// FallbackTemplateID/Version: the schedule's template, used when the
	// matched step has no default template.
	FallbackTemplateID      int64 `json:"fallbackTemplateId"`
	FallbackTemplateVersion int32 `json:"fallbackTemplateVersion"`
}

type RoutePlanFileOutput struct {
	Matched    bool   `json:"matched"`
	PlanRunID  int64  `json:"planRunId,omitempty"`
	WorkflowID string `json:"workflowId,omitempty"`
	// Failed: matched a step but could not be bound (e.g. format mismatch);
	// a failed run records why.
	Failed bool `json:"failed,omitempty"`
}

// RoutePlanFile matches a watched file to a plan step by file pattern and
// records a waiting run for it. Unmatched files are left to the schedule's
// ordinary single-template path. Idempotent per object version.
func (a *Activities) RoutePlanFile(ctx context.Context, in RoutePlanFileInput) (RoutePlanFileOutput, error) {
	tx, err := a.d.DB.Begin(ctx)
	if err != nil {
		return RoutePlanFileOutput{}, err
	}
	defer tx.Rollback(ctx)
	steps, err := loadPlanSteps(ctx, tx, in.PlanID)
	if err != nil {
		return RoutePlanFileOutput{}, err
	}
	stepKey, step, ok := bindStep(steps, "", "", in.Filename)
	if !ok {
		return RoutePlanFileOutput{}, nil
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", in.ScheduleID, in.Key, in.ETag)))
	dedupe := "watch-" + hex.EncodeToString(sum[:12])
	var existingID int64
	err = tx.QueryRow(ctx, `SELECT id FROM plan_runs WHERE dedupe_key = $1`, dedupe).Scan(&existingID)
	if err == nil {
		return RoutePlanFileOutput{Matched: true, PlanRunID: existingID, WorkflowID: PlanRunWorkflowID(existingID)}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RoutePlanFileOutput{}, err
	}

	sid := in.ScheduleID
	bind := planRunBinding{OrgID: in.OrgID, PlanID: in.PlanID, ScheduleID: &sid, StepKey: stepKey, Filename: in.Filename, DedupeKey: dedupe}
	if step.defaultTemplate != "" {
		if err := bind.resolveTemplate(ctx, tx, step.defaultTemplate, in.Filename); err != nil {
			return RoutePlanFileOutput{}, err
		}
	} else {
		bind.TemplateID, bind.TemplateVersion = in.FallbackTemplateID, in.FallbackTemplateVersion
		if err := bind.checkFormat(ctx, tx, in.Filename); err != nil {
			return RoutePlanFileOutput{}, err
		}
	}
	if bind.Failure == "" {
		sourceID, err := registerSource(ctx, tx, newSource{
			OrgID: in.OrgID, Bucket: in.Bucket, Key: in.Key, Filename: in.Filename,
			ETag: in.ETag, Size: in.Size, Provenance: "watched",
		})
		if err != nil {
			return RoutePlanFileOutput{}, err
		}
		bind.SourceID = sourceID
		bind.SourceRef = map[string]any{
			"type": "minio", "source_id": sourceID, "bucket": in.Bucket, "key": in.Key,
			"filename": in.Filename, "size": in.Size, "etag": in.ETag,
		}
	}
	id, err := bind.insert(ctx, tx)
	if err != nil {
		return RoutePlanFileOutput{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RoutePlanFileOutput{}, err
	}
	if bind.Failure != "" {
		return RoutePlanFileOutput{Matched: true, PlanRunID: id, Failed: true}, nil
	}
	return RoutePlanFileOutput{Matched: true, PlanRunID: id, WorkflowID: PlanRunWorkflowID(id)}, nil
}

// ---------------- shared binding helpers ----------------

type planStepRow struct {
	key             string
	ordinal         int
	filePattern     string
	defaultTemplate string
}

func loadPlanSteps(ctx context.Context, tx pgx.Tx, planID int64) ([]planStepRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT step_key, ordinal, COALESCE(file_pattern, ''), COALESCE(default_template_key, '')
		FROM plan_steps WHERE plan_id = $1 ORDER BY ordinal, id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []planStepRow
	for rows.Next() {
		var s planStepRow
		if err := rows.Scan(&s.key, &s.ordinal, &s.filePattern, &s.defaultTemplate); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// bindStep picks the step for a file: explicit choice, then a step whose key
// equals the manifest stage id, then the file patterns.
func bindStep(steps []planStepRow, explicit, stageID, filename string) (string, planStepRow, bool) {
	byKey := map[string]planStepRow{}
	routing := make([]plan.Step, 0, len(steps))
	for _, s := range steps {
		byKey[s.key] = s
		routing = append(routing, plan.Step{Key: s.key, Ordinal: s.ordinal, FilePattern: s.filePattern})
	}
	if explicit != "" {
		s, ok := byKey[explicit]
		return explicit, s, ok
	}
	if s, ok := byKey[stageID]; ok && stageID != "" {
		return stageID, s, true
	}
	if key, ok := plan.MatchStep(routing, filename); ok {
		return key, byKey[key], true
	}
	return "", planStepRow{}, false
}

type planRunBinding struct {
	OrgID           int64
	PlanID          int64
	IntakeID        int64
	ScheduleID      *int64
	CreatedBy       *int64
	StepKey         string
	Filename        string
	TemplateID      int64
	TemplateVersion int32
	SourceID        int64
	SourceRef       map[string]any
	DedupeKey       string
	// Failure, when set, records the run as failed with this reason.
	Failure string
}

// resolveTemplate binds the latest published version of templateKey and
// checks the file's format against the template's source type.
func (b *planRunBinding) resolveTemplate(ctx context.Context, tx pgx.Tx, templateKey, filename string) error {
	if templateKey == "" {
		b.Failure = fmt.Sprintf("no template for %s: the manifest names none and step %q has no default", filename, b.StepKey)
		return nil
	}
	err := tx.QueryRow(ctx, `
		SELECT id, version FROM rule_templates
		WHERE org_id = $1 AND template_key = $2 AND published = true
		ORDER BY version DESC LIMIT 1`, b.OrgID, templateKey,
	).Scan(&b.TemplateID, &b.TemplateVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		b.Failure = fmt.Sprintf("template %q has no published version", templateKey)
		return nil
	}
	if err != nil {
		return err
	}
	return b.checkFormat(ctx, tx, filename)
}

// checkFormat fails the binding when the file's format is not the one the
// template reads, instead of letting the job fail mid-parse.
func (b *planRunBinding) checkFormat(ctx context.Context, tx pgx.Tx, filename string) error {
	var sourceType string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(schema_json->'source'->>'type', '') FROM rule_templates WHERE id = $1`,
		b.TemplateID).Scan(&sourceType); err != nil {
		return err
	}
	if msg := FormatMismatch(sourceType, filename); msg != "" {
		b.Failure = msg
	}
	return nil
}

// FormatMismatch explains why filename cannot feed a template whose source
// type is templateType, or returns "" when it can.
func FormatMismatch(templateType, filename string) string {
	switch templateType {
	case "csv", "json", "xml":
	default:
		return "" // connector-backed or unspecified templates are not file-typed
	}
	got := sourceFormat(filename)
	if got == "archive" {
		return fmt.Sprintf("%s is an archive; a single step needs a %s file", path.Base(filename), strings.ToUpper(templateType))
	}
	if got != templateType {
		return fmt.Sprintf("%s is %s but the template reads %s", path.Base(filename), strings.ToUpper(got), strings.ToUpper(templateType))
	}
	return ""
}

func (b *planRunBinding) insert(ctx context.Context, tx pgx.Tx) (int64, error) {
	status, reason := "waiting", any(nil)
	if b.Failure != "" {
		status, reason = "failed", truncateErr(b.Failure, 2000)
	}
	var srcJSON []byte
	if b.SourceRef != nil {
		var err error
		if srcJSON, err = json.Marshal(b.SourceRef); err != nil {
			return 0, err
		}
	}
	var stepKey any
	if b.StepKey != "" {
		stepKey = b.StepKey
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO plan_runs (org_id, plan_id, step_key, intake_id, source_id, source_ref, filename,
			rule_template_id, rule_template_version, status, reason, schedule_id, created_by, dedupe_key,
			finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			CASE WHEN $10 = 'failed' THEN now() END)
		RETURNING id`,
		b.OrgID, b.PlanID, stepKey, nullIfZero(b.IntakeID), nullIfZero(b.SourceID), srcJSON, b.Filename,
		nullIfZero(b.TemplateID), nullIfZero(int64(b.TemplateVersion)), status, reason, b.ScheduleID, b.CreatedBy,
		nullIfEmpty(b.DedupeKey),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert plan run: %w", err)
	}
	if status == "waiting" {
		if _, err := tx.Exec(ctx, `UPDATE plan_runs SET temporal_workflow_id = $2 WHERE id = $1`, id, PlanRunWorkflowID(id)); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
