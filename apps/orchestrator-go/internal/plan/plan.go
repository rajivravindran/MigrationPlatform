// Package plan holds the decisions behind migration plans: when a step's run
// counts as complete for its dependents, when a waiting run may start, and
// which step an arriving file belongs to. It is pure so the same rules serve
// the workflows, the activities and their tests.
package plan

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

const (
	RuleStrict       = "strict"
	RuleMaxFailedPct = "max_failed_pct"
)

// Rule is a step's completion rule.
type Rule struct {
	Kind         string  `json:"kind"`
	MaxFailedPct float64 `json:"maxFailedPct,omitempty"`
}

// JobOutcome is what a finished job recorded.
type JobOutcome struct {
	Status     string // succeeded | failed | cancelled
	Total      int64  // source rows read
	Processed  int64  // rows accepted by the destination
	Failed     int64
	Unresolved int64
	// ExecError is set when the job's execution itself failed (not its rows).
	ExecError string
}

// Verdict says whether a run met its step's completion rule.
type Verdict struct {
	Met       bool
	FailedPct float64
	Reason    string
}

// Evaluate applies rule to a finished job. Unresolved rows (unknown
// destination effect) count as failures: a step never passes on records the
// platform cannot vouch for. A job that did not account for every row it read
// — cancelled, errored or empty — never meets any rule.
func Evaluate(rule Rule, job JobOutcome) Verdict {
	bad := job.Failed + job.Unresolved
	var pct float64
	if job.Total > 0 {
		pct = float64(bad) * 100 / float64(job.Total)
	}
	switch {
	case job.ExecError != "":
		return Verdict{FailedPct: pct, Reason: "the migration did not finish: " + job.ExecError}
	case job.Status == "cancelled":
		return Verdict{FailedPct: pct, Reason: "the migration was cancelled"}
	case job.Total == 0:
		return Verdict{Reason: "the file had no records"}
	case job.Processed+bad < job.Total:
		return Verdict{FailedPct: pct, Reason: fmt.Sprintf("only %d of %d records were attempted", job.Processed+bad, job.Total)}
	}
	if rule.Kind == RuleMaxFailedPct {
		if pct <= rule.MaxFailedPct {
			return Verdict{Met: true, FailedPct: pct, Reason: fmt.Sprintf("%s of records failed, within the %s allowed", fmtPct(pct), fmtPct(rule.MaxFailedPct))}
		}
		return Verdict{FailedPct: pct, Reason: fmt.Sprintf("%s of records failed, more than the %s allowed (%d of %d)", fmtPct(pct), fmtPct(rule.MaxFailedPct), bad, job.Total)}
	}
	if bad == 0 && job.Status == "succeeded" {
		return Verdict{Met: true, Reason: "every record succeeded"}
	}
	return Verdict{FailedPct: pct, Reason: fmt.Sprintf("%d of %d records failed or are unresolved; this step requires all to succeed", bad, job.Total)}
}

func fmtPct(p float64) string {
	s := fmt.Sprintf("%.2f", p)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + "%"
}

// Prerequisite is the state of one step a run depends on, across every run
// of that step in the plan (all versions).
type Prerequisite struct {
	Step string
	// Completed: some run of the step met its completion rule.
	Completed bool
	// Running: a run of the step is waiting or in progress.
	Running bool
	// FailedReason: the latest finished run did not meet the rule (empty if none).
	FailedReason string
}

// GateDecision is whether a waiting run may start.
type GateDecision struct {
	Ready   bool
	Blocked bool
	Reason  string
}

// Gate decides from the prerequisites. A run is blocked (not merely waiting)
// when a prerequisite's latest run failed its rule and nothing is completing
// it; it still starts if that prerequisite later completes.
func Gate(prereqs []Prerequisite) GateDecision {
	var waiting, blocked []string
	for _, p := range prereqs {
		if p.Completed {
			continue
		}
		if p.FailedReason != "" && !p.Running {
			blocked = append(blocked, fmt.Sprintf("%s (%s)", p.Step, p.FailedReason))
			continue
		}
		waiting = append(waiting, p.Step)
	}
	switch {
	case len(blocked) > 0:
		return GateDecision{Blocked: true, Reason: "blocked: " + strings.Join(blocked, "; ") + ". Starts automatically once re-run successfully."}
	case len(waiting) > 0:
		return GateDecision{Reason: "waiting for " + strings.Join(waiting, ", ") + " to complete"}
	default:
		return GateDecision{Ready: true}
	}
}

// Step is the routing view of a plan step.
type Step struct {
	Key         string
	Ordinal     int
	FilePattern string
}

// MatchStep returns the step whose file pattern matches the file's base name,
// case-insensitively. When several match, the earliest step (by ordinal) wins.
func MatchStep(steps []Step, filename string) (string, bool) {
	base := strings.ToLower(path.Base(filename))
	sorted := append([]Step(nil), steps...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Ordinal < sorted[j].Ordinal })
	for _, s := range sorted {
		pattern := strings.ToLower(strings.TrimSpace(s.FilePattern))
		if pattern == "" {
			continue
		}
		if ok, err := path.Match(pattern, base); err == nil && ok {
			return s.Key, true
		}
	}
	return "", false
}
