# Agent handoff — what was accomplished

**Last updated:** 2026-09-07  
**Status:** Feature-complete MVP + P5.1 licensing hardening (email-gated trials, fingerprint-bound commercial, 24h heartbeat / 72h grace, revocation). Production release remains conditional on the gates below.
**Plan file:** [`.cursor/plans/batch_arrival_features_a5363f5d.plan.md`](../.cursor/plans/batch_arrival_features_a5363f5d.plan.md)  
**Commercial (P5) plan:** [`.cursor/plans/trial_and_distribution_fe014b2f.plan.md`](../.cursor/plans/trial_and_distribution_fe014b2f.plan.md)  
**Per-phase changelog:** [`PHASE_CHANGELOG.md`](PHASE_CHANGELOG.md)

The P0a → P5 feature roadmap is implemented as an MVP. It is not an
unconditional production-readiness claim.

## Production release gates

- License enforcement must pass API and automatic Temporal-schedule tests with
  `LICENSE_ENFORCE=true`, including expiry while schedules already exist.
- Helm requires a shared `ReadWriteMany` license PVC and a stable installation
  Secret; Compose uses the `license-data` named volume.
- SFTP is poll-based and bounded (`max_file_bytes`, `max_list_entries`,
  `settle_seconds`). Operators must configure host-key pinning and egress policy.
- DAG `stop` prevents not-yet-started stages. Siblings already started in the
  same parallel wave continue and cannot be retroactively stopped.
- The bundled license server is an MVP single-instance SQLite service. It
  requires durable storage, bearer authentication, and a runtime-mounted
  signing key; HA and KMS integration are not implemented. Customers ride out a
  72h outage on offline grace; longer outages close the work gate everywhere.
- Heartbeat-required licenses depend on the customer host clock for the grace
  check (`now - issued_at`). Clock roll-back is not detected (documented gap).

---

## Product context (locked decisions)

| Decision | Choice |
|----------|--------|
| Manifest location | Inside archive as `manifest.json` |
| File hierarchy v1 | Ordered `stages[]`; optional **`dependsOn` DAG (P4)** |
| Batch orchestration | Parent `BatchWorkflow` + **one child job per stage** |
| Arrival order | MinIO/S3 poll first → SFTP → events last (events deferred) |
| Row-chain failure | `onFailure`: **`stop` \| `continue` only** |
| Stage failure | `onStageFailure`: **`stop` \| `continue`** |
| SFTP transport | SFTP only — **no classic FTP** |
| SFTP credentials | Existing secrets pattern (password or private_key JSON) |
| SFTP cursor | `mtime+size` fingerprint (no ETag on SFTP) |
| SFTP staging | Download to MinIO staging bucket, reuse minio source_ref path |
| Distribution | Tagged GHCR images + compose/Helm pack |
| Trial | **10-day phone-home trial** bound to install fingerprint |
| License verify | RSA PKCS#1 v1.5 / SHA-256; **public** key embedded in API image |
| License signing | Private key kept **offline**; never shipped in customer images |
| Delivery order | P0a → P0b → P1 → P2 → P3 → P4 → **P5** |

---

## P0a — Renamable steps (done)

- Designer always persists `steps[]` (no flat `mapping`/`destination` on save).
- Pencil rename on every step tab; blocks rename if Response refs point at old name.
- `"main"` remains **load-only** fallback for legacy rows without `steps`.

## P0b — `onFailure` + template `retry` (done)

- Schema field `steps[].onFailure`: `stop` \| `continue` (JSON Schema, Zod, Rust, Go, Python).
- Orchestrator continues chain when `continue`; row still `failed` if any step failed.
- `defaultActivityOptions(template.Retry)` wires maxAttempts / interval / backoff.
- Test: `TestMigrationWorkflowChainContinuesOnFailure`.

---

## P1 — Watched MinIO/S3 arrival (done)

**Problem:** Schedules started bare `MigrationWorkflow` without `jobId`/`sourceRef`. `watched_prefix` required pre-listed `objects`. No ListObjects. Cursor never written.

**Done:**

1. **`connectors.ListPrefixObjects`** — MinIO list + glob + sort
2. **Watch activities** — `LoadConnector`, `ListPrefixObjects`, `StartWatchObjectJob`, `MarkJobRunning`, `AdvanceConnectorCursor`
3. **`WatchPrefixWorkflow`** — list → diff cursor → child `MigrationWorkflow` per plain file
4. **Schedule wiring** — Temporal schedule targets `WatchPrefixWorkflow`

---

## P2 — Archive + manifest batches (done)

1. **Migration** [`0003_batches.sql`](../apps/api/migrations/0003_batches.sql) — `batches`, `batch_stages`, `jobs.batch_id` / `batch_stage_id`
2. **Safe unpack** [`internal/batch`](../apps/orchestrator-go/internal/batch/) — tar.gz/tgz/zip; reject `..`/symlinks/absolute paths; caps (256 MiB compressed / 1 GiB uncompressed / 1000 entries; env `MAX_ARCHIVE_*`)
3. **Manifest** — root `manifest.json` `version:1`, ordered `stages[]` with `id`, `file`, `templateKey`, optional `onStageFailure`
4. **Activities** [`activities/batch.go`](../apps/orchestrator-go/internal/activities/batch.go): `StartBatch`, `MarkBatchRunning`, `UnpackAndStageArchive`, `QuarantineArchive`, `MaterializeBatchStages`, `StartBatchStageJob`, `FinalizeBatchStage`, `FinalizeBatch`
5. **`BatchWorkflow`** — unpack → materialize → child `MigrationWorkflow` per stage; stop/continue; status succeeded/failed/partial/quarantined — [`workflows/batch.go`](../apps/orchestrator-go/internal/workflows/batch.go)
6. **`WatchPrefixWorkflow`** — archives start `BatchWorkflow` (no longer skipped)
7. **API** — `GET/POST /batches`, `GET /batches/:id` — [`routes/batches.rs`](../apps/api/src/routes/batches.rs)
8. **UI** — `/batches`, `/batches/[id]` (stages → job links); nav entry
9. **Sample** — [`samples/demo_batch.tar.gz`](../samples/demo_batch.tar.gz), [`samples/batch_package/`](../samples/batch_package/)

**How to smoke-test P2**

1. Rebuild/redeploy **api** + **orchestrator** (applies `0003_batches`).
2. Publish templates whose `template_key` match the manifest (`contacts-upsert`, `pets-upsert` in the sample — or edit the sample).
3. Watched connector glob `*` (or `*.tar.gz`).
4. Upload `samples/demo_batch.tar.gz` to the watched prefix.
5. Trigger schedule → Batches UI shows batch + two stages with job links.
6. Bad archive (no manifest) → status `quarantined`, object under `…/failed/yyyy/mm/dd/`.

## P3 — SFTP watch (done)

**Goal:** watch an SFTP drop directory the same way we watch a MinIO prefix —
list new files, glob + sort, diff against a cursor, and start a child
`MigrationWorkflow` per plain file or a `BatchWorkflow` per archive. Classic
FTP is intentionally **not** supported.

**Done:**

1. **Connector kind `watched_sftp`** (also accepts `sftp`) — config carries
   `host`, `port`, `username`, `path`/`prefix`, `glob`, `sort`, staging
   bucket/prefix, and host-key trust options. Credentials come through the
   existing encrypted-secrets pattern (password or PEM private key + optional
   passphrase). [`connectors/sftp.go`](../apps/orchestrator-go/internal/connectors/sftp.go)

2. **`connectors.ListSftpObjects`** — dials SSH/SFTP, walks the remote path
   recursively, builds an `ObjectInfo` per file with a stable
   **mtime+size fingerprint** (`SftpFingerprint`) since SFTP has no ETag, then
   reuses the shared `FilterAndSortObjects` glob/sort helper from the S3 path.

3. **Activities** [`activities/watch.go`](../apps/orchestrator-go/internal/activities/watch.go):
   - `ListSftpObjects` — loads connector + decrypted secret, lists remote files (credentials never enter workflow history).
   - `StageSftpObject` — downloads one remote file into the MinIO staging bucket (`StagingObjectKey`) so the rest of the pipeline reuses the existing minio `source_ref` path; preserves the SFTP fingerprint as the cursor token.
   - `loadSftpConnector` — shared connector+secret loader used by both activities.

4. **`WatchPrefixWorkflow`** branches on connector kind:
   - `watched_prefix` → `ListPrefixObjects` (MinIO/S3).
   - `watched_sftp` / `sftp` → `ListSftpObjects` + per-object `StageSftpObject`, then mirrors the MinIO behaviour (plain file → `MigrationWorkflow`, archive → `BatchWorkflow`, cursor advanced via `AdvanceConnectorCursor`).
   [`workflows/watch.go`](../apps/orchestrator-go/internal/workflows/watch.go)

5. **Host-key trust** — explicit only: `config.host_key` (authorized_keys or base64 wire format) pins the key; `insecure_ignore_host_key=true` is the dev-only escape hatch. No silent TOFU.

**How to smoke-test P3**

1. Create a `watched_sftp` connector with `host`, `username`, `path`, `glob`, and a secret (password or `private_key` JSON). Set `host_key` (or `insecure_ignore_host_key=true` for a local test server).
2. Drop a `.csv` and a `.tar.gz` into the remote path.
3. Trigger the bound schedule → WatchPrefixWorkflow lists, stages new files into `sftp-landing/<connectorId>/…`, starts a job for the CSV and a batch for the archive. Cursor `seen` records the mtime:size fingerprints.
4. Re-run the schedule with no changes → `Skipped` increments, no new children.
5. Edit a remote file (changes mtime/size) → re-discovered and re-processed.

Unit tests (no live SFTP server needed): `connectors/sftp_test.go` (config/auth/fingerprint/sort/staging-key/host-key) and `workflows/watch_test.go::TestWatchPrefixWorkflowSftpStagesAndStarts` (stubbed ListSftpObjects + StageSftpObject).

## P4 — Stage DAG (MVP) (done)

**Goal:** let a manifest declare optional `dependsOn: string[]` per stage so
stages can run as a topological DAG instead of strictly in array order, while
keeping the no-`dependsOn` path 100% backward compatible. S3 object-created
events are **deferred** (P4b) — see follow-ups.

**Done:**

1. **Manifest** [`internal/batch/manifest.go`](../apps/orchestrator-go/internal/batch/manifest.go):
   - `ManifestStage.DependsOn []string` (optional stage ids).
   - `Manifest.UsesDependsOn()` — true iff any stage declares `dependsOn`.
   - `Validate()` normalises/dedupes deps, rejects self-deps, rejects unknown deps, and runs a Kahn topological sort to detect cycles (cycle → clear error listing the offending stages).
   - DAG scheduling helpers: `StageNode`, `ReadyStageIDs` (deps all succeeded), `DepsBlocking` (a dep failed/skipped), `StageRunStatus`.

2. **`BatchWorkflow`** [`workflows/batch.go`](../apps/orchestrator-go/internal/workflows/batch.go):
   - No `dependsOn` anywhere → `runStagesSequential` (P2 behaviour, unchanged).
   - Any `dependsOn` → `runStagesDAG`: ready stages (deps all succeeded) run as a **parallel wave** of child `MigrationWorkflow`s; the workflow waits for the wave before scheduling the next.
   - `onStageFailure stop|continue` honoured per stage (override) and per batch (default): a `stop` failure skips all remaining pending stages; dependents of a failed/skipped stage are skipped with a clear reason.
   - Final batch status: `succeeded` / `partial` (mixed) / `failed`.

3. **Activities** — `MaterializeBatchStages` persists `dependsOn` on each `batch_stages` row (carried through `StagedFile` → `ResolvedStage`), so the DAG is available to the workflow after unpack.

**How to smoke-test P4**

1. Build an archive whose `manifest.json` gives one stage `dependsOn: ["a","b"]`.
2. Upload to a watched prefix and trigger the schedule.
3. Batches UI / `batch_stages` rows: `a` and `b` run, then `c` runs only after both succeed.
4. Force a stage to fail with `onStageFailure: stop` → dependents are skipped (status `failed`, reason recorded); siblings of the failed stage are also skipped under global stop.
5. Force a stage to fail with `onStageFailure: continue` → siblings still run; only the dependents of the failed stage are skipped → batch status `partial`.

Unit tests: `batch/manifest_test.go` (valid/unknown/cycle/self-deps, `ReadyStageIDs`/`DepsBlocking`) and `workflows/watch_test.go` (`TestBatchWorkflowDAGParallelRootsThenDependent`, `TestBatchWorkflowDAGStopSkipsDependents`, `TestBatchWorkflowDAGContinueAllowsSibling`).

## P5 — Commercial 10-day phone-home trial MVP (done)

**Goal:** ship as versioned GHCR images + install pack, with a 10-day
phone-home trial bound to a stable install fingerprint so wiping
volumes/images does not reset free use. Runtime enforcement blocks mutating
APIs when unlicensed/expired. Never ship the signing private key.

**Done:**

1. **`LicenseDoc`** [`security/license.rs`](../apps/api/src/security/license.rs):
   - Added `kind: LicenseKind` (`trial` | `commercial`, defaults to `commercial` for legacy licenses) and optional `fingerprint` (install binding). Kept `licensee`, `expires_at`, `features`, `max_seats`, `issued_at`.
   - Legacy licenses without `kind` deserialize as `Commercial` (backward compatible).

2. **Install fingerprint** [`security/fingerprint.rs`](../apps/api/src/security/fingerprint.rs):
   - SHA-256 over `/etc/machine-id` (or `/var/lib/dbus/machine-id`), falling back to hostname. **No cleartext PII** leaves the host — only the hex digest. `short_fingerprint` (first 12 hex) is what UIs/support tickets show.

3. **Startup enforcement** (`enforce_at_startup`, called from `main.rs`):
   - `LICENSE_FILE` or durable store (`LICENSE_STORE_PATH`, default `/var/lib/migration/license.json`) → load + verify signature.
   - `LICENSE_ENFORCE=true` with no valid local license → phone home to `LICENSE_SERVER_URL` (`POST /v1/trial/start`) keyed by fingerprint; persist the returned signed trial to the durable store.
   - Missing/expired under enforce does **not** abort boot — mutating APIs are blocked at runtime instead (so a broken license server never takes the whole platform down).

4. **In-repo license service** [`bin/license_server.rs`](../apps/api/src/bin/license_server.rs):
   - Small Rust binary (`migration-license-server`) with a SQLite `trials` table keyed by fingerprint (`started_at`, `expires_at`, product/version).
   - Signs 10-day trials (`TRIAL_DAYS`, default 10) with the **same** RSA signing key as `migration-admin license-sign`.
   - Same fingerprint → reuses the original `expires_at` (no fresh 10 days); expired fingerprint → `402 Payment Required`. Per-fingerprint rate limit (1 req / 5s). Runs locally for smoke tests.

5. **Runtime gating** — `require_licensed()` is called on the mutating endpoints: `POST /jobs` ([`routes/jobs.rs`](../apps/api/src/routes/jobs.rs)) and `POST /schedules` ([`routes/schedules.rs`](../apps/api/src/routes/schedules.rs)). Under enforce with no valid license → `402 license_required`. Read APIs stay available so operators can diagnose.

6. **Settings UI** [`web/app/settings/page.tsx`](../apps/web/app/settings/page.tsx):
   - License card shows kind (trial/commercial), licensee, days left, expiry, short install ID (fingerprint), features, max seats, and **`enforce`** (on/off) from `GET /license`.
   - Trial with ≤3 days left → warn badge. Unlicensed → development / unlicensed / expired badge + activation hint pointing at `LICENSE_FILE` / `LICENSE_SERVER_URL`.
   - Copy reflects runtime gate: with enforce on, unlicensed mutating APIs return `402 license_required`.

7. **License status API** [`routes/license.rs`](../apps/api/src/routes/license.rs): `GET /license` returns the read-only status above (auth required).

8. **Customer install notes** [`docs/install.md`](install.md): compose/Helm sketch, env reference, fingerprint explanation, local smoke (license server + API), and a release checklist that explicitly forbids shipping `dev_license_signing_key.pem` / any `*signing_key*.pem`.

9. **Admin signing** [`bin/admin.rs`](../apps/api/src/bin/admin.rs): `migration-admin license-sign --key <private.pem> --licensee <name> --expires <YYYY-MM-DD>` produces vendor-signed commercial licenses.

**How to smoke-test P5**

```bash
# Terminal 1 — license service (local dev key; DO NOT ship this key)
export LICENSE_SIGNING_KEY_PATH=infra/license/dev_license_signing_key.pem
export LICENSE_TRIAL_DB=./data/trials.db
export LICENSE_SERVER_BIND=127.0.0.1:8090
cargo run -p migration-api --bin migration-license-server

# Terminal 2 — API with enforce + phone-home
export LICENSE_ENFORCE=true
export LICENSE_SERVER_URL=http://127.0.0.1:8090
export LICENSE_STORE_PATH=./data/license.json
# …plus DATABASE_URL, MASTER_KEY, etc.
cargo run -p migration-api --bin migration-api
```

Verify:

```bash
curl -s http://127.0.0.1:8090/healthz
# After API is up and authenticated:
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/license | jq .
# Mutating call while unlicensed → 402 license_required:
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/jobs
```

Re-run the API after deleting `./data/license.json` — the same fingerprint
should **reuse** the original `expires_at` (not a fresh 10 days). Settings →
License shows kind, days left, and the short install ID.

## P5.1 — Email-gated trials, fingerprint binding, heartbeat + grace (done)

**Goal:** make the trial a real lead-capture funnel, let vendors bind and
revoke commercial licenses, and stop a wiped-and-restarted install from
running unlicensed forever, without breaking legacy/air-gapped license files.

**Done:**

1. **`LicenseDoc.requires_heartbeat: bool`** [`security/license.rs`](../apps/api/src/security/license.rs)
   — serde default `false`, omitted when false, so every existing signed
   license verifies byte-for-byte and stays heartbeat-free. Server-issued
   trials and `license-sign --require-heartbeat` set it.

2. **Process license state is now mutable** — `static STATE: RwLock<LicenseState>`
   replaces the `OnceLock`. `active_license()` returns a clone; `snapshot()`
   exposes doc + raw file + heartbeat bookkeeping. Pure
   `validity(doc, local_fp, now) -> Result<(), LicenseProblem>` drives both
   `is_licensed()` and `require_licensed()`; problems are
   `expired | fingerprint_mismatch | heartbeat_grace_expired | missing_issued_at | revoked`
   and are surfaced in the `402` message and `license_denied_total{reason}`.

3. **Heartbeat client** — `heartbeat_once(server, fp, raw, key) -> Refreshed | Denied | Unavailable`
   (pure; wiremock-tested). `heartbeat_tick()` runs every 60s from a task in
   `main.rs`: (a) re-reads `LICENSE_STORE_PATH` if its mtime/len changed and
   adopts a valid newer document (multi-replica coherence, operator drop-in
   without restart); (b) heartbeats when `now - issued_at ≥ 24h`
   (`LICENSE_HEARTBEAT_INTERVAL_SECS` override), hourly retry after failure.
   Grace = **72h from the signed `issued_at`** — the server re-signs the doc
   with a fresh `issued_at` on every successful heartbeat, so there is no
   editable "last seen" file. Only `402`/`403` are authoritative denials;
   `401`, 5xx and transport errors keep the license inside grace.

4. **Email-gated trial start** — `POST /v1/trial/start` requires `email`
   (`validate_trial_email`, shared client/server); `licensee` = email; the
   original lead email is kept on re-activation. Client: `LICENSE_TRIAL_EMAIL`
   at boot, or admin-only **`POST /license/trial {email}`** (audited as
   `license.trial_start`) + Settings → License form. `409` if a valid
   commercial license is already active.

5. **License server as a lib module** [`license_server.rs`](../apps/api/src/license_server.rs)
   — `ServerConfig`, `AppState`, `open_db` (idempotent schema + `ensure_column`
   migrations), `build_router`, `revoke` / `unrevoke`. New
   `POST /v1/heartbeat`: verify signature with the public half of the signing
   key, fingerprint binding, expiry (`402`), `revocations` table (`403`,
   `licensee` or `*`), trial row authoritative for expiry, bump
   `heartbeat_count` / `last_heartbeat_at`, re-sign with `issued_at = now`.
   Errors use the API's `{error:{code,message}}` envelope. Bearer compare is
   constant-time. The bin is thin and adds
   `migration-license-server revoke|unrevoke --fingerprint …`.

6. **Admin CLI** — `license-sign --require-heartbeat` (needs `--fingerprint`),
   fingerprint hex validation, past-expiry rejection; `license-verify` prints
   `requires_heartbeat`.

7. **`GET /license`** — adds `heartbeat {required, status ∈ not_required|ok|degraded|grace_expired|revoked, last_attested_at, grace_until, last_error, server_configured}`,
   `mode` values `revoked | grace_expired | fingerprint_mismatch`, `problem`,
   `trial_available`, and `install_id_full` (admins only — vendors need it to
   bind commercial licenses).

8. **Settings UI** — heartbeat rows, red banners for revoked / grace expired,
   admin "Start a 10-day trial" email form, full install ID with copy button.

9. **Observability** — gauges `license_valid`,
   `license_heartbeat_grace_seconds_remaining`; counter
   `license_heartbeat_total{result=ok|denied|unavailable|unconfigured}`;
   alerts `LicenseInvalid`, `LicenseHeartbeatFailing`,
   `LicenseGraceExpiringSoon` in `infra/prometheus-alerts.yml`.

10. **Infra/docs** — `LICENSE_TRIAL_EMAIL` in `.env.example`, compose, Helm
    (`license.trialEmail`); `docs/install.md` (trial rules, heartbeat table,
    commercial binding flow, revocation, smoke), `docs/api.md`.

11. **Repo fix (pre-existing)** — `.gitignore` had a bare `bin/` rule that
    swallowed `apps/api/src/bin/` (so `migration-admin` and
    `migration-license-server` sources were never committed) and `*.pem`
    hid `infra/license/license_public_key.pem`, which `license.rs`
    `include_str!`s. A clean clone could not build the API crate. `bin/` is
    now scoped to `/bin/` + `apps/orchestrator-go/bin/`, and only the public
    verify key is un-ignored; `dev_license_signing_key.pem` stays ignored
    (verified with `git check-ignore`). **Commit the three newly visible
    files** with this change.

**How to smoke-test P5.1** — see "Local smoke (developers)" in
[`install.md`](install.md). Verified 2026-09-07 against the built binaries:
401 wrong token → 400 `email_required` → trial issued with
`requires_heartbeat` → `license-verify` OK → re-activation `reused=true` with
original email → heartbeat advances `issued_at` → `revoke` → heartbeat `403`
→ `unrevoke` → `200` → tampered file `400` → commercial
`--require-heartbeat` heartbeat `200`.

Unit tests: `security::license::tests` (validity/grace/fingerprint, due
schedule + retry back-off, revocation precedence, email validation, wiremock
heartbeat 200/403/401/wrong-key/conn-refused) and `license_server::tests`
(schema migration idempotence, config validation, auth + email gate, reuse,
per-fingerprint rate limit, expired trial, heartbeat refresh + counters,
forged / mismatched / expired presentations, revoke + unrevoke + wildcard).

**Full-stack verification (2026-09-07, compose, `LICENSE_ENFORCE=true`,
`LICENSE_HEARTBEAT_INTERVAL_SECS=120`, license server as a container on the
compose network)** — all passed after the fixes in `e6ef0a4`: unlicensed →
`POST /jobs` 402 → `POST /license/trial` → job succeeds 4/4 rows (orchestrator
`RequireLicensed` activity included) → heartbeat re-attests and slides
`grace_until` → `revoke` → next tick `mode=revoked`, 402 → `unrevoke` +
restart → recovered → server stopped → `heartbeat.status=degraded`, still
licensed, jobs allowed → wrong-fingerprint commercial file rejected with log →
correct commercial file adopted in <60s and re-attested → Settings UI shows
trial/commercial/unlicensed states and the Start-trial form works. Prometheus
`migration-api` target UP, `license_*` series scraped. Bugs found and fixed
in that commit: post-success heartbeat back-off (no second heartbeat ever
sent with a short interval), no probing while revoked, `LICENSE_ENFORCE=1`
silently off, `/metrics` on the public port with `METRICS_BIND` unbound,
stale seed preprocess fn name.

Running the stack on a **plain Linux Docker engine** (not Desktop): mode-600
secrets in `infra/secrets` are unreadable by the uid-10001 container (Desktop
masks this) — `chmod 644` the local dev key; the host firewall may drop
bridge→host traffic, so run the license server as a container
(`docker build --target license-server`) rather than on the host.

---

## Not done (follow-ups)

| Phase | Work |
|-------|------|
| **P4b** | S3 object-created events (push instead of poll) — explicitly deferred. Documented as the next arrival source after poll-based MinIO/S3 + SFTP. |
| **P5.1b** | E-mail verification of the trial contact (needs an outbound mail provider); clock roll-back detection for heartbeat grace; `heartbeat_tick()` integration test against a live store file (currently covered by pure-function + wiremock tests and the manual smoke). |
| **P5.2** | Stripe / payment portal; per-SKU feature-flag gating. |
| — | Local-folder source connector. |
| — | Remote connector test for non-SFTP kinds (`watched_prefix`, Salesforce, custom) — API still rejects; UI messaging is accurate. |
| — | `POST /jobs` accepts `source_ref` as a JSON *string*; the Go workflow then fails to decode it and the job row stays `running` forever (no failure propagated back). Validate the shape at the API and mark the job `failed` when the workflow fails before its first activity. Found during the 2026-09-07 full-stack run. |
| — | `heartbeat_due_schedule_and_retry_backoff` reads `LICENSE_HEARTBEAT_INTERVAL_SECS` from the process env; it fails if a developer's shell exports it. Inject the interval instead of reading env inside `heartbeat_due`. |
| — | Recreate pre-P1 schedules that still target `MigrationWorkflow` directly in Temporal (recreate after deploy). |

**Note:** Existing schedules created before P1 still target `MigrationWorkflow`
in Temporal until recreated. Recreate schedules after deploy.

---

## Key verification

```bash
cd apps/orchestrator-go && go test ./internal/workflows/ ./internal/batch/ ./internal/template/ ./internal/connectors/ -count=1
cd apps/api && cargo check -p migration-api --all-targets
cd apps/web && npx tsc --noEmit
```

Integration pass (2026-08-07): `cargo check -p migration-api --all-targets`
and `cd apps/web && npx tsc --noEmit` both clean. `go` is not installed here,
so orchestrator build/tests were not re-run — run them before release.

P5.1 pass (2026-09-07): `cargo test -p migration-api --lib --bins` 77 passed,
`cargo clippy -p migration-api --all-targets` clean, `npx tsc --noEmit` clean,
manual license-server smoke as described above. Go untouched by P5.1.
