# R00 baseline record

Roadmap task R00 ("Establish a reproducible baseline"). This file separates
failures that already existed from regressions introduced later. It records
what was executed; anything not executed is listed as blocked or not run.

## Commits

| Item | Value |
|---|---|
| Reviewed commit (handover) | `18160097` |
| Baseline commit (docs only on top of reviewed) | `404a7f7` |
| Harness introduced on branch | `r00-harness` (on top of `a15ceca`, which adds MP-09 and MP-08) |
| Date | 2026-10-10 |

## Runtime versions (local)

| Tool | Version | Note |
|---|---|---|
| Go | 1.23.4 (`~/.local/go`) | module declares `go 1.22`; CI uses 1.22 |
| Rust / cargo | 1.92.0 | |
| Python (host) | 3.10.12 | worker requires 3.12 → tests run inside the worker image (3.12.14, temporalio 1.8.0, RestrictedPython 7.3) |
| Node / npm | 24.11.1 / 11.6.2 | CI uses Node 20 |
| Docker / Compose | 29.9.0 | `DOCKER_HOST=unix:///var/run/docker.sock` |
| golangci-lint | v1.61.0 (container) | |
| ruff | 0.6.9 (container) | CI installs unpinned |
| Temporal CLI, k6 | not installed | |

## Component checks at baseline (`404a7f7`)

| Check | Command | Result |
|---|---|---|
| Go tests | `cd apps/orchestrator-go && go test ./...` | pass |
| Go vet | `go vet ./...` | pass |
| Go lint | `golangci-lint run` (v1.61.0) | **fail, pre-existing (7):** errcheck on three `defer tx.Rollback`, gofmt `-s` on `template/template.go` and `connectors/connectors.go`, ineffassign in `connectors/sftp.go:408`, SA1019 `tar.TypeRegA` in `batch/unpack.go:103` |
| Rust tests | `cargo test --workspace` | pass (78 API + 3; 4 ignored DB tests) |
| Rust clippy | `cargo clippy --manifest-path apps/api/Cargo.toml --all-targets -- -D warnings` | pass locally on 1.92 (historical CI run failed; toolchain drift, since CI uses floating `stable`) |
| Python tests | `PYTHONPATH=. python -m pytest -q` in worker image, repo mounted | **fail, pre-existing:** 16 passed, 2 failed (`test_sandbox_row_access`, `test_build_payload_simple_mapping`; `NameError: _getitem_`). Matches handover §5 exactly |
| Python lint | `ruff check migration_worker` | **fail, pre-existing (2):** includes UP037 in `template.py:104` |
| Web lint | `npm run lint` | pass |
| Workflow lint | `actionlint` 1.7.3 | `ci.yml` shellcheck SC2034/SC2015 in the stack-wait loop (pre-existing, info/warning) |

Other pre-existing observations:
- `make test-web` runs `npm test || true`, and there is no `test` script, so it always reports success.
- CI installs Python dependencies unpinned (`pip install ruff pytest temporalio …`) and falls back from `npm ci` to `npm install`.
- `tests/load/*.js` read top-level `processed/failed/total_rows`, but the API returns `totals_json.{processed,failed,total}`. `load_10m.js` also sent `source_ref` as a JSON *string* of a `connector` shape that no ingest connector implements. Both are fixed in R00.
- `activities.BootstrapScheduledJob` is registered but has no caller.

## Integrity harness (new in R00)

Entry point: `make harness-up && make harness-setup && make harness`
(details: `tests/harness/README.md`).

| Check | Result |
|---|---|
| Fixture unit tests (`go test -race`, 9 tests) | pass |
| Reconciler unit tests (`python3 -m unittest`, 10 tests) | pass |
| `ordinary` on the compose stack | **pass**: 40/40 rows, 40 receipts, identities match |
| `forced-failure` | **pass**: rows 0 and 7 failed with 0 receipts, 38 succeeded with 1 each, job `failed`, totals agree |
| `throttle-recovers` | **pass**: attempts for A-CUS-0003 were 429, 429, 201; exactly one receipt |

Two-organization fixture: `harness.py setup` creates/updates org A ("Acme
Demo", seed admin) and org B ("Harness Org B", `harness-b@example.test`) via
`migration-admin`.

## Not run / blocked at baseline

- CI compose smoke (k6) and Playwright e2e: k6 is not installed locally and e2e was not run. Remaining CI parity.
- Helm lint: not run locally.
- Salesforce against a real org: out of scope; fixture-based tests come in R03.
- Throughput at volume (MP-07): not measured.
