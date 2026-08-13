# Product backlog

Living list of product work that is **not** in the active implementation queue.
Prioritize against paying-customer value; see also the GTM-oriented roadmap in
[`docs/research/real-world-gtm-and-enterprise-value.md`](research/real-world-gtm-and-enterprise-value.md).

**Status legend:** `proposed` · `accepted` · `in progress` · `done` · `wontfix`

---

## Items

### BL-001 — Results export for BA analysis (CSV / Excel)

| Field | Value |
|-------|--------|
| **Status** | `done` |
| **Priority** | P0 (analyst / cutover evidence) |
| **Added** | 2026-08-07 |
| **Related** | Failed-row DLQ export (GTM P0); Jobs UI; `job_rows` / step trail |

**Problem**

Jobs can ingest CSV (or other sources), call HTTP APIs, and persist per-row /
per-step responses, but business analysts cannot download a flat file of
**source columns + extracted response fields** for offline analysis in Excel
or BI tools. Destination types today are HTTP-only.

**Outcome**

After a job (or batch stage job) completes, a BA can download:

- **CSV** (P0) of projected columns
- **XLSX** (P1, optional) of the same projection

Columns include selected source fields, JSONPath extracts from one or more
step responses (`$fromResponse`-style paths), plus row status / error.

**Scope — P0**

- Column projector on the rule template or job run (source field and/or
  response JSONPath → output column name)
- Write results object to MinIO (e.g. `jobs/{jobId}/results.csv`) when the
  job finalizes
- Jobs UI: **Download results**
- Failed-rows-only variant (overlaps DLQ export)
- API: authenticated download URL or streaming export endpoint

**Out of scope (initially)**

- Live Excel streaming while the job is running
- Arbitrary multi-sheet workbooks
- Fan-out “one response array → many result rows” (separate epic)

**Why**

Closes the “CSV in → API → spreadsheet out” BA workflow without forcing SQL
access to Postgres. Strengthens cutover evidence and post-run analysis.

**Depends on**

- Existing `job_rows` / step-outcome persistence
- MinIO (or S3) object store already used for sources

**Notes / acceptance sketch**

1. Template or job defines projection: e.g. `family` ←
   `fetchPatient:$.name[?(@.use=='official')].family`, `mrn` ←
   `fetchPatient:$.identifier[?(@.system=='…mrn')].value`, plus `email` ←
   source column.
2. On job success/partial/fail finalize, results file is available.
3. Viewer+ role can download; file retained per job retention policy.
4. Empty projection still allows export of core ops columns
   (`row_index`, `status`, `error`).

---

### BL-002 — Jobs UI: show source file / batch context

| Field | Value |
|-------|--------|
| **Status** | `done` |
| **Priority** | P0 (operability) |
| **Added** | 2026-08-07 |
| **Related** | Batches UI; `jobs.source_ref`; `jobs.batch_id` (DB column exists) |

**Problem**

Job detail (e.g. Job #14) shows only `Template #N · Started …`. Operators cannot
tell which archive (`.tar.gz` / `.zip`), staged CSV, bucket/key, etag, size, or
parent **batch** produced the job. For batch stages the job’s `source_ref` is
usually the *extracted stage file* under `batches/{id}/stages/…`, while the
package archive lives on the **batch** — neither is surfaced in the Jobs UI.
The GET `/jobs/{id}` payload already includes `source_ref`, but the web client
ignores it; `batch_id` exists on `jobs` in Postgres but is not selected into
the API `JobRow` model.

**Outcome**

Job header (and jobs list) shows:

- Source summary: type, bucket/key or upload filename, size, etag when present
- If `batch_id` set: link to Batch #N, stage key, and the parent archive
  `source_ref` (the `.tar.gz`)
- Clear distinction: “Package: demo_batch.tar.gz” vs “Stage file: contacts.csv”

**Acceptance sketch**

1. Open a batch-stage job → see package name + stage file + link to batch.
2. Open a plain CSV upload job → see uploaded object key/filename.
3. API returns `batch_id` (and optionally denormalized batch/stage labels).

---

### BL-003 — Jobs UI: errors and response bodies for single-call templates

| Field | Value |
|-------|--------|
| **Status** | `done` |
| **Priority** | P0 (operability) |
| **Added** | 2026-08-07 |
| **Related** | `job_rows.last_error`, `payload_json`, `response_json`; multi-step step trail |

**Problem**

Failed rows only show a truncated `last_error` string in the list (when the UI
receives it). Multi-step templates get a rich step trail (HTTP status, request,
response JSON). **Single-call templates** show “No per-step records”, even
though the API already returns `payload_json` / `response_json` /
`last_error` on `job_rows` — the Jobs page type omits those fields and never
renders them. Operators cannot see HTTP status codes or response bodies without
SQL.

**Outcome**

- Row list: status, attempts, **error summary** (and HTTP status if known)
- Row detail for single-call: Request / Response panels (same UX as step trail)
  plus `last_error`
- Multi-step: keep step trail; ensure failed steps always show `response_status`
  and body when persisted

**Acceptance sketch**

1. Force a 4xx/5xx destination → open failed row → see HTTP code + body + error.
2. Succeeded single-call row → can still inspect last request/response.
3. List view shows non-truncated error on hover/expand.

---

### BL-004 — Edit payload (or source row) then retry

| Field | Value |
|-------|--------|
| **Status** | `proposed` |
| **Priority** | P1 (ops recovery) |
| **Added** | 2026-08-07 |
| **Related** | `POST /jobs/{id}/rows/{row}/retry`; BL-001 export |

**Problem**

Retry today re-runs the **same** stored source row / rendered chain
(`from_start` optional). There is **no** UI or API to correct a bad field
(typo in email, wrong ID) and retry with the edited payload. BAs and operators
must fix the source file and re-run the whole job.

**Outcome**

On a failed (and optionally succeeded) row:

1. View editable **source row** JSON and/or last **request payload**
2. Save a one-off override (audited)
3. Retry using the override (clear or bump idempotency key policy documented)

**Out of scope (initially)**

- Editing mid-chain `$fromResponse` history by hand
- Bulk “fix-up spreadsheet → retry” (may fold into BL-001 later)

**Acceptance sketch**

1. Failed row → Edit → change one field → Retry → new attempt uses override.
2. Audit log records who changed what.
3. Idempotency behavior is explicit in UI copy (new key vs reuse).

---

### BL-005 — Field-level encrypt / tokenize / decrypt + key management

| Field | Value |
|-------|--------|
| **Status** | `proposed` |
| **Priority** | P1 (regulated / PII pipelines) |
| **Added** | 2026-08-07 |
| **Related** | Existing AES-GCM `secrets` + `AES_MASTER_KEY` ([security.md](security.md)); preprocess / `$fromResponse`; destination `secretRef` |

**Problem**

Today secrets cover **connector/auth credentials** (encrypt at rest with the
platform master key). There is **no** first-class way to:

1. **Encrypt or tokenize a source field** before it is placed in an outbound
   payload (e.g. Aadhaar → token for a downstream API)
2. **Decrypt a value from a prior HTTP response** before using it in the next
   step’s URL/payload (e.g. encrypted account id from system A → plaintext for
   system B)
3. Let customers manage **data keys** separately from the platform master key
   (per-org keys, KMS/HSM, rotation, dual-control)

Putting crypto in `$py` is unsafe and impractical (no key access in sandbox;
keys must not enter Temporal history or job_rows plaintext carelessly).

**Outcome**

Declarative field transforms bound to a **named crypto key** (org-scoped),
executed only in a trusted activity (API or orchestrator), never in the
Python sandbox:

```json
{ "$crypto": { "op": "encrypt", "keyRef": "pii-field-key", "alg": "aes-gcm", "$from": "aadhaar" } }
{ "$crypto": { "op": "tokenize", "keyRef": "mrn-vault", "format": "token", "$from": "mrn" } }
{ "$crypto": { "op": "decrypt", "keyRef": "partner-wrap", "$fromResponse": "stepA", "path": "$.encryptedId" } }
```

Key management:

- **P0:** Org-managed data keys stored like today’s `secrets` table (AES-GCM
  wrapped by `AES_MASTER_KEY`), with rotate + audit; keys never returned to UI
- **P1:** Optional external KMS (AWS KMS / GCP KMS / Azure Key Vault /
  HashiCorp Vault transit) via `keyRef` provider config; platform holds only
  KMS references / IAM
- Clear policy: plaintext decrypted values live only in memory for the
  CallEndpoint activity; redaction in logs/audit; optional “do not persist
  decrypted response” flag on steps

**Out of scope (initially)**

- Format-preserving encryption productization beyond a documented alg set
- Client-side (browser) encryption
- Homomorphic / searchable encryption

**Acceptance sketch**

1. Template encrypts a CSV column into payload; ciphertext in request; plaintext
   never in `payload_json` dump shown to viewers (or redacted).
2. Step 2 decrypts `$.cipher` from step 1 response and uses plaintext in path
   param; Temporal inputs do not contain the DEK.
3. Admin can create/rotate `keyRef`; old ciphertexts decrypt via key version.
4. Docs state threat model vs platform `AES_MASTER_KEY` (auth secrets) vs
   field DEKs (data plane).

---

### BL-006 — Notify a person when a job (or batch) fails

| Field | Value |
|-------|--------|
| **Status** | `proposed` |
| **Priority** | P0 (ops) |
| **Added** | 2026-08-07 |
| **Related** | Prometheus `JobFailedRowsRatio` ([prometheus-alerts.yml](../infra/prometheus-alerts.yml)); audit log; SSE job stream |

**Problem**

Failure is visible in the Jobs UI and (if Grafana/Alertmanager is wired)
via infra metrics alerts. There is **no product feature** to notify a named
person or channel when *this* job/batch/schedule fails (email, Slack, Teams,
webhook, PagerDuty).

**Outcome**

Configurable notification targets (org or schedule/job scoped):

- On job status → `failed` / `cancelled`, or failed-row ratio above threshold
- On batch → `failed` / `quarantined` / `partial`
- Channels: webhook (P0), email/Slack (P1)
- Payload includes job/batch id, source summary, error excerpt, deep link

**Notes**

Ops can approximate today with Alertmanager → Slack on `JobFailedRowsRatio`,
but that is cluster-wide metrics, not “email Alice when schedule Nightly fails.”

---

### BL-007 — Intake priority queues for watched buckets / SFTP

| Field | Value |
|-------|--------|
| **Status** | `proposed` |
| **Priority** | P1 (multi-tenant / cutover) |
| **Added** | 2026-08-07 |
| **Related** | Watched prefix `sort`: `lexical` \| `mtime` only ([connectors](connectors.md)) |

**Problem**

Under load, WatchPrefix lists new objects and starts children in list order
(lexical or mtime). There is **no** priority class so “VIP / SLA files” are
always drained before best-effort drops when workers are saturated.

**Outcome**

- Priority bands (e.g. `P0` / `P1` / `P2`) derived from prefix, filename
  pattern, object metadata/tags, or connector config lists
- Scheduler / worker admission: always start highest-priority pending
  packages/files first; fair-share optional so P2 is not starved forever
- Visible in UI: queued with priority + position

**Out of scope (initially)**

- Cross-org global priority preemption of in-flight jobs (cancel running
  low priority to free capacity) — separate, dangerous epic

---

### BL-008 — Processing ETA + per-file / per-class SLA

| Field | Value |
|-------|--------|
| **Status** | `proposed` |
| **Priority** | P1 (program management) |
| **Added** | 2026-08-07 |
| **Related** | `PublishProgress.ETASeconds` field exists but is not a user-facing SLA product; progress SSE |

**Problem**

Users cannot set or see “this file must finish by 02:00” or get a reliable
completion estimate. Progress SSE exposes counters; an `etaSeconds` field
exists on the progress payload shape but there is no SLA policy, breach
alert, or ETA grounded in measured rows/sec + queue depth.

**Outcome**

- **ETA (P0):** job UI shows estimated completion from rolling rows/sec ×
  remaining rows (+ optional queue wait for watched intake)
- **SLA (P1):** attach policy to schedule, connector prefix, or object tag
  (e.g. max age from object `LastModified` → job finished, or wall-clock
  deadline). Breach → notification (BL-006) + badge on job/batch
- Document uncertainty (destination latency, retries, concurrency)

**Acceptance sketch**

1. Running job with 10k rows shows ETA that updates as throughput changes.
2. Object under `incoming/sla/` with 30m SLA; if not finished in 30m after
   discovery → notify + `sla_breached` flag.
3. ETA never claims false precision when sample size is tiny.

---

### BL-009 — First-class agent / MCP surface

| Field | Value |
|-------|--------|
| **Status** | `proposed` |
| **Priority** | P1 (ecosystem) |
| **Added** | 2026-08-07 |
| **Related** | REST + `/openapi.json`; `POST /mapping-suggestions`; dry-run; no MCP server in-repo today |

**Problem**

External agents can already drive the platform via JWT + REST, but there is
**no MCP server**, no agent-oriented tool catalog, and no service account /
scoped API keys. NL template design exists only as `POST /mapping-suggestions`
(plus UI “Suggest mapping”), not as an always-on chat agent that runs jobs.

**Outcome**

- Optional MCP server exposing tools: login/token, list/create/publish
  templates, dry-run, start/pause job, list batches, trigger schedule,
  mapping-suggestions
- Machine identity (service account or long-lived API key with role)
- Documented “agent playbook” (idempotent publish, dry-run before job)
- Keep LLM output schema-validated (never execute unvalidated model JSON)

---

## Index

| ID | Title | Priority | Status |
|----|-------|----------|--------|
| BL-001 | Results export for BA analysis (CSV / Excel) | P0 | done |
| BL-002 | Jobs UI: show source file / batch context | P0 | done |
| BL-003 | Jobs UI: errors and response bodies for single-call templates | P0 | done |
| BL-004 | Edit payload (or source row) then retry | P1 | proposed |
| BL-005 | Field-level encrypt / tokenize / decrypt + key management | P1 | proposed |
| BL-006 | Notify a person when a job (or batch) fails | P0 | proposed |
| BL-007 | Intake priority queues for watched buckets / SFTP | P1 | proposed |
| BL-008 | Processing ETA + per-file / per-class SLA | P1 | proposed |
| BL-009 | First-class agent / MCP surface | P1 | proposed |
