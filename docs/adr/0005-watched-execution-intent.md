# ADR 0005: Execution intent for watched sources

- Status: accepted (P0, architecture review A4).
- Date: 2026-10-19

## Context

`WatchPrefixWorkflow` started one child per discovered object with a workflow
id hashed from organization, bucket, key and ETag (`WatchWorkflowID`,
`BatchWorkflowID`). That id names a **source version** (ADR 0001), not an
execution. Two schedules bound to the same connector but to different
templates or destinations therefore resolved to the same id; the second was
skipped as already started and its intended external effects never happened.
Discovery progress had the same flaw: one `cursor.seen` map on the connector
was shared by every schedule, so an object processed by one schedule was
invisible to the others.

## Decision

**Identity.** An execution intent is a source version plus the schedule that
binds it to a template and destination. `WatchExecutionWorkflowID(org,
schedule, bucket, key, etag)` and `BatchExecutionWorkflowID(...)` include the
schedule in the hash. The id prefix (`watch-{org}-`, `batch-{org}-`) is
unchanged. A schedule id of zero keeps the legacy source-only id.

**Legacy jobs.** `StartWatchObjectJob` and `StartBatch` first look for a job
or batch with the legacy id **for the same schedule** and reuse it. Jobs
created before this change keep their ids; nothing is renamed or
reprocessed by the upgrade.

**Cursor ownership.** Discovery progress lives per schedule at
`connectors.config_json.cursor.bySchedule.<scheduleId>.seen`. The legacy
`cursor.seen` is read as seen by every schedule and is no longer written by
schedule-scoped runs. It is kept because reprocessing objects it names
would repeat external effects that one schedule already caused.

**Replay policy.**

- Redelivery or catch-up of the same schedule reuses the existing job: no
  extra execution.
- A different schedule over the same object is a different intent and
  executes once.
- A new template version within a schedule does **not** reprocess source
  versions that schedule has seen. Reprocessing is an explicit operator
  action: clear `cursor.bySchedule.<scheduleId>` (one schedule) or `cursor`
  (all). Because job identity includes the schedule, clearing a cursor cannot
  create a second execution for an object that schedule already recorded.
- A new source version (changed ETag) is always a new execution.

## Consequences

- `AdvanceConnectorCursorInput` gains `scheduleId`; an activity worker built
  before this change ignores it and writes the connector-wide map, which is
  still read, so a mixed rollout loses no deduplication.
- Rolling the orchestrator back after schedule-scoped cursors were written
  returns to the connector-wide map, which lacks those entries: objects
  processed in between would be rediscovered. Their jobs exist under
  intent-scoped ids, which the old `StartWatchObjectJob` does not look up, so
  a rollback can create duplicate jobs for recently processed objects.
  Before rolling back, run `migration-admin merge-cursors` (optionally
  `--connector <id>`): it folds every per-schedule `seen` map into the
  connector-wide `cursor.seen`, so the older build skips those objects. The
  per-schedule maps are left in place for a later upgrade.
- Nothing yet prevents a connector from being bound to many schedules;
  that is intended. Exclusivity, if wanted, is a schedule-admission rule
  (review A5).

## Verification

- `internal/activities/watch_cursor_test.go`: legacy + per-schedule merge,
  isolation between schedules, size bound.
- `internal/workflows/watch_intent_test.go`: two schedules over one source,
  one already done, the other still executes once under a distinct id and
  records progress for itself only; id stability/distinctness table.
- Real-stack scenario (two schedules, one connector, redelivery) is not yet
  run.
