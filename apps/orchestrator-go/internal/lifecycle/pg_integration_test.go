//go:build integration

package lifecycle

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	"github.com/migration-platform/orchestrator/internal/control"
	"github.com/migration-platform/orchestrator/internal/testdb"
)

// fakeTemporal answers Describe/Query/Signal from a table so the reconciler
// can be driven against a real database without a Temporal server.
type fakeTemporal struct {
	status   map[string]enums.WorkflowExecutionStatus
	pause    map[string]control.PauseState
	signals  []string
	queryErr error
}

func (f *fakeTemporal) DescribeWorkflowExecution(_ context.Context, id, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	st, ok := f.status[id]
	if !ok {
		return nil, serviceerror.NewNotFound("no workflow " + id)
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflow.WorkflowExecutionInfo{Status: st}}, nil
}

func (f *fakeTemporal) QueryWorkflow(_ context.Context, id, _ string, _ string, _ ...interface{}) (converter.EncodedValue, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return encoded{v: f.pause[id]}, nil
}

func (f *fakeTemporal) SignalWorkflow(_ context.Context, id, _ string, name string, _ interface{}) error {
	f.signals = append(f.signals, id+":"+name)
	return nil
}

func reconciler(pool *pgxpool.Pool, t *fakeTemporal) *Reconciler {
	return &Reconciler{DB: pool, Temporal: t, Interval: time.Minute, StaleAfter: 0, Log: slog.Default()}
}

func jobStatus(t *testing.T, pool *pgxpool.Pool, id int64) (status string, control *string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT status::text, control_requested FROM jobs WHERE id = $1`, id).Scan(&status, &control); err != nil {
		t.Fatal(err)
	}
	return
}

func TestPGFinalizeVerdicts(t *testing.T) {
	pool := testdb.New(t)
	org, tpl := testdb.Seed(t, pool)
	ctx := context.Background()

	empty := testdb.Job(t, pool, org, tpl, "running", "wf-empty")
	v, err := Finalize(ctx, pool, empty, Outcome{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusFailed || v.Reason != "no source rows were read" {
		t.Fatalf("empty job verdict = %+v", v)
	}

	ok := testdb.Job(t, pool, org, tpl, "running", "wf-ok")
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO job_rows (job_id, row_index, status, attempts) VALUES ($1, $2, 'succeeded', 1)`, ok, i); err != nil {
			t.Fatal(err)
		}
	}
	v, err = Finalize(ctx, pool, ok, Outcome{})
	if err != nil || v.Status != StatusSucceeded || v.Reason != "all 3 rows succeeded" {
		t.Fatalf("verdict = %+v err=%v", v, err)
	}
	// A sticky execution error survives a later clean retry.
	v, _ = Finalize(ctx, pool, ok, Outcome{Error: "ingest: gone"})
	if v.Status != StatusFailed {
		t.Fatalf("verdict = %+v", v)
	}
	v, _ = Finalize(ctx, pool, ok, Outcome{})
	if v.Status != StatusFailed || v.Reason != "earlier execution error kept: ingest: gone" {
		t.Fatalf("verdict after retry = %+v", v)
	}
}

func TestPGBatchFinalizeSweepsAbandonedStagesAndKeepsQuarantine(t *testing.T) {
	pool := testdb.New(t)
	org, tpl := testdb.Seed(t, pool)
	ctx := context.Background()
	var batch int64
	if err := pool.QueryRow(ctx, `INSERT INTO batches (org_id, source_ref, status, temporal_workflow_id) VALUES ($1, '{}', 'running', 'wf-b') RETURNING id`, org).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	for i, st := range []string{"succeeded", "pending"} {
		if _, err := pool.Exec(ctx, `INSERT INTO batch_stages (batch_id, stage_index, stage_key, file_path, template_key, rule_template_id, rule_template_version, status)
			VALUES ($1, $2, $3, $3 || '.csv', 't', $4, 1, $5::batch_status)`, batch, i, "s"+string(rune('a'+i)), tpl, st); err != nil {
			t.Fatal(err)
		}
	}
	v, err := FinalizeBatch(ctx, pool, batch, Outcome{Error: "worker died"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != BatchFailed || v.Stages.Unfinished != 1 {
		t.Fatalf("verdict = %+v", v)
	}
	var swept string
	var msg *string
	if err := pool.QueryRow(ctx, `SELECT status::text, error_message FROM batch_stages WHERE batch_id = $1 AND stage_index = 1`, batch).Scan(&swept, &msg); err != nil {
		t.Fatal(err)
	}
	if swept != "failed" || msg == nil || *msg == "" {
		t.Fatalf("abandoned stage = %s %v", swept, msg)
	}

	var q int64
	if err := pool.QueryRow(ctx, `INSERT INTO batches (org_id, source_ref, status, temporal_workflow_id) VALUES ($1, '{}', 'quarantined', 'wf-q') RETURNING id`, org).Scan(&q); err != nil {
		t.Fatal(err)
	}
	v, err = FinalizeBatch(ctx, pool, q, Outcome{})
	if err != nil || v.Status != BatchQuarantined {
		t.Fatalf("quarantined verdict = %+v err=%v", v, err)
	}
}

func TestPGReconcilerRepairsStaleBatchAndControlCommands(t *testing.T) {
	pool := testdb.New(t)
	org, tpl := testdb.Seed(t, pool)
	ctx := context.Background()

	// A batch whose workflow was terminated with a stage still pending.
	var batch int64
	if err := pool.QueryRow(ctx, `INSERT INTO batches (org_id, source_ref, status, temporal_workflow_id, updated_at) VALUES ($1, '{}', 'running', 'wf-dead', now() - interval '1 hour') RETURNING id`, org).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO batch_stages (batch_id, stage_index, stage_key, file_path, template_key, rule_template_id, rule_template_version, status)
		VALUES ($1, 0, 'a', 'a.csv', 't', $2, 1, 'running')`, batch, tpl); err != nil {
		t.Fatal(err)
	}
	// A running job whose pause was signalled but never recorded (A3).
	paused := testdb.Job(t, pool, org, tpl, "running", "wf-paused")
	if _, err := pool.Exec(ctx, `UPDATE jobs SET control_requested = 'pause', control_command_id = 'cmd-1', control_requested_at = now() - interval '1 hour' WHERE id = $1`, paused); err != nil {
		t.Fatal(err)
	}
	// A job whose pause request never reached the workflow.
	unsent := testdb.Job(t, pool, org, tpl, "running", "wf-unsent")
	if _, err := pool.Exec(ctx, `UPDATE jobs SET control_requested = 'pause', control_command_id = 'cmd-2', control_requested_at = now() - interval '1 hour' WHERE id = $1`, unsent); err != nil {
		t.Fatal(err)
	}

	tm := &fakeTemporal{
		status: map[string]enums.WorkflowExecutionStatus{
			"wf-dead":   enums.WORKFLOW_EXECUTION_STATUS_TERMINATED,
			"wf-paused": enums.WORKFLOW_EXECUTION_STATUS_RUNNING,
			"wf-unsent": enums.WORKFLOW_EXECUTION_STATUS_RUNNING,
		},
		pause: map[string]control.PauseState{
			"wf-paused": {Paused: true, LastCommandID: "cmd-1", Acknowledged: true},
			"wf-unsent": {Paused: false},
		},
	}
	if _, err := reconciler(pool, tm).Pass(ctx); err != nil {
		t.Fatal(err)
	}

	var bs string
	var berr *string
	if err := pool.QueryRow(ctx, `SELECT status::text, error_message FROM batches WHERE id = $1`, batch).Scan(&bs, &berr); err != nil {
		t.Fatal(err)
	}
	if bs != "failed" || berr == nil {
		t.Fatalf("terminated batch = %s %v, want failed with a reason", bs, berr)
	}
	if s, c := jobStatus(t, pool, paused); s != "paused" || c != nil {
		t.Fatalf("acknowledged pause: status=%s control=%v", s, c)
	}
	if s, c := jobStatus(t, pool, unsent); s != "running" || c == nil {
		t.Fatalf("unsent pause must stay recorded: status=%s control=%v", s, c)
	}
	if len(tm.signals) != 1 || tm.signals[0] != "wf-unsent:pause" {
		t.Fatalf("re-sent signals = %v", tm.signals)
	}
	// Once the workflow reports the pause, the next pass converges it.
	tm.pause["wf-unsent"] = control.PauseState{Paused: true, LastCommandID: "cmd-2", Acknowledged: true}
	if _, err := reconciler(pool, tm).Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if s, c := jobStatus(t, pool, unsent); s != "paused" || c != nil {
		t.Fatalf("after re-send: status=%s control=%v", s, c)
	}
}

func TestPGOutcomeRevisionBumpsOnRowWrites(t *testing.T) {
	pool := testdb.New(t)
	org, tpl := testdb.Seed(t, pool)
	ctx := context.Background()
	job := testdb.Job(t, pool, org, tpl, "running", "wf-rev")
	rev := func() (r int64) {
		if err := pool.QueryRow(ctx, `SELECT outcome_revision FROM jobs WHERE id = $1`, job).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if rev() != 0 {
		t.Fatalf("fresh job revision %d", rev())
	}
	if _, err := pool.Exec(ctx, `INSERT INTO job_rows (job_id, row_index, status, attempts) VALUES ($1, 0, 'failed', 1)`, job); err != nil {
		t.Fatal(err)
	}
	if rev() != 1 {
		t.Fatalf("after insert revision %d", rev())
	}
	if _, err := pool.Exec(ctx, `UPDATE job_rows SET status = 'succeeded', attempts = 2 WHERE job_id = $1 AND row_index = 0`, job); err != nil {
		t.Fatal(err)
	}
	if rev() != 2 {
		t.Fatalf("after retry revision %d", rev())
	}
	// The export fence: publishing revision 1 after the rows moved to 2 is refused.
	tag, err := pool.Exec(ctx, `UPDATE jobs SET results_ref = '{"revision":1}', export_status = 'succeeded'
		WHERE id = $1 AND outcome_revision = $2 AND COALESCE((results_ref->>'revision')::bigint, -1) <= $2`, job, 1)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("stale export must not publish: affected=%d err=%v", tag.RowsAffected(), err)
	}
	tag, err = pool.Exec(ctx, `UPDATE jobs SET results_ref = '{"revision":2}', export_status = 'succeeded'
		WHERE id = $1 AND outcome_revision = $2 AND COALESCE((results_ref->>'revision')::bigint, -1) <= $2`, job, 2)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("current export must publish: affected=%d err=%v", tag.RowsAffected(), err)
	}
}
