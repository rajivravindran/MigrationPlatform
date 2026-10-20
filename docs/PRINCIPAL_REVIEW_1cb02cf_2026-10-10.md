# Production-readiness review — 1cb02cf

Date: 10 October 2026. Review-only. Target `1cb02cff0cae720a3d348472015f5a73f52d331a`; comparison `3b1ee66...1cb02cf`.

## Decision

**Not production-ready for customer-data migrations.** The implementation has materially improved: source ownership, typed checkpoints, operation receipts, Python execution, lifecycle repair, schema tests, and integrity CI are now present. The previous review's four specific issues received fixes. However, the new implementation still permits duplicate effects under recovery, replacement source bytes, history exhaustion, and stale exported evidence. Local M0 closure is not a production release approval.

Use synthetic environments while closing the findings below. No product code was edited. The older untracked review was preserved. HEAD remained at the reviewed SHA during inspection.

## Scope and evidence

Read the roadmap, handover, R07 evidence, relevant ADRs, and changes since the last reviewed revision. Standards and Spec reviewers independently inspected the same revision. Deep inspection covered source authorization/read boundaries, archive staging, checkpoint/history control, operation state decisions, retry and export paths. Sampled CI, Python implementation changes, schema contracts, lifecycle repair, and test coverage.

Executed in this review:

- Python harness reconciler: **16 tests passed**.
- Go `internal/workflows` and `internal/lifecycle`: **passed**, including the workflow package's replay tests.
- Go `internal/activities` and `internal/connectors`: test execution was blocked by sandbox denial of local HTTP test-server sockets; escalation was declined. These are environmental blocks, not product test failures.
- The Standards reviewer separately ran three focused regression reproductions against an isolated archive at `/tmp/mp-standards-1cb`, using an in-memory ledger/loopback fixture and workflow mocks. They reproduced duplicate sends from reusable operator flags and incorrect receipt-error classification. These do not substitute for the blocked full suites or real database fault injection.

The R07 document reports local stack successes on `965c01d`, plus continuation evidence from `58a9c58`. Those reports are useful prior evidence; this review did not rerun the stack. Remote CI promotion denial, full DB-outage recovery, live source overwrite, maximum-chain history, load, restore, and production topology were not verified here.

## Architecture and invariants

Rust authenticates requests and resolves organization-owned sources; Postgres stores jobs, sources, row/step results and operation attempts. Temporal runs Go ingestion and HTTP activities, with Python transformations on a separate queue. Object storage holds inputs and exported CSVs. The operation ledger and external destination do not share a transaction. The registry's metadata check and object download are also separate operations.

| Required invariant | Assessment at this revision |
|---|---|
| Each source read is organization-authorized | Substantial improvement; no cross-org bypass established in reviewed paths |
| Bytes read match the frozen source version | Violated by F2/F3 |
| One deliberate retry does not multiply external effects on redelivery | Violated by F1 |
| History remains bounded for supported templates | Violated by F4 |
| Downloaded evidence describes current row outcomes | Violated by F5 |
| Approved artifact is validated on its exact SHA | Harness now wired into CI; actual remote promotion test remains unverified |

The verdict concerns recoverability and integrity, not feature completeness. No end-to-end exactly-once, measured throughput, RPO/RTO, or general security certification is established.

## Standards

### F1 — P1: A from-start retry can resend after its own success

Location: `apps/orchestrator-go/internal/activities/operations.go:105–110`; workflow input propagation at `internal/workflows/workflows.go:450–451`.

`Resend=true` is carried by every delivery of the CallEndpoint activity for a from-start retry. The succeeded-state branch checks that flag before replaying the saved receipt. Interleaving: operator requests one from-start retry; destination commits; ledger records success; worker loses the activity completion acknowledgment; Temporal retries the same activity input. The ledger sees succeeded plus Resend and issues another POST. Against a manual/non-deduplicating destination, this is an unintended duplicate, beyond the one replay the operator authorized.

This conflicts with roadmap §9's stable intent/attempt distinction and ADR0003's redelivery safety. Scope resend permission to a durable logical execution generation; consume it when that generation begins. Redelivery of that generation must replay its receipt. Do the same analysis for `ConfirmNotApplied`, which must not authorize a later unknown effect simply because the original operator decision remains in activity input.

Acceptance: inject loss of activity completion after destination and ledger commit during from-start retry; exactly one new effect is allowed. Verify a genuinely new operator retry can still create its explicitly authorized new attempt. Preserve old ledger evidence and define legacy-generation migration before rollout.

Verification: isolated regression tests produced three sends instead of the two deliberately requested (initial execution plus one authorized resend). A separate expired-lease case reused ConfirmNotApplied to authorize the newly ambiguous effect. Both tests failed their safety assertions as expected, confirming the defect with the in-memory ledger fixture.

### F6 — P2: Receipt-persistence failure can be reported as ordinary failure

Location: `apps/orchestrator-go/internal/activities/call_endpoint.go:169–173,188–195`; `internal/workflows/workflows.go:504–511`.

After a successful HTTP effect, failure to save the receipt returns an ordinary wrapped error. The workflow recognizes only selected application-error types as potentially applied. With supported maxAttempts=1, it records a failed row rather than unresolved. Failed persistence of an unknown response has the same classification gap. A focused workflow reproduction using the returned error shape produced Failed=1, Unresolved=0.

This violates roadmap §9's explicit unknown-outcome rule. The ledger still limits later manual-policy resends, so this is incorrect disposition and recovery guidance rather than a demonstrated duplicate by itself. Preserve potentially-applied classification on all post-send persistence errors. Acceptance: real ledger-write failure after a destination commit, followed through workflow/API/export, remains unresolved until reconciled.

## Spec

### F2 — P1: Object version verification is separated from the actual read

Location: `internal/activities/source_authz.go:171–185` and `internal/connectors/objectstore.go:52–60` under `apps/orchestrator-go`.

Authorization checks current HEAD metadata against the registered ETag, then returns that metadata as SourceVersion. The subsequent GET supplies empty options. If a producer replaces A with B between HEAD and GET, the activity reads B while reporting A's version. A one-batch source can finish successfully without another check.

R01 and ADR0001/0002 require frozen source identity. Bind the actual GET to an immutable version ID or the expected ETag using a conditional request. Apply the same condition to SDK retries. Acceptance: deterministic barrier between validation and GET, overwrite the object, and assert either original bytes or SourceChanged before any destination effect. Metadata-only mocks do not prove this boundary.

### F3 — P1: Archive execution ignores its registered version

Location: `apps/orchestrator-go/internal/activities/batch.go:185–199,469–472`.

UnpackAndStageArchive looks up ownership but discards the registry result, then downloads the current object. Register archive A, queue the batch, overwrite that key with B before unpacking or retrying: B's manifest and files are used without SourceChanged. Stage registration also supplies size but no ETag, allowing a same-size replacement before first ingestion to escape the intended registration check.

R01 explicitly includes archive-created files and prohibits changed versions silently resuming. Carry/enforce the archive version during download; record staged-file content versions after upload. Acceptance: overwrite the archive before unpacking and a stage with same-sized different content before execution; both fail closed. Treat existing unversioned stage records explicitly; do not invent historical hashes.

### F4 — P1: History checks occur too late for supported multi-step templates

Location: `apps/orchestrator-go/internal/workflows/workflows.go:212–216,234–237`; schema permits 10 steps at `packages/rule-schema/schema/rule-template.schema.json:59`.

All 500 rows are processed before checking whether to continue as new. A successful 10-step chain schedules 20 step activities plus one row-persistence activity per row: 10,500 sequential awaited activities in the first batch. With normal activity and workflow-task events, this is approximately 63,000 history events before the first continuation check. This exceeds the documented default 51,200-event limit; large activity payloads can reach the byte limit earlier. These are analytical counts, not a load-test result. See [Temporal's official limits](https://github.com/temporalio/documentation/blob/main/docs/evaluate/temporal-cloud/limits.mdx).

R03/R07 require bounded histories for the supported envelope. Derive batch limits from step/activity cost and payload size, or checkpoint safely within a batch. Acceptance: real Temporal execution of the maximum supported chain through at least two continuations, including failed rows and exact-boundary EOF; reconcile every source identity. Include replay compatibility for changed command order. Do not solve this by raising limits alone.

Spec axis: 3 findings, worst F2–F4 are P1 integrity blockers. Standards axis: 2 findings, worst F1 is P1 duplicate-effect risk. F5 below is an additional integrated finding.

## Additional integrated finding

### F5 — P1: Concurrent exports can publish stale results as current

Location: `apps/api/src/routes/jobs.rs:599–608,743–768`; `apps/orchestrator-go/internal/activities/export.go:45–70`; download trusts export_status at `jobs.rs:294–297`.

A standalone export uses `{job}-export`; row retries use `{job}-retry`, and normal workflow finalization also exports. These paths do not share exclusion or an outcome revision. Job status remains terminal during a row retry, so the API permits an export concurrently. All exports overwrite the same object keys and unconditionally set export_status=succeeded.

Concrete interleaving: export A reads failed rows and pauses before upload; retry B corrects the rows, exports current successes, and finishes; A resumes, overwrites B's CSV with old failures, and marks it succeeded. The download route serves stale evidence as current. Independent reads of all-results and failed-results files can also reflect different revisions.

R05 requires state/UI/export convergence. Give row outcomes a revision, write export objects under unique generations, and publish results_ref only if the generation still matches. Alternatively serialize every row mutation and export through one durable owner. Test the interleaving with barriers; verify stale completion cannot replace a newer export. Invalidate/regenerate affected exports without repeating destination calls.

## Status, 19 October 2026

All six findings have implementations on the local working tree; see [evidence/P0_CLOSURE_2026-10-19.md](evidence/P0_CLOSURE_2026-10-19.md) for what was executed (unit, workflow-environment, real-Postgres and S3-mock tests) and what still needs the live Temporal/MinIO stack. F1 and F6 are closed at the ledger/activity boundary; F2 is verified against an S3-compatible server; F3, F4 remain implemented-unverified end to end; F5 is verified at the database boundary.

## Failure and security assessment

The important remaining boundaries are not covered by ordinary success tests: receipt commit → activity acknowledgment; HEAD → GET; registered archive → delayed unpack; 500-row dispatch → history limit; and old export upload → new outcome publication.

Source authorization is substantially stronger. The source findings above concern content integrity under concurrent replacement by a producer/operator with storage write access; they are not claims that an arbitrary tenant can overwrite another tenant's object. No live attack was performed.

A further unverified risk needs a destination-specific contract: a negative lookup after timeout does not necessarily prove the original request cannot still commit remotely. Require an atomic business-key uniqueness/deduplication or an authoritative completion guarantee before treating absence as automatic resend permission. This is not counted among the confirmed findings above.

## Release gates and implementation handover

1. Fix F1 before relying on manual/from-start recovery. Add logical generation identity and lost-ack fault injection; retain immutable attempt history. Assess already-retried jobs for duplicates before another replay.
2. Fix F2/F3 together at the object-read boundary. Preserve org checks, use conditional/versioned reads, and require live overwrite regressions. Fail closed for unverifiable legacy inputs; document rollback limitations.
3. Fix F4 without weakening checkpoint/replay guarantees. Run maximum-chain and payload-boundary tests on real Temporal, with two continuations and destination reconciliation.
4. Fix F5 with generation fencing or shared durable serialization. Repair evidence via export-only actions; no destination writes.
5. Rerun the integrated gate on one immutable application revision and retain raw evidence. Exercise receipt-persistence failure, finalize DB outage, source mutation, retry redelivery, and concurrent export explicitly.
6. Verify required GitHub checks and failed-check publication denial for the release SHA and record image digests. Complete measured capacity and backup/restore/install acceptance for the intended production topology. R07 explicitly defers these; local synthetic passes do not substitute for them.

## Coverage limits

This is a focused principal-engineer readiness review of the remediation, not an exhaustive whole-repository audit. Full browser flows, all schedule/watch paths, credentials echoed by arbitrary destinations, complete OS sandbox isolation, dependency advisories, live Salesforce, deployment hardening, load and restore were not comprehensively reviewed or executed. No prior finding is declared universally solved merely because a new module exists. The source and retry boundaries above are the highest-value next tests.
