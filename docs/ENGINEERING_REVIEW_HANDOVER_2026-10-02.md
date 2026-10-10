# Migration Platform — agent implementation and remediation handover

**Prepared:** 2 October 2026 (Asia/Kolkata)

**Intended next working session:** 3 October 2026

**Repository:** rajivravindran/MigrationPlatform

**Reviewed branch / commit:** main / `18160097d3e95994bf8018838c9b43b719bcac49`

**Review type:** Principal-engineer-level code and operational-readiness assessment

**Implementation status:** Findings open; this handover does not implement fixes.

**Agent entry point:** Read sections 1–3 for evidence, then execute the work packages in section 8. Section 9 specifies integration tests and section 10 defines the required completion report. Proposed interfaces and test names are implementation guidance, not claims that those symbols already exist.

**Publication status at preparation:** This document is committed in the reviewer's local checkout. The GitHub integration rejected the remote write with HTTP 403. Do not assume the local documentation commits exist on origin. Obtain this file before starting and inspect the actual branch state.

## 1. Decision and scope

**Assessment: substantial prototype with useful migration workflows; production use with customer data should remain blocked until the integrity and access-control findings are resolved.**

Continue engineering investment and controlled demonstrations using synthetic data. Prioritize evidence that migrations are complete, recoverable, correctly authorized, and accurately reported.

The review traced the API, Temporal workflows, source connectors, Python transformations, row/step persistence, result exports, and CI/release configuration. It also sampled frontend contracts, deployment files, tests, and architecture/security documentation. It was not an exhaustive security audit.

Evidence is separated below into:
- **Executed:** locally reproduced test results.
- **Observed:** GitHub CI results for the reviewed commit.
- **Code inspection:** behavior established by tracing the implementation; proposed failure scenarios still need integration reproduction.
- **Design recommendation:** proposed engineering direction, not an existing capability.

No full distributed stack, customer environment, or large migration was executed during this review. Performance estimates are analytical, not measured benchmarks. Security findings were not exercised against a live service.

Read this alongside [AGENT_HANDOFF.md](AGENT_HANDOFF.md), which records earlier feature delivery and verification. Earlier verification is historical evidence; it does not close the new findings below. Existing licensing, SFTP, DAG, and deployment release gates remain applicable.

## 2. Start here tomorrow

1. Read the priority register and reproduce the Python failures using section 5.
2. Agree on the five release invariants in section 6 before changing implementation.
3. Add focused failing tests for object ownership, continuation, Salesforce pagination, and first-row retry.
4. Fix source authorization first. In parallel workstreams only if explicitly assigned, isolate checkpoint/retry changes from sandbox changes.
5. Make CI green and gate publication on validation of the same commit.
6. Prove one supported file-to-HTTP migration through failure injection and destination reconciliation before adding more connectors.

Do not start with a language rewrite, more infrastructure, or a broad feature expansion. Preserve the useful domain model while correcting its execution guarantees.

### Priority register

Priority meanings: **P0** = release blocker involving access or migration integrity; **P1** = high-priority correctness, operability, or validation work. These are remediation priorities, not CVSS scores or a claim of an active incident.

| ID | Priority | Finding | Evidence | Status |
|---|---|---|---|---|
| MP-01 | P0 | Object ownership and source-access authorization gaps | Code inspection | Open |
| MP-02 | P0 | Continue-as-new loses ingestion position | Code inspection | Open |
| MP-03 | P0 | Salesforce pagination loses page state and stable indexes | Code inspection | Open |
| MP-04 | P0 | Retry safety can lose successful-step evidence | Code inspection | Open |
| MP-05 | P1 | Job state and workflow state can diverge | Code inspection | Open |
| MP-06 | P1 | Python row access fails; blocking work runs in async activities | Executed + code inspection | Open |
| MP-07 | P1 | Execution and ingestion do not implement documented scaling | Code inspection | Open |
| MP-08 | P1 | Images publish independently of successful validation | Observed + code inspection | Open |
| MP-09 | P1 | Retry-all skips failed row zero | Code inspection | Open |
| MP-10 | P1 | Destination auth.secretRef is not wired into execution | Code inspection | Open |
| MP-11 | P1 | Security and architecture claims exceed implementation evidence | Code inspection | Open |

Owners are unassigned. Assign an owner, implementation PR, regression tests, and verification evidence per finding; do not close findings on code changes alone.

## 3. Detailed findings

### MP-01 — Authorize sources and objects against the organization

**Evidence:** [files.rs:128–163][files], [jobs.rs:68–121][jobs], [csv.go:127–153][csv], [export.go][export].

The sampling route authenticates the user, then uses the supplied bucket and key with the platform's S3 client. It does not establish that the object belongs to `claims.org`. Even the viewer role is permitted. Knowing another organization's key can therefore permit reading a sample, within the platform's storage permissions. Export keys such as `jobs/{jobId}/results.csv` are predictable.

Job creation also accepts arbitrary `source_ref`. The source reader supports HTTP via `http.DefaultClient` and local files via `os.Open(path)`. These paths do not use the hardened destination HTTP client. This exposes internal-network fetching and worker-readable file ingestion capabilities to job creators.

**Required changes**
- Resolve an organization-owned source ID into a server-controlled bucket/key or connector configuration.
- Check ownership at every sampling, execution, retry, and download boundary.
- Remove user-selected local paths from the external contract, or constrain them to an explicitly authorized ingestion root.
- Apply source-specific outbound policies, including redirects and resolved addresses. Trusted internal sources should be explicitly configured, not accidentally reachable.
- Consider scoped storage credentials as additional containment; opaque IDs alone are not authorization.

**Acceptance**
- Organization A cannot sample or execute organization B's source or result, including guessed result keys.
- Viewer/operator/editor cases and source-reference tampering are covered.
- Controlled internal-address and local-file fixtures are rejected unless explicitly authorized.
- Legitimate approved internal connectors continue to work.

### MP-02 — Preserve continuation state

**Evidence:** [workflows.go:107–163][workflow].

Every run initializes `offsetRaw` to `{}`. After 100 batches, the workflow writes `SourceRef["_offset"]` and continues as new, but startup never reads that field. If execution reaches the boundary, ingestion restarts from the beginning. At 500 rows per batch the boundary is 50,000 rows. Even an exactly full final batch needs a later EOF check.

Repeated requests can create duplicates where destination idempotency is absent or ineffective. Cumulative result counters also start over on each run.

**Required changes**
- Introduce explicit typed checkpoint input: cursor, source identity/version, and cumulative outcome.
- Restore checkpoint state at startup; reject malformed cursors instead of silently resetting.
- Ensure the source is immutable or detect a changed source before resuming.
- Budget Temporal history by actual events/payload size and step count, not only batch count. A single-step 50,000-row run already schedules roughly 100,000 row HTTP/persistence activities before continuation; actual server limits need validation.

**Acceptance**
- A deterministic test crosses at least two continuation boundaries and consumes each source identity once.
- Include exact-boundary EOF, failures in an earlier run, and source mutation.
- Final totals and parent batch outcome include failures from all runs.
- A real Temporal integration test confirms history remains within the configured limits.

### MP-03 — Make Salesforce pagination resumable

**Evidence:** [salesforce.go:51–100][salesforce], [activities.go:196–233][activities].

The iterator fetches up to 2,000 records but ingestion consumes 500 before closing it. The returned locator is already the next-page locator, so unread records in the current page are not checkpointed. Reopening also creates a new query job and resets the row index to zero.

Risks include skipped records, invalid cursors, repeated source queries, and overwriting earlier outcomes keyed by job and row index. The precise remote API response to reusing a locator across jobs still needs an integration test; the local state-loss defect is visible in code.

**Required changes**
- Persist query job identity, current-page position, next locator, and absolute row index.
- Preserve unconsumed page data or re-fetch the same page deterministically.
- Separate query-job creation from repeated page ingestion.
- Define source snapshot and ordering guarantees.

**Acceptance**
- Test page size 2,000 with batch size 500 across multiple pages and restarts.
- Verify all source IDs appear exactly once and row indexes remain unique.
- Test a final partial page, empty result, expired token, and retry during page consumption.
- Reconcile source IDs against both stored outcomes and destination receipts.

### MP-04 — Make retry guarantees explicit and preserve step evidence

**Evidence:** [workflows.go:286–351][workflow], [activities.go:262–328][activities].

Errors from `PersistStepOutcome` are ignored after successful HTTP calls. Retry then relies on the persisted successes to skip completed steps. A create-customer call can succeed, its checkpoint can fail, a later create-address call can fail, and manual retry can create the customer again.

Idempotency headers are optional. Computing a stable key does not prove destination deduplication. Even a configured header needs a documented destination contract and retention window.

**Required changes**
- Treat failed persistence of step evidence as an execution blocker.
- Record stable operation identity and explicit attempt identity.
- Distinguish retry-safe operations from requests with ambiguous external outcomes.
- For a timeout after possible destination commit, reconcile by stable business key or destination idempotency lookup where supported.
- Prevent overlapping retries for the same operation, or define safe coordination semantics.
- Do not promise exactly-once effects across arbitrary HTTP destinations.

**Acceptance**
- Inject destination commit followed by a lost response.
- Inject DB failure after a successful HTTP step, then request row retry.
- Test destinations with and without idempotency support and expired deduplication windows.
- Verify either one external effect or an explicit unresolved/reconciliation-required state.
- Re-delivery of persistence activities must not misrepresent execution-attempt accounting.

### MP-05 — Establish authoritative job lifecycle and reconciliation

**Evidence:** [workflows.go:122–178][workflow], [jobs.rs:95–142][jobs], [activities.go:568–614][activities].

Ingest/preprocess errors can return before finalization. FinalizeJob and ExportJobResults errors are ignored. The API writes `running` after starting the workflow without a conditional transition, potentially overwriting a terminal state from a fast workflow.

**Required changes**
- Define allowed state transitions and enforce them conditionally.
- Assign clear ownership for execution state; API actions should record intent without overwriting later execution outcomes.
- Persist terminal failure reliably and add a repair/reconciliation process for failures that prevent finalization.
- Track export availability separately from migration outcome.
- Account for cancellation, retries, and batch-parent interpretation.

**Acceptance**
- Ingest, preprocessing, persistence, finalization, and export failures have visible outcomes.
- A fast completion cannot be overwritten as running.
- A killed worker and DB outage recover or converge through reconciliation.
- Job, batch, Temporal, and UI outcomes agree.
- An export failure remains visible and retryable without repeating destination writes.

### MP-06 — Repair Python execution and activity scheduling

**Evidence:** [sandbox.py:33–90][sandbox], [worker.py:32–73][worker]. Local test results are in section 5.

Ordinary row subscripting fails because the RestrictedPython environment lacks `_getitem_`. Both `row['a'] + row['b']` and `row['country'][:2].upper()` fail.

Separately, async activity functions execute synchronous transformation loops and blocking subprocess waits. Heartbeats happen only in `finally`, after the work. Long batches can block the event loop and delay heartbeats; setting activity concurrency does not make that blocking path concurrent.

**Required changes**
- Configure safe guards for the intentionally supported expression operations.
- Maintain negative security tests when enabling valid language features.
- Run blocking work in an appropriate bounded executor/worker arrangement.
- Heartbeat periodically during long batches and honor cancellation.
- Review subprocess/pipe cleanup and bounded output handling, including output larger than pipe capacity.

**Acceptance**
- Existing 18 tests pass with the pinned runtime dependencies.
- Valid indexing, slicing, and intended iteration expressions pass.
- Forbidden imports, file reads, and dunder access remain rejected.
- Concurrent slow transformations do not starve worker heartbeats.
- Large result, timeout, exception, and cancellation paths leave no orphan process or leaked pipe.

### MP-07 — Correct the scaling model before making throughput claims

**Evidence:** [workflow row loop and shard wrapper][workflow], [csv.go:24–66][csv], [architecture.md][architecture].

Rows are processed sequentially within a migration workflow. `Shards` is declared but unused; `ProcessShardWorkflow` delegates without partitioning. Each CSV batch opens the source again and parses past all previous rows.

With fixed-size batches, repeated prefix scans produce approximately quadratic parsing work as file size increases. As an analytical example, 100 ms destination latency limits a sequential job to roughly 10 rows/second before other overhead. This is not a measured benchmark. Multiple independent jobs may run concurrently, but replicas do not make one sequential workflow parallel.

**Required changes**
- Use immutable chunks or format-aware resumable positions.
- Introduce bounded concurrency with per-destination limits and explicit ordering semantics.
- Partition work by stable source identity and ensure shards cannot overlap.
- Bound workflow histories, memory, and response payloads.
- Repair load-test request/response contracts before relying on their thresholds.

**Acceptance**
- Repeated scale runs record input size, record size, latency distribution, concurrency, resources, complete outcomes, duplicate count, and recovery time.
- Increase source size and verify parsing/network work does not grow quadratically.
- Demonstrate bounded memory and history.
- Reconcile destination contents; API acceptance rate alone is not migration throughput.

### MP-08 — Gate releases on validated commits

**Evidence:** [CI run][ci-run], [image run][image-run], [images.yml][images].

The reviewed commit's image build/publish completed despite failing CI. The image workflow is independently triggered on pushes.

**Required changes**
- Gate release publication or promotion on required successful checks for the exact same commit.
- Preserve reproducible dependency installation.
- Run unit, integration, schema-contract, and end-to-end checks as required release gates.
- Distinguish development artifacts from approved release artifacts.

**Acceptance**
- A deliberate test failure prevents release publication/promotion.
- A successful image build cannot substitute for tests.
- The published digest maps to a commit with recorded successful required checks.
- Compose/Playwright and migration integrity tests execute rather than remain skipped.

### MP-09 — Include row zero in retry-all

**Evidence:** [workflows.go:468–476][workflow], [activities.go:522–530][activities], [jobs.rs retry_all_failed][jobs].

The initial `After` defaults to zero and the query uses `row_index > After`. Source indexes begin at zero. Retry-all therefore omits the first row if it failed.

**Required change:** start with -1 or model an absent cursor explicitly; preserve exclusive cursor semantics for later pages.

**Acceptance:** only-row-zero-failed, row-zero-plus-other-failures, empty failures, and multi-page cases retry exactly the intended rows.

### MP-10 — Implement destination authentication references

**Evidence:** [template.go:28–46][template], [workflow HTTP dispatch][workflow], [CallEndpoint][activities].

The destination model represents `auth.secretRef`, but the reviewed execution path neither resolves it nor passes it into HTTP authentication. Explicit headers are forwarded; that is a separate mechanism and does not implement the advertised secret reference.

**Required changes**
- Resolve organization-authorized secret references inside the side-effect activity.
- Carry the necessary trusted identity without placing decrypted credentials in workflow history.
- Apply the selected authentication scheme and fail clearly for missing/unauthorized secrets.
- Redact credentials from diagnostics, history-bound payloads, and audit records.

**Acceptance**
- Use a controlled destination requiring the configured authentication scheme.
- Missing, wrong-organization, rotated, and revoked secrets behave explicitly.
- Inspect stored/history/logged data to verify credentials are not disclosed.

### MP-11 — Reconcile documentation with implemented controls

**Evidence:** [security.md][security-doc], [audit.rs][audit], [transform Dockerfile][dockerfile], [sandbox tests][sandbox-tests], [architecture.md][architecture].

Security documentation claims audit redaction in audit.rs, network-blocking seccomp in the Dockerfile, and over 40 sandbox escape patterns. The inspected files do not substantiate those claims: audit.rs binds before/after JSON directly; the Dockerfile does not configure the claimed network-blocking policy; the sandbox file contains six tests. This does not establish that no external deployment protection exists.

Architecture documentation describes shard fan-out and other behavior that the reviewed execution path does not implement.

**Required changes**
- Label capabilities as implemented, verified, deployment-dependent, or planned.
- Link security claims to concrete enforcement and tests.
- Correct source batch sizes, sharding, recovery, and throughput descriptions.
- Review all credential-bearing audit paths for actual redaction.

**Acceptance**
- Every security guarantee has an implementation/configuration reference and verification evidence.
- A new engineer can follow the architecture document to the real execution path.
- Planned capabilities cannot be mistaken for release guarantees.

## 4. Foundations to preserve and architectural direction

Preserve versioned/published templates, row/step diagnostics, response-dependent request chains, batch dependencies, quarantine, result exports, separation of Python execution, and connection-time destination egress checks.

Feature breadth currently exceeds demonstrated reliability. Make the next milestone a verifiable migration outcome. HTTP success and counting persisted rows do not establish source completeness or destination correctness.

The Rust/Go/Python/TypeScript stack increases build, contract, upgrade, and debugging costs. A rewrite is not the recommended first action. Strengthen contracts at existing boundaries, make one migration path reliable, and demand a concrete operational benefit for additional components.

### Delivery sequence

| Stage | Deliverable | Exit evidence |
|---|---|---|
| 1 | Source ownership and access boundaries | Cross-organization and controlled source-policy tests |
| 2 | Checkpoint, pagination, retry, and lifecycle correctness | Failure-injection and continuation tests |
| 3 | Trustworthy build and release gates | Passing tests gate the exact published commit |
| 4 | One end-to-end supported migration path | Immutable source to reconciled destination plus operator report |
| 5 | Measured scale and recovery | Repeatable throughput, completeness, duplicate, resource, and recovery results |

## 5. Verification record and reproduction

### Executed locally

Runtime: Python 3.12; RestrictedPython 7.3; Pydantic 2.9.2; pytest 9.1.1. A temporary environment installed the dependencies needed by the existing test suite, not the full production worker runtime.

From `apps/transform-worker`, with the temporary environment active:

```bash
PYTHONPATH=. python -m pytest -q
```

Result: **16 passed, 2 failed, 1 warning**.

Failures:
- `tests/test_sandbox.py::test_sandbox_row_access`
- `tests/test_transforms.py::test_build_payload_simple_mapping`

Both report:

```text
migration_worker.sandbox.SandboxError:
NameError: name '_getitem_' is not defined
```

The warning concerned the unrecognized `asyncio_mode` setting in this minimal environment; it is not the cause of either failure.

To reproduce in a fresh local environment from the repository root:

```bash
python3.12 -m venv /tmp/migration-review-venv
/tmp/migration-review-venv/bin/python -m pip install pytest==9.1.1 RestrictedPython==7.3 pydantic==2.9.2
cd apps/transform-worker
PYTHONPATH=. /tmp/migration-review-venv/bin/python -m pytest -q
```

### Observed on GitHub for the reviewed commit

| Check | Result |
|---|---|
| Go vet, tests, race tests | Passed |
| Go lint job | Failed after passing tests |
| Web lint/build | Passed |
| Rust formatting | Passed |
| Rust Clippy | Failed; Rust tests skipped |
| Python Ruff | Failed; pytest skipped |
| Compose smoke / Playwright | Skipped |
| Image workflow | Succeeded |

[CI run 34127087104][ci-run] and [image run 34127087150][image-run] are the historical evidence. This table does not describe future runs triggered by this handover commit.

### Not verified during this review

- Full Compose or Kubernetes deployment.
- Live tenant-boundary exploitation.
- Salesforce integration behavior against a real account.
- Large-volume throughput, memory, or Temporal history limits.
- Recovery under real database/network outages.
- Complete dependency-vulnerability or sandbox-escape audit.
- Customer-specific destination idempotency guarantees.

## 6. Release invariants and closure checklist

Required invariants:
1. Every source record has a stable identity and an accounted-for outcome: succeeded, failed, deliberately skipped, or unresolved.
2. Recovery preserves source and operation identity across retries and continuation.
3. Ambiguous external writes remain visible until reconciled.
4. Completion agrees with execution state and source completeness.
5. Every source, result, and secret is authorized for the organization.

Before closing this handover:

- [ ] Assign owners and PRs for MP-01 through MP-11.
- [ ] Record a failing regression test before each correctness fix where practical.
- [ ] Close P0 findings with integration evidence.
- [ ] Pass the existing Python suite and the new boundary/recovery tests.
- [ ] Verify source-to-destination reconciliation on the supported migration path.
- [ ] Gate release artifacts on validation of the exact commit.
- [ ] Align architecture/security documentation with evidence.
- [ ] Retain applicable production gates from AGENT_HANDOFF.md.
- [ ] Record tested versions, commit, configuration, limitations, and benchmark conditions.
- [ ] Make a fresh production-readiness decision; do not infer approval from completed feature checklists.

### Per-finding closure record

Use this template in follow-up work:

```text
Finding ID:
Owner:
Implementation PR / commit:
Root cause:
Behavior after change:
Regression and integration evidence:
Migration / rollout considerations:
Remaining limitations:
Reviewer and closure date:
```

## 7. Code references

All code links below are pinned to the reviewed commit so later changes do not invalidate the evidence.

[files]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/api/src/routes/files.rs#L128-L163
[jobs]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/api/src/routes/jobs.rs
[csv]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/orchestrator-go/internal/connectors/csv.go
[export]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/orchestrator-go/internal/activities/export.go
[workflow]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/orchestrator-go/internal/workflows/workflows.go
[activities]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/orchestrator-go/internal/activities/activities.go
[salesforce]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/orchestrator-go/internal/connectors/salesforce.go#L51-L100
[sandbox]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/transform-worker/migration_worker/sandbox.py
[worker]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/transform-worker/migration_worker/worker.py
[template]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/orchestrator-go/internal/template/template.go
[architecture]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/docs/architecture.md
[security-doc]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/docs/security.md
[audit]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/api/src/security/audit.rs
[dockerfile]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/transform-worker/Dockerfile
[sandbox-tests]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/apps/transform-worker/tests/test_sandbox.py
[images]: https://github.com/rajivravindran/MigrationPlatform/blob/18160097d3e95994bf8018838c9b43b719bcac49/.github/workflows/images.yml
[ci-run]: https://github.com/rajivravindran/MigrationPlatform/actions/runs/34127087104
[image-run]: https://github.com/rajivravindran/MigrationPlatform/actions/runs/34127087150

## 8. Agent execution playbook

### 8.1 Instructions to the receiving implementation agent

You are implementing the remediation in this document. Verify the findings against your checkout before editing, reproduce the relevant failure, implement the smallest coherent correction, and attach evidence that the corrected behavior works. Preserve the product's established template, batch, scheduling, and licensing semantics unless a documented interface change is necessary.

Do not treat this document as a request to mark findings complete, generate placeholder tests, or rewrite the platform. Do not treat the presence of Temporal, an idempotency hash, or a passed HTTP status as proof of migration integrity.

At the beginning of the task:

1. Inspect branch, HEAD, worktree changes, and all applicable AGENTS.md instructions.
2. Read this document and docs/AGENT_HANDOFF.md. Compare HEAD with the reviewed commit. If the code changed, revalidate each assigned finding.
3. Record runtime versions and determine which tests can actually execute.
4. Select one work package below. State the IDs covered, interfaces affected, and the failing behavior to reproduce.
5. Work on an isolated branch/worktree when sharing the repository. Do not overwrite unrelated changes.
6. Keep a short progress record in the PR or task response: baseline, confirmed findings, decisions, completed checks, and remaining gates.
7. When infrastructure or credentials are unavailable, run local component tests and state the remaining integration limitation. Never report an unexecuted test as passing.

Execution authority comes from the user and the environment. This handover does not grant production deployment access, credentials, permission to change integrations, or permission to push through a rejected access boundary. A normal implementation task can produce reviewable code and local commits; publishing must respect the actual authorization.

### 8.2 Work packages, ownership, and merge order

These are suggested roles for a coordinator or sequential stages for one agent. They do not require multiple agents.

| Package | Findings | Primary files / ownership | Dependency |
|---|---|---|---|
| A — Source authorization | MP-01; authorization foundation for MP-10 | Rust file/job/batch routes, source resolution, Go connector input validation and egress | Start first; agree source identity contract |
| B — Python runtime | MP-06 | Python sandbox, activity worker, Python tests and test dependencies | Can proceed independently |
| C — Continuation and pagination | MP-02, MP-03, MP-09 | Go workflow input/checkpoints, connectors, ingestion, retry paging | Coordinate shared contracts with A |
| D — Side effects and lifecycle | MP-04, MP-05, MP-10 | Go activity dispatch/persistence, Rust job transitions, secrets, DB migrations | Build on A/C interfaces |
| E — Validation and release | MP-08; test harness | CI/images workflows, deterministic fixtures, integration harness | Prepare early; gate release after A–D |
| F — Scale and documentation | MP-07, MP-11 | CSV resume/chunking, bounded concurrency, load harness, docs | Baseline after correctness; docs changes follow behavior |

**Conflict rules**
- C and D both modify workflows.go and activities.go. Integrate C's contract first or explicitly coordinate their patch boundaries; do not independently redesign workflow inputs.
- A and D may both need schema changes. Allocate new migration numbers from the latest branch. Never edit already-applied migrations to change deployed schemas.
- E owns shared CI definitions; B supplies Python test requirements rather than independently replacing the same workflow.
- Changes to rule-template fields must update JSON Schema, TypeScript/Zod, Rust, Go, Python, fixtures, and affected UI forms together.
- Keep security, correctness, and performance commits distinguishable. Avoid a single PR containing a language rewrite and every finding.

Recommended first useful sequence: reproduce B and MP-09, close A, integrate C, implement D, validate E, then optimize F. E's test harness and CI repair may start earlier, but a green pipeline alone does not close A–D.

### 8.3 Package A — Concrete implementation work

**Inspect first**
- apps/api/src/routes/files.rs, jobs.rs, batches.rs, connectors.rs, schedules.rs.
- apps/api/src/state.rs and security/audit.rs.
- apps/orchestrator-go/internal/workflows/workflows.go: extractConnector.
- apps/orchestrator-go/internal/activities/watch.go and batch.go.
- apps/orchestrator-go/internal/connectors/csv.go, objectstore.go, salesforce.go.
- apps/orchestrator-go/internal/security/ssrf.go.

**Implementation sequence**
1. Enumerate every producer and consumer of source_ref: upload, manual job, watched file, archive stage, schedule, sampling, retry, and export. Write down which path is client-supplied and which is generated internally.
2. Choose a canonical source ownership model. A source registry keyed by opaque ID with org, bucket, key, version/hash, format, and provenance is a reasonable design; an equivalent verifiable ownership model is acceptable. Prefix matching alone is insufficient for all source types.
3. Make the API derive the authorized location from that model. Validate format and allowed source kind explicitly rather than accepting arbitrary JSON as an execution instruction.
4. Preserve compatibility deliberately: legacy references may be accepted only after establishing ownership. Reject ambiguous ownership; do not infer it from a user-provided org field.
5. Ensure watched and archive-created sources receive correct organization ownership. Keep privileged internal activity inputs distinct from public request shapes.
6. Make source HTTP clients injectable, bounded by timeout, and subject to configured destination/address policy. Do not disable private-address protection globally to make local tests pass.
7. Apply one tested ownership resolver consistently. Add API-level tests, not just helper-level prefix tests.

**Test fixture**
Create two organizations, distinct users across roles, two upload records, and a generated job export. Attempt cross-org sampling and job creation with known keys. Use a temporary test directory and controlled HTTP servers for local-file and network-policy checks. Never point tests at real metadata services or arbitrary machines.

**Done means**
Every externally reachable source read proves ownership; denied requests issue no storage read or HTTP request; authorized watched/batch flows still work; response codes do not leak another tenant's existence unnecessarily.

### 8.4 Package B — Concrete implementation work

**Inspect first**
- migration_worker/sandbox.py, transforms.py, worker.py, template.py.
- tests/test_sandbox.py and tests/test_transforms.py.
- pyproject.toml and the Python Dockerfile.

**Implementation sequence**
1. Run the existing suite unchanged and retain the two failure traces.
2. Inspect which guards the pinned RestrictedPython compiler emits for supported expressions. Supply a deliberate, restricted policy for indexing/iteration rather than exposing unrestricted builtins.
3. Add valid-operation tests and retain attack-denial tests. A blanket bypass of RestrictedPython is not a fix.
4. Replace blocking work on the async activity event loop with a bounded execution arrangement compatible with the pinned Temporal Python SDK. Check context propagation for heartbeats and cancellation.
5. Heartbeat during batches using a real progress checkpoint. A final heartbeat in a finally block is not enough.
6. Review process lifecycle: read pipe output while the child may be writing, cap result size, terminate/kill and join on timeout where needed, close both pipe ends in all paths.
7. Test the actual worker/container runtime as well as the lightweight unit environment.

**Suggested regression tests (new)**
- Supported dict indexing, slicing, intended iteration, and malformed user code.
- Result larger than pipe capacity returns or fails with an explicit size error, never hangs.
- A timed-out child leaves no live process.
- Two slow activities allow heartbeat/cancellation progress.
- Unsupported import/file/dunder access remains blocked.

Do not assert a hardened multi-tenant OS sandbox merely because Python syntax restrictions pass. Document the process boundary and actual deployment isolation separately.

### 8.5 Package C — Checkpoints, Salesforce, and retry pagination

**Suggested checkpoint shape — design sketch, not an existing type**

```text
checkpoint:
  version
  source_identity          # immutable object version/hash or query snapshot identity
  connector_cursor         # connector-specific, versioned
  next_absolute_row_index
  succeeded_count
  failed_count
  skipped_count
```

Avoid an overloaded underscore field hidden inside source_ref. Define backward compatibility for workflow inputs and any already-running histories.

**Implementation sequence**
1. Extract a checkpoint initialization/validation path and unit-test missing, valid, and malformed inputs.
2. Propagate checkpoint state through ContinueAsNew and restore it in the next execution.
3. Preserve cumulative outcomes and ensure parent batch result interpretation uses the entire job's result.
4. Make continuation thresholds testable without processing 50,000 rows in a unit test. Use safe configuration or a pure decision helper; do not add a test-only branch that changes production semantics.
5. Add history-aware continuation and verify against the pinned Temporal SDK/server behavior. Account for multi-step requests and payload sizes.
6. Make Salesforce query creation independently recoverable and page consumption checkpointable. Never pair a saved locator with a freshly created unrelated query job.
7. Fix MP-09 with an explicitly absent initial cursor or -1. Audit other zero-based paging boundaries for the same mistake.
8. Exercise all of this using real connector output and persistence assertions, not only mocks that always return Done=true.

**Salesforce fake-server scenario**
Return 4,501 distinct records in remote pages of 2,000, 2,000, and 501. Consume them in local batches of 500. Reopen the iterator between batches and inject a failure after a partially consumed page. Record query-job creation count, requested locators, emitted IDs, and row indexes.

The test must prove every record appears once in the completed ingestion sequence, all row indexes are stable, and resume does not skip to the next page prematurely. Add a separate case for an empty final result.

**Temporal compatibility**
Workflow changes may alter commands during replay. Evaluate SDK versioning/patching or a controlled worker rollout before deployment. Preserve a representative pre-change history for replay tests when available. If there are no existing production histories, record that fact; do not assume it silently.

### 8.6 Package D — External effects, persistence, state, and secrets

**Inspect first**
- Go workflows.go: executeStep, processOneRow, retryPersistedRow, finalizeAndExport.
- activities.go: CallEndpoint, PersistOutcome, PersistStepOutcome, FinalizeJob.
- workflows/batch.go and watch.go for child outcome handling.
- Rust routes/jobs.rs, temporal.rs and DB schema.
- Go security/secrets.go and existing SFTP secret-resolution patterns.
- Rust security/secrets.rs, audit.rs and template model/schema files.

**Agree these semantics before implementation**
- Operation identity must be stable across retries of the same intended external effect.
- Attempt identity must distinguish deliberate execution attempts from redelivery of the same persistence activity.
- An HTTP timeout may mean the operation committed.
- Successful-step evidence cannot be ignored when later retry logic depends on it.
- Retrying a whole row is different from replaying an individual activity.
- Existing row_status has no unresolved value. If adding one, update the DB enum, readers, totals, exports, UI, and parent result handling coherently. An equivalent explicit representation is acceptable.

**Implementation sequence**
1. Make critical persistence errors propagate. Stop advancing a chain when successful-step evidence cannot be durably recorded.
2. Define operation identity independently from accidental runtime changes. Clarify whether editing a payload creates a new operation, and how source version, template version, destination, and business key participate.
3. Make persistence redelivery idempotent. Repeated acknowledgement of the same attempt should not increment attempts as if a new business operation occurred.
4. Serialize or safely coordinate overlapping single-row and retry-all operations. Test concurrent requests.
5. Support verified destination idempotency and reconciliation; expose unresolved outcomes where safe automatic retry is impossible.
6. Implement conditional lifecycle transitions. One candidate pattern is a guarded pending-to-running update whose failure causes a reload, not a forced overwrite. Derive the full state machine and test terminal-state protection.
7. Ensure failure/cancellation cleanup can execute despite cancelled workflow context, using SDK-supported patterns. Provide reconciliation for database outages and workflows that cannot run cleanup.
8. Keep export completion separate. An export retry must not re-execute migration HTTP calls.
9. Carry trusted organization and secret reference into CallEndpoint and resolve credentials within the activity. Never pass decrypted credentials back through workflow results.
10. Review request URL/header/body/error logging and audit serialization for secret leakage.

**Required scenario**
Use a controlled destination with a durable receipt ledger. It must support both a deduplicating mode and a non-deduplicating mode. Simulate committing a create and closing the connection before returning a response, then fail the checkpoint write. Assert destination side effects, database evidence, and operator-visible status independently.

A test that checks only the final succeeded counter is insufficient.

### 8.7 Package E — CI and deterministic integration harness

**Inspect first**
- .github/workflows/ci.yml and images.yml.
- apps/api/tests/integration_db.rs (uses Postgres testcontainers).
- apps/orchestrator-go/internal/workflows/workflows_test.go.
- apps/web/e2e and playwright.config.ts.
- tests/load/load_10m.js, smoke_laptop.js, and load_test_plan.md.
- packages/rule-schema/tests and package.json.

**Implementation sequence**
1. Reproduce actual current lint/test failures. Record baseline failures separately from regressions introduced by changes.
2. Ensure CI installs tested runtime/development dependencies reproducibly. Python CI currently installs unpinned packages independently from production pins; align the process.
3. Remove fallback dependency installation that silently discards lockfile reproducibility; fix the lock/configuration when npm ci fails.
4. Ensure required schema contract tests actually execute. The inspected rule-schema package exposes a build script but no test script; establish the runner rather than claiming an existing npm test command.
5. Set up local fake destinations, S3-compatible storage, Postgres, and Temporal fixtures. Prefer deterministic failures over timing-based sleeps.
6. Fix load-client request and response shapes against the implemented API. At baseline, load_10m.js stringifies source_ref and reads top-level counters that do not match the reviewed JobRow/totals_json shape.
7. Gate release artifacts on successful checks of the same SHA. If using workflow_run or artifact promotion, check event trust, branch, conclusion, and exact SHA before checking out/building.
8. Keep build-only development images clearly separate from production-approved releases.

**Done means**
Every required gate runs; intentional failure prevents release promotion; tests verify destination contents and state convergence; CI failure messages identify a concrete failing boundary.

### 8.8 Package F — Scale after correctness

Choose a resumable CSV approach that handles quoted embedded newlines correctly. Do not equate physical newline count with CSV record boundaries. A byte-offset design must retain header/schema and immutable object version; a chunking design must preserve globally stable record identity and avoid overlapping chunks.

Before adding concurrency, define ordering scope: a row's chain is ordered; different rows may be independent only if the destination/business contract permits it. Bound concurrency and buffering, honor destination throttling, and preserve pause/cancel behavior.

Fix one bottleneck at a time and measure:
- Source bytes fetched and records parsed.
- Unique input IDs and unique destination effects.
- Completed and reconciled rows/second.
- p50/p95/p99 request and end-to-end latency.
- Worker memory, CPU, DB writes, and Temporal history growth.
- Duplicate/unresolved outcomes and recovery delay after worker failure.

A performance improvement cannot close MP-07 if it weakens MP-02/03/04. Update docs after measurements and link the benchmark configuration, commit, and raw results.

## 9. Integration test specification

### 9.1 Minimal deterministic fixtures

| Fixture | Required behavior |
|---|---|
| Source generator | Stable unique IDs, declared record count/hash, exact batch-boundary sizes, malformed record option |
| HTTP destination | Records effects in a durable ledger; configurable success, 4xx, 429, 5xx, delay, commit-then-disconnect |
| Idempotency mode | Deduplicates by configured key with controllable retention; separate mode ignores the key |
| Salesforce fake | Query-job identity, paged results, page locators, partial local-page consumption |
| Persistence fault | Fail specific step/row/finalization/export writes; model commit-then-lost-ack where feasible |
| Two-tenant fixture | Known sources, exports, users, roles, and secret references owned by different orgs |
| Runtime fixture | Temporal plus restartable workers and real Postgres; pinned test configuration |

Test names below are proposed. Add them to the appropriate language suites; their existence is not implied.

### 9.2 Required scenario matrix

| Scenario | What must be asserted | Findings |
|---|---|---|
| Sample another org's known export key | Access denied and storage read not issued | MP-01 |
| Submit another org's source/secret reference | No execution or credential disclosure | MP-01/10 |
| Source redirects to forbidden test address | Request blocked under configured policy | MP-01 |
| Continue twice with prior-run row failures | Cursor and cumulative failure outcome preserved | MP-02 |
| Input ends exactly at continuation boundary | EOF handled and job terminates once | MP-02 |
| Salesforce 4,501 records, 2,000-page/500-batch | Complete ID set, stable indexes, no skipped page remainder | MP-03 |
| HTTP commit then response loss | One effect with supported idempotency; explicit ambiguity otherwise | MP-04 |
| Successful step followed by failed checkpoint | Later steps stop; retry cannot silently lose the successful effect | MP-04 |
| Persistence commit followed by lost acknowledgement | Attempt/accounting remains correct on redelivery | MP-04 |
| Fast workflow completes before API marks running | Terminal state remains terminal | MP-05 |
| Ingest failure / worker death / DB outage | Persisted state eventually agrees with execution outcome | MP-05 |
| Export fails then retries | Migration effects remain unchanged; export status visible | MP-05 |
| Normal Python indexing and slicing | Correct value with sandbox restrictions retained | MP-06 |
| Large Python output / slow concurrent work | Bounded resources, cleanup, heartbeats and cancellation | MP-06 |
| Only row index zero is failed | Exactly that row is retried | MP-09 |
| Authenticated destination with org-owned secret | Correct auth applied; secret absent from history/logs | MP-10 |
| Failed test on release candidate | No approved release artifact published/promoted | MP-08 |
| Source size increases 10x | Measured work/memory/history match the chosen bounded design | MP-07 |

### 9.3 Commands and prerequisites

These are commands for the receiving agent to execute after preparing its environment. They were not all executed in the original review.

From the repository root, Rust:

```bash
cargo fmt --manifest-path apps/api/Cargo.toml --all -- --check
cargo clippy --manifest-path apps/api/Cargo.toml --all-targets -- -D warnings
cargo test --manifest-path apps/api/Cargo.toml
cargo test --manifest-path apps/api/Cargo.toml --test integration_db -- --ignored
```

The ignored DB integration tests require an available Docker-compatible testcontainers environment. Record a blocked prerequisite instead of skipping silently.

From apps/orchestrator-go:

```bash
go vet ./...
go test ./...
go test -race ./...
```

Use the repository/CI-compatible Go toolchain and linter versions. A workflow test with mocked activities does not replace real activity/database or Temporal-server integration tests.

For Python, use section 5 to reproduce the minimal failure, then the full project's dependency environment to validate worker changes. Run pytest and the configured Ruff check; include async test dependencies if adding async tests.

From apps/web:

```bash
npm ci
npm run lint
npm run build
npx playwright test
```

Playwright requires its browser dependencies and a configured running stack. From packages/rule-schema, run npm ci and npm run build; inspect and configure the fixture test runner as described in package E.

For a disposable full-stack test environment, inspect .env.example and docs/development.md, then use the project's make up and make seed flow. Generated local credentials are test-only. Do not run destructive reset targets against an existing environment.

### 9.4 Source-to-destination reconciliation

The integration harness must compare identities, not only counts. Two duplicated IDs plus two missing IDs can leave the count unchanged.

Record:
- Declared immutable source ID set and source version.
- Expected destination effect per row and per step.
- Actual destination receipts keyed by operation identity.
- Row outcomes, unresolved effects, and deliberate skips.
- Missing, unexpected, duplicate, or mismatched effects.

Expected accounting must cover all source records. Successful HTTP status is transport/application evidence; business-level validation should be explicit for the selected destination.

## 10. Required agent output and completion rules

Each implementation agent must return:

1. Findings addressed and the exact base/result commits.
2. Root cause and behavioral change in plain language.
3. Files and interfaces changed, including schema migrations and compatibility decisions.
4. Regression reproduction before the fix.
5. Actual commands, results, and relevant integration evidence after the fix.
6. Destination reconciliation results where external effects are involved.
7. Rollout and rollback considerations, including Temporal history compatibility.
8. Remaining findings, unavailable infrastructure, and explicitly unverified claims.
9. PR/commit location; distinguish local-only commits from remotely published commits.

Suggested final report:

```text
Package:
Finding IDs:
Base commit:
Result commit / PR:
Root cause:
Implemented behavior:
Schema/API/workflow compatibility:
Before-fix reproduction:
After-fix tests and results:
External-effect reconciliation:
Rollout / rollback:
Remaining limitations:
Status: implemented-unverified | verified-locally | verified-integration
```

Do not mark a finding verified-integration based solely on unit tests. Do not delete an acceptance criterion to make a task appear complete. If a finding is disproven on a newer checkout, identify the correcting commit and provide test evidence rather than silently removing it.

After A–E are integrated, a final reviewer must assess the five invariants in section 6 against the combined code. Per-package success does not prove that the merged system is correct.

## 11. Ready-to-use task brief for the next agent

> Implement the assigned work package from docs/ENGINEERING_REVIEW_HANDOVER_2026-10-02.md in rajivravindran/MigrationPlatform. Start by inspecting HEAD and applicable repository instructions, then revalidate the assigned findings against the reviewed baseline 18160097d3e95994bf8018838c9b43b719bcac49. Reproduce the failure before fixing it. Preserve existing licensing, scheduling, batch, and template semantics. Coordinate shared workflow/API/schema contracts as described in section 8. Add meaningful regression and integration tests using the scenario matrix in section 9. Do not claim exactly-once behavior for arbitrary HTTP destinations or treat a green counter as proof of source completeness. Complete the required report in section 10, explicitly separating executed tests from blocked or proposed checks. Commit reviewable changes using the available authorized workflow and state clearly whether they reached the remote repository. Begin with Package A unless the coordinator assigns another package; Package B and the MP-09 regression are useful independent early tasks.
