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

## Dev harness (BUILT — commit 6c2bfcbb; docs/architecture/agent-harness.md)

The harness exists: real backend + real SPA headless on an isolated `--data-dir`
(app-config dir and `$HOME` included — harness-local workflow definitions and
profiles per D1/D6), both providers pointed at `cmd/ao-mockprovider` (a real
subprocess speaking both stdio protocols — spawn/env/parser/triage all exercised),
JSON scenarios (deltas, tool calls, approval branches, `waitSignal` gates, stalls,
crashes, turn completion **with or without an envelope** — the D2a absent-payload
case is just omitting the field), wire-level fixture replay through the real
adapter + triage, record/replay bundles (wire + DB snapshot; not filesystem),
declarative `HarnessSeed` (real generated git repos) + `HarnessReset`, and a
Playwright suite (`e2e/`; `make harness` / `make e2e`).

**Obligations this places on the milestones:**

- **M2:** (a) watchdog / backoff / budget values MUST be config/profile-driven so
  harness profiles can shrink them (mock `stall` + `afterTurns:"silent"` produce
  "T elapsed, no events" deterministically); (b) extend `HarnessSeed` with a
  `workflows:` section ("N runs in states X") once migrations land — seeding goes
  through production paths, never raw side doors; (c) **known gap:** the mock acks
  `interrupt` but doesn't abort the in-flight scenario turn — extend the mock's
  engine (the adapter/engine split was built for it) when phase-runner
  interrupt→abort→terminal-frame flows are tested; (d) gate tests use
  `waitSignal` + `HarnessMockCommand({type:"advance"})` + `harness:mock` events —
  zero-sleep determinism for runs parked at human gates.
- **M3:** stable `data-testid`s on every new workflow surface (UI-SPEC
  requirement); e2e specs follow `e2e/tests/harness.spec.ts` conventions — never
  sleep, await `harness:mock` / `provider:turn_completed` / `harness:replay`
  events; backend setup through RPCs, not the UI.
- **M4:** `ao` CLI tests run against a booted harness (isolated config dir).
- **All packets touching harness/transport/mock/provider parsing:** `make e2e`
  passes alongside the standard gates. New `Harness*` methods stay on the Harness
  receiver (inherits the registration gate + LocalOnly marking).

## Sequencing

M0 packets are independent (parallel lanes). M1 depends only on P0.2 (slugs) for
P1.2's dir layout. M2 gates on M1 (def/profile) and P0.1 (observer registry).
M3 gates on M2's channels/methods but its pure-presentation packets can start
against fixtures earlier. M4/M5 gate on M2; M5's scheduler is independent of M3.
