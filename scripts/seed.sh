#!/usr/bin/env bash
# Seed a local compose environment with a demo org, admin user,
# sample CSV in MinIO, and a published rule template ready to run.
set -euo pipefail

COMPOSE="docker compose -f infra/docker-compose.yml"
API="http://localhost:8080"
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@example.com}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-admin123}"

echo "==> Waiting for API to become healthy..."
for i in $(seq 1 60); do
  if curl -fsS "$API/healthz" >/dev/null 2>&1; then
    echo "    API is up."
    break
  fi
  sleep 2
done

echo "==> Creating demo org..."
$COMPOSE exec -T api /app/migration-admin create-org --name "Acme Demo"

echo "==> Creating admin user ($ADMIN_EMAIL)..."
$COMPOSE exec -T api /app/migration-admin create-user \
  --org 1 --email "$ADMIN_EMAIL" --role admin --password "$ADMIN_PASSWORD"

echo "==> Logging in to get JWT..."
TOKEN=$(curl -fsS -X POST "$API/auth/login" \
  -H 'content-type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASSWORD\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')

echo "==> Uploading sample CSV to MinIO..."
cat > /tmp/sample.csv <<EOF
email,country,amount
alice@acme.com,usa,100
bob@acme.com,usa,250
carol@acme.com,canada,75
dave@acme.com,UK,900
EOF
SRC_REF=$(curl -fsS -X POST "$API/files" \
  -H "authorization: Bearer $TOKEN" \
  -F "file=@/tmp/sample.csv" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["source_ref"])')
echo "    source_ref: $SRC_REF"

echo "==> Creating rule template..."
TID=$(curl -fsS -X POST "$API/rule-templates" \
  -H "authorization: Bearer $TOKEN" \
  -H 'content-type: application/json' \
  --data-binary @- <<'JSON' | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'
{
  "name": "Demo customer upload",
  "schema_json": {
    "id": "rt_demo",
    "version": 1,
    "name": "Demo customer upload",
    "source": {
      "type": "csv",
      "schema": [
        {"name": "email",   "type": "string"},
        {"name": "country", "type": "string"},
        {"name": "amount",  "type": "number"}
      ],
      "options": {"header": true, "delimiter": ","}
    },
    "preprocess": [
      {"id": "lc_country", "field": "country", "fn": "builtin.lowercase"}
    ],
    "mapping": {
      "payload": {
        "email":  {"$from": "email"},
        "region": {"$from": "country"},
        "total":  {"$from": "amount"}
      }
    },
    "destination": {
      "type": "http",
      "method": "POST",
      "url": "http://httpbin.org/post"
    }
  }
}
JSON
)
echo "    template id: $TID"

echo "==> Publishing template..."
curl -fsS -X POST "$API/rule-templates/$TID/publish" \
  -H "authorization: Bearer $TOKEN" >/dev/null

cat <<EOF

Seed complete.

  Login (UI):   http://localhost:3000/login
    email:      $ADMIN_EMAIL
    password:   $ADMIN_PASSWORD

  Template id:  $TID
  Source ref:   $SRC_REF

  Start a job via UI, or:
    curl -X POST $API/jobs \\
      -H "authorization: Bearer $TOKEN" \\
      -H 'content-type: application/json' \\
      -d '{"rule_template_id": $TID, "source_ref": "$SRC_REF"}'
EOF
