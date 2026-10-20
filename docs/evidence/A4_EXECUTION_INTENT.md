# A4 — Watched-source execution intent (task evidence)

```text
Task ID / owner: Architecture review A4 (P0 contract repair) / Rajiv Ravindran; implemented by agent session 2026-10-19
Base SHA / result SHA / PR: working tree on local checkout (git state not inspected from this session; no commit made) / uncommitted / none
Dependencies verified: ADR 0001 source registry present; WatchPrefixWorkflow/StartWatchObjectJob/StartBatch as reviewed
Problem and changed behaviour: see ADR 0005. Workflow ids and discovery cursors are now scoped to the
  schedule (execution intent) rather than the source version alone; legacy ids and the legacy
  connector-wide cursor are honoured.
Files and schema/API changes:
  apps/orchestrator-go/internal/activities/watch.go (WatchExecutionWorkflowID, SeenObjects, advanceSeen,
    AdvanceConnectorCursorInput.ScheduleID, legacy-job reuse in StartWatchObjectJob)
  apps/orchestrator-go/internal/activities/batch.go (BatchExecutionWorkflowID, legacy-batch reuse in StartBatch)
  apps/orchestrator-go/internal/workflows/watch.go (per-schedule seen set; ScheduleID on cursor advance)
  Tests: internal/activities/watch_cursor_test.go (new), internal/workflows/watch_intent_test.go (new),
    internal/workflows/watch_test.go (stubs use intent ids; cursor schedule assertion)
  Docs: docs/adr/0005-watched-execution-intent.md (new), docs/scheduling.md, docs/ARCHITECTURE_REVIEW_2026-10-17.md
  No SQL schema change (cursor layout inside connectors.config_json gains cursor.bySchedule). No HTTP API change.
Before-fix reproduction or feature baseline:
  TestTwoSchedulesOverTheSameSourceEachExecuteOnce against the old code: schedule 2 skipped new.csv
  (shared cursor) and, had it not, would have received the same workflow id as schedule 1.
Commands actually run and results:
  go build ./... — ok; go vet ./... — ok; gofmt -l . — clean
  go test ./internal/workflows ./internal/activities — ok (all existing watch/batch/replay tests pass)
Destination receipts / reconciliation evidence: not applicable.
Workflow replay and upgrade compatibility:
  WatchPrefixWorkflow command sequence is unchanged (same activities, extra input field), so old histories
  replay. New ids apply to newly discovered objects only; legacy lookup keeps existing jobs.
Rollout, rollback and recovery limits: see ADR 0005 consequences (rollback while watched schedules are
  active can duplicate jobs for objects processed after the upgrade).
Blocked checks / residual risks:
  Real-stack scenario not run. The Rust schedules API still allows any owned connector into any schedule
  (A5). Operator tooling (`migration-admin schedule reset-cursor`) still clears the whole cursor; a
  per-schedule reset is documented as a manual config edit.
Status and reviewer: implemented-unverified; reviewer unassigned
Remote publication status: not committed, not pushed
```
