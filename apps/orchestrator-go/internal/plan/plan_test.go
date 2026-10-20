package plan_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/migration-platform/orchestrator/internal/plan"
)

func TestStrictRuleRequiresEveryRecord(t *testing.T) {
	strict := plan.Rule{Kind: plan.RuleStrict}
	ok := plan.Evaluate(strict, plan.JobOutcome{Status: "succeeded", Total: 10, Processed: 10})
	require.True(t, ok.Met)

	one := plan.Evaluate(strict, plan.JobOutcome{Status: "failed", Total: 10, Processed: 9, Failed: 1})
	require.False(t, one.Met)
	require.InDelta(t, 10.0, one.FailedPct, 0.001)
	require.Contains(t, one.Reason, "1 of 10")
}

func TestToleranceRule(t *testing.T) {
	five := plan.Rule{Kind: plan.RuleMaxFailedPct, MaxFailedPct: 5}
	// A job with failed rows is "failed" but may still meet a tolerance rule.
	within := plan.Evaluate(five, plan.JobOutcome{Status: "failed", Total: 100, Processed: 97, Failed: 3})
	require.True(t, within.Met, within.Reason)
	require.Contains(t, within.Reason, "3% of records failed, within the 5% allowed")

	edge := plan.Evaluate(five, plan.JobOutcome{Status: "failed", Total: 100, Processed: 95, Failed: 5})
	require.True(t, edge.Met, "exactly at the threshold passes")

	over := plan.Evaluate(five, plan.JobOutcome{Status: "failed", Total: 100, Processed: 94, Failed: 4, Unresolved: 2})
	require.False(t, over.Met, "unresolved rows count as failures")
	require.Contains(t, over.Reason, "6% of records failed, more than the 5% allowed")
}

func TestNoRuleMetWithoutACompleteExecution(t *testing.T) {
	lenient := plan.Rule{Kind: plan.RuleMaxFailedPct, MaxFailedPct: 100}
	for name, job := range map[string]plan.JobOutcome{
		"cancelled":  {Status: "cancelled", Total: 10, Processed: 10},
		"exec error": {Status: "failed", Total: 10, Processed: 2, ExecError: "worker lost"},
		"empty":      {Status: "failed"},
		"incomplete": {Status: "failed", Total: 10, Processed: 4, Failed: 1},
	} {
		require.False(t, plan.Evaluate(lenient, job).Met, name)
	}
}

func TestGate(t *testing.T) {
	require.True(t, plan.Gate(nil).Ready, "a root step is ready immediately")
	require.True(t, plan.Gate([]plan.Prerequisite{{Step: "accounts", Completed: true}}).Ready)

	wait := plan.Gate([]plan.Prerequisite{{Step: "accounts", Running: true}, {Step: "contacts"}})
	require.False(t, wait.Ready)
	require.False(t, wait.Blocked)
	require.Equal(t, "waiting for accounts, contacts to complete", wait.Reason)

	blocked := plan.Gate([]plan.Prerequisite{
		{Step: "accounts", Completed: true},
		{Step: "orders", FailedReason: "12% of records failed"},
	})
	require.True(t, blocked.Blocked)
	require.Contains(t, blocked.Reason, "orders (12% of records failed)")

	rerun := plan.Gate([]plan.Prerequisite{{Step: "orders", FailedReason: "x", Running: true}})
	require.False(t, rerun.Blocked, "a failed step being re-run is waited on, not blocked")
}

func TestMatchStep(t *testing.T) {
	steps := []plan.Step{
		{Key: "orders", Ordinal: 2, FilePattern: "orders_*.csv"},
		{Key: "accounts", Ordinal: 0, FilePattern: "ACCOUNTS*"},
		{Key: "anything", Ordinal: 5, FilePattern: "*.csv"},
		{Key: "none", Ordinal: 1},
	}
	key, ok := plan.MatchStep(steps, "incoming/2026/accounts_oct.xml")
	require.True(t, ok)
	require.Equal(t, "accounts", key, "case-insensitive, base name only")

	key, _ = plan.MatchStep(steps, "orders_1.csv")
	require.Equal(t, "orders", key, "earliest matching step wins")

	_, ok = plan.MatchStep(steps, "notes.txt")
	require.False(t, ok)
}
