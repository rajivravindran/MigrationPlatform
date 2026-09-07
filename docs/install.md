# Customer install (commercial / trial)

## Quick start (Docker Hub demo — no git)

Share **`infra/migration-demo-pack.zip`** (rebuilt with `make demo-pack`). Recipients
do **not** need this repository. The repo is private — send them the zip file
rather than a raw GitHub URL.

```bash
unzip migration-demo-pack.zip
cd demo-pack
./up.sh
open http://localhost:3000   # admin@example.com / admin123
```

Images pull from `rajivravindran/migration-*:demo`. The zip only has Compose + scripts.

---

Self-hosted Migration Platform is a **conditional-release MVP** distributed as versioned container images (GHCR)
plus a compose/Helm pack. Images alone do not limit use — release installs must set
`LICENSE_ENFORCE=true` and either mount a commercial license or reach the vendor
license service for a **10-day phone-home trial**.

## What you get

| Piece | Notes |
|-------|--------|
| Images | `ghcr.io/<org>/migration-{api,orchestrator,transform-worker,web}:vX.Y.Z` (pin tags; avoid floating `latest` in prod) |
| License | RSA-signed JSON verified with the **public** key embedded in the API image |
| Trial | Email-gated, 10 days, bound to the install fingerprint. Started at boot when `LICENSE_TRIAL_EMAIL` is set, or by an admin from **Settings → License** (no restart) |
| Heartbeat | Trials (and commercial licenses signed with `--require-heartbeat`) re-attest with the license server every **24h**; **72h** offline grace, then work-producing APIs return `402` until connectivity returns |

**Never** ship or mount the vendor **signing private key** into customer environments.
Only `infra/license/license_public_key.pem` (already baked into the API) is used to verify.

## Quick start (compose sketch)

1. Copy the vendor install pack (compose + `.env.example`).
2. Pin image tags to a release (`v1.2.0`).
3. Create a **named volume** for the durable license store.
4. Set:

```bash
LICENSE_ENFORCE=true
LICENSE_SERVER_URL=https://license.example.com   # vendor trial + heartbeat endpoint
LICENSE_SERVER_TOKEN=<vendor-issued-32+-character-token>
LICENSE_TRIAL_EMAIL=ops@example.com              # optional: auto-start the trial at boot
LICENSE_STORE_PATH=/var/lib/migration/license.json
LICENSE_INSTALLATION_ID_PATH=/run/installation/installation-id
```

5. Mount the license volume on the API container, e.g. `license_data:/var/lib/migration`.
6. `docker compose up -d` (or Helm install).
7. Open **Settings → License**. If `LICENSE_TRIAL_EMAIL` was not set, an admin
   enters a contact email and clicks **Start trial**. Confirm days left, the
   heartbeat row, and the short install ID (fingerprint).

### Trial rules

- A contact email is required; the license server stores it as the lead
  record and it becomes the `licensee`. It is not verified by e-mail (follow-up).
- The trial is keyed by the install fingerprint (`sha256` of the durable
  `installation-id` file). Re-activating for the same install — different
  email, deleted `license.json`, redeployed containers — returns the **same**
  `expires_at`; an expired fingerprint gets `402`.
- Limitation: wiping the license volume creates a new `installation-id`, hence a
  new fingerprint and a fresh trial. Hostnames and machine IDs are deliberately
  not mixed in (they change across replicas/restarts), so the deterrent for
  serial trials is the email lead record on the vendor side, not the fingerprint.
- Every trial carries `requires_heartbeat: true` (see below).

### Heartbeat and offline grace

Licenses with `requires_heartbeat` are re-attested by the API in the
background:

| Setting | Value |
|---------|-------|
| Interval | 24h (`LICENSE_HEARTBEAT_INTERVAL_SECS` overrides for tests only) |
| Retry after failure | hourly |
| Offline grace | 72h from the **signed** `issued_at` of the current license |
| On `403 revoked` / `402 expired` from the server | work-producing APIs blocked immediately; the API keeps probing hourly so a vendor `unrevoke` self-heals without a restart |
| On network error / 5xx / `401` | keep working inside grace; `heartbeat.status = degraded` |

A successful heartbeat returns the same license re-signed with a fresh
`issued_at`, which the API persists to `LICENSE_STORE_PATH`. The grace clock
therefore lives inside a vendor-signed document, not in an editable local file.
Every API replica heartbeats on its own (one small request a day) and all
replicas re-read the shared store within a minute, so an activation on one
replica reaches the others.

Alerts: `LicenseInvalid`, `LicenseHeartbeatFailing`, `LicenseGraceExpiringSoon`
in `infra/prometheus-alerts.yml` (metrics `license_valid`,
`license_heartbeat_total{result}`, `license_heartbeat_grace_seconds_remaining`).

### Activating a commercial license

1. Read the **Install ID** from Settings → License and send it to the vendor.
2. Vendor signs a license bound to it:

   ```bash
   migration-admin license-sign --key license_signing_key.pem \
     --licensee "Acme Corp" --expires 2027-12-31 --features core,llm \
     --fingerprint <full-hex-install-id> --out acme-license.json
   # add --require-heartbeat for a connected install that must phone home;
   # omit it for air-gapped installs (no heartbeat, expiry only)
   ```

   The full hex install ID (SHA-256 of the installation UUID) is shown to
   **admins** in Settings → License ("Full install ID for the vendor", with a
   copy button) and returned as `install_id_full` by `GET /license` for admin
   tokens. Non-admins and logs only see the 12-char short form.
3. Customer mounts the file read-only as `LICENSE_FILE=/licenses/license.json`
   **or** copies it to `LICENSE_STORE_PATH`. A changed store file is adopted
   within a minute without a restart; `LICENSE_FILE` is read at boot.
4. Settings shows `kind=commercial`, the new expiry, and `Heartbeat: not
   required` (air-gapped) or `ok` (connected).

A fingerprint-bound license mounted on a different install is rejected
(`fingerprint_mismatch`) and the API stays on whatever license it had.

### Revoking a license (vendor)

Revocations take effect at the next heartbeat (≤ 24h, immediately on restart):

```bash
LICENSE_TRIAL_DB=/var/lib/license/trials.db \
  migration-license-server revoke --fingerprint <hex> [--licensee "Acme Corp"] --reason "chargeback"
migration-license-server unrevoke --fingerprint <hex>
```

Without `--licensee` the revocation applies to every license for that install
(`*`). A revoked install is blocked at its next heartbeat and then keeps probing
hourly, so after `unrevoke` it recovers on its own within an hour (or immediately
on API restart). Air-gapped licenses without `requires_heartbeat` cannot be
revoked remotely — that is the trade-off of issuing them.

## Environment reference

| Variable | Required | Description |
|----------|----------|-------------|
| `LICENSE_ENFORCE` | prod | `true` blocks all work creation/resume/retry and automatic schedule execution when unlicensed/expired/revoked/out of grace |
| `LICENSE_SERVER_URL` | trial / heartbeat | Base URL of the phone-home service (no trailing path) |
| `LICENSE_SERVER_TOKEN` | trial / heartbeat | Bearer credential for trial activation and heartbeats |
| `LICENSE_TRIAL_EMAIL` | optional | Contact email; when set, the trial auto-starts at boot. Otherwise an admin starts it from Settings |
| `LICENSE_STORE_PATH` | optional | Durable path for auto-issued trial (default `/var/lib/migration/license.json`) |
| `LICENSE_INSTALLATION_ID_PATH` | prod | Mounted durable installation UUID shared by all API replicas |
| `LICENSE_FILE` | commercial | Path to a mounted vendor-signed license |
| `LICENSE_PUBLIC_KEY_PATH` | rare | Override embedded verify key (rotation only) |

## Install fingerprint

The API hashes an operator-provisioned durable installation UUID. Compose stores
it in `license-data`; Helm stores it in a retained Secret shared by all replicas.
Hostnames and container machine IDs are not used.
Wiping the license volume alone does **not** reset the clock (server remembers the fingerprint).

## Local smoke (developers)

```bash
# Terminal 1 — license service (uses local dev signing key; DO NOT ship this key)
export LICENSE_SIGNING_KEY_PATH=infra/license/dev_license_signing_key.pem
export LICENSE_SERVER_TOKEN=local-dev-token-0123456789abcdef0123456789
export LICENSE_TRIAL_DB=./data/trials.db
export LICENSE_SERVER_BIND=127.0.0.1:8090
cargo run -p migration-api --bin migration-license-server

# Terminal 2 — API with enforce + phone-home
export LICENSE_ENFORCE=true
export LICENSE_SERVER_URL=http://127.0.0.1:8090
export LICENSE_SERVER_TOKEN=local-dev-token-0123456789abcdef0123456789
export LICENSE_TRIAL_EMAIL=dev@example.com
export LICENSE_STORE_PATH=./data/license.json
export LICENSE_INSTALLATION_ID_PATH=./data/installation-id
export LICENSE_HEARTBEAT_INTERVAL_SECS=120   # force a heartbeat every 2 min for the smoke
# …plus the usual DATABASE_URL, MASTER_KEY, etc.
cargo run -p migration-api --bin migration-api
```

Verify:

```bash
curl -s http://127.0.0.1:8090/healthz
# After API is up and authenticated:
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/license | jq .heartbeat
# Within ~3 minutes: heartbeat.last_attested_at advances, API log "license heartbeat ok".
# Revoke and watch the next heartbeat close the gate (402 on POST /jobs):
LICENSE_TRIAL_DB=./data/trials.db cargo run -p migration-api --bin migration-license-server -- \
  revoke --fingerprint $(jq -r .license.fingerprint ./data/license.json) --reason smoke
```

Re-run the API after deleting `./data/license.json` — the same fingerprint should
**reuse** the original `expires_at` (not a fresh 10 days). Stop the license
server and the API keeps working (`heartbeat.status = degraded`) until 72h after
the last signed `issued_at`.

Direct license-server checks without the API:

```bash
FP=$(python3 -c "print('ab'*32)"); T="Authorization: Bearer $LICENSE_SERVER_TOKEN"
curl -s -X POST localhost:8090/v1/trial/start -H "$T" -H 'Content-Type: application/json' \
  -d "{\"fingerprint\":\"$FP\",\"email\":\"ops@example.com\"}" | jq .license_file > lic.json
curl -s -X POST localhost:8090/v1/heartbeat -H "$T" -H 'Content-Type: application/json' \
  -d "{\"fingerprint\":\"$FP\",\"license_file\":$(cat lic.json)}" | jq .license_file.license.issued_at
```

## Release checklist (vendor)

- [ ] Tag `v*` and publish images via CI (`.github/workflows/images.yml`)
- [ ] Confirm customer images contain **only** the public verify key
- [ ] Confirm `dev_license_signing_key.pem` / any `*signing_key*.pem` is **not** in the image
- [ ] License service deployed with offline-held signing key + durable `trials` DB
- [ ] Install pack sets `LICENSE_ENFORCE=true` and a durable license volume
- [ ] Helm storage class supports `ReadWriteMany` for multi-replica license storage

## License server boundaries

`migration-license-server` is a vendor-operated, authenticated MVP. The image
contains no signing key; mount `LICENSE_SIGNING_KEY_PATH` at runtime. SQLite must
be on durable storage (`trials` holds lead emails and heartbeat counters,
`revocations` the deny list). The service is not HA and does not integrate with
KMS; run one replica and include its database in backups. Customers tolerate a
72h outage thanks to offline grace — keep planned maintenance well inside that.

Endpoints: `POST /v1/trial/start`, `POST /v1/heartbeat` (bearer token), `GET /healthz`,
`GET /readyz`. Rate limits: 20 req/s global, 1 req / 5s per fingerprint.

## Out of scope (later)

Stripe / payment portal (P5.2), e-mail verification of the trial contact,
per-SKU feature gating, and clock-tamper detection on the customer host are
deferred past this MVP.
