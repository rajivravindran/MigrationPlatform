//go:build integration

package activities

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/migration-platform/orchestrator/internal/template"
	"github.com/migration-platform/orchestrator/internal/testdb"
)

func pgActivities(pool *pgxpool.Pool) *Activities {
	return NewActivities(Deps{DB: pool, SecretsMasterKey: "0123456789abcdef0123456789abcdef"})
}

// F1 on the real ledger: a resend generation is spent once; redelivery
// replays; a new generation sends again.
func TestPGLedgerConsumesDecisionGenerations(t *testing.T) {
	pool := testdb.New(t)
	org, tpl := testdb.Seed(t, pool)
	job := testdb.Job(t, pool, org, tpl, "running", "wf-ledger")
	ctx := context.Background()
	l := pgOperationLedger{db: pool}
	op := operationRef{JobID: job, RowIndex: 0, StepIndex: 0, StepName: "main"}
	intent := attemptIntent{PayloadSHA256: "p1", Mode: template.DeliveryManual, RetentionHours: 24}

	begin := func(attempt string, in attemptIntent) decisionKind {
		t.Helper()
		res, err := l.begin(ctx, beginInput{Op: op, AttemptID: attempt, OperationKey: "k", Intent: in})
		if err != nil {
			t.Fatal(err)
		}
		return res.Decision.Kind
	}
	finishOK := func(attempt string) {
		t.Helper()
		if err := l.finish(ctx, finishInput{Op: op, AttemptID: attempt, Outcome: outcomeResponded, State: opSucceeded, HTTPStatus: 201, Receipt: []byte(`{"id":1}`)}); err != nil {
			t.Fatal(err)
		}
	}

	if k := begin("a1", intent); k != decideSend {
		t.Fatalf("first attempt = %v", k)
	}
	finishOK("a1")
	if k := begin("a2", intent); k != decideReplay {
		t.Fatalf("plain retry = %v, want replay", k)
	}
	resend := intent
	resend.Resend, resend.Generation = true, "gen-1"
	if k := begin("a3", resend); k != decideSend {
		t.Fatalf("authorized resend = %v", k)
	}
	finishOK("a3")
	if k := begin("a4", resend); k != decideReplay {
		t.Fatalf("redelivered resend of the same generation = %v, want replay", k)
	}
	var gen string
	if err := pool.QueryRow(ctx, `SELECT decision_generation FROM operations WHERE job_id = $1 AND row_index = 0 AND step_index = 0`, job).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	if gen != "gen-1" {
		t.Fatalf("recorded generation %q", gen)
	}
	resend.Generation = "gen-2"
	if k := begin("a5", resend); k != decideSend {
		t.Fatalf("new generation = %v, want send", k)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operation_attempts WHERE job_id = $1`, job).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 5 {
		t.Fatalf("attempt rows %d, want 5 (two replayed, three sent)", attempts)
	}
}

// Plan binding: a redelivered MaterializeBatchStages keeps the version first
// bound even after a newer template version is published.
func TestPGMaterializeBatchStagesBindsOnce(t *testing.T) {
	pool := testdb.New(t)
	org, tpl := testdb.Seed(t, pool)
	ctx := context.Background()
	a := pgActivities(pool)
	var batch int64
	if err := pool.QueryRow(ctx, `INSERT INTO batches (org_id, source_ref, status, temporal_workflow_id) VALUES ($1, '{}', 'running', 'wf-m') RETURNING id`, org).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	in := MaterializeBatchStagesInput{OrgID: org, BatchID: batch, OnStageFailure: "stop", Stages: []StagedFile{{
		StageKey: "customers", File: "customers.csv", TemplateKey: "customers", Bucket: "migration", Key: "batches/1/stages/customers/customers.csv", Filename: "customers.csv", Size: 10, ETag: "e1",
	}}}
	first, err := a.MaterializeBatchStages(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stages[0].RuleTemplateID != tpl || first.Stages[0].RuleTemplateVersion != 1 {
		t.Fatalf("first binding = %+v", first.Stages[0])
	}
	// A newer version is published between the first attempt and a redelivery.
	if _, err := pool.Exec(ctx, `INSERT INTO rule_templates (org_id, template_key, version, name, schema_json, published)
		VALUES ($1, 'customers', 2, 'Customers', '{}', true)`, org); err != nil {
		t.Fatal(err)
	}
	again, err := a.MaterializeBatchStages(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.Stages[0].RuleTemplateID != tpl || again.Stages[0].RuleTemplateVersion != 1 || again.Stages[0].StageRowID != first.Stages[0].StageRowID {
		t.Fatalf("redelivery changed the binding: %+v", again.Stages[0])
	}
	// A manifest that disagrees with the persisted stage is a conflict.
	bad := in
	bad.Stages = []StagedFile{{StageKey: "customers", File: "other.csv", TemplateKey: "customers", Bucket: "migration", Key: "k", Filename: "other.csv"}}
	if _, err := a.MaterializeBatchStages(ctx, bad); err == nil {
		t.Fatal("a conflicting manifest must be refused")
	}
}

// A4 on the real registry: two schedules over the same object each get a job;
// a legacy job of the same schedule is reused instead of duplicated.
func TestPGWatchedObjectIdentityIsPerSchedule(t *testing.T) {
	pool := testdb.New(t)
	org, tpl := testdb.Seed(t, pool)
	ctx := context.Background()
	a := pgActivities(pool)
	var connector int64
	if err := pool.QueryRow(ctx, `INSERT INTO connectors (org_id, name, connector_kind, config_json) VALUES ($1, 'landing', 'watched_prefix', '{}') RETURNING id`, org).Scan(&connector); err != nil {
		t.Fatal(err)
	}
	schedule := func(name string) (id int64) {
		if err := pool.QueryRow(ctx, `INSERT INTO schedules (org_id, name, rule_template_id, rule_template_version, connector_id, spec_json, temporal_schedule_id)
			VALUES ($1, $2, $3, 1, $4, '{}', $2) RETURNING id`, org, name, tpl, connector).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	s1, s2 := schedule("s1"), schedule("s2")
	start := func(s int64) StartWatchObjectJobOutput {
		t.Helper()
		out, err := a.StartWatchObjectJob(ctx, StartWatchObjectJobInput{
			OrgID: org, ScheduleID: s, RuleTemplateID: tpl, RuleTemplateVersion: 1, ConnectorID: connector,
			Bucket: "migration", Key: "orgs/1/incoming/new.csv", ETag: "etag-new", Size: 20, ScheduledTime: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	j1, j2 := start(s1), start(s2)
	if !j1.Created || !j2.Created || j1.JobID == j2.JobID || j1.WorkflowID == j2.WorkflowID {
		t.Fatalf("distinct schedules must get distinct jobs: %+v %+v", j1, j2)
	}
	if again := start(s1); again.Created || again.JobID != j1.JobID {
		t.Fatalf("redelivery of schedule 1 must reuse its job: %+v", again)
	}

	// A job created before intent-scoped ids, for a third schedule.
	s3 := schedule("s3")
	legacyID := WatchWorkflowID(org, "migration", "orgs/1/incoming/old.csv", "etag-old")
	var legacyJob int64
	if err := pool.QueryRow(ctx, `INSERT INTO jobs (org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status, temporal_workflow_id)
		VALUES ($1, $2, 1, $3, '{"type":"minio","source_id":0}', 'succeeded', $4) RETURNING id`, org, tpl, s3, legacyID).Scan(&legacyJob); err != nil {
		t.Fatal(err)
	}
	out, err := a.StartWatchObjectJob(ctx, StartWatchObjectJobInput{
		OrgID: org, ScheduleID: s3, RuleTemplateID: tpl, RuleTemplateVersion: 1, ConnectorID: connector,
		Bucket: "migration", Key: "orgs/1/incoming/old.csv", ETag: "etag-old", Size: 20, ScheduledTime: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Created || out.JobID != legacyJob || out.WorkflowID != legacyID {
		t.Fatalf("legacy job must be reused: %+v", out)
	}
}
