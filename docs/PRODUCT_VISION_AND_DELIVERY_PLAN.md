# Product vision and delivery plan

Updated: 17 October 2026. Evidence baseline: `1cb02cff0cae720a3d348472015f5a73f52d331a`.

## Goal and decisions

Build sellable migration software for teams moving legacy data into applications with suitable APIs. Provide role-specific workspaces for designing and testing mappings, operating migrations, coordinating dependency graphs, and reporting executive progress. Promote immutable templates and job definitions through GitHub into separate Development, UAT, and Production installations. Scale processing and preprocessing workers across pods. Make an uploaded archive the primary migration job, with file-level execution and recovery beneath it.

Additional goal requirements confirmed during UI review: replace anonymous job-number listings with meaningful migration identities; redesign the operations dashboard and row inspection experience; show the actual request, response and original source inline; and support in-place source correction and individual failed-row retry in test executions. These are required product outcomes, not optional cosmetic polish. Implementation remains paused while the user reviews and refines the feature plan.

Confirmed product decisions:

- Dependency completion is scoped to **campaign + environment + dependency version**, with a dependency key identifying the prerequisite within that scope. Production cannot inherit UAT completion.
- Development, UAT, and Production are **separate installations**, exchanging approved artifacts through GitHub.
- Release scope includes all existing features. The initial customer model is single-customer self-hosted, with a trusted team on a private network and a target of **one million rows per run**. These choices do not remove authentication, data-integrity, or credential-isolation requirements.
- Increasing processing and preprocessing replicas must increase useful capacity where destination quotas and available independent work permit. Compose remains a development/initial installation path; multi-pod behavior needs its own deployment evidence.

This document records the expanded product direction and delivery sequence. It supersedes the older roadmap's narrow first-release scope, but retains its engineering requirements and R08–R18 work. Historical `verified locally` statuses are historical evidence, not current release approval. The later principal and architecture reviews reopen relevant guarantees. This is a plan, not a claim of completed implementation.

## What exists and what remains

Presence means code exists; it does not mean a production acceptance gate has passed. Paths below are repository-relative.

| Feature | Existing foundation and evidence | Required refinement/addition |
|---|---|---|
| Access and roles | JWT auth, organization checks, admin/editor/operator/viewer; `apps/api/src/middleware/auth.rs` | Capability matrix, coordinator/executive experiences, authorization tests for every new action |
| Template designer | Draft/published versions, mapping canvas, literals, source/response fields, Python; `apps/web/app/templates/`, Go template package | Guided authoring, reusable test fixtures, immutable release bundle, clear validation and version comparison |
| Mapping tests | `apps/api/src/routes/dry_run.rs` previews up to 20 rows | Real isolated transformations and representative multi-step responses; full-source preflight and explicitly authorized destination rehearsal; preview currently uses placeholders for Python/previous responses |
| API destinations | HTTP operation chains, OpenAPI import, encrypted secret references | Declared adapter capabilities for auth, quotas, pagination, idempotency, reconciliation; compatibility tests per destination contract, not a universal compatibility claim |
| AI assistance | Configurable provider and mapping suggestions; `routes/llm.rs` | Preserve optionality, review generated mapping changes, test provider failure and data-disclosure controls |
| Source ingestion | CSV/JSON/XML, uploads, S3/MinIO watch, SFTP, Salesforce; Go connectors | Unified admission, immutable reads, connector-specific continuation and validation; verify all existing connectors |
| Archive migrations | ZIP/tar.gz unpacking, v1 manifest stages, dependency validation and stage execution; Go `internal/batch`, `workflows/batch.go` | Package-first UI, pinned bindings, manifest v2, campaign linkage, durable prior-package dependencies |
| Dependency graph | In-package DAG and sequential v1 execution | Coordinator graph editor, versioned graph releases, historical completion index, cross-package gating and history drilldown |
| Execution/recovery | Temporal, operation ledger, row outcomes, retries, pause/resume/cancel, continuation | Close integrity/lifecycle review findings; one canonical verdict and durable control/decision identities |
| Scheduling | Schedules and watched-source discovery | Intent-scoped deduplication/cursors, consistent connector/template admission, explicit replay policy |
| Scaling | Multiple Temporal workers can poll queues; Python workers are separate | Bounded chunk work, preprocessing limits, global destination quotas, measured autoscaling and recovery; current sequential row loops do not establish within-job parallelism |
| Promotion/CI | Versioned templates, CI/deployment assets | Portable artifacts, GitHub export/import, approvals, digest verification, installation-local binding and provenance; never copy live execution state |
| Operator evidence | Job details, counters, errors/events, CSV exports | Package summaries, exception workflow, revision-fenced evidence, reconciliation and audited corrections |
| Executive reporting | Common operations overview and basic counts; `apps/web/app/page.tsx` | Campaign progress, blocked paths, reconciled completion, trends and scoped drilldowns |
| Throughput | Metrics/telemetry packages | Useful-work throughput, preprocessing/API bottlenecks, queue lag, destination throttling, defensible ETA |
| Operations | Compose, Helm, licensing, secrets, audit foundations | Clean install/upgrade, backup/restore, retention, isolation, release evidence and support runbooks |

## User experiences

Use a shared campaign/environment context and consistent package status language. Users with multiple capabilities can switch workspaces; hiding a button never substitutes for server authorization. Preserve existing role compatibility while introducing capabilities rather than silently granting expanded privileges.

| Workspace | Primary journey | Useful first screen |
|---|---|---|
| Designer | Choose source schema → map → preview → run fixtures → test job → publish release → request promotion | Templates needing attention, validation failures, recent tests and release differences |
| Operator | Upload package → inspect manifest/preflight → inspect dependency blockers → submit → investigate → recover → reconcile | Pending/active packages, actionable exceptions, source/template versions and destination context |
| Coordinator | Create campaign → draw/version prerequisites → associate package definitions → inspect readiness → drill into prior executions | Dependency graph with blocked reasons, ready work and completion evidence |
| Executive | Choose campaign → compare planned/processed/reconciled totals → inspect risks and trend → drill into a blocked milestone | Business progress, unresolved counts, milestone status and measured throughput |
| Administrator | Configure destination bindings, access, limits, workers and installation integrations | Configuration health, capacity, approvals and audit history |

The package detail page should lead with a meaningful name, campaign, environment, progress and current blocker. Beneath it: file graph/table, outcomes, exceptions, evidence, and execution history. Keep internal IDs searchable but secondary. A dependency node opens a side panel containing its required version, qualifying completion, contributing package/file executions, errors, reconciliation evidence and historical attempts; links open the exact historical execution.

Graph UX needs status text/icons as well as color, keyboard-accessible list/table alternatives, search, filtering, collapsed groups and a focused dependency path for large campaigns. Executive percentages must use declared business units; do not average percentages across differently sized files or count retries as new records.

## UI goals added from screenshot review

Status: required in the target product; planned, not implemented or verified. Review a visual mockup and refine each feature before changing application code.

### UX-01 — Meaningful migration identity

Lead with migration purpose (for example, Customer migration), then package/archive name, source filename, destination, campaign and environment. Keep execution IDs searchable and copyable as secondary metadata. Show the hierarchy package → file migration → source record consistently across overview, lists, detail pages and dependency history.

Use explicit display names from the manifest/job definition. Default migration purpose to the template display name and package name to the archive filename; allow editing before submission. Do not infer names from sensitive payload contents. Retain separate stable identifiers so identical filenames and repeat executions remain distinguishable.

Illustrative presentation: **Customer migration · UAT**, followed by `wave-01.tar.gz › customers.csv`, destination and campaign, then progress, outcome, timestamp and execution ID. Actual values must come from stored execution metadata.

Acceptance: a user can identify purpose, source, destination and environment without opening an execution; repeated runs remain distinguishable; older records with missing metadata show truthful fallbacks rather than fabricated labels.

### UX-02 — Operations dashboard and visual redesign

Make active migration work the primary content. Use role-appropriate navigation, persistent campaign/environment context, and Upload package as the operator's primary action. Show running, blocked, needs-attention and reconciled work, with meaningful names, progress, destination, throughput and last activity. Provide search and status filters with counts. Explain blockers and their next actions. Display setup guidance only when relevant instead of permanently dedicating half the dashboard to it.

Use consistent sans-serif typography, deliberate spacing/alignment, restrained status colors, subtle dividers and fewer competing boxes. Include status text/icons, visible keyboard focus, accessible contrast and responsive layouts. Support the distinct coordinator and executive views already specified above.

Acceptance: users can locate a migration by name, archive, file or execution ID; distinguish failure from cancellation and unresolved outcomes; and reach relevant work directly from summaries. Dashboard metrics reconcile with execution evidence. Review the visual design before implementation.

### UX-03 — Inline row inspector

Clicking or keyboard-expanding a row opens a full-width inspector directly underneath that row, replacing the detached request/response panel. Keep the selected row highlighted. Its summary shows row number, configured business key when available, status, attempts, duration and a short failure reason. Provide Next failed row navigation.

Tabs:

- **Request** (default): actual constructed payload sent, HTTP method, destination URL and safely redacted headers. Select the API step for multi-step migrations. Separate previewed payloads from evidence of actual dispatch.
- **Response**: HTTP status, body, duration and understandable error details for the selected step/attempt.
- **Source**: original CSV columns/values and original raw record, with equivalent source inspection for supported JSON/XML input.
- **Attempts**: each attempt's timestamp, input revision, request, response and outcome; switching attempts must not mix evidence.

Use formatted, syntax-highlighted JSON with wrapping, copy and expanded-view controls. Explain No request was sent, Request was not captured, and an intentionally empty body distinctly; do not render an unexplained `null`. Keep the failure explanation and recovery action above technical details. Apply access controls/redaction to source and destination evidence.

Acceptance: expansion stays adjacent to the correct row; source, request and response are bound to the selected step/attempt; keyboard users can operate the inspector; large payloads remain usable without overwhelming the page.

### UX-04 — Edit and retry a failed test row

In an authorized **test execution**, let users edit failed-row source values in place through a column/value grid, validate the changes, preview the regenerated payload, and choose Retry this row. Clearly label Test mode and show the resulting attempt immediately. Preserve the original uploaded record and every previous attempt; corrections create a new input revision rather than rewriting history.

Editing is unavailable in production executions and must be enforced server-side. A timeout or other ambiguous destination effect requires an explicit recovery decision before redispatch. Specify whether a retry resumes a failed step or restarts the chain; source edits that affect earlier steps must not silently reuse incompatible earlier results. Review that policy as part of feature refinement.

Acceptance: invalid edits cannot dispatch; preview uses the edited input and bound template version; only the selected row is retried; repeated submission/redelivery does not create unintended extra effects; original evidence is retained; production editing is rejected by the API.

Delivery mapping: UX-01/02/03/04 belong to P2, with UX-04's durable recovery requirements also gated by P0/P6. Executive/coordinator dashboard integration continues through P4/P7. These requirements augment, rather than replace, the full original goal and release gates.

Status update, 19 October 2026: UX-01, UX-02 and UX-03 are implemented-unverified in `apps/web` together with role-specific workspaces (operator, designer, coordinator, executive, administrator) as client-side views over the existing server roles; UX-04 waits on an API for audited source overrides. See [ui/WORKSPACES_AND_REDESIGN_2026-10-19.md](ui/WORKSPACES_AND_REDESIGN_2026-10-19.md). The mapping canvas now supports one source field feeding several destinations (a layout defect hid the handles).

## Package and dependency contract

Current storage/API terminology calls the archive a `batch` and a file execution a `job`. Introduce user-facing terminology from `../CONTEXT.md` through an adapter, retaining existing IDs and compatible endpoints during migration.

Proposed manifest v2 shape (design example, **not accepted by the current parser**):

```json
{
  "version": 2,
  "packageKey": "wave-1-subscriptions",
  "plan": {"key": "billing-wave", "version": "1"},
  "files": [
    {
      "id": "subscriptions",
      "path": "subscriptions.csv",
      "sha256": "<source-content-digest>",
      "template": {"key": "subscriptions", "version": 3, "digest": "<release-digest>"},
      "requires": [{"key": "customers", "version": "1"}],
      "provides": [{"key": "subscriptions", "version": "1"}],
      "onFailure": "stop"
    }
  ]
}
```

Campaign and environment come from authenticated submission context and the installation identity, not unchecked archive claims. Resolve template/plan digests and source versions once at admission and persist the binding. Check paths, archive expansion/file limits, checksums, duplicate entries, missing files, schema compatibility and graph cycles before dispatch. Preserve unknown-version rejection.

The graph release defines prerequisite keys and versions. A requirement may resolve to a provider inside the package or a qualified historical completion in the same campaign/environment; persist the selected evidence identity. Never satisfy it merely because an unrelated old file used the same template or name. Define deterministic handling of multiple qualifying providers; fail ambiguous input rather than silently choosing an arbitrary result.

Proposed safe completion policy: successful execution plus required reconciliation evidence. Partial, cancelled, unresolved and failed executions do not satisfy dependencies. Evaluate readiness atomically with recording dispatch intent, including evidence revision, so concurrent packages cannot bypass the same gate. Invalidated evidence blocks work that has not started; already-started dependents become explicitly affected and require investigation. Do not pretend destination effects can be automatically undone.

Keep manifest v1 import support: when v1 has no dependency edges, preserve its existing sequential array order; when edges exist, preserve DAG semantics. Translate to a pinned internal plan without mutating historical executions. Freeze graph version for an execution; coordinator edits create a new version.

## GitHub promotion contract

Export a portable release bundle containing template definitions, job/graph definitions, transformation code and dependency versions, schemas, synthetic test fixtures, capability requirements and checksums. Exclude credentials, real customer payloads, responses and execution databases. Installation-local identifiers are resolved from logical references during import.

Development publishes a proposed change to GitHub. CI validates schemas, compatibility and tests and produces an immutable digest-addressed artifact. A reviewed approval promotes that same digest to UAT, where local secrets/destination bindings are validated and new test executions produce evidence. Production approval references the digest and UAT evidence; Production independently checks dependencies and creates new executions. A mutable tag or branch alone is not release identity.

Record author, commit, digest, checks, approver and installation import result. Import is idempotent and rejects incompatible versions or missing bindings. Reverting a definition selects an earlier release for future executions; it does not roll back migrated destination data. Credentials for the GitHub integration must be installation-managed and least-privilege. Promotion authorization and audit must be enforced on the server.

## Scalable execution architecture

Retain Rust API, Postgres, Temporal, object storage, Go processing and isolated Python transformation workers. Strengthen domain boundaries rather than introduce a language rewrite or a service per feature.

1. Admission owns validated immutable source/plan bindings and execution intent across upload, schedule and archive entry points.
2. Package orchestration owns graph readiness and file execution verdicts; it consumes the canonical persisted outcome rather than recalculating success from counters.
3. Preparation streams inputs into immutable bounded chunks with stable row identities. Format-aware parsing preserves CSV quoting, JSON/XML record boundaries and original row provenance. Avoid repeatedly parsing the entire CSV prefix on resume.
4. Processing dispatches bounded chunk activities to shared queues. Multiple workers can claim independent work; crash/redelivery uses durable operation identities and receipts. Increasing replicas does not authorize duplicate external effects.
5. Preprocessing has separately bounded queues/concurrency, memory/time limits and versioned transformation dependencies. Persist reusable results where required for deterministic recovery.
6. Destination admission enforces aggregate quota by destination account/contract across workers, with backpressure and fairness. Per-process token buckets are insufficient as the product-wide guarantee.
7. Checkpointing bounds both history events and payload size before dispatching more work. Preserve ordering for dependent operations; parallelize only independent records/chunks under explicit template semantics.
8. Evidence publication is fenced by outcome revision. Retries cannot let an old exporter replace newer reconciled results.

Measure committed useful rows per second separately from API attempts and preprocessing throughput. Report queue age, active work, latency distributions, retry/throttle rate, unresolved outcomes and worker resource saturation. Use low-cardinality metric labels; retain detailed row identity in scoped evidence stores. ETA requires a known denominator and sustained representative throughput; display unavailable/uncertain when those conditions do not hold.

## Delivery sequence and acceptance

All stages are planned unless stated otherwise. Each feature must have a written contract, observable user flow, failure semantics and executed acceptance evidence before it is called complete.

| Stage | Deliverable | Depends on | Exit evidence |
|---|---|---|---|
| P0 | Close principal F1–F6 and architecture A1–A6 findings; unified admission, lifecycle, source and operation contracts | Current review baseline | Real-stack interleavings: worker redelivery, ambiguous writes, mutable source, stale export, empty child, pause persistence failure, two schedule intents and multi-worker quota |
| P1 | Domain model, campaign/environment identities, immutable plan/release binding, graph versions and manifest v2 with v1 adapter | P0 contracts | Schema/API compatibility, v1 order preserved, bindings stable under retry, cross-campaign/environment/version negatives |
| P2 | Designer/operator UI, truthful preview, full preflight and isolated test jobs | P1 | Representative CSV/JSON/XML fixtures, Python/response mappings, explicit live test destination, actionable errors and accessibility checks |
| P3 | GitHub artifact export, CI validation, approvals and installation import | P1/P2 | Dev→UAT→Prod round trip preserves digest, excludes secrets/data, rejects tampering and missing bindings; independent execution evidence |
| P4 | Coordinator graph, cross-package gating and history drilldown | P1 and canonical outcomes | Prior package satisfaction, concurrent submissions, cycles, version mismatch, invalidation and graph edits tested; correct historical node links |
| P5 | Chunk preparation, worker/preprocessor scaling, aggregate quotas and bounded histories | P0/P1; integrate P4 | One-million-row runs, multi-pod worker loss/resume, bounded memory/history, stable identities and measured throughput under fixed quota |
| P6 | Business reconciliation, exception correction, cutover controls and revisioned evidence | P2/P4/P5 | Expected vs observed destination records, unresolved cases retained, audited correction, approval and repeatable evidence export |
| P7 | Executive dashboard, throughput/ETA, product notifications | P4/P5/P6 | Numbers reconcile to canonical outcomes; retries do not inflate progress; blocked/unknown states remain visible |
| P8 | Whole-product release qualification | P0–P7 | Exact-commit CI, all existing feature regressions, fresh install/upgrade, backup/restore, retention, licensing and operational runbooks; representative user acceptance |

P2 visual design and P3 artifact schema can be developed against stable P1 contracts while integrity fixes proceed; release cannot bypass P0. P5 load testing starts early enough to change partition design, then repeats against the final integrated build. Preserve R08–R18 requirements for relationships, reconciliation, recovery, cutover, alerts, performance, installation and customer evaluation rather than replacing them with UI work.

## Production release gate

No production-ready claim at this baseline. Required evidence includes:

- All confirmed correctness findings closed on the release commit, with regression tests exercising actual module boundaries and fault scenarios.
- All existing sources, transforms, schedules, archive flows, secrets, auth, exports, optional AI integration and licensing exercised or explicitly blocked from unsupported configurations. Scope cannot silently shrink.
- One-million-row tests with declared formats, row sizes, transformation costs, API steps, destination quotas and hardware. Set a numerical throughput/SLO target from representative measured workloads before performance sign-off; none has yet been established.
- Multiple processing/preprocessing replicas, worker loss, queue backlog, dependency concurrency and destination throttling tested with no silent missing/duplicate effects; uncertain external effects remain visible.
- Clean deployment and upgrade, backup restoration, bounded retention and redaction, authorization negatives and Python isolation verified on supported installation profiles.
- GitHub promotion evidence and coordinator/designer/operator/executive acceptance on the same release candidate.

## Evidence and next work

Read `PRINCIPAL_REVIEW_1cb02cf_2026-10-10.md`, `ARCHITECTURE_REVIEW_2026-10-17.md`, `ENGINEERING_REVIEW_HANDOVER_2026-10-02.md` and the retained R-task cards in `PRODUCT_ROADMAP.md` for detailed findings and acceptance requirements. Existing local verification does not replace remote CI, restore or capacity evidence.

Next implementation work is P0 contract repair and P1 schema/API design, accompanied by a clickable role-workspace prototype for user evaluation before replacing the current UI. This planning update changes no application behavior, executes no promotions and does not complete the active product goal.

### P0 progress — 19 October 2026 (orchestrator, local working tree, uncommitted)

Implemented-unverified in `apps/orchestrator-go`; unit, workflow and replay tests pass locally, real-stack interleavings not yet run. Evidence records are in `docs/evidence/`.

| Finding | State | Evidence |
|---|---|---|
| A1 one persisted terminal verdict; batch consumes it | implemented-unverified | [A1](evidence/A1_VERDICT_AUTHORITY.md) |
| A2 batch lifecycle convergence (both halves) | verified at API/DB boundary | [A2](evidence/A2_BATCH_LIFECYCLE.md), [P0 closure](evidence/P0_CLOSURE_2026-10-19.md) |
| A3 pause/resume command identity and acknowledgement | verified at API/DB/workflow-env boundary | [P0 closure](evidence/P0_CLOSURE_2026-10-19.md) |
| A4 execution intent for watched sources, per-schedule cursors | verified at DB boundary | [A4](evidence/A4_EXECUTION_INTENT.md), [ADR 0005](adr/0005-watched-execution-intent.md) |
| A5 unified admission | implemented-unverified | `apps/api/src/admission.rs` |
| A6 destination rate scope | interim deployment-wide budget; coordinated quota open | review A6 status |
| Plan binding: resolve stage template once | verified at DB boundary | [P0 closure](evidence/P0_CLOSURE_2026-10-19.md) |
| F1 generation-scoped retry decisions · F6 unknown-outcome classification | verified at ledger/activity boundary | [P0 closure](evidence/P0_CLOSURE_2026-10-19.md) |
| F2 version-bound reads | verified against an S3-compatible server | [P0 closure](evidence/P0_CLOSURE_2026-10-19.md) |
| F3 archive/stage versions · F4 history-aware batch sizing | implemented-unverified | [P0 closure](evidence/P0_CLOSURE_2026-10-19.md) |
| F5 export revision fence | verified at DB boundary | [P0 closure](evidence/P0_CLOSURE_2026-10-19.md) |

Remaining before P0 can be called closed: the eight live-stack scenarios listed in the closure record (Temporal + MinIO + workers), run on one immutable revision with raw evidence retained.
