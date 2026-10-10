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
import csv
import hashlib
import io
import json
import os
import subprocess
import sys
import time
import urllib.error
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
              expect_ambiguous: set[str] | None = None) -> Reconciliation:
    """Compare source identities, platform rows and destination receipts.

    Invariants (handover §6): every source record has exactly one accounted-for
    outcome; succeeded rows have exactly one destination effect; failed rows have
    none unless declared ambiguous; no effect exists without a source record; no
    effect lands in another tenant; completion agrees with row outcomes.
    """
    expect_ambiguous = expect_ambiguous or set()
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
            if n > 1:
                bad.append({"key": key, "why": "ambiguous write produced duplicate effects", "receipts": n})
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
    n_ok = sum(1 for r in rows if r["status"] == "succeeded")
    want_status = "failed" if n_failed else "succeeded"
    rec.check("job terminal status agrees with row outcomes", job.get("status") == want_status,
              {"job": job.get("status"), "expected": want_status})
    totals = job.get("totals_json") or {}
    rec.check("job totals agree with persisted rows",
              totals.get("processed") == n_ok and totals.get("failed") == n_failed,
              {"totals_json": totals, "rows_succeeded": n_ok, "rows_failed": n_failed})
    return rec


# --------------------------------------------------------------------- scenarios


@dataclass
class Scenario:
    name: str
    description: str
    run: Callable[["Scenario"], dict]


def run_customer_migration(*, tenant: str, faults: list[dict], expect_failed: set[str],
                           expect_ambiguous: set[str] | None = None, dest_config: dict | None = None,
                           retry: dict | None = None) -> dict:
    run_id = uuid.uuid4().hex[:8]
    dest_reset(dest_config, faults)
    token = login(tenant)
    source_path = DATA / tenant / "customers.csv"
    source = read_source(tenant, "customers")
    source_ref = multipart_upload(token, source_path)
    tpl_id = create_published_template(token, customer_template(tenant, run_id, retry))
    job = start_job(token, tpl_id, source_ref)
    job = wait_job(token, job["id"])
    rows = job_rows(token, job["id"])
    receipts = dest_receipts(tenant)
    rec = reconcile(tenant=tenant, source=source, rows=rows, receipts=receipts,
                    all_receipts=dest_receipts(), job=job, expect_failed=expect_failed,
                    expect_ambiguous=expect_ambiguous)
    return {
        "passed": rec.passed,
        "job": {k: job.get(k) for k in ("id", "status", "totals_json", "temporal_workflow_id")},
        "template_id": tpl_id,
        "source": {"path": str(source_path.relative_to(REPO)), "records": len(source),
                   "sha256": hashlib.sha256(source_path.read_bytes()).hexdigest()},
        "faults": faults,
        "checks": rec.checks,
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
    return run_customer_migration(tenant="org-a", faults=faults, expect_failed=set())


SCENARIOS: dict[str, Scenario] = {
    s.name: s
    for s in [
        Scenario("ordinary", "40 customers, no faults: one receipt per record", scenario_ordinary),
        Scenario("forced-failure", "destination rejects row 0 and row 7: failed rows have no receipts",
                 scenario_forced_failure),
        Scenario("throttle-recovers", "429 twice for one record, retried to exactly one receipt",
                 scenario_throttle_recovers),
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
