# Phase changelog — Migration Platform roadmap

**Last updated:** 2026-08-07

Per-phase record of what changed, key files, how to test, what is NOT done, and
locked product decisions. Companion to [`AGENT_HANDOFF.md`](AGENT_HANDOFF.md).

> Production status: conditional-release MVP. Feature completion below does not
> supersede the release gates in `AGENT_HANDOFF.md` and `install.md`.

---

## P0a — Renamable steps

**What changed**
- Rule-template designer always persists `steps[]`; flat `mapping`/`destination`
  on save is gone. `"main"` is a load-only fallback for legacy rows.
- Pencil rename on every step tab; rename blocked when Response refs point at
  the old name.

**Key files**
- `apps/web/app/templates/` (designer), `apps/api/src/routes/templates.rs`,
  `packages/*/` schema (Zod/JSON Schema), Go/Rust/Python step types.

**How to test**
- Open a template, rename a step, save. Reload — name persists and refs update.
- A legacy row without `steps` still loads via `"main"`.

**Not done / follow-ups**
- None for P0a.

**Locked decisions**
- `steps[]` is the source of truth; `"main"` is legacy fallback only.

---

## P0b — `onFailure` + template `retry`

**What changed**
- `steps[].onFailure`: `stop` | `continue` across JSON Schema, Zod, Rust, Go, Python.
- Orchestrator continues the chain on `continue`; row is still `failed` if any
  step failed.
- `defaultActivityOptions(template.Retry)` wires maxAttempts / initial interval /
  backoff coefficient.

**Key files**
- `apps/orchestrator-go/internal/workflows/workflows.go`,
  `apps/orchestrator-go/internal/template/`, `apps/api/src/rule_template_types.rs`,
  `packages/*/schema`.

**How to test**
- `TestMigrationWorkflowChainContinuesOnFailure` (Go).
- Template with `onFailure: continue` and a failing middle step → later steps
  still run; row status `failed`.

**Not done / follow-ups**
- None for P0b.

**Locked decisions**
- `onFailure` is `stop` | `continue` only (no `retry`/`skip` row verbs here).

---

## P1 — Watched MinIO/S3 arrival

**What changed**
- `connectors.ListPrefixObjects` (MinIO list + glob + sort).
- Watch activities: `LoadConnector`, `ListPrefixObjects`, `StartWatchObjectJob`,
  `MarkJobRunning`, `AdvanceConnectorCursor`.
- `WatchPrefixWorkflow`: list → diff cursor → child `MigrationWorkflow` per plain file.
- Temporal schedules target `WatchPrefixWorkflow`.

**Key files**
- `apps/orchestrator-go/internal/workflows/watch.go`,
  `apps/orchestrator-go/internal/activities/watch.go`,
  `apps/orchestrator-go/internal/connectors/objectstore.go`,
  `apps/api/src/routes/schedules.rs`.

**How to test**
- `go test ./internal/workflows/ ./internal/connectors/`.
- Watched connector glob `*`; drop a CSV into the prefix; trigger schedule → a
  job starts and the cursor records the etag.

**Not done / follow-ups**
- Recreate pre-P1 schedules that still target `MigrationWorkflow` directly.

**Locked decisions**
- Arrival order: MinIO/S3 poll first → SFTP → events last.
- Cursor stored on the connector config (`cursor.seen` map key→etag).

---

## P2 — Archive + manifest batches

**What changed**
- Migration `0003_batches.sql`: `batches`, `batch_stages`, `jobs.batch_id` /
  `batch_stage_id`.
- Safe unpack (`internal/batch`): tar.gz/tgz/zip; rejects `..`, symlinks,
  absolute paths; caps via `MAX_ARCHIVE_*`.
- Manifest `manifest.json` `version:1`, ordered `stages[]` with `id`, `file`,
  `templateKey`, optional `onStageFailure`.
- Activities: `StartBatch`, `MarkBatchRunning`, `UnpackAndStageArchive`,
  `QuarantineArchive`, `MaterializeBatchStages`, `StartBatchStageJob`,
  `FinalizeBatchStage`, `FinalizeBatch`.
- `BatchWorkflow`: unpack → materialize → child `MigrationWorkflow` per stage;
  stop/continue; status succeeded/failed/partial/quarantined.
- `WatchPrefixWorkflow` now starts `BatchWorkflow` for archives.
- API `GET/POST /batches`, `GET /batches/:id`; UI `/batches`, `/batches/[id]`.

**Key files**
- `apps/api/migrations/0003_batches.sql`,
  `apps/orchestrator-go/internal/batch/`,
  `apps/orchestrator-go/internal/activities/batch.go`,
  `apps/orchestrator-go/internal/workflows/batch.go`,
  `apps/api/src/routes/batches.rs`,
  `apps/web/app/batches/`,
  `samples/demo_batch.tar.gz`.

**How to test**
- `go test ./internal/batch/`.
- Publish templates matching the sample manifest keys; upload
  `samples/demo_batch.tar.gz` to a watched prefix; trigger schedule → Batches
  UI shows batch + stage job links. A bad archive (no manifest) →
  `quarantined` under `…/failed/yyyy/mm/dd/`.

**Not done / follow-ups**
- None for P2.

**Locked decisions**
- Manifest lives inside the archive as `manifest.json`.
- One child job per stage; parent `BatchWorkflow` orchestrates.

---

## P3 — SFTP watch

**What changed**
- Connector kind `watched_sftp` (alias `sftp`): `host`, `port`, `username`,
  `path`/`prefix`, `glob`, `sort`, staging bucket/prefix, host-key trust.
  Credentials via existing encrypted-secrets pattern (password or PEM
  private_key + optional passphrase).
- `connectors.ListSftpObjects`: SSH/SFTP dial, recursive walk, mtime+size
  fingerprint (`SftpFingerprint`), shared `FilterAndSortObjects` glob/sort.
- Activities `ListSftpObjects`, `StageSftpObject` (download into MinIO staging
  so the rest of the pipeline reuses the minio `source_ref` path), shared
  `loadSftpConnector`. Credentials never enter workflow history.
- `WatchPrefixWorkflow` branches on connector kind; SFTP mirrors the MinIO
  behaviour (plain file → `MigrationWorkflow`, archive → `BatchWorkflow`,
  cursor advanced via `AdvanceConnectorCursor`).
- Host-key trust is explicit only (`host_key` or dev-only
  `insecure_ignore_host_key`). No classic FTP.

**Key files**
- `apps/orchestrator-go/internal/connectors/sftp.go`,
  `apps/orchestrator-go/internal/connectors/sftp_test.go`,
  `apps/orchestrator-go/internal/activities/watch.go`,
  `apps/orchestrator-go/internal/workflows/watch.go`,
  `apps/orchestrator-go/internal/workflows/watch_test.go`.

**How to test**
- `go test ./internal/connectors/ ./internal/workflows/`.
- Create a `watched_sftp` connector + secret; drop a CSV and a `.tar.gz` into
  the remote path; trigger the schedule → CSV starts a job, archive starts a
  batch, cursor records mtime:size fingerprints. Re-run with no changes →
  `Skipped` increments. Edit a remote file → re-discovered.

**Not done / follow-ups**
- Connector config UI by kind (SFTP fields in the connectors screen).
- Key-tab/agent auth beyond password + PEM private_key.

**Locked decisions**
- SFTP only; no classic FTP.
- Cursor token is mtime+size (SFTP has no ETag).
- Stage to MinIO and reuse the minio source_ref path (single ingest code path).

---

## P4 — Stage DAG (MVP)

**What changed**
- `ManifestStage.DependsOn []string` (optional stage ids).
- `Manifest.UsesDependsOn()` toggles DAG vs sequential mode.
- `Validate()` normalises/dedupes deps, rejects self-deps and unknown deps,
  and runs a Kahn topological sort to detect cycles (clear error listing the
  offending stages).
- DAG scheduling helpers: `StageNode`, `ReadyStageIDs`, `DepsBlocking`,
  `StageRunStatus`.
- `BatchWorkflow`: no `dependsOn` → `runStagesSequential` (P2, unchanged);
  any `dependsOn` → `runStagesDAG` runs ready stages as a parallel wave of
  child `MigrationWorkflow`s, waits, then schedules the next.
- `onStageFailure stop|continue` honoured per stage (override) and per batch
  (default): `stop` skips remaining pending stages; dependents of a
  failed/skipped stage are skipped with a recorded reason.
- `MaterializeBatchStages` persists `dependsOn` on each `batch_stages` row.

**Key files**
- `apps/orchestrator-go/internal/batch/manifest.go`,
  `apps/orchestrator-go/internal/batch/manifest_test.go`,
  `apps/orchestrator-go/internal/workflows/batch.go`,
  `apps/orchestrator-go/internal/activities/batch.go`,
  `apps/orchestrator-go/internal/workflows/watch_test.go`.

**How to test**
- `go test ./internal/batch/ ./internal/workflows/`.
- Manifest with `dependsOn: ["a","b"]` on stage `c` → `a` and `b` run, then
  `c` runs only after both succeed. Force a stage to fail with
  `onStageFailure: stop` → dependents skipped; with `continue` → siblings
  still run, batch status `partial`.

**Not done / follow-ups**
- **P4b (deferred):** S3 object-created push events (next arrival source after
  poll-based MinIO/S3 + SFTP). Documented as deferred; not implemented.

**Locked decisions**
- No `dependsOn` anywhere ⇒ keep ordered sequential (backward compatible).
- Any `dependsOn` ⇒ topological DAG with parallel ready waves.
- Unknown deps / cycles ⇒ manifest rejected (quarantine path), not silently
  ignored.

---

## P5 — Commercial 10-day phone-home trial MVP

**What changed**
- `LicenseDoc` gains `kind` (`trial` | `commercial`; defaults to
  `commercial` for legacy licenses) and optional `fingerprint` (install
  binding). `licensee`, `expires_at`, `features`, `max_seats`, `issued_at`
  unchanged. Legacy licenses without `kind` deserialize as `Commercial`.
- Install fingerprint (`security/fingerprint.rs`): SHA-256 over
  `/etc/machine-id` (or `/var/lib/dbus/machine-id`), hostname fallback. No
  cleartext PII leaves the host; `short_fingerprint` (first 12 hex) for UIs.
- Startup enforcement (`enforce_at_startup`, called from `main.rs`):
  `LICENSE_FILE` or durable store (`LICENSE_STORE_PATH`, default
  `/var/lib/migration/license.json`) → load + verify; `LICENSE_ENFORCE=true`
  with no valid local license → phone home to `LICENSE_SERVER_URL`
  (`POST /v1/trial/start`) keyed by fingerprint and persist the signed trial.
  Missing/expired under enforce does NOT abort boot — mutating APIs are
  blocked at runtime instead.
- In-repo license service (`bin/license_server.rs`): small Rust binary
  (`migration-license-server`) with a SQLite `trials` table keyed by
  fingerprint; signs 10-day trials (`TRIAL_DAYS`, default 10) with the same
  RSA key as `migration-admin license-sign`; same fingerprint reuses the
  original `expires_at`; expired → `402`; per-fingerprint rate limit.
- Runtime gating: `require_licensed()` on `POST /jobs` and
  `POST /schedules` → `402 license_required` under enforce when
  unlicensed/expired. Read APIs stay available.
- Settings UI license card: kind, licensee, days left, expiry, short install
  ID, features, max seats; warn badge at ≤3 days; activation hint.
- `GET /license` read-only status API (auth required).
- `docs/install.md` customer install notes + release checklist (never ship
  `dev_license_signing_key.pem` / any `*signing_key*.pem`).
- `migration-admin license-sign` produces vendor-signed commercial licenses.

**Key files**
- `apps/api/src/security/license.rs`,
  `apps/api/src/security/fingerprint.rs`,
  `apps/api/src/bin/license_server.rs`,
  `apps/api/src/bin/admin.rs`,
  `apps/api/src/routes/license.rs`,
  `apps/api/src/routes/jobs.rs`, `apps/api/src/routes/schedules.rs`,
  `apps/api/src/error.rs` (`LicenseRequired` → `402`),
  `apps/web/app/settings/page.tsx`,
  `infra/license/license_public_key.pem`,
  `infra/license/dev_license_signing_key.pem` (dev only — never shipped),
  `docs/install.md`.

**How to test**
- `cargo check -p migration-api --all-targets` (passes; 3 pre-existing
  warnings only). `cargo test -p migration-api` runs the license/fingerprint
  unit tests (sign/verify roundtrip, trial+fingerprint, legacy default,
  tamper/expiry/wrong-key, embedded key parse, days remaining).
- `npx tsc --noEmit` in `apps/web` (passes clean).
- Local smoke: run `migration-license-server` with the dev signing key, then
  run the API with `LICENSE_ENFORCE=true` + `LICENSE_SERVER_URL`; `GET /license`
  shows the trial; `POST /jobs` while unlicensed → `402`. Delete the license
  store and restart → same fingerprint reuses the original `expires_at`.

**Not done / follow-ups**
- **P5.1:** email-gated trials; commercial license bound to fingerprint; 24h
  heartbeat with 72h offline grace.
- **P5.2:** Stripe / payment portal; per-SKU feature-flag gating.
- Fully offline perpetual trial (intentionally out of scope — too easy to
  reset by wiping volumes).

**Locked decisions**
- Distribution: tagged GHCR images + compose/Helm pack.
- Trial: 10-day phone-home, bound to install fingerprint (anti-reinstall).
- License verify uses the **public** key embedded in the API image; the
  **private** signing key stays offline and is never shipped in customer
  images.
- Enforcement blocks mutating APIs only (job/schedule create); the platform
  stays readable when unlicensed so operators can diagnose.
