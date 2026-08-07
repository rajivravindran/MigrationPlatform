# Customer install (commercial / trial)

Self-hosted Migration Platform is distributed as **versioned container images** (GHCR)
plus a compose/Helm pack. Images alone do not limit use — release installs must set
`LICENSE_ENFORCE=true` and either mount a commercial license or reach the vendor
license service for a **10-day phone-home trial**.

## What you get

| Piece | Notes |
|-------|--------|
| Images | `ghcr.io/<org>/migration-{api,orchestrator,transform-worker,web}:vX.Y.Z` (pin tags; avoid floating `latest` in prod) |
| License | RSA-signed JSON verified with the **public** key embedded in the API image |
| Trial | First boot with enforce + no local license → `POST {LICENSE_SERVER_URL}/v1/trial/start` keyed by install fingerprint |

**Never** ship or mount the vendor **signing private key** into customer environments.
Only `infra/license/license_public_key.pem` (already baked into the API) is used to verify.

## Quick start (compose sketch)

1. Copy the vendor install pack (compose + `.env.example`).
2. Pin image tags to a release (`v1.2.0`).
3. Create a **named volume** for the durable license store.
4. Set:

```bash
LICENSE_ENFORCE=true
LICENSE_SERVER_URL=https://license.example.com   # vendor trial endpoint
LICENSE_STORE_PATH=/var/lib/migration/license.json
```

5. Mount the license volume on the API container, e.g. `license_data:/var/lib/migration`.
6. `docker compose up -d` (or Helm install).
7. Open **Settings → License**: confirm days left and the short install ID (fingerprint).

### Activating a commercial license

1. Obtain a signed `license.json` from the vendor (`migration-admin license-sign`).
2. Mount it read-only as `LICENSE_FILE=/licenses/license.json` **or** copy it to
   `LICENSE_STORE_PATH` and restart the API.
3. Settings should show `kind=commercial` and the new expiry.

## Environment reference

| Variable | Required | Description |
|----------|----------|-------------|
| `LICENSE_ENFORCE` | prod | `true` blocks job/schedule **create** when unlicensed/expired |
| `LICENSE_SERVER_URL` | trial | Base URL of the phone-home service (no trailing path) |
| `LICENSE_STORE_PATH` | optional | Durable path for auto-issued trial (default `/var/lib/migration/license.json`) |
| `LICENSE_FILE` | commercial | Path to a mounted vendor-signed license |
| `LICENSE_PUBLIC_KEY_PATH` | rare | Override embedded verify key (rotation only) |

## Install fingerprint

The API hashes `/etc/machine-id` (or hostname fallback) with SHA-256. Only the
hex digest leaves the host — no cleartext hostname/machine-id in license traffic.
Wiping containers but keeping the same machine keeps the same trial end date.
Wiping the license volume alone does **not** reset the clock (server remembers the fingerprint).

## Local smoke (developers)

```bash
# Terminal 1 — license service (uses local dev signing key; DO NOT ship this key)
export LICENSE_SIGNING_KEY_PATH=infra/license/dev_license_signing_key.pem
export LICENSE_TRIAL_DB=./data/trials.db
export LICENSE_SERVER_BIND=127.0.0.1:8090
cargo run -p migration-api --bin migration-license-server

# Terminal 2 — API with enforce + phone-home
export LICENSE_ENFORCE=true
export LICENSE_SERVER_URL=http://127.0.0.1:8090
export LICENSE_STORE_PATH=./data/license.json
# …plus the usual DATABASE_URL, MASTER_KEY, etc.
cargo run -p migration-api --bin migration-api
```

Verify:

```bash
curl -s http://127.0.0.1:8090/healthz
# After API is up and authenticated:
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/license | jq .
```

Re-run the API after deleting `./data/license.json` — the same fingerprint should
**reuse** the original `expires_at` (not a fresh 10 days).

## Release checklist (vendor)

- [ ] Tag `v*` and publish images via CI (`.github/workflows/images.yml`)
- [ ] Confirm customer images contain **only** the public verify key
- [ ] Confirm `dev_license_signing_key.pem` / any `*signing_key*.pem` is **not** in the image
- [ ] License service deployed with offline-held signing key + durable `trials` DB
- [ ] Install pack sets `LICENSE_ENFORCE=true` and a durable license volume

## Out of scope (later)

Stripe / payment portal, email-gated trials, and periodic heartbeats with offline grace
are deferred past this MVP.
