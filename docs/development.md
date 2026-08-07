# Development guide

## Prerequisites

| Tool                  | Version  | Notes                                  |
| --------------------- | -------- | -------------------------------------- |
| Docker Engine         | 24+      | Docker Desktop on macOS/Windows works. |
| Docker Compose plugin | v2.20+   | Shipped with Docker Desktop.           |
| Rust                  | 1.82+    | For `apps/api`.                        |
| Go                    | 1.22+    | For `apps/orchestrator-go`.            |
| Python                | 3.12     | For `apps/transform-worker`.           |
| Node.js               | 20 LTS   | For `apps/web`.                        |
| Make                  | any      | Top-level workflow.                    |
| k6                    | 0.49+    | Optional, for the smoke test.          |

## Laptop quickstart

```bash
make dev-up          # Build + start Docker Compose
make seed            # Demo org/user/template/connector
make dev-logs        # Tail everything
make dev-down        # Stop and remove containers + volumes
```

Default credentials created by `make seed`:
- Email: `admin@example.com`
- Password: `admin123`

Visit:
- UI: http://localhost:3000
- Swagger: http://localhost:8080/swagger-ui
- Grafana: http://localhost:3001 (admin/admin)
- Temporal: http://localhost:8088

## Makefile targets

```
make help
```

Main ones:

- `make dev-up` / `dev-down` / `dev-restart`
- `make seed` — idempotent seed script
- `make api-shell` / `go-shell` / `py-shell` / `web-shell` — exec into a container
- `make fmt` — format Rust, Go, TS, Python
- `make lint` — clippy, go vet + golangci-lint, ruff, eslint
- `make test` — run all unit/integration tests in containers
- `make e2e` — Playwright tests against the laptop stack
- `make smoke` — k6 smoke test (requires k6 installed locally)
- `make generate` — regen shared types from `packages/rule-schema/schema/*`
- `make migrate` — run DB migrations against a local compose Postgres
- `make admin ARGS="create-user --org 1 --role admin --email ..."` — admin CLI

## Repository layout

```
MigrationPlatform/
├── apps/
│   ├── api/                  # Rust axum API + sqlx migrations + admin CLI
│   ├── orchestrator-go/      # Go Temporal worker + connectors
│   ├── transform-worker/     # Python sandboxed transform worker
│   └── web/                  # Next.js UI
├── packages/
│   └── rule-schema/          # JSON Schema + generated Zod/Rust/Go/Py types
├── infra/
│   ├── docker-compose.yml    # Dev + laptop stack
│   ├── helm/                 # Kubernetes Helm chart
│   ├── otel-collector.yaml   # OTLP receive + export config
│   ├── prometheus.yml        # Scrape targets
│   └── grafana/              # Dashboards + alerts + datasources
├── scripts/
│   ├── seed.sh               # Laptop demo data seeder
│   └── login.sh              # Convenience token fetch
├── tests/
│   └── load/                 # k6 smoke + 10M row plan
├── docs/                     # This documentation
└── .github/workflows/        # CI + image build
```

## Running individual services natively

Sometimes it's faster to run one service outside Compose while using
Compose for its dependencies.

### Rust API

```bash
cd apps/api
export DATABASE_URL=postgres://migration:migration@localhost:5432/migration
export REDIS_URL=redis://localhost:6379
export TEMPORAL_ADDRESS=localhost:7233
export JWT_SIGNING_KEY=$(openssl rand -hex 64)
export AES_MASTER_KEY=$(openssl rand -hex 32)
export S3_ENDPOINT=http://localhost:9000
export S3_ACCESS_KEY=minioadmin
export S3_SECRET_KEY=minioadmin
cargo run --bin migration-api
```

### Go orchestrator

```bash
cd apps/orchestrator-go
export DATABASE_URL=postgres://migration:migration@localhost:5432/migration
export REDIS_URL=redis://localhost:6379
export TEMPORAL_ADDRESS=localhost:7233
go run ./cmd/worker
```

### Python transform worker

```bash
cd apps/transform-worker
pip install -e .
python -m migration_worker.worker
```

### Next.js UI

```bash
cd apps/web
npm install
NEXT_PUBLIC_API_URL=http://localhost:8080 npm run dev
```

## Tests

| Suite                   | Command                                           |
| ----------------------- | ------------------------------------------------- |
| Rust unit + integration | `cargo test --manifest-path apps/api/Cargo.toml`  |
| Go                      | `cd apps/orchestrator-go && go test ./...`        |
| Python                  | `cd apps/transform-worker && pytest`              |
| Web unit                | `cd apps/web && npm run lint && npm run build`    |
| Playwright e2e          | `cd apps/web && npx playwright test`              |
| k6 smoke                | `k6 run tests/load/smoke_laptop.js`               |

Rust integration tests spin up a real Postgres via `testcontainers`; no
manual setup needed.

## Debugging tips

- **SSE stream stops mid-job**: check `ingress.maxConcurrentStreamsPerConnection`
  and `keepalive_timeout`; defaults to 120s on nginx, which the code handles
  via heartbeat pings.
- **Templates fail to validate**: the server echoes the JSONPath of the
  failing node in `error.detail`; use the UI's "validate" button before save.
- **Job stuck in `pending`**: confirm orchestrator-go is subscribed to the
  task queue `migration.default` in Temporal UI.
- **Transform kills**: check `migration_transform_sandbox_kills_total` —
  frequent kills typically mean a `$py` snippet exceeds CPU/memory limits.

## Conventions

- Commit messages: conventional (`feat(api): ...`, `fix(orch): ...`).
- Every mutating endpoint writes an audit log row.
- Feature flags live in `apps/api/src/config.rs`; prefer env-configurable
  toggles over branches.
- Generated code never edited by hand: `make generate`.
