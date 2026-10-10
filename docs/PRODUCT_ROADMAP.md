# MigrationPlatform — executable product roadmap

Date: 3 October 2026. Owner: Rajiv Ravindran. Implementation owners: unassigned.
Planning baseline: local `2395f5d`; underlying reviewed application commit `18160097d3e95994bf8018838c9b43b719bcac49`.
M0 closed locally on `965c01d` (2026-10-10). Evidence: [docs/evidence/R07_INTEGRITY.md](evidence/R07_INTEGRITY.md).
Status vocabulary (authoritative): `ready`, `blocked`, `in-progress`, `implemented-unverified`, `verified`, `deferred`. A task is `verified` only when its stated acceptance evidence was executed. This document does not approve production use or remote publication.

## 1. Product decision

Build a self-hosted migration workbench for implementation teams moving related business records from legacy files into HTTP-based business applications. The first reference scenario is synthetic billing migration: customers, subscriptions, and invoices. This is a demonstrator of the domain, not a supported Ensemble or Zuora connector or a claim of compatibility with those products.

The intended user can prepare a mapping, rehearse a migration, execute dependent API operations, investigate failures, recover safely, and produce evidence that the result is correct. The intended buyer is an implementation team or migration consultancy repeating this work for multiple customers. Buyer demand and willingness to pay remain hypotheses.

API orchestration alone is not a differentiator. Workato and Boomi already address that problem. Our hypothesis is that a focused, reusable migration workflow can reduce preparation, recovery, and reconciliation effort for a specific customer segment. Do not claim better reliability, lower total cost, or greater speed without comparison evidence.

The first release promise is deliberately bounded: one documented file-to-HTTP migration path, with accounted-for records and visible uncertainty after ambiguous writes. Do not promise exactly-once effects against arbitrary APIs, universal rollback, or production readiness based on Temporal's presence.

## 2. Reading order and authority

1. Inspect checkout, branch, dirty files, and applicable repository instructions.
2. Read this roadmap for sequence and product outcomes.
3. Read [engineering handover](ENGINEERING_REVIEW_HANDOVER_2026-10-02.md) for MP-01–MP-11 evidence, implementation packages, test scenarios, and release invariants.
4. Read [historical agent handoff](AGENT_HANDOFF.md) for existing functionality and deployment/license gates.
5. Consult [backlog](BACKLOG.md) for retained feature specifications and [older market research](research/real-world-gtm-and-enterprise-value.md) for hypotheses, not current capability certification.

This roadmap replaces the older research document's delivery ordering. It does not erase historical feature completion or close engineering findings. The handover remains authoritative for unresolved engineering acceptance criteria. Revalidate findings on a newer checkout; record correcting commits if already fixed.

Repository state was inspected locally for this plan. The remote was not refreshed in this task. Earlier remote documentation writes received HTTP 403; confirm publication before instructing agents working only on GitHub to use these files.

## 3. Existing foundation and limitations

| Foundation already present | Work still needed before relying on it |
|---|---|
| CSV/JSON/XML and Salesforce sources | Tenant-owned sources, safe ingress, continuation and pagination integrity |
| HTTP mappings and chained steps | Explicit operation identity, durable receipts, ambiguous-write recovery |
| Temporal jobs and batch DAGs | Reliable lifecycle convergence, bounded histories and verified restart behaviour |
| Python transformations | Guard fixes, bounded execution, timely heartbeat/cancellation |
| Results CSV and request/response UI | Business reconciliation, safe disclosure, export lifecycle independence |
| Templates, schedules, S3/SFTP watch | Immutable run bindings and preserved compatibility |
| Auth, roles, secrets, trial licensing | Close ownership and secret-resolution gaps; retain existing license gates |
| Compose, deployment assets, CI | Reproducible integrity tests and release promotion tied to tested commit |

A capability existing in the code does not mean its operational guarantees are verified. BL-001–003 are historically done; extend them rather than rebuilding exports and job inspection.

## 4. Milestones and exit gates

Use gates, not promised calendar dates. Estimate after R00 baseline reproduction. A milestone cannot advance because its UI looks complete.

| Milestone | Customer outcome | Tasks | Exit evidence |
|---|---|---|---|
| M0 — Trustworthy execution | A synthetic migration can fail and recover without silent loss or unauthorized access | R00–R07 | MP correctness/security closures, real-stack fault injection, exact-commit release gate |
| M1 — Complete migration workflow | An operator prepares and proves a related-record migration | R08–R11 | Immutable plan, preflight, durable relationships, destination reconciliation |
| M2 — Operator recovery | Operators resolve exceptions and approve an evidence-backed cutover | R12–R14 | Audited correction, explicit ambiguity handling, approval and alerts |
| M3 — Pilot readiness | A fresh installation completes the reference scenario under measured load | R15–R17 | Reproducible benchmark, install/restore evidence, complete demo runbook |
| M4 — Validate product value | Potential users establish whether the workflow saves meaningful effort | R18 | Recorded user evaluation and explicit continue/narrow/stop decision |

R18 discovery can begin before M0 finishes; real-data pilots cannot bypass technical gates. A synthetic demo, a pilot candidate, and a production-approved release are separate statuses.

## 5. Execution queue

M0 statuses below are from the local integrated commit `965c01d`. Later tasks stay `blocked` on R07 being published and reviewed remotely.

| ID | Task | Dependency | Status |
|---|---|---|---|
| R00 | Baseline and deterministic test harness | None | verified (`3b1ee66`) |
| R01 | Tenant-owned source and egress contract | R00 | verified (`9997c17`) |
| R02 | Python execution correctness | R00 | verified (`8c79a1c`) |
| R03 | Checkpoints, Salesforce pagination, row-zero retry | R01 contract agreed | verified (`48ee096`) |
| R04 | External-effect ledger and secret resolution | R01, R03 contracts | verified (`58a9c58`) |
| R05 | Job lifecycle and export convergence | R03, R04 | verified (`58a9c58`) |
| R06 | CI release gate and truthful capability docs | R01–R05 | verified locally (`965c01d`); GitHub run of this SHA is unimplemented |
| R07 | Integrated integrity acceptance | R00–R06 | verified locally ([R07](evidence/R07_INTEGRITY.md)); not published |
| R08 | Immutable migration plan and run manifest | R07 | ready |
| R09 | Preflight and rehearsal | R08 | blocked |
| R10 | Persistent business-key relationships | R08, R04 | blocked |
| R11 | Business reconciliation and evidence bundle | R09, R10 | blocked |
| R12 | Exception queue and audited correction | R11 | blocked |
| R13 | Cutover approval and execution controls | R11, R12 | blocked |
| R14 | Failure and unresolved-outcome notifications | R05, R12 | blocked |
| R15 | Bounded throughput and measured recovery | R07; final regression after R13 | blocked |
| R16 | Installation, retention and restore readiness | R11, R14, R15 | blocked |
| R17 | Reusable billing-migration reference pack | R09–R16 | blocked |
| R18 | Customer evidence and next-product decision | None for discovery; R17 for evaluation | ready for owner-led discovery |

R00 fixture preparation, R02, documentation corrections, and owner-led discovery can be separated if work is explicitly assigned. R03/R04/R05 share workflows and activities; integrate contracts in order. These roles do not require multiple agents. Do not launch sub-agents unless authorized.

## 6. Task cards: trustworthy execution

### R00 — Establish a reproducible baseline

**Outcome:** Every agent can distinguish existing failures from regressions.
**Inspect:** `.github/workflows/ci.yml`, `images.yml`, `docs/development.md`, `infra/docker-compose.yml`, `apps/api/tests`, `apps/orchestrator-go/internal/workflows/workflows_test.go`, Python tests, `apps/web/e2e`, `tests/load`.
**Deliver:** Record base SHA, runtime versions, commands and failures. Build a local destination fixture with a durable receipt ledger and deterministic fault switches: reject, throttle, timeout before write, commit then disconnect, and idempotency enabled/disabled. Add immutable synthetic sources and two-organization fixtures. New harness paths are an implementation decision; document the chosen entry point.
**Acceptance:** A documented command reproduces an ordinary migration and a forced failure; destination receipts can be inspected independently of platform counters. Fixtures cannot call real external services. Missing Docker/Temporal credentials or tooling are recorded as blocked checks. No real customer data or secrets are committed.
**Tests:** Use handover §9; fix stale load-client request shapes before relying on results.

### R01 — Authorize ingestion and freeze source identity

**Outcome:** A user can execute only approved, organization-owned sources.
**Inspect:** `apps/api/src/routes/{files,jobs,batches,connectors}.rs`, Go `internal/connectors/{csv,objectstore,salesforce}.go`, `internal/security/ssrf.go`, migrations.
**Deliver:** Implement handover Package A / MP-01. Agree the trusted source identifier/version contract with R03. Validate `source_ref` at the API rather than allowing a JSON string to fail later. Preserve explicitly configured private-network integrations while rejecting arbitrary local paths and unauthorized network targets.
**Acceptance:** Cross-organization sample/start/download/ retry requests cannot access data; redirected source requests obey policy; malformed source requests receive a clear client error and do not leave orphan running jobs. Changed source versions cannot silently resume. Include batch-extracted files and exports, not only uploads.

### R02 — Make transformations dependable

**Outcome:** Valid row expressions execute without starving worker control operations.
**Inspect:** `apps/transform-worker/migration_worker/{sandbox,worker}.py`, associated tests and pinned dependencies.
**Deliver:** Handover Package B / MP-06: supported guards, bounded blocking execution, periodic heartbeat, cancellation and subprocess cleanup.
**Acceptance:** Existing expressions, including indexing/slicing, work; forbidden operations remain rejected. Slow concurrent jobs, large output, timeouts and cancellation do not leak processes or block all heartbeats. Do not describe RestrictedPython alone as a security isolation boundary.

### R03 — Preserve ingestion position and retry coverage

**Outcome:** Restarts and continue-as-new cannot silently skip or repeat ingestion identities.
**Inspect:** Go `internal/workflows/workflows.go`, `internal/activities/activities.go`, `internal/connectors/{csv,salesforce}.go`.
**Deliver:** Handover Package C / MP-02, MP-03, MP-09. Typed versioned checkpoints include source version, cursor/page position, absolute identity/index and cumulative outcomes. Define replay compatibility before changing workflow input. Use explicit absent retry cursor or a correct initial sentinel.
**Acceptance:** Cross two continuation boundaries; include exact EOF boundary and failures in earlier runs. Salesforce fixture has 4,501 records with 2,000-record remote pages and 500-record local batches: all IDs survive restart. Retry-all includes row zero, multiple pages and the empty case. Malformed or expired checkpoints fail visibly instead of restarting at zero.

### R04 — Track external operations and resolve credentials safely

**Outcome:** The system knows what was intended, attempted, confirmed or left uncertain.
**Inspect:** Go `internal/workflows/workflows.go`, `internal/activities/activities.go`, `internal/security/secrets.go`, Rust secret/audit code, migrations and shared rule schema.
**Deliver:** MP-04/MP-10. Separate stable operation identity from execution attempt and persistence redelivery. Propagate receipt persistence failures. Resolve organization-owned secret references inside the HTTP activity; never return decrypted credentials into workflow history. Define a destination capability policy: verified deduplication, safe lookup/reconciliation, or manual resolution.
**Acceptance:** Commit-then-disconnect produces one effect with valid destination deduplication, or an explicit unresolved outcome when safety cannot be established. A failed receipt write stops dependent steps. Concurrent retry requests cannot double-issue an uncoordinated operation. Secret rotation/revocation and tenant isolation are tested; logs, exports, audit and Temporal history do not expose credentials.
**Contract:** Do not use the presence of an idempotency header as proof the destination honors it. Record scope and retention assumptions. Payload changes require explicit new intent/version policy.

### R05 — Make lifecycle and exports converge

**Outcome:** The UI tells operators the truth after errors and restarts.
**Inspect:** Rust `routes/jobs.rs`, Go workflow finalization, `internal/workflows/{batch,watch}.go`, `internal/activities/export.go`, job UI and API client types.
**Deliver:** MP-05. Document and enforce conditional state transitions. Separate execution outcome, reconciliation outcome and export availability. Add a repair mechanism for DB outages or lost finalization. Preserve licensing and batch failure/quarantine semantics.
**Acceptance:** A fast terminal workflow cannot be overwritten as running. Ingest, preprocess, cancel, worker crash and database outage converge to an explained state. Retrying an export causes zero destination writes. Batch, job, Temporal and UI states agree after recovery.

### R06 — Require validated releases and accurate documentation

**Outcome:** Operators can identify exactly what was tested and what remains unsupported.
**Inspect:** CI/images workflows, `packages/rule-schema`, architecture/security/operations docs.
**Deliver:** MP-08 and MP-11. Required checks gate release promotion of the exact SHA; development builds are not approved releases. Run schema tests, reproducible installs and the deterministic integrity harness. Correct unverified sharding, sandbox, redaction and throughput claims.
**Acceptance:** An intentionally failing test prevents approved image promotion. Record image digest, SHA and checks. Deployment-dependent controls link to actual configuration and evidence. Unit tests alone cannot close a real-stack requirement.

### R07 — Close the combined-system integrity gate

**Outcome:** Establish the first trustworthy synthetic file-to-HTTP path.
**Deliver:** Run handover §9 scenarios on the integrated commit, not separately passing branches. Record each MP finding as open, disproven with evidence, or verified. MP-07 scale completion may remain assigned to R15, but histories/resources must be bounded for the declared M0 test envelope.
**Acceptance:** All P0 findings and R01–R06 correctness gates closed with evidence; source IDs, destination receipts and persisted outcomes reconcile. Demonstrate worker restart, ambiguous write, first-row failure, source mutation, auth failure and export-only recovery. Published capability scope excludes unverified connectors. Existing license/deployment gates remain visible.

## 7. Task cards: complete operator workflow

### R08 — Bind a migration to an immutable plan

**Outcome:** A saved run can be explained and reproduced despite later edits.
**Inspect:** `apps/api/src/routes/{templates,jobs,batches}.rs`, migrations, Go template model, `packages/rule-schema`, `apps/web/app/templates` and `jobs`.
**Deliver:** Reuse template versioning if already sufficient. Add missing run bindings for source versions, template version/hash, destination identity, schema/validation versions and relevant execution settings. Secret references are captured, not secret values. Specify schema/API changes in a short ADR before implementation.
**Acceptance:** Editing a template cannot alter an existing run or resume. A manifest identifies exact inputs without credentials. Runs are organization-scoped. Old job records have an explicit legacy representation; do not invent missing historical versions. DB migrations preserve existing results and roll forward safely.

### R09 — Preflight and rehearsal

**Outcome:** Catch predictable errors before production writes.
**Inspect:** Existing `apps/api/src/routes/dry_run.rs`, template/spec routes, Python transforms, template designer and new-job UI.
**Deliver:** Extend the existing dry-run path. Validate required fields, types, duplicate/missing business keys, references, payload schemas and configured destination limits. Distinguish sample validation, whole-source validation and rehearsal against a sandbox destination. Dependent response values require declared fixtures or sandbox execution, not invented IDs.
**Acceptance:** Preflight issues no destination writes; reports scanned coverage and unvalidated checks. Invalid records have actionable locations. A changed plan invalidates prior validation. Sandbox rehearsal is visibly distinct from production execution. Never label a sampled pass as whole-file validation.

### R10 — Preserve relationships across records and stages

**Outcome:** Subscriptions and invoices reference the correct migrated customer.
**Inspect:** Go template context/destination files, batch manifest and workflow, persistence activities, Rust batch routes and migrations.
**Deliver:** Design an organization/run-scoped mapping from entity type + source business key to confirmed destination ID/receipt. Add or extend a validated lookup expression. Reuse existing `$fromResponse` for same-row chains; do not mistake that for durable cross-row relationships. Define stage dependencies and unresolved-parent behaviour.
**Acceptance:** Input reordering and restart preserve relationships. Duplicate business keys fail deterministically or follow an explicit approved rule. A child cannot write with an absent, failed or unresolved parent. Tenant/run lookups cannot cross boundaries. No N+1 unbounded scans; benchmark representative lookup volume.

### R11 — Reconcile business outcomes and export evidence

**Outcome:** An analyst can establish what actually migrated.
**Inspect:** `apps/orchestrator-go/internal/export/project.go`, `internal/activities/export.go`, Rust job routes, job/batch UI, existing BL-001 projection.
**Deliver:** Extend current export into a versioned evidence bundle: run manifest, source inventory, record dispositions, destination receipts, relationship checks, errors, unresolved operations and field-level reconciliation results. Support a destination read-back adapter for the reference fixture; label receipt-only checks as such. Include checksums and generation time without claiming a cryptographic signature unless implemented.
**Acceptance:** Equal counts with missing/duplicated IDs fail reconciliation. Expected-versus-actual IDs, parent links and selected values are compared. Evidence generation is repeatable and never mutates the destination. Unauthorized download is rejected; sensitive fields follow configured masking/retention. Every source record has one current disposition and an append-only attempt history. Unresolved records prevent a clean sign-off.

### R12 — Exception queue and safe correction

**Outcome:** Operators can repair a rejected record without rerunning the migration blindly.
**Inspect:** Existing row retry API/workflow, `apps/web/app/jobs/[id]/page.tsx`, audit code, BL-004.
**Deliver:** Filter failed, blocked and unresolved records. Permit validated source overrides for definitively rejected operations, with actor, reason, before/after redacted diff and new attempt identity. Preserve original input and history. Route ambiguous external effects through reconciliation before permitting replay.
**Acceptance:** Correction affects only selected intent. Successful parent effects are not repeated unintentionally; dependent payloads are reevaluated under explicit rules. Two operators cannot overwrite each other's resolution. Viewers cannot mutate. No editing saved response history or automatic reissue of an unresolved create. Test stale UI actions and overlapping retry-all.

### R13 — Cutover approval and controls

**Outcome:** A responsible operator can approve a known plan and control execution.
**Inspect:** Job/batch routes, pause/cancel controls, role model and UI.
**Deliver:** Approval references immutable plan, validation result and reconciliation policy. Changed bindings invalidate approval. Define pause/cancel semantics for queued versus in-flight requests. Provide a runbook for unresolved operations and corrective actions; no universal rollback button.
**Acceptance:** API as well as UI enforces permission and approval. A stale approval cannot start changed inputs. Pause prevents new dispatch after acknowledgement; in-flight effects remain accounted for. Cancellation never claims to undo completed HTTP writes. Resume preserves identity. Any future compensation requires a separately specified destination-specific contract.

### R14 — Notify on actionable failures

**Outcome:** Operators learn when intervention is required without watching the UI.
**Inspect:** BL-006, finalization/reconciliation transitions, organization settings and existing metrics.
**Deliver:** First support an authenticated, organization-owned webhook with a durable outbox, stable event ID, bounded retry and delivery visibility. Cover failed, unresolved and reconciliation-failed outcomes. Configure endpoints using egress protections; secrets stay out of logs. Email/Slack/Teams adapters remain later extensions.
**Acceptance:** Failed notification delivery cannot change migration outcome. Retries retain event identity and recipients can deduplicate. No source payloads or credentials in notifications. Use a local receiver fixture; this task does not authorize sending messages to real people or external channels.

## 8. Task cards: readiness and validation

### R15 — Scale within measured limits

**Outcome:** A migration completes predictably within a declared resource envelope.
**Inspect:** MP-07, CSV ingestion, workflow dispatch, destination rate limiting and `tests/load`.
**Deliver:** Format-aware resumable reading or immutable chunks; stable partitions; bounded row concurrency; explicit per-row ordering and destination limits; fair admission across jobs. Honor 429/Retry-After where applicable. Keep retry storms and Temporal history/payload sizes bounded.
**Acceptance:** Run repeatable 1k/10k/100k synthetic cases, plus two continuation boundaries using a test-configurable threshold. Record record width, steps, destination latency, concurrency, hardware, memory, source bytes parsed, history size, reconciliation and recovery time. Tenfold input growth must not require repeated quadratic prefix parsing. Larger volume claims need separate measurements. Run integrity regressions after introducing concurrency.
**Success measure:** Reconciled records per second and time to recover, never just API request acceptance rate. No unmeasured production SLA.

### R16 — Operable self-hosted installation

**Outcome:** Another engineer can install, upgrade, back up and recover the supported path.
**Inspect:** `infra/demo-pack`, Compose/Helm, `docs/install.md`, `operations.md`, `runbooks/restore.md`, existing licensing handoff.
**Deliver:** Clean-environment smoke, pinned release manifest, health checks, secret/bootstrap guidance, supported environment matrix, retention policy and a tested restore procedure covering DB, objects, required keys and Temporal state. Explain retention dependencies: deleting receipts can destroy safe-retry ability. Preserve licensing enforcement and clearly document network requirements.
**Acceptance:** Fresh install runs the reference migration; upgrade retains older results. Restore with pending work either resumes under verified identity guarantees or blocks execution pending reconciliation. Expired/deleted evidence cannot silently permit unsafe replay. No default demo credentials on the shared pilot installation. Kubernetes support is claimed only if tested; Compose evidence alone is insufficient.

### R17 — Deliver one reusable reference migration

**Outcome:** An evaluator understands the product through a complete scenario.
**Deliver:** Proposed directory `samples/billing_migration/` containing synthetic customer/subscription/invoice data, mapping templates, manifest, expected results, destination fixture instructions and an operator walkthrough. Include valid records, broken references, duplicate keys, rejection, 429 and commit-then-disconnect cases. Use existing batch/template packaging; keep credentials out of packs.
**Acceptance:** A fresh evaluator completes import → preflight → rehearsal → execution → exception resolution → reconciliation → evidence download using documented UI/API steps. Relationship checks pass, ambiguous writes are explained, exports agree with the UI. A second synthetic customer dataset reuses the pack without core-code edits. Demonstrate supported constraints openly. Do not use vendor branding to imply certified integrations.

### R18 — Validate value before expanding

**Outcome:** Decide whether to invest in the next domain or simplify the product.
**Owner:** Rajiv for recruiting/evaluating users; agents can prepare scripts and analyze supplied results. No unsolicited outreach.
**Deliver:** Interview guide and evaluation sheet for three prospective implementation users. Capture current workflow/tools, migration frequency, preparation hours, recovery pain, reconciliation effort, deployment constraints and purchasing process. Have evaluators run R17; record baseline and product-assisted completion time using comparable tasks.
**Decision gate:** At least two evaluators can complete the reference workflow and identify a concrete reason to use it again; at least one identifies a realistic pilot and buyer. This is a proposed internal threshold, not market proof. If unmet, record whether to narrow the domain, improve usability, retain as a portfolio/internal tool, or stop expansion. Do not invent customers, feedback, savings or willingness to pay.
**Next pack selection:** Choose based on observed migration demand and accessible sandbox/API contracts. Salesforce, ServiceNow, Dataverse and billing adapters remain candidates, not commitments.

## 9. Design rules across tasks

- Reuse Postgres, object storage, Temporal and existing services. New infrastructure or language rewrites need a concrete demonstrated requirement.
- Version shared contracts across Rust, Go, Python and TypeScript/schema fixtures together. Proposed symbols/routes in this plan do not already exist by implication.
- Keep secrets out of history, audit, evidence and logs. Payload PII needs a documented retention/disclosure policy independent of credential protection.
- Keep external effects in activities; preserve deterministic workflow replay. Document compatibility with existing histories and schedules before deploying workflow changes.
- Use additive migrations where practical; document backfill, concurrent old/new workers, rollback limits and restore consequences. Never drop production evidence to simplify a schema change.
- One current record disposition must be explainable from history. Separate source-record totals, API-operation totals and retry attempts; they are not interchangeable.
- Persist unknown outcomes explicitly. A successful HTTP status alone does not establish business correctness, and a timeout alone does not establish failure.
- Choose an operation-key policy before implementing edits: same intent and payload retain identity; a changed intent is linked and versioned, and cannot bypass reconciliation of an earlier uncertain effect.

Suggested ADRs, created by the owning task: source identity/checkpoints (R01/R03), operation/attempt/receipt model (R04), lifecycle/export authority (R05), immutable plan/approval (R08/R13), relationship/reconciliation model (R10/R11). These are deliverables, not files already present.

## 10. Backlog reconciliation and explicit deferrals

| Existing item | Roadmap treatment |
|---|---|
| BL-001 results export | Preserve completed implementation; extend in R11 |
| BL-002 source/batch context | Preserve; consume in R08/R17 |
| BL-003 request/error inspection | Preserve; extend in R12 |
| BL-004 correction/retry | R12 after identity and reconciliation |
| BL-005 field cryptography/KMS | Defer pending a real requirement and threat model; credential security remains M0 |
| BL-006 notifications | R14; webhook first |
| BL-007 intake priorities | Defer until measured admission/fairness needs; avoid premature scheduling complexity |
| BL-008 ETA/SLA | Measure in R15; expose uncertain ETA only after sufficient evidence; contractual SLA deferred |
| BL-009 MCP/agent surface | Defer until core API permissions, approval and retry semantics are safe; agent-ready documentation does not require MCP |
| Historical P4b S3 events | Retain polling until a pilot demonstrates need |
| Historical P5.1 license follow-ups | Preserve applicable security/release gates; do not disable licensing to get a pilot running |
| P5.2 Stripe/SKUs/marketplace | Defer until buyer validation |
| SSO/air-gap | Pilot-specific eligibility requirements; move forward if required, never imply existing support |

No general iPaaS clone, SAP protocol engine, broad connector marketplace, automatic AI production execution, guaranteed rollback, multi-tenant hosted SaaS rollout, or new monetization system in M0–M3. Preserve existing product functionality while narrowing what is verified and advertised.

## 11. Agent start procedure and completion contract

1. Read the entry documents and verify HEAD. Record uncommitted changes; do not overwrite them.
2. Choose one eligible task, assign yourself in your working branch, and state prerequisites and exact scope. Start with R00 unless an owner has assigned another ready task.
3. Reproduce the relevant failure or establish a measurable feature baseline. Consult handover §9.3 for existing language commands; do not invent a test runner that is not configured.
4. Agree shared contracts before schema/workflow changes. Use an isolated branch/worktree when agents share files.
5. Implement the smallest end-to-end slice including API/UI/schema consequences. Use deterministic tests at the risky boundary, plus real-stack validation for external effects.
6. Update the task's status/evidence record. A code commit without required integration infrastructure is `implemented-unverified`, not `done`.
7. Commit reviewable changes when authorized. Report local and remote state separately. The roadmap does not authorize customer writes, sending messages, production deployments, or bypassing failed permissions.

Status vocabulary: `ready`, `blocked`, `in-progress`, `implemented-unverified`, `verified`, `deferred`. A task is verified only when all its stated acceptance evidence exists. Record legitimate scope revisions with rationale; do not silently weaken criteria.

Use this record in the implementation PR or a task evidence file linked from the queue:

```text
Task ID / owner:
Base SHA / result SHA / PR:
Dependencies verified:
Problem and changed behaviour:
Files and schema/API changes:
Before-fix reproduction or feature baseline:
Commands actually run and results:
Destination receipts / reconciliation evidence:
Workflow replay and upgrade compatibility:
Rollout, rollback and recovery limits:
Blocked checks / residual risks:
Status and reviewer:
Remote publication status:
```

### Copy-paste kickoff

> Work in MigrationPlatform. Read docs/PRODUCT_ROADMAP.md, docs/ENGINEERING_REVIEW_HANDOVER_2026-10-02.md and applicable repository instructions. Start R00 unless a specific eligible task is assigned. Inspect current HEAD and preserve unrelated edits. Execute one coherent task through implementation and meaningful validation; record blocked integration checks honestly. Reuse existing functionality, preserve licensing and batch semantics, and do not treat counters or HTTP success as proof of data integrity. Update task evidence and commit reviewable changes using the available authorized workflow. Report whether the commit is local or published. Do not implement later roadmap phases merely because their dependencies are inconvenient.

## 12. Product success evidence

For each evaluated migration record: source size and entity counts, missing/duplicate/unresolved IDs, relationship errors, setup effort, exception-resolution effort, evidence preparation effort, reconciled throughput, recovery delay and environment cost inputs. Store raw measurements with the tested commit and configuration.

Technical acceptance for the reference scenario: zero unexplained missing records, zero unintended duplicate effects in destinations with the declared deduplication contract, zero broken required relationships, and all deliberately injected ambiguous effects visible until resolved. These are test gates, not universal guarantees.

Commercial acceptance remains an owner decision based on R18. A completed roadmap demonstrates a functioning, evaluated product workflow; it does not establish product-market fit.
