# Security

## Threat model

The platform handles potentially sensitive source data (PII, financial
records) and downstream API credentials. The assumed attacker is a
low-privilege cluster tenant, a malicious template author, or a stolen
user session. We defend in depth across auth, secret handling, sandboxing,
transport, and audit.

## Authentication

- JWT RS256, 1h access token TTL. Refresh via re-login.
- Passwords hashed with **Argon2id**: m=65_536, t=3, p=1.
- Signing key stored in the cluster Secret `migration-platform`; rotated via
  `migration-admin rotate-jwt`, with zero-downtime dual-kid publication at
  `/.well-known/jwks.json`.
- `/auth/login` is rate limited (10/min/IP) and logs failed attempts to the
  audit log.

## Authorization

Roles (least → most privileged):

| Role       | Capabilities                                                             |
| ---------- | ------------------------------------------------------------------------ |
| `viewer`   | Read templates, jobs, schedules, rows, connectors (sans secrets).        |
| `operator` | viewer + start/pause/resume/cancel/retry jobs + trigger schedules.       |
| `editor`   | operator + create/update templates, connectors, schedules (incl. publish). |
| `admin`    | editor + user management, delete, rotate secrets.                        |

Enforced by an `axum` middleware that inspects the JWT `role` claim and
rejects with `403 forbidden.insufficient_role`.

## Secret handling

- User-provided secrets (Salesforce refresh tokens, custom API keys, etc.)
  are encrypted **before** hitting the database with **AES-GCM-256**.
- Key material is read from `AES_MASTER_KEY` (32 bytes hex, required).
- Ciphertext layout: `nonce (12B) || ct || tag (16B)`.
- `AES_MASTER_KEY_PREV` supports rolling key rotation: reads attempt current
  then previous; writes always use current. After `migration-admin rotate-secrets`
  re-wraps all rows, remove the previous key from the Secret.
- Secrets **never** leave the API: connectors return `config_json` with
  sensitive fields redacted (`"refresh_token":"<redacted>"`).
- Audit log + structured logs go through a redaction layer (`security/audit.rs`)
  that scrubs known secret-shaped fields (regex on suffix `_token`, `_key`,
  `password`, `secret`).

## Python sandbox (`$py`)

User-written expressions run in a sidecar Python worker:

- `RestrictedPython` compiles AST to disallow `import`, `open`, attribute
  access on dunder names, `exec`, `eval`, etc.
- A per-invocation subprocess runs with `resource.setrlimit`:
  - `RLIMIT_CPU = 1s`
  - `RLIMIT_AS = 256 MiB`
  - `RLIMIT_NOFILE = 16`
  - `RLIMIT_NPROC = 0` (cannot fork)
- Network syscalls are blocked via seccomp in the Dockerfile.
- The worker process is killed and respawned if a single invocation exceeds
  a hard wall clock of 2s (activity heartbeat detects it).
- Escape-fuzz suite (`tests/test_sandbox.py`) asserts over 40 known escape
  patterns (`().__class__`, `getattr(__builtins__, ...)`, etc.) are blocked.

## Destination endpoints

- HTTPS only. Outbound HTTP->HTTPS is rejected unless `destination.allow_http`
  is explicitly set (emits a template-publish warning).
- DNS pinning: the orchestrator resolves the destination host once per job
  and re-uses the resolution; prevents `example.com` swapping to internal
  addresses mid-job.
- Private IPs (RFC1918, 169.254.*, 127.*) are rejected with
  `destination.private_address_forbidden` unless the template is marked
  `internal: true` (admin-only).
- Static request headers are allowed; values are template-rendered but not
  arbitrary code.

## Transport

- API serves TLS in production (ingress-managed certs).
- Strict HTTP headers via middleware:
  - `Strict-Transport-Security: max-age=63072000; includeSubDomains; preload`
  - `Content-Security-Policy: default-src 'none'; connect-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; frame-ancestors 'none'`
  - `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`
  - `Referrer-Policy: no-referrer`
  - `Permissions-Policy: geolocation=(), microphone=(), camera=()`
- CSRF: login/logout + mutating endpoints require a bearer token, not
  cookies, which sidesteps CSRF; the `/api/files` browser-proxy endpoint
  is same-origin-only.
- NetworkPolicies restrict which pods can talk to Postgres/Redis/Temporal.

## Audit log

Every mutating API call writes to `audit_log`:

```
audit_log(id, occurred_at, actor_user_id, actor_ip, request_id,
          action, resource_type, resource_id, diff_json, outcome)
```

Retention: 180 days by default; export via `migration-admin audit export`.

## Secure defaults checklist

- Strong admin password enforced at `create-user`.
- MinIO keys rotated on first boot (`scripts/seed.sh` warns if default).
- Temporal connection uses TLS + server-name verification in prod values.
- All Dockerfiles run as non-root (`uid=10001`).
- Read-only root filesystem enforced in Helm `securityContext`.
- No `:latest` tags anywhere; images are pinned + signed via cosign.
