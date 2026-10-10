# Migration Platform — user handbook

This handbook is the operator’s guide: what the product does, how a job
actually runs, how to configure templates/connectors/schedules, and how to
run the sample files in `examples/handbook/`.

It describes **what the software does today**, not an aspirational
architecture. Limits are called out in [§14](#14-what-this-is-not).

---

## Contents

1. [What this product is](#1-what-this-product-is)
2. [Start the stack and log in](#2-start-the-stack-and-log-in)
3. [How a click becomes a migration](#3-how-a-click-becomes-a-migration)
4. [Screens and roles](#4-screens-and-roles)
5. [First job (CSV → HTTP)](#5-first-job-csv--http)
6. [JSON and XML sources](#6-json-and-xml-sources)
7. [Templates — the mapping contract](#7-templates--the-mapping-contract)
8. [Preprocess builtins and `$py`](#8-preprocess-builtins-and-py)
9. [Destinations, auth, retries, idempotency](#9-destinations-auth-retries-idempotency)
10. [Multi-step chains](#10-multi-step-chains)
11. [Jobs — watch, pause, retry, export](#11-jobs--watch-pause-retry-export)
12. [Connectors](#12-connectors)
13. [Schedules, watches, and batches](#13-schedules-watches-and-batches)
14. [License](#14-license)
15. [What this is not](#15-what-this-is-not)
16. [Troubleshooting](#16-troubleshooting)
17. [Reference](#17-reference)

Sample files live in [`examples/handbook/`](../examples/handbook/).

---

## 1. What this product is

A **self-hosted tool** that takes rows from a file (or a watched folder /
SFTP / Salesforce), transforms them, and posts them to **HTTP APIs** — for
example a hotel PMS or CRS.

Typical hospitality story:

1. Export guests from the old PMS as CSV.
2. Design a **template**: lowercase emails, map country codes, POST guest,
   then POST reservation using the first API’s response.
3. Start a **job**. The platform calls the target once per row, stores
   request/response, and lets you retry only the failures.

It is good for **pilots, demos, and small-to-medium cutovers**. It is not
yet reliable for a single job of millions of rows ([§15](#15-what-this-is-not)).

### Pieces you will see

| Piece | URL (local) | Job |
|-------|-------------|-----|
| Web UI | http://localhost:3000 | Design templates, start/watch jobs |
| API | http://localhost:8080 | Auth, templates, files, jobs |
| Temporal UI | http://localhost:8233 | Workflow history (engineers) |
| MinIO | http://localhost:9001 | Uploaded files (`minio` / `minio123`) |
| Grafana | http://localhost:3001 | Metrics (`admin` / `admin`) — full `make up` only |

The UI is a remote control. It does **not** migrate rows. The Rust API
accepts commands; Temporal + Go workers do the work; Python runs
preprocess / `$py` snippets.

```
You (browser)
    → Next.js UI (:3000)
        → Rust API (:8080)     auth, save template, start job
            → Postgres          jobs, templates, row outcomes
            → MinIO             uploaded CSV/JSON/XML
            → Temporal          durable workflow
                → Go worker     ingest → HTTP call → persist
                → Python worker preprocess / $py
            → Redis             live progress → UI stream
```

---

## 2. Start the stack and log in

### From this repo (developers)

```bash
cd /path/to/MigrationPlatform
make up          # first time: builds images (slow)
make seed        # demo org + admin + published template
```

### From the demo pack (no source build)

```bash
cd infra/demo-pack
./up.sh
```

### Login

| Field | Value |
|-------|--------|
| URL | http://localhost:3000/login |
| Email | `admin@example.com` |
| Password | `admin123` |

`make seed` creates that user. Change it before any shared host.

If **Start job** returns `402` / “license required”, open
**Settings → License** and start a trial, or set
`LICENSE_ENFORCE=false` for local development (compose default is off
unless you exported it).

### Other useful URLs

- API health: http://localhost:8080/healthz
- OpenAPI: http://localhost:8080/openapi.json
- Swagger: http://localhost:8080/swagger-ui

Stop the stack: `make down` (keeps data) or `make nuke` (wipes volumes).

---

## 3. How a click becomes a migration

This is the path you will use 90% of the time.

### Login

1. UI `POST /auth/login` with email/password.
2. API returns a JWT.
3. Browser stores it in `localStorage` as `migration.jwt`.
4. Every later call sends `Authorization: Bearer <token>`.
5. `GET /auth/me` tells the UI your role.

### Start job

1. **Jobs → Start a job**.
2. Choose a **published** template.
3. Upload a file. The browser posts multipart to the Next.js proxy
   `POST /api/files`, which forwards to `POST /files` on the API.
4. API writes the object to MinIO and returns a `source_ref`
   (`type=minio`, bucket, key, sha256, filename).
5. UI `POST /jobs` with `{ rule_template_id, source_ref }`.
6. API inserts a `jobs` row and starts Temporal `MigrationWorkflow`.
7. UI opens `/jobs/{id}`.

### While the job runs

For each batch of rows the Go worker:

1. **Ingest** — stream CSV/JSON/XML from MinIO.
2. **Preprocess** — Python sandbox (`builtin.lowercase`, `$py`, …).
3. **Render payload** — `$from` / `$fromResponse` / `$py`.
4. **CallEndpoint** — HTTP to the destination (retries 5xx/429).
5. **PersistOutcome** — upsert `job_rows` (and `job_row_steps` if multi-step).
6. **PublishProgress** — Redis → `GET /jobs/{id}/stream` (SSE) → live counters.

You can **Pause / Resume / Cancel** from the job page. Those hit
`POST /jobs/{id}/pause|resume|cancel` and become Temporal signals.

---

## 4. Screens and roles

| Screen | What you do |
|--------|-------------|
| **Overview** | Counts + recent jobs; “set up first migration” checklist |
| **Templates** | Create, edit mapping, publish a version |
| **Connectors** | Salesforce / S3 prefix / SFTP / custom HTTP credentials |
| **Schedules** | Cron + connector → recurring or watched-folder runs |
| **Jobs** | One-off runs: start, inspect rows, retry, download results |
| **Batches** | Multi-file archive runs (manifest + stages) |
| **Settings** | Optional AI mapping provider; license status / start trial |

### Roles

| Role | Typical rights |
|------|----------------|
| `admin` | Everything, including users and license trial |
| `editor` | Templates, connectors, start jobs |
| `operator` | Run/pause/retry jobs; not template design |
| `viewer` | Read-only |

The seed user is `admin`.

---

## 5. First job (CSV → HTTP)

Sample file: [`examples/handbook/guests.csv`](../examples/handbook/guests.csv)

```
email,first_name,last_name,country,loyalty_id,check_in,amount
alice@acme.com,Alice,Ng,usa,LOY-1001,2026-09-01,240
...
```

### Option A — use the seeded demo template

After `make seed` there is already a published template
**Demo customer upload** (`email`, `country`, `amount` → `httpbin.org/post`).

That template’s columns are **email, country, amount**. Either:

- upload a 3-column CSV, or
- use `guests.csv` only after you create the handbook template below
  (extra columns are ignored if not in the mapping, but the source schema
  should match what you designed).

### Option B — create the handbook template in the UI

1. **Templates → New**.
2. Name: `Handbook guest → HTTP echo`.
3. Source type: **CSV**, header on, delimiter `,`.
4. Fields: `email` (string), `first_name`, `last_name`, `country`,
   `loyalty_id`, `check_in` (string), `amount` (number).
5. Preprocess:
   - field `email`, function `builtin.lowercase`
   - field `country`, function `builtin.lowercase`
   - field `country`, function `builtin.lookup` with args:

     ```json
     {
       "table": { "usa": "US", "canada": "CA", "uk": "GB", "india": "IN" },
       "default": "XX"
     }
     ```

6. Mapping / payload:

   | Payload key | From |
   |-------------|------|
   | `email` | `email` |
   | `firstName` | `first_name` |
   | `lastName` | `last_name` |
   | `country` | `country` |
   | `loyaltyId` | `loyalty_id` |
   | `checkIn` | `check_in` |
   | `total` | `amount` |

7. Destination: method `POST`, URL `https://httpbin.org/post`
   (needs outbound internet from Docker). Optional:
   Idempotency header `Idempotency-Key`.
8. **Save**, then **Publish**. Unpublished templates cannot start jobs.

### Option C — create the template via API

```bash
TOKEN=$(curl -s -X POST http://localhost:8080/auth/login \
  -H 'content-type: application/json' \
  -d '{"email":"admin@example.com","password":"admin123"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')

TID=$(curl -s -X POST http://localhost:8080/rule-templates \
  -H "authorization: Bearer $TOKEN" \
  -H 'content-type: application/json' \
  --data-binary @examples/handbook/templates/guest-httpbin.json \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

curl -s -X POST "http://localhost:8080/rule-templates/$TID/publish" \
  -H "authorization: Bearer $TOKEN"
echo "published template id=$TID key=guest-httpbin"
```

`template_key` `guest-httpbin` is what a batch `manifest.json` would
resolve (see [§13](#13-schedules-watches-and-batches)).

### Run it

1. **Jobs → Start a job**.
2. Select the published template.
3. Upload `examples/handbook/guests.csv`.
4. **Start job**.
5. On the job page you should see 5 rows **succeeded**, each with a
   request JSON and httpbin’s echoed response.

What you should observe for Alice after preprocess:

```json
{
  "email": "alice@acme.com",
  "firstName": "Alice",
  "lastName": "Ng",
  "country": "US",
  "loyaltyId": "LOY-1001",
  "checkIn": "2026-09-01",
  "total": 240
}
```

`usa` became `US` because of `builtin.lookup` after lowercase.

### Partial failure drill

Upload [`guests-with-errors.csv`](../examples/handbook/guests-with-errors.csv)
against a template that POSTs to an API which rejects `not-an-email`.
httpbin accepts anything, so to *see* a row fail use a destination that
validates, or temporarily point the URL at `https://httpbin.org/status/400`
for a one-off test (every row will fail — useful to practise
**Retry failed**).

---

## 6. JSON and XML sources

### JSON

File: [`examples/handbook/guests.json`](../examples/handbook/guests.json)

- Must be a **top-level array** of objects. An object `{ "guests": [...] }`
  is rejected (`connector.json.expected_array`).
- Create a template with `source.type = json` and the same field names.
- **Jobs → Start a job**, upload the `.json` file.

### XML

File: [`examples/handbook/guests.xml`](../examples/handbook/guests.xml)

- Each `<record>...</record>` is one row (default `recordTag` is `record`).
- Child tags become fields (`email`, `first_name`, …).
- Attributes appear as `@attr`; element text as `$text` when mixed.
- Template `source.type = xml`.

Upload the `.xml` file the same way as CSV.

---

## 7. Templates — the mapping contract

A **rule template** is the versioned JSON document that says: source shape,
preprocess, payload mapping, destination(s), retry.

Only a **published** version can run. Editing creates a new draft; publish
again to cut over new jobs (in-flight jobs keep the version they started
with).

### Source

```json
"source": {
  "type": "csv",
  "schema": [
    { "name": "email", "type": "string" },
    { "name": "amount", "type": "number" }
  ],
  "options": { "header": true, "delimiter": "," }
}
```

`type` is one of: `csv`, `json`, `xml`, `salesforce`, `watchedPrefix`.

### Mapping expressions

| Expression | Meaning |
|------------|---------|
| `{ "$from": "email" }` | Value from the (preprocessed) row |
| `{ "$literal": "walk-in" }` | Constant |
| `{ "$py": "row['country'][:2]" }` | Python, row in scope |
| `{ "$fromResponse": "createGuest", "path": "$.id" }` | JSONPath into a **previous step** response |
| `"plain string"` | Literal (URLs/headers) |

Nested objects are allowed:

```json
"address": {
  "country": { "$from": "country" }
}
```

### Publish checklist

- Source fields match the file header / JSON keys.
- Every `$from` names a real field.
- Destination URL is reachable **from the orchestrator container**
  (not “from your laptop”). `localhost` inside Docker is the container
  itself. Use `host.docker.internal` (Desktop) or a public URL.
- Template is **published**.

---

## 8. Preprocess builtins and `$py`

Preprocess runs **before** mapping. Each step has `id`, `field`, `fn`.

**Always use the `builtin.` prefix.** `fn: "lowercase"` fails in the
worker (`unknown preprocess fn`).

| `fn` | What it does | Useful `args` |
|------|----------------|---------------|
| `builtin.lowercase` / `uppercase` | Case fold | — |
| `builtin.trim` | Strip whitespace | `chars` |
| `builtin.regex_replace` | Regex replace | `pattern`, `replacement`, `ignore_case` |
| `builtin.date_format` | Reformat dates | `input`, `output` (`%Y-%m-%d`) |
| `builtin.lookup` | Map codes | `table`, `default` |
| `builtin.concat` | Join strings | `values`, `separator`, `include_value` |
| `builtin.split` | Split to list | `separator` |
| `builtin.default` | Empty → fallback | `value` |
| `builtin.coerce` | Cast | `to`: `string` / `integer` / `number` / `boolean` |
| `builtin.null_if` | Sentinel → null | `equals` |
| `builtin.hash` | Hash | `algorithm`: md5/sha1/sha256/sha512 |
| `builtin.substring` | Slice | `start`, `end` |
| `python` | Custom | `code` (sandboxed) |

Python preprocess example:

```json
{
  "id": "upper_country",
  "field": "country",
  "fn": "python",
  "code": "result = value.upper() if value else ''"
}
```

The snippet sees `value` (this field) and `row`, and must assign `result`;
a bare `return` is a syntax error.

`$py` in the **payload** is evaluated later, with `row` in scope. Use it
when a builtin is not enough. Keep snippets small; this is not a place
for network I/O.

---

## 9. Destinations, auth, retries, idempotency

Destinations today are **HTTP only**.

```json
"destination": {
  "type": "http",
  "method": "POST",
  "url": "https://pms.example.com/v1/guests/{loyaltyId}",
  "pathParams": { "loyaltyId": { "$from": "loyalty_id" } },
  "queryParams": { "region": { "$from": "country" } },
  "headers": { "X-Property": { "$from": "property_code" } },
  "auth": { "type": "bearer", "secretRef": "pms_token" },
  "idempotencyHeader": "Idempotency-Key"
}
```

See [url-templating.md](url-templating.md) for path/query/header rules.
Missing path params **fail the row** (no retry). Missing query/headers
are omitted.

### Auth types

`bearer`, `basic`, `apiKey`, `oauth2Client` — secret material lives in
the encrypted secrets store, referenced by `secretRef`. Do not paste
production tokens into the destination URL.

### Retries

Template-level:

```json
"retry": {
  "maxAttempts": 5,
  "backoff": "exponential",
  "initialIntervalMs": 1000
}
```

The HTTP activity **retries** 429 and 5xx (Temporal). **4xx** is treated
as a deterministic row failure (no retry budget). After retries are
exhausted the row is `failed`; you can **Retry failed** later.

### Idempotency

The platform computes a stable key from job + row + step + payload and
sends it if you set `idempotencyHeader` (e.g. `Idempotency-Key`). That
only helps if the **target API** honours the header. Legacy PMS APIs
often do not — then design **lookup-then-create** (GET by loyalty id,
then POST or PUT) so retries do not create duplicates.

---

## 10. Multi-step chains

Hospitality pattern: create guest → create reservation with the guest id.

Ready-made template:
[`examples/handbook/templates/guest-two-step.json`](../examples/handbook/templates/guest-two-step.json)

POST it the same way as [§5 Option C](#option-c--create-the-template-via-api),
then publish, then run `guests.csv`.

The second step reads the first response:

```json
"guestEmail": {
  "$fromResponse": "createGuest",
  "path": "$.json.email"
}
```

httpbin echoes the body under `json`, so that path works for the sample.
A real PMS would be something like `$.data.id`.

### `onFailure`

Per step: `stop` (default) or `continue`.

- `stop` — do not run later steps for that row.
- `continue` — run later steps anyway; the **row** is still `failed` if
  any step failed.

On **Retry** (default): succeeded steps are skipped and their responses
are replayed into `$fromResponse`. **Retry from start** re-fires every
step (dangerous if step 1 is a non-idempotent create).

---

## 11. Jobs — watch, pause, retry, export

Job page (`/jobs/{id}`):

| Control | API | Effect |
|---------|-----|--------|
| Live counters | `GET /jobs/{id}/stream` | Redis SSE |
| Row table | `GET /jobs/{id}/rows` | Status, error, payload |
| Step trail | `GET /jobs/{id}/rows/{n}/steps` | Multi-step request/response |
| Pause / Resume | `POST .../pause` / `resume` | Temporal signals |
| Cancel | `POST .../cancel` | Cancel workflow |
| Retry failed | `POST .../retry-failed` | `RetryFailedRowsWorkflow` |
| Retry one row | `POST .../rows/{n}/retry` | Optional `{ "from_start": true }` |
| Download results | `GET .../results` | CSV of outcomes |
| Download failures | `GET .../results?failed=true` | Failed rows only |

Statuses: `pending`, `running`, `paused`, `succeeded`, `failed`,
`cancelled`. A job can finish with some failed rows (inspect the table;
totals show `failed`).

If Cancel says **workflow not found**, the Temporal run already ended
but the job row may still say `running` (known gap — see [§15](#15-what-this-is-not)).
Use a new job rather than hammering Cancel.

---

## 12. Connectors

Connectors store **how to reach a source**, not the mapping. Create them
under **Connectors**.

| Kind | When to use | Config highlights |
|------|-------------|-------------------|
| **watched_prefix** | New files land in MinIO/S3 | `bucket`, `prefix` (must start with `orgs/<org id>/`), `glob` (`*.csv` or `*`), `sort` |
| **watched_sftp** | Hotel drops files on SFTP | host, user, path, **staging bucket** (files land in `orgs/<org id>/sftp-landing/`), host key (or insecure flag in dev) |
| **salesforce** | CRM extract | instance URL, OAuth consumer, SOQL; Bulk API for large queries |
| **custom** | HTTP list APIs | base URL + pagination (limited; UI test is SFTP-only today) |

Secrets (SFTP password / private key, Salesforce refresh token) are
encrypted at rest. The **Test** button performs a real connection only
for SFTP today.

A connector is useless until a **Schedule** (or advanced `source_ref`)
points at it. One-off uploads do **not** need a connector.

### Advanced source on Start job

The “Advanced source reference” box sends a JSON `source_ref` that must name
a source your organization owns, e.g. a stored Salesforce connector:

```json
{
  "type": "connector",
  "connector_id": 3,
  "extra": { "soql": "SELECT Id, Email FROM Account" }
}
```

`extra` may only set `soql`, `cursorField` and `cursor`; the endpoint and
credentials come from the connector. `{"source_id": N}` from an upload also
works. File paths, URLs and inline secrets are refused with `400`.

---

## 13. Schedules, watches, and batches

**Schedules** = Temporal cron + a published template + usually a connector.

| Field | Example |
|-------|---------|
| Cron | `*/5 * * * *` (every 5 min), `0 2 * * *` (02:00) |
| Timezone | `Asia/Kolkata`, `UTC` |
| Overlap | `skip` (default), `buffer_one`, `buffer_all`, `cancel_other`, `allow_all` |
| Catch-up | seconds to backfill after downtime (default 3600) |

Watched-prefix / SFTP schedules do **not** start a single
`MigrationWorkflow`. They run `WatchPrefixWorkflow`: list objects, skip
already-seen keys, start **one job per new file**, or **one batch per
archive**.

### Batch archives

Folder: [`examples/handbook/batch/`](../examples/handbook/batch/)

```
manifest.json
properties.csv
room-types.csv
```

Zip **the files at the zip root** (not a parent folder):

```bash
cd examples/handbook/batch
zip handbook-batch.zip manifest.json properties.csv room-types.csv
```

`manifest.json`:

- `templateKey` must match a **published** `template_key`
  (`property-upsert`, `room-type-upsert` in the sample). Create those
  templates via API with `"template_key": "property-upsert"` — the UI
  otherwise generates `rt_<uuid>`.
- `dependsOn` → DAG: `room-types` waits for `properties`.
- No `dependsOn` anywhere → stages run in array order.
- `onStageFailure`: `stop` or `continue`.

Drop the zip into the watched prefix (MinIO console → bucket
`migration` → `orgs/<org id>/incoming/`) or configure a schedule on that
prefix with `glob: *` or `*.zip`. Watched prefixes outside your
organization's `orgs/<org id>/` namespace are refused.

Bad archives go to `{prefix}/failed/yyyy/mm/dd/` and show as
**quarantined** on **Batches**.

---

## 14. License

Commercial installs set `LICENSE_ENFORCE=true`. Then mutating work
(jobs, schedules, batches) needs a valid trial or commercial file.

| Item | Behaviour |
|------|-----------|
| Trial | 10 days, email-gated, **Settings → License** |
| Binding | Install fingerprint (from durable `installation-id`) |
| Heartbeat | Every 24h; 72h offline grace |
| Commercial | Vendor-signed file → `LICENSE_FILE` or store path; adopted within ~1 minute |

Local `make up` defaults enforcement **off** unless you exported
`LICENSE_ENFORCE`. Details: [install.md](install.md).

---

## 15. What this is not

Be accurate with stakeholders:

| Claim | Reality |
|-------|---------|
| Warehouse / ELT | Destinations are HTTP APIs, not Snowflake/BigQuery |
| Millions of rows in one job | Sequential per-row Temporal activities; history limits bite around low tens of thousands |
| Event-driven S3 | Watches **poll**; object-created events are deferred |
| Exactly-once into any PMS | Only as strong as the target’s idempotency / natural keys |
| High-availability license server | Single SQLite process |
| “Shard fan-out” / circuit breaker | Described in architecture notes; **not** how the worker runs today |

Realistic: demos, POCs, and carefully watched hotel cutovers of thousands
of rows. The bottleneck is usually the **target API rate limit**, not the
UI.

---

## 16. Troubleshooting

| Symptom | What to check |
|---------|----------------|
| Cannot log in | `make seed` ran; API http://localhost:8080/healthz |
| No published templates | Publish on the template page |
| `402 license required` | Settings → License, or unset `LICENSE_ENFORCE` |
| Job stuck `running`, worker log `unknown preprocess fn 'lowercase'` | Use `builtin.lowercase` |
| `400 source_ref must be a JSON object` | Send the object returned by upload, not a string |
| `400 source not found for this organization` | Upload the file again with your own login; uploads are per organization |
| Job failed with `SourceChanged` | The stored object changed after upload; upload it again and start a new job |
| Cancel 502 workflow not found | Workflow already finished; job row stale |
| All rows fail connection | URL must be reachable from **orchestrator** container; httpbin needs egress |
| `localhost` destination fails | That is the container, not your laptop |
| JSON ingest error `expected_array` | File must start with `[` |
| XML no rows | Use `<record>` or set `recordTag` |
| Upload CORS / huge file | UI already proxies via `/api/files` |
| SSE counters stale | Refresh; Redis down → counters update on next GET `/jobs/{id}` |

Orchestrator logs:

```bash
make logs s=orchestrator
make logs s=api
make logs s=transform-worker
```

Temporal UI (http://localhost:8233) shows workflow id `mig-…` and
activity failures.

---

## 17. Reference

### Sample files

| Path | Purpose |
|------|---------|
| [`examples/handbook/guests.csv`](../examples/handbook/guests.csv) | 5 guests |
| [`examples/handbook/guests.json`](../examples/handbook/guests.json) | Same, JSON array |
| [`examples/handbook/guests.xml`](../examples/handbook/guests.xml) | Same, XML records |
| [`examples/handbook/guests-with-errors.csv`](../examples/handbook/guests-with-errors.csv) | One bad row |
| [`examples/handbook/templates/guest-httpbin.json`](../examples/handbook/templates/guest-httpbin.json) | Create+publish via API |
| [`examples/handbook/templates/guest-two-step.json`](../examples/handbook/templates/guest-two-step.json) | Two-step chain |
| [`examples/handbook/batch/`](../examples/handbook/batch/) | Manifest + two CSVs |

### Related docs

| Doc | Audience |
|-----|----------|
| [install.md](install.md) | Trial / commercial deploy |
| [api.md](api.md) | REST surface |
| [connectors.md](connectors.md) | Connector internals |
| [scheduling.md](scheduling.md) | Cron, overlap, cursors |
| [url-templating.md](url-templating.md) | Path/query/header grammar |
| [architecture.md](architecture.md) | Component diagram (some items are design-intent) |
| [development.md](development.md) | Tests, Makefile |
| [operations.md](operations.md) | Backup / upgrade |
| [security.md](security.md) | JWT, secrets, sandbox |

### Default demo credentials

`admin@example.com` / `admin123` — local only.
