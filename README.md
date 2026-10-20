# Migration Platform

Self-hosted tool for **file and CRM → HTTP API** migrations.

You design a mapping (CSV/JSON/XML, Salesforce, or a watched bucket), the platform
runs the job on Temporal, retries failed rows, and lets you inspect results.
For multi-file migrations a coordinator designs a **plan** — the dependency tree
of steps — up front; files arriving later (uploads, archives, watched folders)
are held until the steps they depend on have completed.

The UI offers one **workspace** per role: operator, mapping designer, migration
coordinator, executive and administrator. Switch between them from the account
block at the bottom of the sidebar (the demo admin can open all five).

---

## Install (recommended): Docker Hub, no source build

You need **Docker Desktop** (or Docker Engine 24+ with Compose v2), plus
`openssl`, `curl`, and `python3`. Podman works too: install `podman-docker`
and the `docker-compose` v2 binary (or run `podman compose` against the same
files).

**CPU architectures.** Everything builds from source on x86-64 and arm64
(Apple Silicon, Graviton, Ampere, Raspberry Pi 5) — see "Install from
source". The published images are multi-arch for the same two platforms once
pushed with `make hub-push` or by the `images` workflow; a tag pushed with a
plain `docker push` from a laptop is single-arch and runs only on that CPU
family. 32-bit ARM is not supported (Temporal and MinIO do not ship it).

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

Share **`infra/migration-demo-pack.zip`** from this repo (the repo is private, so a raw GitHub URL will 404 unless the recipient is signed in). Then:

```bash
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
make test        # Rust, Go, Python, web and schema tests (needs the toolchains)
make help
```

The integrity harness runs end-to-end scenarios against the stack with a
deterministic destination fixture: `make harness-up`, `make harness-setup`,
`make harness` (see [docs/development.md](docs/development.md)).

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
| Grafana | http://localhost:3001 | no login (anonymous admin, full stack only) |

The demo pack starts Web, API, workers, Postgres, Redis, MinIO, and Temporal.
Grafana/Prometheus are on the full `make up` stack, not the slim demo pack.

---

## First migration (2 minutes)

1. Sign in at http://localhost:3000 — you land in the **Operator** workspace
2. A demo template (*Demo customer upload*) is already published by `./up.sh` or
   `make seed`. It posts to `httpbin.org`, so this first run needs internet access.
3. **Migrations → Start a migration**, pick the template, upload a small CSV
   (`email,country,amount`), start
4. Open the migration: records, request/response per record, retry, **Download results** (CSV)

## A multi-file migration with dependencies

1. Switch to the **Migration coordinator** workspace and open **Plans → Design a plan**
2. Add a step per part of the migration (accounts, contacts, orders…), draw the
   arrows between them on the canvas, and give each step a default template, an
   optional file-name pattern (`accounts_*.csv`) and a completion rule: every
   record must succeed, or failures within a percentage
3. **Publish** the plan. No files are needed until now.
4. On the plan page, **Run a file**: a single CSV / JSON / XML file feeds one step
   (step and template dropdowns, pre-filled); a `.tar.gz` / `.zip` feeds several
   (its `manifest.json` names the template per file, and the mapping can be
   adjusted before upload). A schedule on a watched folder can also route files
   into a plan by file-name pattern.
5. Files arriving early wait (purple) until their prerequisites complete, start
   by themselves, and show **blocked** (orange) with the reason when a
   prerequisite failed its rule. The graph glows per step: pending, waiting,
   blocked, running, completed, failed, cancelled.

Several files you already have can also be run together as a **package**
(**Packages → Build package**): the same canvas, but the files are bundled now.

**Full operator guide** (screens, configuration, JSON/XML, multi-step, packages,
and ready-made sample files): **[docs/USER_HANDBOOK.md](docs/USER_HANDBOOK.md)**.
Plans: **[docs/PLANS.md](docs/PLANS.md)**. Samples: [`examples/handbook/`](examples/handbook/).

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
| `apps/web` | Role workspaces: mapping designer, migration console, plans and packages with the live dependency graph |
| `apps/api` | Auth, templates, jobs, files, plans, packages, schedules, licenses |
| `apps/orchestrator-go` | Temporal workflows (ingest, HTTP calls, packages, plan runs and gating, watched folders) |
| `apps/transform-worker` | Sandboxed preprocess / `$py` mapping |
| `infra/demo-pack` | Hub-only install (this is what `./up.sh` uses) |
| `infra/docker-compose.yml` | Full local stack including observability |

---

## Docs

| Doc | Contents |
|-----|----------|
| [docs/USER_HANDBOOK.md](docs/USER_HANDBOOK.md) | Operator handbook + sample files |
| [docs/PLANS.md](docs/PLANS.md) | Migration plans: steps, completion rules, how files arrive, workflows |
| [docs/ui/WORKSPACES_AND_REDESIGN_2026-10-19.md](docs/ui/WORKSPACES_AND_REDESIGN_2026-10-19.md) | Role workspaces and the operations UI |
| [docs/scheduling.md](docs/scheduling.md) | Schedules and watched folders |
| [docs/install.md](docs/install.md) | Commercial/trial license install |
| [docs/architecture.md](docs/architecture.md) | Components and job data flow |
| [docs/api.md](docs/api.md) | REST API |
| [docs/connectors.md](docs/connectors.md) | CSV, JSON, XML, Salesforce, watched prefix, SFTP |
| [docs/operations.md](docs/operations.md) | Upgrades, backups, runbooks |
| [docs/security.md](docs/security.md) | Auth, secrets, sandboxing |
| [docs/observability.md](docs/observability.md) | Metrics, logs and traces (full stack) |
| [docs/development.md](docs/development.md) | Tests, integrity harness and local toolchain |

---

## Requirements and limits

- **RAM:** plan on 8 GB+ for the full Compose stack; 4 GB can work for the demo pack.
- **Ports:** 3000, 8080, 8233, 9000, 9001 must be free (3001 Grafana and 9464 API metrics on the full stack).
- Demo credentials are for **local/demo only**. Change them before any shared or production host.
- Do not commit or ship `infra/license/*signing_key*.pem` or `infra/secrets/jwt_private.pem`.

---

## License

Internal — see [LICENSE](LICENSE).
