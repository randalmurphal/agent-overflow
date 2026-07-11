# Workflows System — Milestones & Delegation Packets

> Execution plan for implementing `workflows-system.md` per
> `workflows-system-decisions.md` (binding) and `workflows-system-ui/UI-SPEC.md`.
> Claude orchestrates and reviews; codex executes well-scoped packets (isolated
> lanes for parallel work, direct dispatch for serial work); fable agents handle
> taste-critical UI passes. Every packet leaves `make check` + `make test` green
> and gets an independent codex review + Claude adjudication before merge.

## Milestone map (revised from D10 after D13–D15)

| M | Scope | Notes |
|---|---|---|
| M0 | Foundations + refactors | Shrunk: MCP-composition refactor removed (D15) |
| M1 | Definitions + validation + `ao` skeleton | Pure Go, test-heavy, highly parallel |
| M2 | Engine core | The heart; serial-ish, tight packets |
| M3 | Surfaces | UI-SPEC implementation |
| M4 | `ao` CLI full surface + studio + triage agent | Was "MCP toolset"; now CLI (D15) |
| M5 | Automations + starter content | Scheduler, pollers, notes, summaries |

Cross-milestone invariants (every packet inherits):
- Spec/decisions-log conformance is a review criterion, not a suggestion.
- New bound methods: methodgen regen + `LocalOnlyMethods` classification.
- New event kinds/channels follow the documented add-a-channel path; no generic
  passthrough.
- Migrations: append-only, own test, schema.md + AGENTS.md updated in-change.
- No silent failure paths; typed reasons everywhere a run stops.

## M0 packets

**P0.1 — Turn-observer registry (refactor).** An App-level subscription API for
turn lifecycle events (turn started/completed/errored, per-thread), replacing the
hard-coded per-feature branches in `app_provider_events.go`. Migrate discussions
(`syncDiscussionTurn`) onto it; design/dormant hooks unchanged. Invariants: no
ordering regression (observer dispatch after triage persistence); no new
goroutines per observer; unsubscribe is leak-free (invariant 17 applies).
Verification: existing discussion tests pass unmodified + new registry tests.

**P0.2 — Project slugs + per-project config dirs.** `projects.slug` migration
(stable, unique, generated from name with collision suffix; backfill), slug
resolution helper, `<app-config>/projects/<slug>/` layout helper. No UI.

**P0.3 — OS notifications service.** Register the Wails `NotificationService`,
authorization request at startup, one internal emit helper
(`notifyWorkflowEvent`), deep-link payload → frontend navigation event plumbing.
**User-assisted verification on the real Windows/WSL launcher** (the held spike):
confirm presentation + click-through on the actual install before the helper is
considered done.

**P0.4 — Docs hygiene.** CLAUDE.md: Core Principle 1 gains the workflow engine as
the second named coordination exception; remove the workflow entry from
"Deferred". Rewrite `docs/architecture/design-mode.md` to match the v42+ code
(flagged stale during investigation). Fix `schema.md`'s stale migrations-dir
pointer.

## M1 packets

**P1.1 — `internal/workflow/def`.** Workflow YAML types + parser; embedded
published JSON Schema (authoring aid, `$schema` header line); **envelope schema
generator** (D2a shape: flat, all-required, nullable branch fields, nested
`outputs`, status enum); the full dry-run validator (D2/D5/D6: producer/type
wiring, reachability, gate targets, loop bounds + ancestry, name collisions,
profile bindability, derived workspace need). Table-driven golden tests for every
validator error, each naming the offending element.

**P1.2 — Profile loader.** `profile.yaml` types/parse/validate (checks,
capacities, command bindings, reliability defaults, disposition policy,
worktree_setup, secret refs env|file). Secret resolution helper (resolve →
phase env, mask registry for D2 masking). No engine coupling.

**P1.3 — `ao` CLI skeleton.** Binary + loopback transport client (existing wire
shape, token/endpoint discovery from the app's config dir), `ao workflow list`,
`ao workflow validate` (links `def` directly — works with no app running).
Establishes the D15 token-scope plumbing shape (full command surface lands M4).

**P1.4 — Starter workflows + scaffold (content).** `build-and-validate`,
`multi-lens-review`, `poll-jira-and-start` (acli-bound via profile), `ao workflow
new` scaffold. Content packet; validated by P1.1's validator in CI.

## M2–M5 (outline; packets authored at milestone start)

- **M2:** migrations (work_items/phases/effects/automations/cursors/usage
  attribution) → engine FSM + queue toggle → phase runner (envelope wiring both
  providers per D2a, post-validation, question/stuck flow) → gates → teardown +
  watchdog + transient retry + budget → worktree-per-run + setup hooks + derived
  read-only + step mode → crash sweep. Integration-tested against both real CLIs
  behind a build tag.
- **M3:** UI-SPEC top-to-bottom: workflows pane (drill stack), sidebar section +
  footer badge, run detail + sweep, intake + chat confirm, disposition
  (merge machinery lands here), ReviewPane integration, notifications UX,
  `workflow:*` channels + stores. Fable pass on final polish.
- **M4:** full `ao` surface (run start/wait/status/output/list, notes, enqueue
  post-back, scoped tokens + effects ledger), studio mode (skill + entry
  points + hidden-history threads), triage agent (D4.9 as CLI-backed prompts).
- **M5:** scheduler (cron + internal events), pollers + cursors + Run now,
  job notes injection/update, coalesced summaries, artifact store (D13) if not
  already landed with M2 envelope work.

## Dev harness (user-owned, M0-adjacent)

A headless/dev harness the user is building before/alongside M0-M1. The milestones
consume it as follows — these are the interface expectations that make it maximally
useful to this work:

1. **Isolated state:** `--db`/env override for the SQLite path AND the app-config
   dir (workflow definitions + profiles live there per D1/D6 — a harness scenario
   must be able to ship its own workflows/profiles, not read the user's).
2. **Stub provider sessions:** a fake `provider.Session` implementation selectable
   per-thread/env — accepts Send/Interrupt, emits scripted provider events
   (streaming deltas, tool calls, turn-complete with/without `structured_output`).
   This is the piece that makes M2's phase-runner testable without live CLIs:
   question→answer→continue flows, envelope-absent (the D2a Claude failure mode),
   mid-turn interrupts. Pure replay can't do interactive flows; a stub session can.
3. **Event replay at the provider-event boundary** (post-adapter, pre-triage),
   reusing the existing `docs/references/fixtures/` NDJSON format — deterministic
   reproduction of session death, overload errors, backgrounded-task edge cases
   for M2 reliability tests (watchdog, transient allowlist, crash sweep).
4. **Browser-reachable frontend:** the transport already serves HTTP+WS, so
   Playwright drives the real frontend without the native webview. Stable
   `data-testid`s on new workflow surfaces become a UI-SPEC implementation
   requirement (M3), so delegated UI agents can self-verify against seeded states.
5. **Scenario seeding:** a dev-only path ("seed N runs in states X across
   projects") so M3 UI work and screenshots don't require executing real runs.
6. **Time control where cheap:** configurable-short watchdog/backoff values in the
   harness profile beat a fake clock; M2 tests need "T elapsed with no events"
   without waiting 15 real minutes.

Consumers: M2 integration tests (stub sessions + replay + crash-kill scenarios),
M3 UI verification (seeded states + Playwright), M4 `ao` CLI tests (isolated
config dir + running harness app). M0/M1 do NOT depend on it and can proceed in
parallel with its construction.

## Sequencing

M0 packets are independent (parallel lanes). M1 depends only on P0.2 (slugs) for
P1.2's dir layout. M2 gates on M1 (def/profile) and P0.1 (observer registry).
M3 gates on M2's channels/methods but its pure-presentation packets can start
against fixtures earlier. M4/M5 gate on M2; M5's scheduler is independent of M3.
