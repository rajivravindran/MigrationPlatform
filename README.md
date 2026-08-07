# Migration Platform

A production-grade data migration platform that ingests data from files
(CSV/JSON/XML) or CRM sources (Salesforce, watched S3/MinIO prefixes),
applies configurable preprocess + payload mapping transforms, and calls
downstream API endpoints with fault tolerance, resumability, and
observability to 10M-row scale.

## Components

| Path                       | Stack                           | Purpose                                                                 |
| -------------------------- | ------------------------------- | ----------------------------------------------------------------------- |
| `apps/api`                 | Rust (axum, sqlx, tokio)        | REST API: auth, templates, jobs, connectors, schedules, SSE, OpenAPI.   |
| `apps/orchestrator-go`     | Go (Temporal Go SDK)            | Temporal workflows/activities, connectors, schedule bootstrap.          |
| `apps/transform-worker`    | Python 3.12 (Temporal, RestrictedPython) | Preprocess + payload mapping, sandboxed `$py` evaluation.      |
| `apps/web`                 | Next.js 14 + Tailwind + shadcn  | Designer (React Flow), job console, schedules, connectors.             |
| `packages/rule-schema`     | JSON Schema + Zod/Pydantic/Rust/Go types | Single source of truth for RuleTemplate.                        |
| `infra/`                   | Docker Compose, Prometheus, Grafana, Loki, Tempo, OTEL Collector | Local + production-parity observability.        |
| `infra/helm`               | Helm                            | Kubernetes deployment + HPA + NetworkPolicy + pg_dump CronJob.         |

## Documentation

- [`docs/architecture.md`](docs/architecture.md) — components, data flow, workflow topology.
- [`docs/operations.md`](docs/operations.md) — deploy, upgrade, backups, runbooks.
- [`docs/api.md`](docs/api.md) — REST API reference (auth, templates, jobs, SSE).
- [`docs/connectors.md`](docs/connectors.md) — CSV/JSON/XML/Salesforce/watched-prefix.
- [`docs/scheduling.md`](docs/scheduling.md) — cron, timezones, overlap policies, incremental mode.
- [`docs/security.md`](docs/security.md) — AuthN/Z, secret handling, sandboxing, hardening.
- [`docs/observability.md`](docs/observability.md) — metrics, logs, traces, dashboards, alerts.
- [`docs/development.md`](docs/development.md) — laptop setup, Makefile, tests, CI.
- [`docs/AGENT_HANDOFF.md`](docs/AGENT_HANDOFF.md) — what recent agents shipped (P0a/P0b) and what remains.

## Laptop quickstart

Prerequisites: Docker 24+, Docker Compose v2, GNU Make, Python 3 (for the seed
helper), k6 (optional, for the smoke test).

```bash
make dev-up          # builds images and starts the stack
make seed            # creates a demo org, admin user, sample template
open http://localhost:3000       # sign in as admin@example.com / admin123
```

What's running:

| Service         | URL                    | Notes                                                    |
| --------------- | ---------------------- | -------------------------------------------------------- |
| Web UI          | http://localhost:3000  | Next.js dev/prod build depending on compose override.    |
| API             | http://localhost:8080  | OpenAPI at `/openapi.json`, docs at `/swagger-ui`.       |
| Temporal UI     | http://localhost:8088  | Workflow + Schedule browser.                             |
| Grafana         | http://localhost:3001  | Preloaded dashboards + alerts (admin/admin).             |
| Prometheus      | http://localhost:9090  | Scraping API/orchestrator/worker + cAdvisor.             |
| MinIO console   | http://localhost:9001  | S3-compatible storage (minioadmin/minioadmin).           |
| PostgreSQL      | localhost:5432         | Database `migration`, user `migration/migration`.        |
| Redis           | localhost:6379         | Pub/sub for SSE + distributed locks.                     |

Run `make help` for the full command list.

## Smoke test

```bash
python3 tests/load/generate_csv.py --rows 10000 --out /tmp/smoke.csv
TOKEN=$(./scripts/login.sh) \
  SOURCE_CSV=/tmp/smoke.csv \
  RULE_TEMPLATE_ID=1 \
  k6 run tests/load/smoke_laptop.js
```

Target on a laptop: 10k rows in under 90 seconds with zero failures.
See [`tests/load/README.md`](tests/load/README.md) for the 10M-row
Kubernetes load test.

## License

Internal — see LICENSE.
