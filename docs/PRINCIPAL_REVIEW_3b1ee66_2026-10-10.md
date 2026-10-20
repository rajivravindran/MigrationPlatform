# Principal engineering review — 3b1ee66

Date: 10 October 2026. Mode: review only.

## Executive assessment

**Customer-data production use remains blocked.** The latest work is useful progress toward a trustworthy synthetic migration path, but does not close the handover's access-control and migration-integrity blockers. The R00 harness provides independent destination receipts and identity reconciliation. MP-09 fixes initial row-zero selection when all workers run the new code. MP-08 now ties publication to successful CI for the selected SHA, but the validation pipeline still excludes the integrity harness.

Do not interpret this as a rejection of the agents' in-progress work: this review assesses the requested committed revision, not their unfinished local changes. Continue synthetic development; require integrated acceptance evidence before changing the release decision.

## Scope and evidence

- Target: `3b1ee66fdde3b5819fcc7420cc779a9273bea117`, explicitly confirmed by the user.
- Comparison: handover baseline `18160097d3e95994bf8018838c9b43b719bcac49` through target. Commits: `404a7f7` documentation, `40881f0` retry cursor, `a15ceca` publication gating, `3b1ee66` harness.
- Read the engineering handover, product roadmap, and supplied principal-engineer review guide. Their implementation kickoff instructions were treated as task context, not authorization to implement fixes.
- Initial working tree contained edits to CI and Rust lib/files/jobs/batches, plus untracked source migration/module/tests and ADRs. These were excluded from the committed assessment and preserved.
- Inspected committed files and extracted the target to `/tmp/mp-principal-review-3b1ee66` to avoid moving-agent changes. Separate Standards and Spec reviewers inspected the same target.
- Executed: `python3 -m unittest discover -s tests/harness -p 'test_*.py' -v` in that extraction: **10 passed**.
- Executed by the Spec reviewer: pinned-code counterexamples for wrong totals and ambiguous success; both incorrectly passed reconciliation.
- Executed by the Standards reviewer: a small cursor serialization reproduction confirmed omitted `after` decodes to zero in the old contract.
- Go fixture race tests and retry workflow tests were attempted, blocked by read-only Go cache access; the escalation request was rejected. They were **not verified in this review**.
- No stack, remote CI, Docker, deployment, live authorization attack, load test, or customer endpoint was exercised. R00_BASELINE.md records historical stack results; those are agent-reported evidence, not new results from this review.

## Architecture and critical flows

The web client calls the Rust API, which authenticates users and stores jobs/templates in Postgres. File bodies live in object storage. The API inserts a pending job and starts a Temporal workflow through the bridge. Go activities ingest sources, dispatch optional Python transforms, render HTTP operations, persist row/step outcomes, and export results. The new harness compares source identities, API-visible row outcomes, and the fixture's independent durable receipt ledger.

Critical boundaries: client source references → privileged storage/network access; API DB insert → Temporal start; destination HTTP commit → Postgres receipt persistence; execution completion → export availability. None of those pairs shares one atomic transaction. Destination deduplication is a destination contract, not a property established by the local hash alone.

The intended lifecycle is pending → running → terminal, but the current API and workflow can both write state without ordering protection. Retry uses saved row/step evidence; lost step evidence can therefore cause repeated external effects.

## Invariant and guarantee register

| Invariant / owner | Enforcement and evidence at target | Status |
|---|---|---|
| Organization-owned source reads / API + ingestion | Template ownership is checked; raw sampling bucket/key and execution source references remain accepted | Violated, MP-01 |
| Stable ingestion identity / workflow + connector | Fresh workflow initializes empty offset; Salesforce iterator discards page position | Violated, MP-02/03 |
| Durable confirmed effects / HTTP + persistence | Step-persistence errors ignored; destination idempotency optional | Violated, MP-04 |
| Terminal state agrees with execution / API + workflow | Unconditional running update; early errors bypass finalization | Violated, MP-05 |
| Retry includes row zero / workflow + activity | Nullable cursor in new implementation; old workers interpret omission as zero | Improved; rollout gap S1 |
| Approved SHA passes integrity validation / release workflow | Exact SHA checked; harness absent from CI | Partial, C1 |
| Reconciliation proves declared result / harness | Independent identity checks, but total and throttle assertions incomplete | Partial, C2/C3 |

Delivery: retries can repeat an external request. Consistency: DB, Temporal, destination, and exports can disagree. Durability: fixture receipts are persisted independently, but platform HTTP effects and receipt recording are not atomic. Recovery: continuation and manual retry are not generally safe against arbitrary destinations. No exactly-once or tested recovery-time guarantee is established here.

## Standards

### S1 — P2: Preserve the cursor contract during worker upgrades

**Location:** `apps/orchestrator-go/internal/activities/activities.go:512`; `internal/workflows/workflows.go:477–479`. Introduced by `40881f0`. High confidence; serialization reproduced, live rolling upgrade untested.

The new workflow omits `after` for its first page. An old activity worker decodes omission into `After=0` and queries `row_index > 0`, skipping a failed first row. Both versions register the same activity on the same queue; no build routing is configured, and the Helm Deployment permits overlapping workers. New-only mocked tests do not cover this combination.

This violates roadmap §9's requirement to document compatibility with existing histories before deployment. Send an explicit initial `-1` to maintain the old activity contract, and define old-workflow draining/deployment ordering; alternatively enforce a coordinated stop/start rollout. Verify old/new producer-consumer combinations and replay a historical workflow. Do not infer replay nondeterminism solely from changed payload fields. Affected row-zero failures need another retry after the rollout.

## Spec

### C1 — P1: Approved publication does not require the integrity harness

**Location:** `.github/workflows/images.yml:49–56` and `.github/workflows/ci.yml`. MP-08 acceptance remains incomplete; source-inspection confidence high.

Handover MP-08 requires that “migration integrity tests execute rather than remain skipped”; R06 explicitly requires the deterministic integrity harness. The new publisher accepts successful CI jobs, but the referenced CI contains no fixture tests, reconciler tests, or harness scenario execution. The existing smoke test does not independently reconcile destination receipts. Consequently, a commit that breaks only these integrity checks can still qualify for an image labeled approved if other CI checks pass.

Add required harness component and real-stack scenario checks to CI, then verify an intentionally failing reconciliation blocks publication for that SHA. This is an unmet release gate, not evidence that a bad image has already been published. Preserve commit, image digest, and scenario evidence together.

### C2 — P2: Reconciliation accepts an incorrect total record count

**Location:** `tests/harness/harness.py:329–337`. Introduced by `3b1ee66`; reproduced.

The harness promises that `totals_json` agrees with persisted rows; handover §6 requires completion to agree with source completeness. Only `processed` and `failed` are compared. One source record, one successful persisted row, and one receipt pass even with `totals_json.total=999`.

Compare total with the source and persisted-row counts for these scenarios, and add the counterexample as a regression. No migration is needed; rerun prior evidence after strengthening the assertion.

### C3 — P2: The throttle scenario can pass without any throttling

**Location:** `tests/harness/harness.py:394–397`, evidence collection at line 377. Introduced by `3b1ee66`; source-grounded counterexample.

The declared result is “two 429s then success → exactly one receipt.” The scenario only verifies final outcomes and saves the number of attempts. If fault matching breaks or the requested rule is ignored, an ordinary all-success run passes this recovery scenario.

Assert the selected record's two 429 responses followed by success, with stable operation identity, and retain the relevant attempt records. Verify that suppressing the injected fault makes the scenario fail. This changes test evidence, not production behavior.

Standards: **1 finding**, worst S1 (mixed-worker retry omission). Spec: **3 findings**, worst C1 (integrity validation absent from approved publication gate).

## Existing release blockers revalidated

These are not regressions introduced by the reviewed commits. Priorities retain the handover's release-gate convention.

| Finding | Concrete trace at target | Required closure |
|---|---|---|
| MP-01, P0 | `files.rs:133–161` authenticates a viewer then reads supplied bucket/key without org ownership. `jobs.rs:95–118` stores/forwards raw source; `csv.go:128–150` supports platform-privileged object, default HTTP, and local file reads | Tenant-owned resolver across all producers/consumers; deny before reads; redirected-source and cross-org integration tests |
| MP-02, P0 | `workflows.go:108` resets offset; lines 159–163 place continuation offset in sourceRef but startup never restores it | Typed immutable checkpoint, cumulative outcomes, two continuation boundaries and exact EOF |
| MP-03, P0 | `salesforce.go:51–64,83–100` recreates query/iterator and advances locator for a 2,000-row page; ingestion closes after 500 rows | Persist query/page position and absolute index; reconcile a 4,501-record fixture after restart |
| MP-04, P0 | `workflows.go:346–351` ignores successful-step receipt persistence errors; subsequent retry trusts saved successes | Stop dependents on persistence failure; durable intent/receipt and explicit unknown outcomes; commit/disconnect and DB-failure tests |
| MP-05, P1 | `jobs.rs:134–142` writes running unconditionally after workflow start; workflow early errors bypass finalization and finalization/export errors are ignored | Conditional lifecycle authority and repair; export-only retry with zero HTTP effects |

MP-06's missing indexing guard and blocking async activity loops remain in inspected Python code; this review did not rerun the Python worker suite. MP-07's sequential row loop and repeated CSV prefix parsing remain. MP-10 still lacks auth.secretRef in the inspected dispatch contract. MP-11 was not exhaustively re-audited. None is closed by this review. MP-09 is improved but requires S1's compatibility evidence; MP-08 is partial under C1.

## Failure, concurrency, and security analysis

| Failure/interleaving | Actual source-grounded outcome | Recovery / verification |
|---|---|---|
| Destination commits; DB step receipt fails; later step fails; operator retries | Earlier successful operation may be reissued because its receipt is missing | Reconcile destination before replay; inject both failures |
| Workflow finalizes before API's post-start update | API overwrites terminal status with running | Conditional transitions; deterministic fast-completion test |
| New workflow requests first failed page; old activity worker handles it | Row zero is skipped | S1 compatibility matrix and controlled rollout |
| Continue-as-new after 100 full batches | Fresh workflow resumes at empty cursor | Versioned checkpoint and immutable source binding |
| Harness fault silently does not match | Throttle test still passes | Assert actual fault/attempt sequence |

Authorization risk requires an authenticated tenant user and knowledge of an accessible object key, or permission to create a job using source references. Storage/network permissions bound the impact; no live exploitation is asserted. Destination connection-time egress controls exist, but do not protect the default source HTTP client. Preserve them while fixing source access. Full identity-provider, license, dependency-vulnerability, and OS sandbox assessments remain outside this pass.

## Performance, evidence gaps, and handover

The supplied R00 evidence covers 40 customers, not capacity. No latency, throughput, resource ceiling, or recovery target was measured here. Sequential HTTP dispatch and repeated prefix reads limit scale; increasing replicas does not fix one workflow's serial loop. Validate correctness before concurrency changes, then use the roadmap's measured 1k/10k/100k envelope and history limits.

Ordered remediation:

1. **Source/identity contract — MP-01, R01.** Complete the current agent work across manual, batch, watched, retry and export paths. Gate on two-tenant API tests and denied-read evidence. Use additive registry migration, fail closed for ambiguous legacy ownership, and document rollback behavior. Do not backfill ownership from untrusted client fields.
2. **Checkpoint/upgrade safety — MP-02/03, S1, R03.** Agree the source version contract first. Implement page/cursor identity and compatibility; test two boundaries, old/new workers and historical replay. Inspect existing runs for repeated/missing identities before resuming; code changes alone cannot repair past external effects.
3. **External-effect/lifecycle authority — MP-04/05, R04/R05.** Build on stable identity. Propagate receipt errors, represent uncertainty, serialize conflicting recovery, and enforce state transitions. Fault-inject commit/disconnect, receipt DB outage, and fast completion. Preserve old evidence and block unsafe replay during upgrades/rollback.
4. **Validation credibility — C1/C2/C3, R00/R06.** Strengthen assertions, retain attempt evidence, and require harness execution in the exact-SHA gate. Also fix the unused ambiguous branch before R04 uses it: lines 311–314 currently accept a succeeded row with zero receipts when listed as ambiguous (reproduced). No schema migration; regenerate affected evidence and test failed-check promotion denial.
5. **Integrated acceptance — R07.** Re-run the combined revision across ownership, restart, source mutation, auth failure, ambiguous effects, and export-only recovery. Record image digest and configuration. Keep customer-data release blocked until evidence closes the P0 gates.

Effort estimates are withheld because the source and operation contracts are actively changing. No production code, dependencies, database, remote branch, or deployment was changed by this review.

## Coverage ledger and remaining uncertainty

- **Deep inspection:** recent committed retry cursor changes, harness runner/reconciler, CI publication gate; critical source sampling/job-start, continuation, step-persistence and Salesforce page-state traces.
- **Sampled:** destination fixture and tests, Python worker/sandbox, destination egress client, deployment worker overlap, load-test contracts, historical baseline evidence.
- **Executed checks:** Python reconciliation suite and the stated focused counterexamples/serialization reproduction only.
- **Unreviewed or not executed:** full frontend/browser behavior, all batch/watch/schedule compatibility, live DB constraints, full auth/licensing, dependency advisories, restore drills, Kubernetes execution, full-stack fault injection and load.
- **Highest-value next investigation:** review the completed R01 commit against every source producer/consumer, then run the integrated two-tenant denial tests before integrating checkpoint work. This report does not certify the unfinished R01 local changes.
