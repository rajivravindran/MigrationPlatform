package lifecycle

import (
	"testing"

	"go.temporal.io/api/enums/v1"
)

func TestTerminalStatus(t *testing.T) {
	ok := Totals{Processed: 3, Total: 3}
	cases := []struct {
		name string
		t    Totals
		o    Outcome
		want string
	}{
		{"all rows succeeded", ok, Outcome{}, "succeeded"},
		{"a failed row", Totals{Processed: 2, Failed: 1, Total: 3}, Outcome{}, "failed"},
		{"an unresolved row", Totals{Processed: 2, Unresolved: 1, Total: 3}, Outcome{}, "failed"},
		{"no rows at all", Totals{}, Outcome{}, "failed"},
		{"execution error despite clean rows", ok, Outcome{Error: "ingest: gone"}, "failed"},
		{"cancelled wins over row outcomes", Totals{Failed: 1, Total: 1}, Outcome{Cancelled: true}, "cancelled"},
	}
	for _, c := range cases {
		if got := TerminalStatus(c.t, c.o); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRepairFor(t *testing.T) {
	cases := []struct {
		name      string
		status    enums.WorkflowExecutionStatus
		found     bool
		finalize  bool
		cancelled bool
		withError bool
	}{
		{"running workflow owns the job", enums.WORKFLOW_EXECUTION_STATUS_RUNNING, true, false, false, false},
		{"continued-as-new is still running", enums.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW, true, false, false, false},
		{"completed without finalizing: rows decide", enums.WORKFLOW_EXECUTION_STATUS_COMPLETED, true, true, false, false},
		{"cancelled", enums.WORKFLOW_EXECUTION_STATUS_CANCELED, true, true, true, false},
		{"failed", enums.WORKFLOW_EXECUTION_STATUS_FAILED, true, true, false, true},
		{"terminated", enums.WORKFLOW_EXECUTION_STATUS_TERMINATED, true, true, false, true},
		{"timed out", enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT, true, true, false, true},
		{"never started or expired", enums.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, false, true, false, true},
	}
	for _, c := range cases {
		r := RepairFor(c.status, c.found)
		if r.Finalize != c.finalize || r.Outcome.Cancelled != c.cancelled || (r.Outcome.Error != "") != c.withError {
			t.Errorf("%s: got %+v", c.name, r)
		}
	}
}

func TestExplain(t *testing.T) {
	cases := []struct {
		name   string
		status string
		t      Totals
		o      Outcome
		prior  string
		want   string
	}{
		{"success", StatusSucceeded, Totals{Processed: 3, Total: 3}, Outcome{}, "", "all 3 rows succeeded"},
		{"row failures", StatusFailed, Totals{Processed: 1, Failed: 1, Unresolved: 1, Total: 3}, Outcome{}, "", "1 of 3 rows failed, 1 unresolved"},
		{"empty job", StatusFailed, Totals{}, Outcome{}, "", "no source rows were read"},
		{"execution error", StatusFailed, Totals{Processed: 3, Total: 3}, Outcome{Error: "ingest: gone"}, "", "execution error: ingest: gone"},
		{"sticky earlier error after a clean retry", StatusFailed, Totals{Processed: 3, Total: 3}, Outcome{}, "ingest: gone", "earlier execution error kept: ingest: gone"},
		{"cancelled", StatusCancelled, Totals{Failed: 1, Total: 1}, Outcome{Cancelled: true}, "", "execution was cancelled"},
		{"stays cancelled on a later finalize", StatusCancelled, Totals{Processed: 1, Total: 1}, Outcome{}, "", "execution was cancelled"},
	}
	for _, c := range cases {
		if got := Explain(c.status, c.t, c.o, c.prior); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if !(Verdict{Status: StatusSucceeded}).Succeeded() || (Verdict{Status: StatusFailed}).Succeeded() || (Verdict{}).Succeeded() {
		t.Error("only a persisted succeeded status counts as success")
	}
}

func TestBatchTerminalStatusAndExplain(t *testing.T) {
	cases := []struct {
		name   string
		t      StageTotals
		o      Outcome
		status string
		reason string
	}{
		{"all stages succeeded", StageTotals{Succeeded: 2, Total: 2}, Outcome{}, BatchSucceeded, "all 2 stages succeeded"},
		{"mixed", StageTotals{Succeeded: 1, Failed: 1, Total: 2}, Outcome{}, BatchPartial, "1 of 2 stages failed, 1 succeeded"},
		{"all failed or skipped", StageTotals{Failed: 2, Total: 2}, Outcome{}, BatchFailed, "all 2 stages failed or were skipped"},
		{"abandoned stages", StageTotals{Succeeded: 1, Unfinished: 1, Total: 2}, Outcome{}, BatchFailed, "1 of 2 stages never reached a terminal state"},
		{"no stages", StageTotals{}, Outcome{}, BatchFailed, "no stages were materialized"},
		{"execution error beats clean stages", StageTotals{Succeeded: 2, Total: 2}, Outcome{Error: "finalize batch stage 3: db down"}, BatchFailed, "execution error: finalize batch stage 3: db down"},
		{"cancelled", StageTotals{Succeeded: 1, Total: 2, Unfinished: 1}, Outcome{Cancelled: true}, BatchFailed, "execution was cancelled"},
	}
	for _, c := range cases {
		st := BatchTerminalStatus(c.t, c.o)
		if st != c.status {
			t.Errorf("%s: status %q, want %q", c.name, st, c.status)
		}
		if got := ExplainBatch(st, c.t, c.o); got != c.reason {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.reason)
		}
	}
}
