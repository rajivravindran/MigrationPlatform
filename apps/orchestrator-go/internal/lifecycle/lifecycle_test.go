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
