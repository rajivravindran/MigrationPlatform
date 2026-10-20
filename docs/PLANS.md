# Migration plans

A **plan** is a coordinator's dependency tree for a migration, designed before any file exists. Files arrive later and run against the plan's **steps**; each is held until the steps it depends on have completed.

## Concepts

| Term | Meaning |
|---|---|
| Plan | Named, versioned tree of steps (`plans`). A draft is edited in place; publishing freezes it; revising a published plan creates the next version. Runs pin the version they ran under. |
| Step | One part of the migration, e.g. `accounts` (`plan_steps`): `depends_on`, optional default template, optional file name pattern, completion rule. |
| Completion rule | When a run of the step counts as complete for its dependents. `strict`: every record succeeded. `max_failed_pct`: failed + unresolved records are at most N% of records read. A run that was cancelled, errored, read nothing or did not attempt every record never passes. |
| Plan run | One file bound to one step (`plan_runs`): `waiting → running → completed | failed | cancelled`. A waiting run is **blocked** when a prerequisite's latest run failed its rule; it still starts automatically if that prerequisite later completes. |
| Intake | An archive (`.tar.gz`/`.zip`) received for a plan (`plan_intakes`), unpacked and fanned out into runs. |

A step is **completed** once any of its runs (in any version of the plan) met its rule; later re-runs do not undo that.

## How files arrive

- **Single file** (CSV, JSON / JSON Lines, XML) — `POST /plans/:id/runs {source_ref, step_key, rule_template_id?}`. The template defaults to the step's. A file whose format differs from the template's source type is refused at submit.
- **Archive** — `POST /plans/:id/runs {source_ref, overrides?}`. The archive's `manifest.json` names a template per file (`templateKey`). Each file is bound to a step by: an override, else a step whose key equals the manifest stage id, else the step file patterns. The template is: override, else manifest, else step default. Files that cannot be bound are recorded as failed runs saying why.
- **Watched folder** — a schedule created with `plan_id` routes arrivals: archives become intakes; files matching a step pattern become runs; other files keep the schedule's single-template behaviour. Each object version is routed once.

Manifest `dependsOn` is ignored for plan archives: the plan's dependencies govern order.

## Execution

- `PlanRunWorkflow` (orchestrator) re-checks the run's prerequisites every 15 s (`CheckPlanRunGate`, recording the waiting/blocked reason), then creates and runs an ordinary migration job (`StartPlanRunJob`, child `MigrationWorkflow`), then applies the step's rule to the job's persisted totals (`FinalizePlanRun`). It continues as new every 400 polls.
- `PlanArchiveWorkflow` unpacks (`StagePlanArchive`, sharing the batch unpack and verification path), binds files (`CreatePlanIntakeRuns`) and starts a detached `PlanRunWorkflow` per file.
- Cancelling a waiting run (`POST /plan-runs/:id/cancel`) records it cancelled immediately; cancelling a running one cancels its job.

Rule logic, gate decisions and pattern matching are pure functions in `apps/orchestrator-go/internal/plan`.

## UI

- **Plans** (`/plans`): list with progress. **Design a plan** (`/plans/new`): canvas plus step forms (name, default template, file pattern, completion rule, waits-for). Positions are saved with the plan.
- **Plan page** (`/plans/:id`): live graph — pending (no file yet), waiting, blocked, running, completed, failed, cancelled — a **Run a file** panel (archives are read in the browser to preview and adjust the file → step/template mapping before upload), the runs list with reasons, and archives received.
- Schedule creation offers **Route files into a plan**.

## Not yet

- A step completes on its first passing run; there is no "expect N files" rule.
- Dependencies are between steps of one plan, not across plans.
- JSON files must be an array of records or JSON Lines; records nested under a key (`{"data": [...]}`) are not yet supported. XML records are found by element name (`source.options.recordTag`, default `record`).
- There is no reconciler for plan runs whose workflow was lost; the job reconciler still settles their jobs.
