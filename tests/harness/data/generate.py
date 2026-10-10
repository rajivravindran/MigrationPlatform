#!/usr/bin/env python3
"""Generate the immutable synthetic billing sources for the integrity harness.

Output is fully deterministic (fixed seed, no clock), committed to the repo and
pinned by SHA256SUMS. The harness refuses to run if a file no longer matches its
checksum, so a changed fixture is always an explicit, reviewed change:

    python3 tests/harness/data/generate.py && (cd tests/harness/data && sha256sum -c SHA256SUMS)

All data is synthetic. Do not replace it with real customer data.
"""

from __future__ import annotations

import csv
import hashlib
import random
from pathlib import Path

HERE = Path(__file__).resolve().parent
PLANS = [("starter", 900), ("growth", 4900), ("scale", 19900)]
COUNTRIES = ["US", "CA", "GB", "IN", "DE"]

ORGS = {
    # tenant: (seed, customers, invoices per subscription)
    "org-a": (1001, 40, 2),
    "org-b": (2002, 15, 1),
}


def generate_org(tenant: str, seed: int, n_customers: int, invoices_per_sub: int) -> dict[str, list[dict]]:
    rnd = random.Random(seed)
    prefix = tenant.split("-")[1].upper()
    customers, subscriptions, invoices = [], [], []
    for i in range(n_customers):
        cid = f"{prefix}-CUS-{i:04d}"
        customers.append(
            {
                "external_id": cid,
                "email": f"customer{i:04d}@{tenant}.example.test",
                "name": f"Synthetic Customer {prefix}{i:04d}",
                "country": rnd.choice(COUNTRIES),
            }
        )
        plan, price = rnd.choice(PLANS)
        sid = f"{prefix}-SUB-{i:04d}"
        subscriptions.append(
            {
                "external_id": sid,
                "customer_external_id": cid,
                "plan": plan,
                "amount_cents": price,
                "currency": "USD",
                "started_on": f"2026-{rnd.randint(1, 9):02d}-{rnd.randint(1, 28):02d}",
            }
        )
        for k in range(invoices_per_sub):
            invoices.append(
                {
                    "external_id": f"{prefix}-INV-{i:04d}-{k}",
                    "subscription_external_id": sid,
                    "customer_external_id": cid,
                    "amount_cents": price,
                    "currency": "USD",
                    "issued_on": f"2026-{k + 10:02d}-01",
                    "status": "paid" if rnd.random() < 0.85 else "open",
                }
            )
    return {"customers": customers, "subscriptions": subscriptions, "invoices": invoices}


def write_csv(path: Path, rows: list[dict]) -> None:
    with path.open("w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()), lineterminator="\n")
        w.writeheader()
        w.writerows(rows)


def main() -> None:
    files: list[Path] = []
    for tenant, (seed, n, inv) in ORGS.items():
        out = HERE / tenant
        out.mkdir(exist_ok=True)
        for entity, rows in generate_org(tenant, seed, n, inv).items():
            p = out / f"{entity}.csv"
            write_csv(p, rows)
            files.append(p)
    lines = []
    for p in sorted(files):
        digest = hashlib.sha256(p.read_bytes()).hexdigest()
        lines.append(f"{digest}  {p.relative_to(HERE).as_posix()}")
    (HERE / "SHA256SUMS").write_text("\n".join(lines) + "\n", encoding="utf-8")
    print("\n".join(lines))


if __name__ == "__main__":
    main()
