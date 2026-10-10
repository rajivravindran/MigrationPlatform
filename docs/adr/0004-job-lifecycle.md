# ADR 0004: Job lifecycle ownership, conditional transitions and reconciliation

- Status: accepted (R05, MP-05).
- Date: 2026-10-10

## Context

A job's status is stored in `jobs.status`, while its execution happens in a
Temporal workflow. The two drifted apart in several ways:

1. **The API overwrote outcomes.** `POST /jobs` set `running` after the start
   call returned, with no condition. A fast workflow that had already
   finalized the job was put back to `running` and stayed there.
2. **Early exits never finalized.** License refusal, template load, ingest,
   preprocess, checkpoint and persistence errors returned from the workflow
   before `FinalizeJob`. Cancellation did too. The job stayed `running`.
3. **Finalization and export errors were ignored.** A `FinalizeJob` failure
   still completed the workflow, and a failed export was invisible.
4. **No repair.** A worker crash during finalize, a terminated workflow or a
   start that never happened left the job non-terminal forever. Cancel then
   returned `502 workflow not found`.

## Decision

**Ownership.** The workflow owns the terminal outcome. The API records
operator intent and moves the job between non-terminal states only. The
reconciler finalizes a job only when its workflow can no longer do it.

**Allowed transitions.** Every write is conditional on the current state:

| From | To | Writer |
|------|----|--------|
| `pending` | `running` | API after start, `MarkJobRunning`, reconciler (workflow is running) |
| `pending` | `failed` | API, only when the bridge refused the connection (nothing can have started) |
| `running` | `paused` | API pause |
| `paused` | `running` | API resume |
| `pending`, `running`, `paused` | `cancelled` | API cancel |
| any except `cancelled` | terminal | `FinalizeJob` (workflow) |
| `pending`, `running`, `paused` | terminal | reconciler |

`cancelled` is never overwritten. Terminal states are rewritten only by
`FinalizeJob` after a row retry, which recomputes the outcome from
`job_rows`.

An ambiguous start, such as a timeout or a 5xx from the bridge, leaves the
job `pending`. The workflow may exist, so the reconciler decides from
Temporal's view instead of the API guessing.

**Finalize on every exit.** `MigrationWorkflow` runs the work in
`runMigration`. Every exit except continue-as-new goes through
`finishJob`, which runs on a disconnected context so cancellation cannot
skip it:

- `FinalizeJob` records `cancelled`, or `failed` with `last_error`, or the
  status derived from rows (`lifecycle.TerminalStatus`).
- A `FinalizeJob` failure fails the workflow, and the reconciler repairs
  the job.
- `last_error` is sticky. A job that stopped before reading every source
  record stays `failed` after row retries.

The behaviour is gated by `GetVersion("finalize-on-exit")`, so histories
recorded before the change replay their old commands.

**Export is separate.** `export_status` (`pending`/`succeeded`/`failed`) and
`export_error` are owned by the export path: `RecordExportStarted`,
`ExportJobResults` and `RecordExportFailure`. An export failure does not
change the job outcome. `POST /jobs/{id}/export` starts
`ExportResultsWorkflow` (ID `{wf}-export`, one at a time). That workflow
only reads `job_rows` and uploads files, so retrying an export makes zero
destination calls. Downloads use the exported files only while the latest
export succeeded, and read `job_rows` otherwise.

**Reconciler.** The reconciler runs in the orchestrator process every
`RECONCILE_INTERVAL` (default 1m). Each pass holds a Postgres advisory lock,
so replicas do not duplicate work. It pages through jobs that are
`pending`/`running`/`paused` and unchanged for `RECONCILE_STALE_AFTER`
(default 5m), and describes each job's workflow:

| Workflow | Repair |
|----------|--------|
| Running / continued-as-new | none, except `pending` → `running` |
| Completed | finalize from rows |
| Canceled | `cancelled` |
| Failed / terminated / timed out | `failed` with the reason |
| Not found | `failed` ("never started or history expired") |

Repairs use the same `lifecycle.Finalize` as the workflow, restricted to
non-terminal jobs. They are logged as `job reconciled` and counted in
`jobs_reconciled_total{workflow_status,job_status}`, and pass errors are
counted in `job_reconcile_errors_total`.

**Cancel when the workflow is gone.** If the bridge reports the workflow
as not found (now a 404 rather than 502), nothing is running. The API
cancels the job directly, refreshes totals from rows, and records
`workflow_found: false` in the audit entry.

## Consequences

- Each job costs one more activity on success (`RecordExportStarted`), plus
  one on export failure.
- The reconciler adds one indexed query per pass, and one Temporal
  `DescribeWorkflowExecution` call per stale non-terminal job. Long-running
  healthy jobs are described once per interval once they pass the
  staleness threshold. That is cheap at hundreds of jobs. Revisit if
  thousands run concurrently.
- Jobs left non-terminal by earlier builds are repaired on the first pass
  after deploying. This happened in local development: two `running` jobs
  with no workflow became `failed`.
- Batch-parent interpretation is unchanged. A stage counts as failed when
  its child result reports failed or unresolved rows, or when the child
  errors.

## Verification

- Go: `lifecycle` tests for terminal status and repair decisions;
  workflow tests for ingest failure, cancellation on a disconnected
  context, finalize failure, export failure and export-only workflows. All
  existing replay tests pass.
- API (`tests/job_lifecycle.rs`): a fast outcome is not overwritten; an
  ambiguous start stays `pending`; pause/resume/cancel return 409 outside
  their states; cancel settles a vanished workflow; export retry starts
  only `ExportResultsWorkflow`.
- Harness: `ingest-failure`, `terminated-workflow` (reconciler) and
  `export-recovery` (zero destination calls on export retry).
