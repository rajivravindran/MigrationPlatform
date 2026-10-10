#!/usr/bin/env python3
"""Deterministic integrity harness for the Migration Platform.

Drives the real API against the test-only destination fixture and reconciles
what the destination durably received (its receipt ledger) with what the
platform persisted. Platform counters are never accepted as proof on their own.

Prerequisites (see tests/harness/README.md):
  * stack running with the harness overlay:
      docker compose -f infra/docker-compose.yml -f infra/docker-compose.harness.yml up -d --build
  * a license that permits job creation (dev mode or a mounted trial)

Usage:
  python3 tests/harness/harness.py setup              # idempotent org/user fixtures
  python3 tests/harness/harness.py run [scenario ...] # default: all scenarios
  python3 tests/harness/harness.py list

Exit status is non-zero when any scenario fails or cannot run. Evidence for
every scenario is written to tests/harness/out/<scenario>.json.

Stdlib only; never calls external services.
"""

from __future__ import annotations

import argparse
import base64
import csv
import hashlib
import io
import json
import os
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
DATA = HERE / "data"
OUT = HERE / "out"

API = os.environ.get("HARNESS_API", "http://localhost:8080")
DEST_ADMIN = os.environ.get("HARNESS_DEST_ADMIN", "http://localhost:18099")
DEST_INTERNAL = os.environ.get("HARNESS_DEST_INTERNAL", "http://destination-fixture:8099")
TEMPORAL_HTTP = os.environ.get("HARNESS_TEMPORAL_HTTP", "http://localhost:8233")
COMPOSE = os.environ.get(
    "HARNESS_COMPOSE",
    "docker compose -f infra/docker-compose.yml -f infra/docker-compose.harness.yml",
)
JOB_TIMEOUT_S = int(os.environ.get("HARNESS_JOB_TIMEOUT_S", "300"))
TERMINAL = {"succeeded", "failed", "cancelled"}

ORGS = {
    "org-a": {
        "name": os.environ.get("HARNESS_ORG_A_NAME", "Acme Demo"),
        "email": os.environ.get("HARNESS_ORG_A_EMAIL", "admin@example.com"),
        "password": os.environ.get("HARNESS_ORG_A_PASSWORD", "admin123"),
    },
    "org-b": {
        "name": "Harness Org B",
        "email": "harness-b@example.test",
        "password": "harness-b-password",
    },
}


class HarnessError(Exception):
    pass


# --------------------------------------------------------------------------- http


def http(method: str, url: str, *, token: str | None = None, body: Any = None,
         raw: bytes | None = None, headers: dict[str, str] | None = None,
         timeout: float = 30) -> tuple[int, Any]:
    hdrs = dict(headers or {})
    data = raw
    if body is not None:
        data = json.dumps(body).encode()
        hdrs["content-type"] = "application/json"
    if token:
        hdrs["authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        payload = e.read()
        status = e.code
    except urllib.error.URLError as e:
        raise HarnessError(f"{method} {url}: {e.reason}") from e
    try:
        return status, json.loads(payload) if payload else None
    except json.JSONDecodeError:
        return status, payload.decode(errors="replace")


def ok(status: int, payload: Any, what: str) -> Any:
    if status >= 300:
        raise HarnessError(f"{what}: HTTP {status}: {payload}")
    return payload


def multipart_upload(token: str, path: Path) -> dict:
    boundary = uuid.uuid4().hex
    body = io.BytesIO()
    body.write(f"--{boundary}\r\n".encode())
    body.write(f'Content-Disposition: form-data; name="file"; filename="{path.name}"\r\n'.encode())
    body.write(b"Content-Type: text/csv\r\n\r\n")
    body.write(path.read_bytes())
    body.write(f"\r\n--{boundary}--\r\n".encode())
    status, payload = http("POST", f"{API}/files", token=token, raw=body.getvalue(),
                           headers={"content-type": f"multipart/form-data; boundary={boundary}"})
    return ok(status, payload, f"upload {path.name}")["source_ref"]


# ------------------------------------------------------------------ preconditions


def verify_fixture_checksums() -> None:
    for line in (DATA / "SHA256SUMS").read_text().splitlines():
        digest, rel = line.split(maxsplit=1)
        actual = hashlib.sha256((DATA / rel).read_bytes()).hexdigest()
        if actual != digest:
            raise HarnessError(f"fixture {rel} changed (sha256 {actual} != {digest}); "
                               "regenerate deliberately with tests/harness/data/generate.py")


def preflight() -> None:
    verify_fixture_checksums()
    status, _ = http("GET", f"{API}/healthz")
    if status != 200:
        raise HarnessError(f"API not healthy at {API} (HTTP {status})")
    status, _ = http("GET", f"{DEST_ADMIN}/_admin/healthz")
    if status != 200:
        raise HarnessError(f"destination fixture not reachable at {DEST_ADMIN}")


def admin_cli(*args: str) -> str:
    cmd = COMPOSE.split() + ["exec", "-T", "api", "/app/migration-admin", *args]
    res = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, check=False)
    if res.returncode != 0:
        raise HarnessError(f"{' '.join(args[:1])} failed: {res.stderr.strip() or res.stdout.strip()}")
    return res.stdout.strip()


def setup() -> dict[str, int]:
    """Create both organizations and an admin per org. Idempotent."""
    ids: dict[str, int] = {}
    for tenant, org in ORGS.items():
        out = admin_cli("create-org", "--name", org["name"])
        org_id = int(out.split("id=")[1].split()[0])
        admin_cli("create-user", "--org", str(org_id), "--email", org["email"],
                  "--role", "admin", "--password", org["password"])
        ids[tenant] = org_id
        print(f"  {tenant}: org id={org_id} user={org['email']}")
    return ids


def login(tenant: str) -> str:
    org = ORGS[tenant]
    status, payload = http("POST", f"{API}/auth/login",
                           body={"email": org["email"], "password": org["password"]})
    return ok(status, payload, f"login {tenant}")["token"]


# ------------------------------------------------------------------- destination


def dest_reset(config: dict | None = None, faults: list[dict] | None = None) -> None:
    ok(*http("POST", f"{DEST_ADMIN}/_admin/reset"), "fixture reset")
    cfg = {"idempotency": True, "unique_business_key": False, "idempotency_header": "Idempotency-Key"}
    cfg.update(config or {})
    ok(*http("PUT", f"{DEST_ADMIN}/_admin/config", body=cfg), "fixture config")
    ok(*http("PUT", f"{DEST_ADMIN}/_admin/faults", body=faults or []), "fixture faults")


def dest_receipts(tenant: str | None = None) -> list[dict]:
    q = f"?tenant={tenant}" if tenant else ""
    return ok(*http("GET", f"{DEST_ADMIN}/_admin/receipts{q}"), "receipts") or []


def dest_attempts() -> list[dict]:
    return ok(*http("GET", f"{DEST_ADMIN}/_admin/attempts"), "attempts") or []


# ---------------------------------------------------------------------- platform


def read_source(tenant: str, entity: str) -> list[dict]:
    with (DATA / tenant / f"{entity}.csv").open(newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def customer_template(tenant: str, run_id: str, retry: dict | None = None) -> dict:
    return {
        "name": f"harness {tenant} customers {run_id}",
        "schema_json": {
            "id": f"harness-customers-{run_id}",
            "version": 1,
            "name": f"harness {tenant} customers",
            "source": {
                "type": "csv",
                "schema": [{"name": c, "type": "string"} for c in ("external_id", "email", "name", "country")],
                "options": {"header": True, "delimiter": ","},
            },
            "preprocess": [],
            "mapping": {"payload": {c: {"$from": c} for c in ("external_id", "email", "name", "country")}},
            "destination": {
                "type": "http",
                "method": "POST",
                "url": f"{DEST_INTERNAL}/v1/customers",
                "headers": {"X-Tenant": tenant},
                "idempotencyHeader": "Idempotency-Key",
            },
            "retry": retry or {"maxAttempts": 3, "backoff": "fixed", "initialIntervalMs": 200},
        },
    }


def create_published_template(token: str, tpl: dict) -> int:
    created = ok(*http("POST", f"{API}/rule-templates", token=token, body=tpl), "create template")
    ok(*http("POST", f"{API}/rule-templates/{created['id']}/publish", token=token), "publish template")
    return created["id"]


def start_job(token: str, template_id: int, source_ref: Any) -> dict:
    return ok(*http("POST", f"{API}/jobs", token=token,
                    body={"rule_template_id": template_id, "source_ref": source_ref}), "start job")


def wait_job(token: str, job_id: int, timeout_s: int = JOB_TIMEOUT_S) -> dict:
    deadline = time.monotonic() + timeout_s
    job: dict = {}
    while time.monotonic() < deadline:
        job = ok(*http("GET", f"{API}/jobs/{job_id}", token=token), "get job")
        if job.get("status") in TERMINAL:
            return job
        time.sleep(1)
    raise HarnessError(f"job {job_id} not terminal after {timeout_s}s (status={job.get('status')})")


def job_rows(token: str, job_id: int) -> list[dict]:
    rows: list[dict] = []
    cursor = None
    while True:
        q = f"?limit=1000" + (f"&cursor={cursor}" if cursor is not None else "")
        page = ok(*http("GET", f"{API}/jobs/{job_id}/rows{q}", token=token), "rows")
        rows.extend(page["items"])
        if len(page["items"]) < 1000:
            return rows
        cursor = page["next_cursor"]


# --------------------------------------------------------------------- reconcile


@dataclass
class Reconciliation:
    checks: list[dict] = field(default_factory=list)
    dispositions: dict[str, dict] = field(default_factory=dict)

    def check(self, name: str, passed: bool, detail: Any = None) -> None:
        self.checks.append({"check": name, "passed": bool(passed), "detail": detail})

    @property
    def passed(self) -> bool:
        return all(c["passed"] for c in self.checks)


def reconcile(*, tenant: str, source: list[dict], rows: list[dict], receipts: list[dict],
              all_receipts: list[dict], job: dict, expect_failed: set[str],
              expect_ambiguous: set[str] | None = None,
              expect_unresolved: set[str] | None = None) -> Reconciliation:
    """Compare source identities, platform rows and destination receipts.

    Invariants (handover §6): every source record has exactly one accounted-for
    outcome; succeeded rows have exactly one destination effect; failed rows have
    none unless declared ambiguous; unresolved rows (ADR 0003) have at most one
    and are never reported as succeeded; no effect exists without a source
    record; no effect lands in another tenant; completion agrees with row
    outcomes.
    """
    expect_ambiguous = expect_ambiguous or set()
    expect_unresolved = expect_unresolved or set()
    rec = Reconciliation()
    source_keys = [r["external_id"] for r in source]
    rows_by_key: dict[str, list[dict]] = {}
    for r in rows:
        key = (r.get("row_json") or {}).get("external_id")
        rows_by_key.setdefault(str(key), []).append(r)
    receipts_by_key: dict[str, list[dict]] = {}
    for r in receipts:
        receipts_by_key.setdefault(r["external_id"], []).append(r)

    missing_rows = [k for k in source_keys if k not in rows_by_key]
    dup_rows = [k for k, v in rows_by_key.items() if len(v) > 1]
    rec.check("every source record has a platform row", not missing_rows, missing_rows[:20])
    rec.check("no duplicate platform rows per source record", not dup_rows, dup_rows[:20])
    rec.check("platform row count equals source count", len(rows) == len(source_keys),
              {"rows": len(rows), "source": len(source_keys)})

    bad: list[dict] = []
    for key in source_keys:
        row = (rows_by_key.get(key) or [{}])[0]
        status = row.get("status", "missing")
        n = len(receipts_by_key.get(key, []))
        rec.dispositions[key] = {"row_index": row.get("row_index"), "platform": status, "receipts": n}
        if key in expect_ambiguous:
            # Zero or one effect is acceptable, but the platform may only
            # claim success when the effect exists.
            if n > 1:
                bad.append({"key": key, "why": "ambiguous write produced duplicate effects", "receipts": n})
            if status == "succeeded" and n != 1:
                bad.append({"key": key, "why": f"succeeded with {n} destination receipts"})
            continue
        if key in expect_unresolved:
            if status != "unresolved":
                bad.append({"key": key, "why": f"platform status {status}, expected unresolved"})
            if n > 1:
                bad.append({"key": key, "why": f"unresolved row produced {n} destination effects"})
            continue
        want = "failed" if key in expect_failed else "succeeded"
        if status != want:
            bad.append({"key": key, "why": f"platform status {status}, expected {want}"})
        if status == "succeeded" and n != 1:
            bad.append({"key": key, "why": f"succeeded with {n} destination receipts"})
        if status == "failed" and n != 0:
            bad.append({"key": key, "why": f"failed but destination holds {n} receipts"})
    rec.check("dispositions match expectations and destination receipts", not bad, bad[:20])

    orphan = sorted(set(receipts_by_key) - set(source_keys))
    rec.check("no destination effect without a source record", not orphan, orphan[:20])
    foreign = [r["id"] for r in all_receipts if r["tenant"] != tenant and r["external_id"] in set(source_keys)]
    rec.check("no effect written to another tenant", not foreign, foreign[:20])

    n_failed = sum(1 for r in rows if r["status"] == "failed")
    n_unresolved = sum(1 for r in rows if r["status"] == "unresolved")
    n_ok = sum(1 for r in rows if r["status"] == "succeeded")
    want_status = "failed" if n_failed or n_unresolved else "succeeded"
    rec.check("job terminal status agrees with row outcomes", job.get("status") == want_status,
              {"job": job.get("status"), "expected": want_status})
    totals = job.get("totals_json") or {}
    rec.check("job totals agree with persisted rows and source",
              totals.get("processed") == n_ok and totals.get("failed") == n_failed
              and totals.get("unresolved", 0) == n_unresolved
              and totals.get("total") == len(rows) == len(source_keys),
              {"totals_json": totals, "rows_succeeded": n_ok, "rows_failed": n_failed,
               "rows_unresolved": n_unresolved, "rows": len(rows), "source": len(source_keys)})
    return rec


def attempt_sequence_check(attempts: list[dict], *, tenant: str, external_id: str,
                           statuses: list[int]) -> dict:
    """The destination saw exactly ``statuses`` for one record, under one idempotency key."""
    seen = sorted((a for a in attempts if a.get("tenant") == tenant and a.get("external_id") == external_id),
                  key=lambda a: a["seq"])
    got = [a["status"] for a in seen]
    keys = {a.get("idempotency_key") for a in seen}
    return {"check": f"destination saw {statuses} for {external_id} under one idempotency key",
            "passed": got == statuses and len(keys) == 1 and None not in keys,
            "detail": {"statuses": got, "idempotency_keys": sorted(k or "" for k in keys)}}


# --------------------------------------------------------------------- scenarios


@dataclass
class Scenario:
    name: str
    description: str
    run: Callable[["Scenario"], dict]


def run_customer_migration(*, tenant: str, faults: list[dict], expect_failed: set[str],
                           expect_ambiguous: set[str] | None = None, dest_config: dict | None = None,
                           expect_unresolved: set[str] | None = None,
                           retry: dict | None = None,
                           customize: Callable[[dict], None] | None = None,
                           extra_checks: Callable[[list[dict], list[dict]], list[dict]] | None = None,
                           job_checks: Callable[[dict], list[dict]] | None = None,
                           source_path: Path | None = None, job_timeout_s: int = JOB_TIMEOUT_S,
                           template_id: int | None = None) -> dict:
    run_id = uuid.uuid4().hex[:8]
    dest_reset(dest_config, faults)
    token = login(tenant)
    source_path = source_path or DATA / tenant / "customers.csv"
    with source_path.open(newline="", encoding="utf-8") as f:
        source = list(csv.DictReader(f))
    source_ref = multipart_upload(token, source_path)
    tpl_id = template_id
    if tpl_id is None:
        tpl = customer_template(tenant, run_id, retry)
        if customize:
            customize(tpl["schema_json"])
        tpl_id = create_published_template(token, tpl)
    job = start_job(token, tpl_id, source_ref)
    job = wait_job(token, job["id"], job_timeout_s)
    rows = job_rows(token, job["id"])
    receipts = dest_receipts(tenant)
    rec = reconcile(tenant=tenant, source=source, rows=rows, receipts=receipts,
                    all_receipts=dest_receipts(), job=job, expect_failed=expect_failed,
                    expect_ambiguous=expect_ambiguous, expect_unresolved=expect_unresolved)
    checks = rec.checks + (extra_checks(source, receipts) if extra_checks else [])
    checks += job_checks(job) if job_checks else []
    return {
        "passed": all(c["passed"] for c in checks),
        "job": {k: job.get(k) for k in ("id", "status", "totals_json", "temporal_workflow_id")},
        "template_id": tpl_id,
        "row_errors": sorted({(r.get("last_error") or "")[:200] for r in rows if r.get("last_error")})[:10],
        "source": {"path": str(source_path.relative_to(REPO)), "records": len(source),
                   "sha256": hashlib.sha256(source_path.read_bytes()).hexdigest()},
        "faults": faults,
        "checks": checks,
        "dispositions": rec.dispositions,
        "destination_attempts": len(dest_attempts()),
        "destination_receipts": len(receipts),
    }


def scenario_ordinary(_: Scenario) -> dict:
    return run_customer_migration(tenant="org-a", faults=[], expect_failed=set())


def scenario_forced_failure(_: Scenario) -> dict:
    # Row zero is deliberately among the rejected records (MP-09 boundary).
    rejected = {"A-CUS-0000", "A-CUS-0007"}
    faults = [{"id": f"reject-{k}", "tenant": "org-a", "external_id": k, "action": "reject", "status": 400}
              for k in sorted(rejected)]
    return run_customer_migration(tenant="org-a", faults=faults, expect_failed=rejected)


def scenario_throttle_recovers(_: Scenario) -> dict:
    faults = [{"id": "throttle-once", "tenant": "org-a", "external_id": "A-CUS-0003",
               "action": "throttle", "times": 2}]
    return run_customer_migration(
        tenant="org-a", faults=faults, expect_failed=set(),
        extra_checks=lambda _src, _rcpt: [attempt_sequence_check(
            dest_attempts(), tenant="org-a", external_id="A-CUS-0003", statuses=[429, 429, 201])])


def scenario_python_transforms(_: Scenario) -> dict:
    """Python preprocess + $py payload expression subscripting the row (MP-06)."""
    def customize(schema: dict) -> None:
        schema["preprocess"] = [{"id": "lower_country", "field": "country", "fn": "python",
                                 "code": "result = value.lower() if value else ''"}]
        schema["mapping"]["payload"]["email_domain"] = {"$py": "row['email'].split('@')[1]"}

    def payload_checks(source: list[dict], receipts: list[dict]) -> list[dict]:
        expected = {r["external_id"]: {"country": r["country"].lower(), "email_domain": r["email"].split("@")[1]}
                    for r in source}
        wrong = []
        for rec in receipts:
            got = rec.get("payload") or {}
            want = expected.get(rec["external_id"], {})
            if any(got.get(k) != v for k, v in want.items()):
                wrong.append({"external_id": rec["external_id"], "got": {k: got.get(k) for k in want}, "want": want})
        return [{"check": "python-transformed fields match independently computed values",
                 "passed": not wrong and len(receipts) == len(source), "mismatches": wrong[:5]}]

    return run_customer_migration(tenant="org-a", faults=[], expect_failed=set(),
                                  customize=customize, extra_checks=payload_checks)


def generated_customers(tenant: str, count: int) -> Path:
    """Deterministic synthetic customers, larger than one ingest batch."""
    prefix = tenant[-1].upper()
    path = OUT / "generated" / f"{tenant}-customers-{count}.csv"
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(["external_id", "email", "name", "country"])
        for i in range(count):
            w.writerow([f"{prefix}-GEN-{i:05d}", f"gen{i:05d}@{tenant}.example.test",
                        f"Generated Customer {prefix}{i:05d}", ("US", "IN", "DE", "GB")[i % 4]])
    return path


def workflow_runs(workflow_id: str) -> list[dict]:
    q = urllib.parse.quote(f'WorkflowId="{workflow_id}"')
    payload = ok(*http("GET", f"{TEMPORAL_HTTP}/api/v1/namespaces/default/workflows?query={q}&pageSize=100"),
                 "list workflow runs")
    return payload.get("executions") or []


def scenario_continue_as_new(_: Scenario) -> dict:
    """1,250 rows span three ingest batches and at least one continue-as-new (MP-02)."""
    def job_checks(job: dict) -> list[dict]:
        runs = workflow_runs(job["temporal_workflow_id"])
        statuses = sorted(r.get("status", "") for r in runs)
        return [{"check": "job continued as new at least once and its last run completed",
                 "passed": len(runs) >= 2 and statuses.count("WORKFLOW_EXECUTION_STATUS_COMPLETED") == 1
                 and all(s in ("WORKFLOW_EXECUTION_STATUS_COMPLETED", "WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW")
                         for s in statuses),
                 "detail": {"runs": len(runs), "statuses": statuses}}]

    return run_customer_migration(tenant="org-a", faults=[], expect_failed=set(), job_checks=job_checks,
                                  source_path=generated_customers("org-a", 1250),
                                  job_timeout_s=max(JOB_TIMEOUT_S, 900))


def latest_job_id(token: str) -> int | None:
    items = ok(*http("GET", f"{API}/jobs?limit=1", token=token), "list jobs")["items"]
    return items[0]["id"] if items else None


def scenario_source_authz(_: Scenario) -> dict:
    """Org B names org A's upload (by ref, id and key) plus local/URL sources."""
    dest_reset()
    token_a, token_b = login("org-a"), login("org-b")
    a_ref = multipart_upload(token_a, DATA / "org-a" / "customers.csv")
    tpl_b = create_published_template(token_b, customer_template("org-b", uuid.uuid4().hex[:8]))
    before = latest_job_id(token_b)

    attempts = {
        "job: org A source_ref": ("/jobs", {"rule_template_id": tpl_b, "source_ref": a_ref}),
        "job: org A source_id": ("/jobs", {"rule_template_id": tpl_b, "source_ref": {"source_id": a_ref["source_id"]}}),
        "job: local path": ("/jobs", {"rule_template_id": tpl_b, "source_ref": {"type": "csv", "path": "/etc/passwd"}}),
        "job: metadata url": ("/jobs", {"rule_template_id": tpl_b,
                                        "source_ref": {"type": "csv", "url": "http://169.254.169.254/latest/meta-data/"}}),
        "job: JSON string": ("/jobs", {"rule_template_id": tpl_b, "source_ref": json.dumps(a_ref)}),
        "sample: org A key": ("/files/sample", {"bucket": a_ref["bucket"], "key": a_ref["key"], "format": "csv"}),
        "sample: org A source_id": ("/files/sample", {"source_id": a_ref["source_id"]}),
        "batch: org A source_id": ("/batches", {"source_ref": {"source_id": a_ref["source_id"]}}),
        "watched connector over org A uploads": ("/connectors", {
            "name": f"harness-watch-{uuid.uuid4().hex[:6]}", "connector_kind": "watched_prefix",
            "config_json": {"bucket": a_ref["bucket"], "prefix": a_ref["key"].rsplit("/", 2)[0] + "/"}}),
    }
    checks: list[dict] = []
    for name, (path, body) in attempts.items():
        status, payload = http("POST", f"{API}{path}", token=token_b, body=body)
        checks.append({"check": name, "passed": 400 <= status < 500, "status": status,
                       "detail": payload if isinstance(payload, str) else (payload or {}).get("error")})
    after = latest_job_id(token_b)
    checks.append({"check": "no org B job row created", "passed": after == before,
                   "before": before, "after": after})
    receipts = dest_receipts("org-b")
    checks.append({"check": "no org B destination receipts", "passed": not receipts, "receipts": len(receipts)})
    return {"passed": all(c["passed"] for c in checks), "checks": checks, "org_a_source_id": a_ref["source_id"]}


# ------------------------------------------------- operation ledger (ADR 0003)


def attempts_for(tenant: str, external_id: str) -> list[int]:
    return [a["status"] for a in sorted(dest_attempts(), key=lambda a: a["seq"])
            if a.get("tenant") == tenant and a.get("external_id") == external_id]


def receipts_for(tenant: str, external_id: str) -> int:
    return sum(1 for r in dest_receipts(tenant) if r["external_id"] == external_id)


def exact(name: str, got: Any, want: Any) -> dict:
    return {"check": name, "passed": got == want, "detail": {"got": got, "want": want}}


def wait_retry(job: dict, timeout_s: int = 120, suffix: str = "retry") -> str:
    """Wait for the job's retry (or export) workflow and return its final status."""
    wid = f"{job['temporal_workflow_id']}-{suffix}"
    deadline = time.monotonic() + timeout_s
    status = ""
    while time.monotonic() < deadline:
        runs = workflow_runs(wid)
        status = runs[0].get("status", "") if runs else ""
        if status and status != "WORKFLOW_EXECUTION_STATUS_RUNNING":
            return status
        time.sleep(1)
    raise HarnessError(f"retry workflow {wid} still {status or 'absent'} after {timeout_s}s")


def row_by_key(token: str, job_id: int, external_id: str) -> dict:
    for r in job_rows(token, job_id):
        if (r.get("row_json") or {}).get("external_id") == external_id:
            return r
    raise HarnessError(f"job {job_id} has no row for {external_id}")


def disconnect_after_commit(external_id: str) -> dict:
    return {"id": f"commit-drop-{external_id}", "tenant": "org-a", "external_id": external_id,
            "action": "commit_then_disconnect", "times": 1}


def scenario_ambiguous_idempotent(_: Scenario) -> dict:
    """Response lost after commit; verified deduplication: re-sent with the same key, one effect."""
    key = "A-CUS-0005"
    return run_customer_migration(
        tenant="org-a", faults=[disconnect_after_commit(key)], expect_failed=set(),
        customize=lambda s: s["destination"].update({"delivery": {"mode": "idempotent"}}),
        extra_checks=lambda _src, _rcpt: [attempt_sequence_check(
            dest_attempts(), tenant="org-a", external_id=key, statuses=[0, 200])])


def scenario_ambiguous_lookup(_: Scenario) -> dict:
    """Response lost after commit; no deduplication but a business-key lookup finds the effect."""
    key = "A-CUS-0005"

    def customize(schema: dict) -> None:
        schema["destination"]["delivery"] = {"mode": "lookup", "lookup": {
            "url": f"{DEST_INTERNAL}/v1/customers",
            "queryParams": {"external_id": {"$from": "external_id"}},
            "headers": {"X-Tenant": "org-a"},
            "foundPath": "$.data[0].id"}}

    return run_customer_migration(
        tenant="org-a", faults=[disconnect_after_commit(key)], expect_failed=set(),
        dest_config={"idempotency": False, "unique_business_key": True}, customize=customize,
        extra_checks=lambda _src, _rcpt: [
            exact(f"the destination saw one create for {key} (the lookup supplied the receipt)",
                  attempts_for("org-a", key), [0])])


def scenario_ambiguous_manual(_: Scenario) -> dict:
    """POST without a delivery policy: unknown outcomes stop as unresolved; only a confirmed retry re-sends."""
    committed, not_committed = "A-CUS-0005", "A-CUS-0011"
    faults = [disconnect_after_commit(committed),
              {"id": "503-once", "tenant": "org-a", "external_id": not_committed,
               "action": "server_error", "status": 503, "times": 1}]

    def job_checks(job: dict) -> list[dict]:
        token = login("org-a")
        checks = [
            exact(f"{committed}: sent once, effect kept, never re-sent", attempts_for("org-a", committed), [0]),
            exact(f"{not_committed}: sent once and not re-sent", attempts_for("org-a", not_committed), [503]),
        ]
        rows = {k: row_by_key(token, job["id"], k) for k in (committed, not_committed)}
        status, body = http("POST", f"{API}/jobs/{job['id']}/rows/{rows[committed]['row_index']}/retry",
                            token=token, body={})
        checks.append(exact("retrying an unresolved row without confirm_not_applied is refused (409)", status, 409))
        status, body = http("POST", f"{API}/jobs/{job['id']}/rows/{rows[not_committed]['row_index']}/retry",
                            token=token, body={"confirm_not_applied": True})
        checks.append(exact("a confirmed retry is accepted", status, 200))
        if status == 200:
            checks.append(exact("the confirmed retry workflow completed", wait_retry(job),
                                "WORKFLOW_EXECUTION_STATUS_COMPLETED"))
            checks.append(exact(f"{not_committed}: confirmed retry sent it once more",
                                attempts_for("org-a", not_committed), [503, 201]))
            checks.append(exact(f"{not_committed}: exactly one effect", receipts_for("org-a", not_committed), 1))
            after = row_by_key(token, job["id"], not_committed)
            checks.append(exact(f"{not_committed}: row succeeded after the confirmed retry", after["status"], "succeeded"))
            final = ok(*http("GET", f"{API}/jobs/{job['id']}", token=token), "get job")
            checks.append(exact("job stays failed while a row is unresolved", final["status"], "failed"))
            checks.append(exact("job totals count the remaining unresolved row",
                                (final.get("totals_json") or {}).get("unresolved"), 1))
        checks.append(exact(f"{committed}: still exactly one effect", receipts_for("org-a", committed), 1))
        return checks

    return run_customer_migration(
        tenant="org-a", faults=faults, expect_failed=set(), expect_unresolved={committed, not_committed},
        dest_config={"idempotency": False}, job_checks=job_checks)


def decoded_payloads(node: Any, out: list[str]) -> None:
    if isinstance(node, dict):
        for k, v in node.items():
            if k == "data" and isinstance(v, str):
                try:
                    out.append(base64.b64decode(v, validate=True).decode("utf-8", "replace"))
                except ValueError:
                    pass
            decoded_payloads(v, out)
    elif isinstance(node, list):
        for v in node:
            decoded_payloads(v, out)


def workflow_history_text(workflow_id: str) -> str:
    """Every event of every run of a workflow, with payloads base64-decoded."""
    parts: list[str] = []
    for run in workflow_runs(workflow_id):
        run_id = run["execution"]["runId"]
        page_token = ""
        while True:
            q = f"execution.runId={run_id}&maximumPageSize=1000"
            if page_token:
                q += f"&nextPageToken={urllib.parse.quote(page_token)}"
            page = ok(*http("GET", f"{TEMPORAL_HTTP}/api/v1/namespaces/default/workflows/"
                                   f"{urllib.parse.quote(workflow_id)}/history?{q}"), "workflow history")
            parts.append(json.dumps(page))
            decoded_payloads(page, parts)
            page_token = page.get("nextPageToken") or ""
            if not page_token:
                break
    return "\n".join(parts)


def service_logs(since: str) -> tuple[bool, str]:
    cmd = COMPOSE.split() + ["logs", "--no-color", "--since", since, "api", "orchestrator"]
    res = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, check=False)
    return res.returncode == 0, res.stdout + res.stderr


def scenario_destination_auth(_: Scenario) -> dict:
    """auth.secretRef resolves per call: missing, other-org, rotated and revoked secrets; values never leak."""
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 5))
    tok1, tok2 = f"hx-{uuid.uuid4().hex}", f"hx-{uuid.uuid4().hex}"
    name, other_org_name = f"hx_crm_{uuid.uuid4().hex[:6]}", f"hx_orgb_{uuid.uuid4().hex[:6]}"
    token_a, token_b = login("org-a"), login("org-b")
    ok(*http("PUT", f"{API}/secrets/{name}", token=token_a, body={"value": tok1}), "create secret")
    ok(*http("PUT", f"{API}/secrets/{other_org_name}", token=token_b, body={"value": tok1}), "org B secret")
    source_path = generated_customers("org-a", 5)
    every = {f"A-GEN-{i:05d}" for i in range(5)}

    def with_auth(ref: str) -> Callable[[dict], None]:
        return lambda s: s["destination"].update({"auth": {"type": "bearer", "secretRef": ref}})

    def nothing_sent(_src: list[dict], _rcpt: list[dict]) -> list[dict]:
        return [exact("nothing was sent to the destination", len(dest_attempts()), 0)]

    phases: dict[str, dict] = {}
    phases["authorized"] = run_customer_migration(
        tenant="org-a", faults=[], expect_failed=set(), source_path=source_path,
        dest_config={"required_token": tok1}, customize=with_auth(name))
    template_id = phases["authorized"]["template_id"]
    phases["missing secret"] = run_customer_migration(
        tenant="org-a", faults=[], expect_failed=every, source_path=source_path,
        dest_config={"required_token": tok1}, customize=with_auth(f"hx_missing_{uuid.uuid4().hex[:6]}"),
        extra_checks=nothing_sent)
    phases["other org's secret"] = run_customer_migration(
        tenant="org-a", faults=[], expect_failed=every, source_path=source_path,
        dest_config={"required_token": tok1}, customize=with_auth(other_org_name), extra_checks=nothing_sent)
    ok(*http("PUT", f"{API}/secrets/{name}", token=token_a, body={"value": tok2}), "rotate secret")
    phases["rotated (same template)"] = run_customer_migration(
        tenant="org-a", faults=[], expect_failed=set(), source_path=source_path,
        dest_config={"required_token": tok2}, template_id=template_id)
    ok(*http("DELETE", f"{API}/secrets/{name}", token=token_a), "revoke secret")
    phases["revoked (same template)"] = run_customer_migration(
        tenant="org-a", faults=[], expect_failed=every, source_path=source_path,
        dest_config={"required_token": tok2}, template_id=template_id, extra_checks=nothing_sent)

    checks: list[dict] = []
    for phase, result in phases.items():
        for c in result["checks"]:
            checks.append({**c, "check": f"{phase}: {c['check']}"})
    for phase in ("missing secret", "other org's secret", "revoked (same template)"):
        errors = phases[phase]["row_errors"]
        checks.append({"check": f"{phase}: rows say the secret is unavailable",
                       "passed": bool(errors) and all("not available" in e for e in errors), "detail": errors})

    haystack = "".join(json.dumps(p) for p in phases.values())
    for result in phases.values():
        wid = result["job"]["temporal_workflow_id"]
        haystack += workflow_history_text(wid)
    logs_ok, logs = service_logs(started)
    checks.append({"check": "service logs were collected", "passed": logs_ok and len(logs) > 0,
                   "detail": logs[-300:] if not logs_ok else len(logs)})
    leaked = [label for label, tok in (("v1", tok1), ("v2", tok2)) if tok in haystack or tok in logs]
    checks.append({"check": "secret values absent from workflow history, job rows and service logs",
                   "passed": not leaked, "detail": {"leaked": leaked, "history_bytes": len(haystack),
                                                    "log_bytes": len(logs)}})
    return {"passed": all(c["passed"] for c in checks), "checks": checks,
            "jobs": {p: r["job"] for p, r in phases.items()}}


def scenario_retry_serialized(_: Scenario) -> dict:
    """Retries are refused while the job runs; two concurrent retries of one job start one workflow."""
    rejected = {"A-CUS-0000", "A-CUS-0007"}
    faults = [{"id": f"reject-{k}", "tenant": "org-a", "external_id": k, "action": "reject", "status": 400}
              for k in sorted(rejected)]
    dest_reset(None, faults)
    token = login("org-a")
    tpl_id = create_published_template(token, customer_template("org-a", uuid.uuid4().hex[:8]))
    job = start_job(token, tpl_id, multipart_upload(token, DATA / "org-a" / "customers.csv"))
    checks: list[dict] = []
    status, body = http("POST", f"{API}/jobs/{job['id']}/retry-failed", token=token, body={})
    checks.append(exact("retry while the job is pending or running is refused (409)", status, 409))

    job = wait_job(token, job["id"])
    statuses: list[int] = []

    def fire(path: str) -> None:
        statuses.append(http("POST", f"{API}{path}", token=token, body={})[0])

    rows = {k: row_by_key(token, job["id"], k) for k in sorted(rejected)}
    threads = [threading.Thread(target=fire, args=(p,)) for p in (
        f"/jobs/{job['id']}/retry-failed",
        f"/jobs/{job['id']}/rows/{rows['A-CUS-0007']['row_index']}/retry")]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    checks.append(exact("two concurrent retries: one accepted, one refused (409)", sorted(statuses), [200, 409]))
    if 200 in statuses:
        checks.append(exact("the accepted retry completed", wait_retry(job), "WORKFLOW_EXECUTION_STATUS_COMPLETED"))
    runs = workflow_runs(f"{job['temporal_workflow_id']}-retry")
    checks.append(exact("exactly one retry workflow ran", len(runs), 1))
    for k in sorted(rejected):
        checks.append(exact(f"{k}: still rejected, no effect", receipts_for("org-a", k), 0))
    return {"passed": all(c["passed"] for c in checks), "checks": checks,
            "job": {"id": job["id"], "temporal_workflow_id": job["temporal_workflow_id"]}}


# ---------------------------------------------------------------- job lifecycle


def compose_exec(service: str, *args: str) -> str:
    cmd = COMPOSE.split() + ["exec", "-T", service, *args]
    res = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, check=False)
    if res.returncode != 0:
        raise HarnessError(f"{service} {' '.join(args[:3])}: {res.stderr.strip() or res.stdout.strip()}")
    return res.stdout


def mc(script: str) -> str:
    """Runs an `mc` script inside the MinIO container against the local server."""
    return compose_exec("minio", "sh", "-c",
                        'mc alias set local http://localhost:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD"'
                        f" >/dev/null && {script}")


def get_job(token: str, job_id: int) -> dict:
    return ok(*http("GET", f"{API}/jobs/{job_id}", token=token), "get job")


def wait_until(what: str, probe: Callable[[], Any], timeout_s: float, interval_s: float = 1) -> Any:
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        value = probe()
        if value:
            return value
        time.sleep(interval_s)
    raise HarnessError(f"timed out after {timeout_s}s waiting for {what}")


def download_rows(token: str, job_id: int, failed: bool = False) -> int:
    q = "?failed=true" if failed else ""
    status, body = http("GET", f"{API}/jobs/{job_id}/results{q}", token=token)
    text = ok(status, body, "download results")
    return max(0, len([ln for ln in str(text).splitlines() if ln.strip()]) - 1)


def persisted_totals(rows: list[dict]) -> dict:
    count = lambda s: sum(1 for r in rows if r["status"] == s)  # noqa: E731
    return {"processed": count("succeeded"), "failed": count("failed"),
            "unresolved": count("unresolved"), "total": len(rows)}


def scenario_ingest_failure(_: Scenario) -> dict:
    """The source object is gone when the job starts: the job fails visibly with the cause."""
    dest_reset()
    token = login("org-a")
    tpl_id = create_published_template(token, customer_template("org-a", uuid.uuid4().hex[:8]))
    ref = multipart_upload(token, DATA / "org-a" / "customers.csv")
    mc(f"mc rm local/{ref['bucket']}/{ref['key']}")
    job = wait_job(token, start_job(token, tpl_id, ref)["id"], 180)
    checks = [
        exact("job failed", job["status"], "failed"),
        {"check": "the job records the ingest failure", "passed": "ingest" in (job.get("last_error") or ""),
         "detail": job.get("last_error")},
        exact("nothing was sent to the destination", len(dest_attempts()), 0),
        exact("job totals agree with persisted rows", job["totals_json"], persisted_totals(job_rows(token, job["id"]))),
    ]
    return {"passed": all(c["passed"] for c in checks), "checks": checks,
            "job": {k: job.get(k) for k in ("id", "status", "last_error", "export_status")}}


def scenario_terminated_workflow(_: Scenario) -> dict:
    """A workflow ends without finalizing (terminated mid-row): the reconciler marks the job failed."""
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 5))
    held = "A-CUS-0003"
    dest_reset(None, [{"id": "hold", "tenant": "org-a", "external_id": held,
                       "action": "timeout_before_write", "delay_ms": 45000, "times": 1}])
    token = login("org-a")
    tpl_id = create_published_template(token, customer_template("org-a", uuid.uuid4().hex[:8]))
    job = start_job(token, tpl_id, multipart_upload(token, DATA / "org-a" / "customers.csv"))
    wait_until(f"{held} to reach the destination", lambda: attempts_for("org-a", held), 60)
    compose_exec("temporal", "temporal", "workflow", "terminate", "--address", "temporal:7233",
                 "--workflow-id", job["temporal_workflow_id"], "--reason", "harness: execution lost")
    terminated = time.monotonic()
    receipts_at_termination = len(dest_receipts("org-a"))
    job = wait_job(token, job["id"], 180)
    repaired_after = round(time.monotonic() - terminated, 1)
    rows = job_rows(token, job["id"])
    _, logs = service_logs(started)
    checks = [
        exact("job failed", job["status"], "failed"),
        {"check": "the job says its workflow was terminated", "passed": "terminated" in (job.get("last_error") or ""),
         "detail": job.get("last_error")},
        {"check": "repaired by the reconciler within 90s", "passed": repaired_after < 90, "detail": repaired_after},
        {"check": "the reconciler logged the repair",
         "passed": "job reconciled" in logs and f'"jobId":{job["id"]}' in logs, "detail": None},
        exact("job totals agree with persisted rows", job["totals_json"], persisted_totals(rows)),
        exact(f"{held}: no effect", receipts_for("org-a", held), 0),
        exact("no destination effects after termination", len(dest_receipts("org-a")), receipts_at_termination),
    ]
    return {"passed": all(c["passed"] for c in checks), "checks": checks,
            "job": {k: job.get(k) for k in ("id", "status", "last_error", "temporal_workflow_id")},
            "repaired_after_s": repaired_after}


def scenario_export_recovery(_: Scenario) -> dict:
    """The results upload fails: the outcome stands, and an export-only retry makes zero destination calls."""
    dest_reset()
    token = login("org-a")
    tpl_id = create_published_template(token, customer_template("org-a", uuid.uuid4().hex[:8]))
    ref = multipart_upload(token, DATA / "org-a" / "customers.csv")
    # A 1-byte hard quota makes MinIO refuse every upload; reads still work.
    mc(f"mc quota set local/{ref['bucket']} --size 1B")
    try:
        job = start_job(token, tpl_id, ref)
        job = wait_job(token, job["id"])
        job = wait_until("the export failure to be recorded",
                         lambda: (j := get_job(token, job["id"]))["export_status"] == "failed" and j, 180)
    finally:
        mc(f"mc quota clear local/{ref['bucket']}")
    source_count = len(read_source("org-a", "customers"))
    checks = [
        exact("migration succeeded despite the export failure", job["status"], "succeeded"),
        {"check": "export failure is visible on the job", "passed": bool(job.get("export_error")),
         "detail": job.get("export_error")},
        exact("results still download (from the database)", download_rows(token, job["id"]), source_count),
    ]
    attempts, receipts = len(dest_attempts()), len(dest_receipts())

    status, body = http("POST", f"{API}/jobs/{job['id']}/export", token=token)
    checks.append(exact("export retry accepted (202)", status, 202))
    checks.append(exact("the export workflow completed", wait_retry(job, suffix="export"),
                        "WORKFLOW_EXECUTION_STATUS_COMPLETED"))
    job = get_job(token, job["id"])
    checks += [
        exact("export retry succeeded", job["export_status"], "succeeded"),
        exact("job outcome unchanged", job["status"], "succeeded"),
        exact("results file covers every row", (job.get("results_ref") or {}).get("rows"), source_count),
        exact("results download from the exported file", download_rows(token, job["id"]), source_count),
        exact("export retry made zero destination calls", len(dest_attempts()), attempts),
        exact("destination effects unchanged", len(dest_receipts()), receipts),
        exact("one export workflow ran", len(workflow_runs(f"{job['temporal_workflow_id']}-export")), 1),
    ]
    return {"passed": all(c["passed"] for c in checks), "checks": checks,
            "job": {k: job.get(k) for k in ("id", "status", "export_status", "export_error", "results_ref")}}


SCENARIOS: dict[str, Scenario] = {
    s.name: s
    for s in [
        Scenario("source-authz", "org B cannot start, sample or batch org A's source; path/url refs refused",
                 scenario_source_authz),
        Scenario("ordinary", "40 customers, no faults: one receipt per record", scenario_ordinary),
        Scenario("python-transforms", "python preprocess and $py row subscripting produce expected payloads",
                 scenario_python_transforms),
        Scenario("forced-failure", "destination rejects row 0 and row 7: failed rows have no receipts",
                 scenario_forced_failure),
        Scenario("throttle-recovers", "429 twice for one record, retried to exactly one receipt",
                 scenario_throttle_recovers),
        Scenario("ambiguous-idempotent", "response lost after commit, verified dedup: re-sent once, one effect",
                 scenario_ambiguous_idempotent),
        Scenario("ambiguous-lookup", "response lost after commit, lookup by business key: no re-send, one effect",
                 scenario_ambiguous_lookup),
        Scenario("ambiguous-manual", "unknown outcomes without a policy are unresolved; only confirmed retries re-send",
                 scenario_ambiguous_manual),
        Scenario("destination-auth", "secretRef: missing, other-org, rotated, revoked; values never in history/logs",
                 scenario_destination_auth),
        Scenario("retry-serialized", "retry refused while the job runs; concurrent retries start one workflow",
                 scenario_retry_serialized),
        Scenario("ingest-failure", "source object missing at start: job fails with the cause, nothing sent",
                 scenario_ingest_failure),
        Scenario("terminated-workflow", "workflow terminated mid-row: reconciler marks the job failed",
                 scenario_terminated_workflow),
        Scenario("export-recovery", "results upload fails: outcome stands; export-only retry makes zero writes",
                 scenario_export_recovery),
        Scenario("continue-as-new", "1,250 rows across continue-as-new: one receipt per record, totals carried",
                 scenario_continue_as_new),
    ]
}


# --------------------------------------------------------------------------- cli


def cmd_run(names: list[str]) -> int:
    preflight()
    OUT.mkdir(exist_ok=True)
    selected = names or list(SCENARIOS)
    unknown = [n for n in selected if n not in SCENARIOS]
    if unknown:
        raise HarnessError(f"unknown scenario(s): {', '.join(unknown)}")
    failed = []
    for name in selected:
        sc = SCENARIOS[name]
        print(f"==> {name}: {sc.description}")
        started = time.time()
        try:
            result = sc.run(sc)
        except HarnessError as e:
            result = {"passed": False, "error": str(e)}
        result.update({"scenario": name, "duration_s": round(time.time() - started, 1),
                       "api": API, "git_sha": git_sha()})
        (OUT / f"{name}.json").write_text(json.dumps(result, indent=2, default=str))
        for c in result.get("checks", []):
            print(f"    [{'PASS' if c['passed'] else 'FAIL'}] {c['check']}"
                  + ("" if c["passed"] else f": {json.dumps(c['detail'])[:400]}"))
        if "error" in result:
            print(f"    [ERROR] {result['error']}")
        print(f"    => {'PASSED' if result['passed'] else 'FAILED'} ({result['duration_s']}s)")
        if not result["passed"]:
            failed.append(name)
    print(f"\n{len(selected) - len(failed)}/{len(selected)} scenarios passed; evidence in {OUT.relative_to(REPO)}/")
    return 1 if failed else 0


def git_sha() -> str:
    res = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO, capture_output=True, text=True, check=False)
    return res.stdout.strip()


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("setup")
    sub.add_parser("list")
    r = sub.add_parser("run")
    r.add_argument("scenarios", nargs="*")
    args = p.parse_args()
    try:
        if args.cmd == "setup":
            setup()
            return 0
        if args.cmd == "list":
            for s in SCENARIOS.values():
                print(f"{s.name:22} {s.description}")
            return 0
        return cmd_run(args.scenarios)
    except HarnessError as e:
        print(f"harness: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
