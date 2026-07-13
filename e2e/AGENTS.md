# e2e/

Playwright suite for the agent test harness: real backend + real SPA,
headless, isolated data dir, mocked providers. Full harness guide:
[docs/architecture/agent-harness.md](../docs/architecture/agent-harness.md).

## Layout

- `src/harness.ts` — the TS client: `launchHarness()` spawns
  `bin/agent-overflow --harness` on a temp data dir, parses the
  `__AO_HARNESS__` bootstrap line, and returns a `HarnessApp` speaking
  the transport wire (RPC by method name, event push) over one
  WebSocket. Also the reference for driving the harness from any other
  client (Playwright MCP sessions, ad-hoc scripts).
- `tests/fixtures.ts` — worker-scoped backend, per-test
  `harness.reset()`.
- `tests/harness.spec.ts` — the reference specs: boot, seeded history,
  live mock turn, frame-by-frame `step-gated` stepping, reset.
- `tests/workflows.spec.ts` — RPC/event-only workflow coverage: two-phase
  drain, human gate approval, same-session question answer, watchdog stall,
  and cancel/interrupt.
- `tests/workflows-sidebar.spec.ts` — workflow footer, status ordering,
  project attention roll-up, run navigation, and hidden workflow-phase threads.
- `tests/workflows-pane.spec.ts` — workflow overview/detail/run navigation,
  breadcrumbs, reload restoration, queued-item removal, and queue controls.
- `tests/workflows-run-actions.spec.ts` — approve/discard, question reply,
  stalled-run recovery, keyboard sweep navigation, and review companion UI.
- `tests/workflows-intake.spec.ts` — intake validation and typed seed submission.
- `tests/workflows-deeplinks.spec.ts` — cold workflow-item and triage-agent
  notification activation, including deleted-run fallback.
- `tests/workflows-helpers.ts` — shared workflow seeds, mock-provider scenarios,
  state waits, result envelopes, and compact workflow definitions.
- `tests/notifications.spec.ts` — OS-notification pipe: `HarnessNotify`'s
  typed degraded send error, cold activation through transport replay and
  the pre-hydration queue, and the `none`-target no-op log.

## Running

`make e2e` (builds `bin/agent-overflow` + `bin/ao-mockprovider`, then
`pnpm test` here). Override the binary with `AO_HARNESS_BIN`. Chromium
comes from the Playwright cache (`pnpm exec playwright install
chromium` on a fresh machine).

## Writing specs

- **Never sleep.** Await `harness.waitForEvent('harness:mock', ...)` /
  `'harness:replay'` / `'provider:turn_completed'` /
  `'workflow:item-state'` for backend
  progress, and Playwright's auto-waiting locators for the DOM.
- **Backend setup goes through RPCs** (`HarnessSeed`,
  `HarnessSetScenario`, `SendMessage`, ...), not the UI, unless the UI
  interaction is the thing under test.
- Draft threads (no items yet) are hidden from the sidebar — seed at
  least one turn, or send the first message before navigating, when a
  spec needs the thread visible.
- Each worker owns one backend; tests share it and must leave it reset
  (the fixture does this) rather than booting their own.
- Transport notification replay survives `HarnessReset`. Workflow UI surface
  specs therefore declare distinct no-op worker fixture identities, and each
  cold deep-link case declares its own identity, so an activation for deleted
  test state cannot redirect or satisfy a later spec.
