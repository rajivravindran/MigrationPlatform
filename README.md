# Migration Platform

Self-hosted tool for **file and CRM → HTTP API** migrations.

You design a mapping (CSV/JSON/XML, Salesforce, or a watched bucket), the platform
runs the job on Temporal, retries failed rows, and lets you inspect results.

---

## Install (recommended): Docker Hub, no source build

You need **Docker Desktop** (or Docker Engine 24+ with Compose v2), plus
`openssl`, `curl`, and `python3`.

```bash
git clone https://github.com/rajivravindran/MigrationPlatform.git
cd MigrationPlatform/infra/demo-pack
chmod +x *.sh
./up.sh
```

That pulls published images, starts the stack, and seeds a demo user.

Open **http://localhost:3000**

| | |
|---|---|
| Email | `admin@example.com` |
| Password | `admin123` |

First start takes several minutes (image pulls). Later starts are faster.

### Stop

```bash
cd infra/demo-pack
docker compose down          # keep data
docker compose down -v       # wipe database and MinIO
```

### Without git

Download the pack (Compose + scripts; images still pull from Docker Hub):

```bash
curl -fsSL -o migration-demo-pack.zip \
  https://github.com/rajivravindran/MigrationPlatform/raw/main/infra/migration-demo-pack.zip
unzip migration-demo-pack.zip
cd demo-pack
chmod +x *.sh
./up.sh
```

Images used: `rajivravindran/migration-{api,web,orchestrator,transform-worker}:demo`.

---

## Install from source (developers)

Use this when you want to change code and rebuild.

**Extra tools:** Docker, Make, `openssl`. A full language toolchain is only
needed if you run tests outside Docker (Rust 1.80+, Go 1.22+, Python 3.12, Node 20).

```bash
git clone https://github.com/rajivravindran/MigrationPlatform.git
cd MigrationPlatform
make up          # generates JWT keys, builds images, starts Compose
make seed        # demo org + admin user + sample template
```

Same UI: **http://localhost:3000** — `admin@example.com` / `admin123`

```bash
make down        # stop, keep volumes
make nuke        # stop and delete volumes
make logs s=api  # tail one service
make help
```

To run the same Compose file but **pull Hub images instead of building**:

```bash
make hub-up
make seed
```

---

## URLs after install

| What | URL | Login |
|------|-----|--------|
| Web UI | http://localhost:3000 | `admin@example.com` / `admin123` |
| API | http://localhost:8080 | JWT from UI or `POST /auth/login` |
| OpenAPI | http://localhost:8080/openapi.json | — |
| Swagger | http://localhost:8080/swagger-ui | — |
| Temporal UI | http://localhost:8233 | — |
| MinIO console | http://localhost:9001 | `minio` / `minio123` |
| Grafana | http://localhost:3001 | `admin` / `admin` (full stack only) |

The demo pack starts Web, API, workers, Postgres, Redis, MinIO, and Temporal.
Grafana/Prometheus are on the full `make up` stack, not the slim demo pack.

---

## First job (2 minutes)

1. Sign in at http://localhost:3000
2. Open **Templates** — a demo template is already published after `./up.sh` or `make seed`
3. **Jobs → Start a job**, upload a small CSV (`email,country,amount`), start
4. Open the job: row list, request/response, **Download results** (CSV)

---

## What you just installed

```
Browser  →  Next.js UI (:3000)
                ↓
           Rust API (:8080)  →  Postgres, Redis, MinIO
                ↓
           Temporal  →  Go orchestrator  →  Python transform worker
```

| Path | Role |
|------|------|
| `apps/web` | Mapping designer and job console |
| `apps/api` | Auth, templates, jobs, files, licenses |
| `apps/orchestrator-go` | Temporal workflows (ingest, HTTP calls, batches) |
| `apps/transform-worker` | Sandboxed preprocess / `$py` mapping |
| `infra/demo-pack` | Hub-only install (this is what `./up.sh` uses) |
| `infra/docker-compose.yml` | Full local stack including observability |

---

## Docs

| Doc | Contents |
|-----|----------|
| [docs/install.md](docs/install.md) | Commercial/trial license install |
| [docs/architecture.md](docs/architecture.md) | Components and job data flow |
| [docs/api.md](docs/api.md) | REST API |
| [docs/connectors.md](docs/connectors.md) | CSV, JSON, XML, Salesforce, watched prefix, SFTP |
| [docs/operations.md](docs/operations.md) | Upgrades, backups, runbooks |
| [docs/security.md](docs/security.md) | Auth, secrets, sandboxing |
| [docs/development.md](docs/development.md) | Tests and local toolchain |

---

## Requirements and limits

- **RAM:** plan on 8 GB+ for the full Compose stack; 4 GB can work for the demo pack.
- **Ports:** 3000, 8080, 8233, 9000, 9001 must be free (Grafana 3001 on the full stack).
- Demo credentials are for **local/demo only**. Change them before any shared or production host.
- Do not commit or ship `infra/license/*signing_key*.pem` or `infra/secrets/jwt_private.pem`.

---

## License

Internal — see [LICENSE](LICENSE).
