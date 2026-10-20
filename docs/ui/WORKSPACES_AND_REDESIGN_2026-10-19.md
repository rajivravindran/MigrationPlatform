# Web app: role workspaces, operations redesign, mapping fan-out (19 October 2026)

Status: implemented-unverified (built, linted, type-checked; Playwright run against a mocked API in a sandbox; not yet run against the real stack or the e2e suite).

## What changed

### Role-specific workspaces (product plan "User experiences")

- `lib/workspace.tsx`: a *workspace* is a view, not a permission. `workspacesFor(role)` maps the server roles to the workspaces a person may open (admin → all; editor → designer, executive; operator → operator, coordinator, executive; viewer → executive). The choice is remembered per browser (`migration.workspace`). The API still authorizes every action by role; hiding a button never substitutes.
- `components/shell.tsx`: sidebar per workspace (`navFor`), switcher in the account block, top bar with the Operations overview / Migration details toggle.
- `app/page.tsx`: one home page, five leads: Operator (running / needs attention / reconciled / quarantined, search and status filters with counts, next-action line per migration), Executive (planned / processed / reconciled % / unresolved, blocked work, packages — percentages use source records as the unit; retries do not inflate), Coordinator (ready / in flight / blocked packages / scheduled, packages with blockers, migrations blocking dependents), Designer (drafts, templates needing attention, recent runs), Administrator (counts and recent migrations).
- Campaign/environment context is not yet recorded by the API (P1 work), so the executive and coordinator views scope to the installation and say so.

### UX-01 meaningful identity, UX-02 dashboard, UX-03 inline inspector

- `lib/identity.ts`: purpose = template display name (fallback "Template #n · name not recorded"), package › file, Execution #id searchable but secondary; `outcomeLabel` distinguishes Reconciled / Needs attention / Cancelled / Paused / Running / Queued.
- `app/jobs/page.tsx`: migrations list by name, archive, file and execution id, with outcome filters.
- `app/jobs/[id]/page.tsx`: lead with purpose and source, stat tiles (records, succeeded, failed/unresolved with the first failure's plain-language reason, throughput for this run), source-record table with an inspector that opens *under* the selected row: failure explanation and recovery action first, then Request / Response / Source / Attempts tabs, step selector for multi-step templates, "Next failed row". Empty states are distinct: "No request was sent", "Request was not captured", "The body was intentionally empty". `data-status` carries the raw job status for tests.
- `components/ops.tsx`: Chip (text + icon, never colour alone), Stat, Panel, JsonView (highlighted, wrapping, copy, expand), Tabs (keyboard-operable).
- Visual system: dark navy surfaces, one serif family for text and numerals, monospace for payloads and ids; `tailwind.config.ts` remaps the neutral and tinted scales so the existing pages render on the dark surfaces without per-file edits.

Not done (needs API work): UX-04 edit-and-retry of a failed test row (no source-override endpoint or test-mode flag yet); per-attempt history (only the latest attempt per step is stored); configured business keys; campaign/environment chips.

### Mapping canvas: one source to many destinations

The data model and serializer already allowed a source field to feed several destination fields (`deriveStepJson` buckets by target). What blocked it in practice was layout: field cards were ~100 px tall on a 64 px pitch, so once a source had a mapping its card — and its drag handle — was covered by the next card. Changes in `components/mapper/`:

- `types.ts`: `ROW_H` 64 → 108 with a fixed 92 px card (`FieldNode`), so cards never overlap; a source card summarises "feeds N fields".
- `MappingCanvas.tsx`: after a click-to-connect the source stays selected so several destinations can be clicked in a row (Esc, clicking the source again, or clicking the pane clears it); the hint says so; the minimap only appears for larger graphs (it covered nodes); dark edge colours.
- Rules unchanged: a destination field accepts one source (query parameters accept several); a new connection to a mapped destination replaces its input.

Verified in a sandbox with Playwright: `name → email` and `name → display_name` created from one selection; dragging from the handle to a second destination also works.

## How to verify

```
cd apps/web && npm ci && npm run lint && npx tsc --noEmit && npm run build
npm run test:e2e   # against a seeded stack; job.spec/login.spec updated to the new wording
```
