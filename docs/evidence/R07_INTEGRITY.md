# R07 integrity acceptance

Date: 2026-10-10. Integrated commit: `965c01d` (R06 on top of R04+R05
`58a9c58`). Local only; not pushed. Stack:
`docker compose -f infra/docker-compose.yml -f infra/docker-compose.harness.yml`.

This is the combined-system gate. Per-lane unit tests do not close it.

## Status vocabulary (roadmap §11)

| Status | Meaning |
|--------|---------|
| `verified` | Stated acceptance evidence was executed on this commit |
| `implemented-unverified` | Code exists; required real-stack or remote check was not run |
| `deferred` | Explicitly assigned to a later task |
| `open` | Finding still stands |
| `disproven` | The original finding does not hold on this checkout, with evidence |

A task is `verified` only when all of its acceptance evidence exists.

## Handover §9.2 on `965c01d`

| §9 scenario | Result | Evidence |
|-------------|--------|----------|
| Sample / start / batch another org's source | **verified** | harness `source-authz`; API `source_authz` (ignored, Docker) |
| Another org's secret reference | **verified** | harness `destination-auth` (other-org phase: nothing sent) |
| Source redirects to forbidden address | **verified** (unit) | Go SSRF client tests; not a live harness redirect |
| Continue-as-new preserves totals | **verified** | harness `continue-as-new` on `58a9c58` (same runtime): 1,250 rows, 3 runs, 1 receipt each, 252s |
| Salesforce 4,501 / 2,000 / 500 | **verified** (fixture) | Go Salesforce pagination test; no live Salesforce |
| HTTP commit then response loss | **verified** | `ambiguous-idempotent` (one effect), `ambiguous-lookup` (no re-send), `ambiguous-manual` (unresolved until confirm) |
| Successful step then failed persist | **verified** (workflow) | `TestStepPersistenceErrorStopsTheRow`; `TestUnresolvedStepStopsTheChain` |
| Fast workflow vs API `running` | **verified** | API `a_fast_workflow_outcome_is_not_overwritten_as_running` |
| Ingest failure / lost execution | **verified** | `ingest-failure`; `terminated-workflow` (reconciler, 24.5s); worker container restart (below) |
| Export fails then retries | **verified** | `export-recovery`: 0 destination calls on retry |
| Python indexing / slicing | **verified** | harness `python-transforms`; worker pytest 89 passed |
| Only row zero failed | **verified** | `forced-failure` includes `A-CUS-0000` |
| Org-owned destination secret | **verified** | `destination-auth`: values absent from history/rows/logs |
| Failed test blocks image promote | **implemented-unverified** | `images.yml` requires every CI job success for the SHA; no GitHub Actions run of `965c01d` |
| Source size 10x / throughput | **deferred** (R15 / MP-07) | History bounded (continue-as-new); throughput not measured |

## Plan-named recoveries

| Recovery | Result |
|----------|--------|
| Worker restart | `docker compose restart orchestrator` 2s after start of a 40-row job: status `succeeded`, 40 rows, 40 receipts, `last_error` null |
| Ambiguous write | see commit-then-disconnect rows above |
| First-row failure | `forced-failure` A-CUS-0000 |
| Source mutation | object gone at start: `ingest-failure`. Mid-read ETag change is `SourceChanged` in Ingest (unit); not a live overwrite script |
| Auth failure | `destination-auth` missing / other-org / revoked |
| Export-only recovery | `export-recovery` |

Harness command on `965c01d` (12/12 passed, evidence in `tests/harness/out/`):

```text
python3 tests/harness/harness.py run source-authz ordinary forced-failure
  throttle-recovers ambiguous-idempotent ambiguous-manual ingest-failure
  terminated-workflow export-recovery destination-auth retry-serialized
  python-transforms
```

## MP closure

| ID | Status | Commit | Remaining |
|----|--------|--------|-----------|
| MP-01 | verified | `9997c17` + harness | Live HTTP redirect-to-link-local not harnessed |
| MP-02 | verified | `48ee096` | — |
| MP-03 | verified | `48ee096` | Fixture only; no live Salesforce org |
| MP-04 | verified | `58a9c58` | — |
| MP-05 | verified | `58a9c58` | DB-outage-during-finalize not injected; reconciler covers lost finalize |
| MP-06 | verified | `8c79a1c` | Per-call interpreter (~0.3s) is a throughput limit, not a correctness gap |
| MP-07 | deferred | — | R15. Docs no longer claim shards / circuit breaker / 10M capacity |
| MP-08 | implemented-unverified | `a15ceca` + `965c01d` | Workflows written; this SHA was not built on GitHub |
| MP-09 | verified | `40881f0` + `forced-failure` | — |
| MP-10 | verified | `58a9c58` | `oauth2_client` is refused, not implemented |
| MP-11 | verified | `965c01d` | Claims corrected; leftover research notes point at architecture.md |

## Not closed by M0

- Customer data, production deploy, or remote push.
- Live Salesforce, Stripe, SSO, S3 eventing.
- Measured throughput, HA license server, automatic rollback.
- GitHub Actions green on `965c01d` (local evidence only).
