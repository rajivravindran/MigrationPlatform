#!/usr/bin/env bash
# Generate a local JWT RSA keypair for the demo API (one-time).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$ROOT/secrets"
if [[ -f "$ROOT/secrets/jwt_private.pem" ]]; then
  echo "JWT keys already exist under secrets/"
  exit 0
fi
openssl genpkey -algorithm RSA -out "$ROOT/secrets/jwt_private.pem" -pkeyopt rsa_keygen_bits:2048
openssl rsa -in "$ROOT/secrets/jwt_private.pem" -pubout -out "$ROOT/secrets/jwt_public.pem"
chmod 600 "$ROOT/secrets/jwt_private.pem"
echo "Generated secrets/jwt_private.pem and secrets/jwt_public.pem"
