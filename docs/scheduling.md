# Scheduling recurring jobs

Schedules are backed by **Temporal Schedules**. The Rust API mirrors the
relevant config into Postgres (`schedules` + `schedule_runs`) so the UI can
list/filter them with the rest of the platform data, and audit changes.

## Model

`schedules` row:

| Column                   | Example                                   |
| ------------------------ | ----------------------------------------- |
| `id`                     | 42                                        |
| `name`                   | "Nightly Salesforce sync"                |
| `rule_template_id`       | 7 (must be published)                     |
| `connector_id`           | 3                                         |
| `spec_json`              | `{ "cron":"0 2 * * *" }`                 |
| `timezone`               | `"America/Los_Angeles"` (IANA)            |
| `overlap_policy`         | one of `skip`/`buffer_one`/`buffer_all`/`cancel_other`/`allow_all` |
| `catchup_window_seconds` | 3600                                      |
| `enabled`                | true                                      |

Each scheduled firing creates a `schedule_runs` row and, if overlap policy
allows, a new `jobs` row with `schedule_id` set.

## Cron spec

Use POSIX 5-field cron (`m h dom mon dow`). Seconds and lists are
supported via the standard Temporal cron parser. Examples:

| Expression        | Meaning                          |
| ----------------- | -------------------------------- |
| `*/5 * * * *`     | every 5 minutes                  |
| `0 2 * * *`       | 02:00 every day                  |
| `0 9 * * MON-FRI` | 09:00 weekdays                   |
| `0 0 1 * *`       | midnight on the 1st of each month|

The UI's **Next 5 fires** preview uses a client-side parser; the server
returns `next_run_at` from Temporal which is the authoritative source.

## Timezones

`timezone` uses IANA names (`America/Los_Angeles`, `Europe/Paris`, `Asia/Tokyo`,
`UTC`). DST is handled correctly by Temporal; use explicit `Etc/UTC` if you
don't want DST shifts.

## Overlap policies

| Policy         | Behaviour                                                                  |
| -------------- | -------------------------------------------------------------------------- |
| `skip`         | If a run is still active, drop the next fire. (Default.)                  |
| `buffer_one`   | Queue at most one additional run.                                          |
| `buffer_all`   | Queue every fire; run them serially.                                       |
| `cancel_other` | Cancel the in-progress run and start the new one.                          |
| `allow_all`    | Start runs in parallel.                                                    |

Use `skip` for long-running migrations to avoid pile-ups; `cancel_other` for
"latest data wins" destinations.

## Catch-up

`catchup_window_seconds` controls how far back Temporal will backfill missed
fires after an outage. Default 3600 (1h). Set to 0 to disable catch-up.

## Incremental mode

When a schedule runs repeatedly against the same source, persistent state
avoids reprocessing. The orchestrator stores a **cursor** per
(schedule, connector):

- Salesforce: last `SystemModstamp` (or template-specified field).
- Watched prefix: map of `{ key: etag }` under `connectors.config_json.cursor.seen`,
  advanced by `WatchPrefixWorkflow` after successfully starting each object job.
- Custom HTTP: last pagination cursor value.

Watched-prefix schedules fire **`WatchPrefixWorkflow`** (not `MigrationWorkflow`
directly). That workflow lists the bucket/prefix, diffs against the cursor, and
starts one child `MigrationWorkflow` per new plain file (CSV/JSON/XML), or one
child `BatchWorkflow` per archive (`.tar.gz` / `.tgz` / `.zip` with in-archive
`manifest.json`).

The cursor is stored in `connectors.config_json.cursor` and updated via the
`AdvanceConnectorCursor` activity. Clear `cursor` on the connector to reprocess.

## Controls

UI buttons and their API equivalents:

| UI                | API                                        | Notes                               |
| ----------------- | ------------------------------------------ | ----------------------------------- |
| Pause             | `POST /schedules/{id}/pause`               | Temporal `pauseSchedule`.           |
| Resume            | `POST /schedules/{id}/resume`              | Temporal `unpauseSchedule`.         |
| Trigger now       | `POST /schedules/{id}/trigger`             | Immediate ad-hoc run; respects overlap. |
| Run history       | `GET /schedules/{id}/runs`                 | Latest 50 `schedule_runs` rows.     |

## Admin CLI

```bash
migration-admin schedule list
migration-admin schedule pause 42
migration-admin schedule trigger 42
migration-admin schedule reset-cursor 42  # clears incremental cursor
```
