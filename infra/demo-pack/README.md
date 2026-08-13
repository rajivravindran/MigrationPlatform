# Migration Platform — offline demo pack

Run the product from **Docker Hub only**. No git access and no source build.

## Prerequisites

- Docker 24+ with Compose v2
- `openssl`, `curl`, `python3`

## Start

```bash
chmod +x *.sh
./up.sh
```

That will:

1. Generate local JWT keys under `secrets/`
2. Pull `rajivravindran/migration-*:demo` (and public infra images)
3. Start Postgres, Redis, MinIO, Temporal, API, workers, and Web
4. Seed a demo admin user and sample template

Open **http://localhost:3000** → `admin@example.com` / `admin123`

## Stop

```bash
docker compose down
```

Remove data volumes too:

```bash
docker compose down -v
```

## Optional

```bash
HUB_IMAGE_PREFIX=rajivravindran IMAGE_TAG=demo ./up.sh
SKIP_SEED=1 ./up.sh    # start without seeding
./seed.sh              # seed later
```

## What this pack contains

| File | Purpose |
|------|---------|
| `docker-compose.yml` | Full runnable stack (Hub app images) |
| `up.sh` | One-command start |
| `keys.sh` | Generate JWT keypair |
| `seed.sh` | Demo org / user / template |

Images are public on Docker Hub under `rajivravindran/migration-*`. This zip is the only install material you need to share.
