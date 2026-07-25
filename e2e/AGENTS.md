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
  chain, human gate approval, same-session question answer, watchdog stall,
  and cancel/interrupt.
- `tests/workflows-rerun.spec.ts` — RPC-only global-pause hold, then rerun of a
  failed run with guidance, through to completion.
- `tests/workflows-tool.spec.ts` — `driver: tool` phases, which run **real**
  subprocesses: a green check routing its gate into an agent phase, a red check
  looping back and then parking (a non-zero exit is `passed: false`, not a
  failure), and a command writing its own envelope to `AO_ENVELOPE`.
- `tests/workflows-fanout.spec.ts` — fan-out phases: a static two-unit fan-out
  where each writing unit is isolated on its own branch and the join drives the
  gate, a dynamic `over:` fan-out whose width comes from a prior phase's array
  (claude plan phase, codex units), and a mixed-provider fan-out whose failed
  unit parks the run until `WorkflowRetryUnit` repairs it in place.
- `tests/workflows-wake.spec.ts` — thread binding and disposal (§5, D17/D23): a
  run bound to a chat thread waking it with its declared outputs when it rests,
  a pause interrupting a held turn and a resume continuing as turn 2 of the very
  same mock session, and a discard preview naming a unit checkout's uncommitted
  file before the discard removes every checkout and branch in the tree. The
  discard case reads and writes the run's real worktrees from Node, which is the
  only way to assert on work that exists nowhere else.
- `tests/workflows-automations.spec.ts` — automations (§11) driven entirely
  through RPCs: Run now going through the one start path with the reserved
  `trigger` / `job-notes` seeds reaching the phase's rendered prompt, a second
  press refused loudly while the first run is still active (and *not* recorded
  as a skip, because a human is present), and an `item-done` automation chaining
  off a finished run but recording a `self-chain` skip instead of chaining off
  its own. Cron *ticks* are not exercised — the scheduler's granularity is a
  minute, so tick arithmetic is unit-tested against a fake clock in
  `internal/workflow/scheduler` instead of costing a minute of wall clock per
  assertion.
- `tests/workflows-helpers.ts` — shared workflow seeds, mock-provider scenarios,
  direct start (`WorkflowStartRun`), the global-pause switch, state waits,
  result envelopes, and compact workflow definitions.
  `seedWorkflowProject`'s `repoFiles` argument commits files into the seeded
  git repository, which is how a tool fixture ships the script its profile
  binding names. Profile bindings are argv arrays pointing at real binaries or
  committed scripts — never shell strings, because the runner does not use a
  shell. `setCodexScenario` / `sessionConfigs` / `mockSessions` are the shared
  mock-observation surface: a scenario reaches only the mocks that register
  after it is set, which is how a spec stages one behaviour for a run and a
  different one for the session a recovery action starts.
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
- Transport notification replay survives `HarnessReset`. Any spec whose
  backend state can produce a notification therefore declares a distinct
  no-op worker fixture identity, and each cold-activation case declares its
  own identity, so an activation for deleted test state cannot redirect or
  satisfy a later spec.
