# Integrity harness

Deterministic, local-only checks that a migration actually reached the
destination exactly as the platform claims. Test-only: nothing here may be
deployed, pointed at real services, or fed real customer data.

| Piece | Path | Purpose |
|---|---|---|
| Destination fixture | `destination/` (Go, stdlib only) | HTTP destination with a durable receipt ledger and deterministic fault switches |
| Synthetic sources | `data/` | Immutable billing CSVs for two tenants (`org-a`, `org-b`), pinned by `SHA256SUMS` |
| Runner | `harness.py` (Python stdlib) | Drives the real API, then reconciles source ↔ platform rows ↔ destination receipts |
| Compose overlay | `../../infra/docker-compose.harness.yml` | Adds `destination-fixture` to the stack network |

## Entry point

```bash
make harness-up      # stack + destination fixture (builds images)
make harness-setup   # idempotent: org A (seed admin) + org B (harness-b@example.test)
make harness         # run every scenario; non-zero exit on any failure
make harness-unit    # fixture + reconciler unit tests, no stack required
```

Single scenarios: `python3 tests/harness/harness.py run forced-failure`.
`python3 tests/harness/harness.py list` prints the catalogue. Evidence for each
scenario (job, checks, per-record dispositions, receipt counts, git SHA) is
written to `tests/harness/out/<scenario>.json` (git-ignored).

Prerequisites: Docker with Compose, Python ≥ 3.10, and a license state that
permits job creation (the local dev stack). If Docker, Temporal or the license
are unavailable, record the scenario as **blocked**; never as passed.

## What the reconciler proves

For each run it compares three independent views: source records (by
`external_id`), persisted platform rows (`GET /jobs/:id/rows`, keyed by the
`external_id` inside `row_json`, not by counters) and the destination's receipt
ledger (`GET /_admin/receipts`). Checks:

- every source record has exactly one platform row (row zero included)
- succeeded ⇒ exactly one destination receipt; failed ⇒ none, unless the
  scenario declares the record ambiguous (then at most one)
- no receipt without a source record; no receipt in another tenant
- job terminal status and `totals_json` agree with persisted rows

Equal counts with wrong identities fail (see `test_reconcile.py`).

## Destination fixture

Endpoints used by templates (`http://destination-fixture:8099` inside the
compose network):

- `POST /v1/{entity}` – body must be a JSON object with a string `external_id`.
  Tenant comes from `X-Tenant`. Returns `201 {"id", "entity", "external_id", "receipt_seq"}`.
- `GET /v1/{entity}?external_id=…`, `GET /v1/{entity}/{id}` – read-back/lookup.

Admin API (host: `http://localhost:18099`):

| Call | Effect |
|---|---|
| `POST /_admin/reset` | truncate ledger, clear attempts and faults |
| `PUT /_admin/config` | `{"idempotency": bool, "unique_business_key": bool, "required_token": "...", "idempotency_header": "Idempotency-Key"}` |
| `PUT /_admin/faults` | replace fault rules (see below) |
| `GET /_admin/receipts?tenant=&entity=&external_id=` | durable writes |
| `GET /_admin/attempts` | every request and its outcome |

Fault rule: `{"id", "tenant"?, "entity"?, "external_id"?, "action", "status"?, "times"?, "delay_ms"?}`.
Empty match fields match anything; `times: 0` means unlimited. Actions:

| Action | Writes? | Client sees |
|---|---|---|
| `reject` | no | `status` (default 422) |
| `throttle` | no | 429 + `Retry-After: 1` |
| `server_error` | no | `status` (default 500) |
| `timeout_before_write` | no | nothing until `delay_ms` (default 30s), then 504 |
| `commit_then_disconnect` | **yes** | connection closed with no response |

With `idempotency: true`, a repeated `Idempotency-Key` for the same tenant and
payload replays the original receipt (200, `Idempotent-Replayed: true`) without
consuming fault budget; the same key with a different payload is a 422. With
`idempotency: false` every write is a new effect, which is how a destination
that ignores the header behaves.

## Scenarios

| Name | Proves |
|---|---|
| `ordinary` | 40 records → 40 rows → 40 receipts, identities match |
| `forced-failure` | rejected records (including row 0) are failed with zero receipts; job is `failed` |
| `throttle-recovers` | two 429s then success → exactly one receipt |

Later roadmap tasks (R01, R03–R05, R07) add scenarios here rather than
building separate harnesses.

## Changing fixtures

`python3 tests/harness/data/generate.py` regenerates the CSVs and
`SHA256SUMS` deterministically. The runner refuses to start when a file does
not match its checksum, so edits are always deliberate and reviewed.
