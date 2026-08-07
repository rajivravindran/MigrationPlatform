#!/usr/bin/env bash
# Prints a JWT for the default admin user to stdout.
# Useful for one-liner curl/k6 pipelines:
#   TOKEN=$(./scripts/login.sh)
set -euo pipefail

API="${API:-http://localhost:8080}"
EMAIL="${ADMIN_EMAIL:-admin@example.com}"
PASSWORD="${ADMIN_PASSWORD:-admin123}"

curl -fsS -X POST "$API/auth/login" \
  -H 'content-type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])'
