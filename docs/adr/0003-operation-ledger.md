# ADR 0003: Operation ledger, delivery policy and destination secrets

- Status: accepted (R04, MP-04, MP-10)
- Date: 2026-10-10

## Context

Each row runs a chain of HTTP steps against a customer destination. Before
this ADR:

- `CallEndpoint` sent the request and returned. A transport error (timeout,
  reset, lost response) was a retryable activity error, so Temporal re-sent
  the request up to five times. Against a destination without deduplication,
  commit-then-disconnect created the record again on every attempt.
- An idempotency header was sent when configured, and its presence was
  treated as proof that the destination deduplicates.
- `PersistStepOutcome` errors were ignored. A step could succeed while its
  evidence was lost. A later retry would then repeat the successful step,
  because retries skip only the steps recorded as succeeded.
- `PersistOutcome` incremented `attempts` on every delivery, so a Temporal
  redelivery after a lost acknowledgement counted one processing attempt
  twice.
- Single-row retry and retry-all used fresh workflow IDs, so two retries of
  the same row could run at once.
- `destination.auth.secretRef` was accepted by the schema but never applied.

## Decision

### 1. Operation, attempt, receipt

- **Operation**: the intent to apply one step to one row. Its identity is
  `(job_id, row_index, step_index)`; `operation_key` (the existing idempotency
  key, a hash of job, row, step and payload) and `payload_sha256` record what
  was intended. One row in `operations`.
- **Attempt**: one try at sending it. Each attempt is a row in
  `operation_attempts` with its own `attempt_id`
  (`{workflow run}:{activity id}:{activity attempt}`), its start and finish
  times, and an outcome: `responded`, `not_sent` or `unknown`.
- **Receipt**: the destination's response to a successful attempt, or the
  record a lookup found. It is stored on the operation and copied to
  `job_row_steps`/`job_rows` as before.

Operation states: `pending` → `sending` → `succeeded` | `failed` |
`unknown` | `unresolved`.

`CallEndpoint` opens an attempt before sending. That takes a lease on the
operation (`sending` until `lease_until`). It closes the attempt after the
response:

| Prior state when an attempt starts | Action |
|---|---|
| none, `pending`, `failed` | send |
| `succeeded`, same payload | return the stored receipt; nothing is sent |
| `sending`, lease live | another worker is sending: retryable "busy" error |
| `sending` with an expired lease, or `unknown` | outcome unknown: apply the delivery policy |
| `unresolved` | refuse unless the operator confirmed the effect was not applied |
| any state, different payload, and an effect may exist | `unresolved`: a changed payload is a new intent and needs an explicit decision |

The lease is the activity's start-to-close timeout plus a margin. Temporal
cancels the activity context at that deadline, so a holder past its lease is
no longer sending.

### 2. Outcome classification

- 2xx: `succeeded`.
- 4xx other than 408/429: `failed`. The destination refused the request,
  and the user can fix and retry it.
- 429, or a transport error before the request was written (DNS, refused
  connection, TLS): `not_sent`. Retrying is safe.
- 408, 5xx, or a transport error after the request was written: `unknown`.
  The destination may have applied it.

### 3. Delivery policy (`destination.delivery`)

```json
"delivery": {
  "mode": "idempotent" | "lookup" | "manual",
  "idempotencyRetentionHours": 24,
  "lookup": {"method": "GET", "url": "...", "queryParams": {...}, "foundPath": "$.data[0].id"}
}
```

What happens when the outcome is `unknown`:

- **`idempotent`** (verified deduplication): re-send with the same
  idempotency key, within `idempotencyRetentionHours` of the first attempt.
  This requires `idempotencyHeader`. The template author is asserting that
  the destination honours it. The platform does not infer this from the
  header. After the retention window the operation becomes `unresolved`.
- **`lookup`**: before re-sending, run the rendered lookup request. A 2xx
  response where `foundPath` matches a value is the receipt (`succeeded`,
  with nothing sent). A 404, or a 2xx with no match, re-sends. Any other
  response is a retryable error, because the lookup is a safe read.
- **`manual`**: stop with `unresolved`. The row is reported as unresolved
  and is not retried automatically.

Without `delivery`, GET, HEAD, PUT, DELETE and OPTIONS default to
`idempotent` (their HTTP semantics). POST and PATCH default to `manual`. This
default refuses to guess for creates.

### 4. Rows, retries and persistence errors

- A new row status `unresolved` stops the chain: dependent steps do not
  run. The job finishes `failed`, with `totals_json.unresolved`.
- Retry-all retries `failed` rows only. A single-row retry of an
  `unresolved` row returns 409 unless the request sets
  `confirm_not_applied: true`. That records the operator's decision in the
  audit log and lets the next attempt send.
- Retries of one job share the workflow ID `{job workflow}-retry`. A second
  retry while one runs, or a retry while the job itself is running, is 409.
- `PersistStepOutcome` and `PersistOutcome` errors propagate. A failed
  receipt write stops the row, and the operation stays `sending` until its
  lease expires. The next attempt then goes through the unknown-outcome path
  above, so the successful effect is neither lost nor silently repeated.
- `job_rows.last_attempt_id` and `job_row_steps.last_attempt_id` make the
  `attempts` counters idempotent under redelivery. The counter increments
  only when the processing attempt ID changes.

### 5. Destination secrets (`auth.secretRef`)

- `secrets.kind = 'destination'` marks secrets that templates may reference.
  Connector and LLM secrets (`kind = 'internal'`) can never be resolved by a
  template, so a template cannot send them to an arbitrary URL.
- `PUT /secrets/{name}` creates or rotates a value (admin only).
  `DELETE /secrets/{name}` revokes it (`revoked_at`). `GET /secrets` lists
  names and timestamps, never values. Each change is audited without the
  value.
- `CallEndpoint` resolves `secretRef` for the org that owns the job (a join
  through `jobs`), on every call, so rotation and revocation take effect on
  the next request. Supported schemes: `bearer`, `basic` (`user:password`),
  and `api_key` (`headerName`, default `X-API-Key`). Saving a template is
  refused when `auth` has no `secretRef` or uses `oauth2_client`, which is
  not implemented.
- A missing, revoked or other-org secret fails the step with the
  non-retryable `DestinationSecretUnavailable`, and nothing is sent. The
  decrypted value only ever lives in the outgoing request headers. It is
  never in activity input or output, workflow history, step evidence, logs
  or exports.

## Consequences

- Each HTTP call adds two small writes (open and close the attempt) and,
  when auth is used, one indexed read. At roughly 5 rows/s the per-call DB
  cost is dominated by the existing `PersistOutcome`.
- The `operations` and `operation_attempts` tables grow with rows × steps ×
  attempts. They are hash-partitioned by `job_id` like `job_rows`, so
  retention can follow job retention.
- POST destinations without a `delivery` policy now stop with `unresolved`
  on 5xx and lost responses instead of being retried. This is deliberate: it
  can no longer create duplicates. Templates that need automatic retries must
  declare `idempotent` (with retention) or `lookup`.
- Before this change `destination.auth` was accepted but never applied, so
  requests went out unauthenticated. Templates that declare `auth` now need
  a destination secret of that name, or their rows fail without sending.
- Schema: migration `0008` adds the `unresolved` row status (enum values
  need a non-transactional migration). Migration `0009` adds the ledger
  tables, `last_attempt_id` and the secret columns.
- No guarantee of exactly-once delivery is made for arbitrary HTTP
  destinations. The guarantee is: one effect when the declared policy holds,
  otherwise an explicit `unresolved` row.

## Alternatives considered

- **Always retry with the idempotency key.** Rejected: it assumes behaviour
  the platform cannot verify. That is the defect being fixed.
- **Separate lookup activity in the workflow.** Rejected: it would add
  workflow commands (and a versioning gate) for every ambiguous call, and the
  lease and ledger already live in `CallEndpoint`.
- **Store attempts as a JSON array on the operation.** Fewer rows, but every
  attempt would rewrite the whole array, and concurrent attempts could not be
  told apart.
