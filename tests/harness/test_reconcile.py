"""Unit tests for the harness reconciler (no stack required).

    python3 -m unittest discover -s tests/harness -p 'test_*.py'
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import harness  # noqa: E402

SOURCE = [{"external_id": k} for k in ("K0", "K1", "K2")]


def row(i: int, key: str, status: str) -> dict:
    return {"row_index": i, "status": status, "row_json": {"external_id": key}}


def receipt(key: str, tenant: str = "org-a") -> dict:
    return {"id": f"r-{key}-{tenant}", "external_id": key, "tenant": tenant}


def run(rows, receipts, *, job_status="succeeded", totals=None, expect_failed=(), ambiguous=(), all_receipts=None):
    if totals is None:
        totals = {"processed": sum(r["status"] == "succeeded" for r in rows),
                  "failed": sum(r["status"] == "failed" for r in rows), "total": len(rows)}
    return harness.reconcile(
        tenant="org-a", source=SOURCE, rows=rows, receipts=receipts,
        all_receipts=receipts if all_receipts is None else all_receipts,
        job={"status": job_status, "totals_json": totals},
        expect_failed=set(expect_failed), expect_ambiguous=set(ambiguous),
    )


def failed_checks(rec):
    return [c["check"] for c in rec.checks if not c["passed"]]


class ReconcileTest(unittest.TestCase):
    def test_clean_run_passes(self):
        rec = run([row(i, k["external_id"], "succeeded") for i, k in enumerate(SOURCE)],
                  [receipt(k["external_id"]) for k in SOURCE])
        self.assertTrue(rec.passed, failed_checks(rec))

    def test_equal_counts_with_wrong_identity_fails(self):
        rows = [row(0, "K0", "succeeded"), row(1, "K1", "succeeded"), row(2, "K2", "succeeded")]
        receipts = [receipt("K0"), receipt("K1"), receipt("K1")]  # K2 missing, K1 duplicated
        rec = run(rows, receipts)
        self.assertIn("dispositions match expectations and destination receipts", failed_checks(rec))

    def test_missing_row_zero_fails(self):
        rows = [row(1, "K1", "succeeded"), row(2, "K2", "succeeded")]
        rec = run(rows, [receipt("K1"), receipt("K2")])
        self.assertIn("every source record has a platform row", failed_checks(rec))

    def test_failed_row_with_receipt_fails(self):
        rows = [row(0, "K0", "failed"), row(1, "K1", "succeeded"), row(2, "K2", "succeeded")]
        rec = run(rows, [receipt("K0"), receipt("K1"), receipt("K2")], job_status="failed", expect_failed={"K0"})
        self.assertIn("dispositions match expectations and destination receipts", failed_checks(rec))

    def test_expected_failure_passes(self):
        rows = [row(0, "K0", "failed"), row(1, "K1", "succeeded"), row(2, "K2", "succeeded")]
        rec = run(rows, [receipt("K1"), receipt("K2")], job_status="failed", expect_failed={"K0"})
        self.assertTrue(rec.passed, failed_checks(rec))

    def test_job_status_disagreement_fails(self):
        rows = [row(0, "K0", "failed"), row(1, "K1", "succeeded"), row(2, "K2", "succeeded")]
        rec = run(rows, [receipt("K1"), receipt("K2")], job_status="succeeded", expect_failed={"K0"})
        self.assertIn("job terminal status agrees with row outcomes", failed_checks(rec))

    def test_stale_totals_fail(self):
        rows = [row(i, k["external_id"], "succeeded") for i, k in enumerate(SOURCE)]
        rec = run(rows, [receipt(k["external_id"]) for k in SOURCE], totals={"processed": 2, "failed": 0, "total": 3})
        self.assertIn("job totals agree with persisted rows and source", failed_checks(rec))

    def test_orphan_and_foreign_tenant_effects_fail(self):
        rows = [row(i, k["external_id"], "succeeded") for i, k in enumerate(SOURCE)]
        receipts = [receipt(k["external_id"]) for k in SOURCE] + [receipt("ZZ")]
        rec = run(rows, receipts, all_receipts=receipts + [receipt("K0", tenant="org-b")])
        failed = failed_checks(rec)
        self.assertIn("no destination effect without a source record", failed)
        self.assertIn("no effect written to another tenant", failed)

    def test_ambiguous_allows_zero_or_one_but_not_two(self):
        rows = [row(0, "K0", "failed"), row(1, "K1", "succeeded"), row(2, "K2", "succeeded")]
        one = run(rows, [receipt("K0"), receipt("K1"), receipt("K2")], job_status="failed", ambiguous={"K0"})
        self.assertTrue(one.passed, failed_checks(one))
        two = run(rows, [receipt("K0"), receipt("K0"), receipt("K1"), receipt("K2")],
                  job_status="failed", ambiguous={"K0"})
        self.assertFalse(two.passed)

    def test_wrong_total_fails(self):
        rows = [row(i, k["external_id"], "succeeded") for i, k in enumerate(SOURCE)]
        rec = run(rows, [receipt(k["external_id"]) for k in SOURCE],
                  totals={"processed": 3, "failed": 0, "total": 999})
        self.assertIn("job totals agree with persisted rows and source", failed_checks(rec))

    def test_ambiguous_success_without_effect_fails(self):
        rows = [row(0, "K0", "succeeded"), row(1, "K1", "succeeded"), row(2, "K2", "succeeded")]
        rec = run(rows, [receipt("K1"), receipt("K2")], ambiguous={"K0"})
        self.assertIn("dispositions match expectations and destination receipts", failed_checks(rec))

    def test_attempt_sequence_requires_the_injected_fault(self):
        def attempt(seq, status, key="idem-1", ext="K0"):
            return {"seq": seq, "tenant": "org-a", "external_id": ext, "status": status, "idempotency_key": key}
        want = dict(tenant="org-a", external_id="K0", statuses=[429, 429, 201])
        ok = harness.attempt_sequence_check([attempt(1, 429), attempt(2, 429), attempt(3, 201)], **want)
        self.assertTrue(ok["passed"], ok)
        no_fault = harness.attempt_sequence_check([attempt(1, 201)], **want)
        self.assertFalse(no_fault["passed"])
        new_key = harness.attempt_sequence_check(
            [attempt(1, 429), attempt(2, 429, key="idem-2"), attempt(3, 201)], **want)
        self.assertFalse(new_key["passed"])

    def test_fixture_checksums_hold(self):
        harness.verify_fixture_checksums()


if __name__ == "__main__":
    unittest.main()
