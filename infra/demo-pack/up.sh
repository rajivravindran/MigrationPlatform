#!/usr/bin/env bash
# Pull Hub images, start the demo stack, optionally seed.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

export HUB_IMAGE_PREFIX="${HUB_IMAGE_PREFIX:-rajivravindran}"
export IMAGE_TAG="${IMAGE_TAG:-demo}"

command -v docker >/dev/null || { echo "Docker is required"; exit 1; }
command -v openssl >/dev/null || { echo "openssl is required (for JWT keys)"; exit 1; }
command -v curl >/dev/null || { echo "curl is required"; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required (for seed)"; exit 1; }

./keys.sh

echo "==> Pulling app images from Docker Hub (${HUB_IMAGE_PREFIX}/*:${IMAGE_TAG})..."
for s in api orchestrator transform-worker web; do
  docker pull "${HUB_IMAGE_PREFIX}/migration-${s}:${IMAGE_TAG}"
done

echo "==> Starting stack..."
docker compose up -d

echo "==> Waiting for API health..."
for _ in $(seq 1 90); do
  if curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

if [[ "${SKIP_SEED:-0}" != "1" ]]; then
  ./seed.sh
fi

cat <<EOF

Demo is up (no git clone required).

  Web UI:      http://localhost:3000
  API:         http://localhost:8080
  Temporal UI: http://localhost:8233
  MinIO:       http://localhost:9001  (minio / minio123)

  Login: admin@example.com / admin123

Stop with:  docker compose -f $ROOT/docker-compose.yml down
EOF
