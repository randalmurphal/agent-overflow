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
- `tests/workflows-access.spec.ts` — `access` enforced at the provider session
  (§9, D22), asserted from the launch config each mock observed: one run per
  provider whose read-only phase is followed by a write phase, so both sessions
  share a worktree and any difference in permission mode / sandbox came from
  `access` alone. The Go unit tests cover the mapping; this covers the wiring
  surviving to argv and thread-start params.
- `tests/workflows-call.spec.ts` — `shape: call` phases (§3a, D18): a call phase
  whose child run lands in the *caller's* worktree rather than provisioning one,
  completing the parent on the child's declared outputs, and a bounded
  self-call recursion terminating inside its declared `max_depth`.
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
- `tests/workflows-cli.spec.ts` — the `ao` execution surface (§5, D15/D17) driven
  by the REAL `bin/ao` binary as a subprocess: an interactive session starting a
  run that binds to its thread, `run wait` / `run output` / `run list`, one-shot
  `run start --wait`, and the credential dying with its session. Then a phase
  holding `grants: [start-run, introspect]` starting a child run, the identical
  call surfacing the prior start instead of firing twice, a row-level refusal on
  a run it did not start, a typed `grant_required` refusal for `ao schedule`, and
  `ListThreads` coming back `method_not_found` for a scoped token. The mock
  provider has no exec step and cannot shell out, so the spec reads the AO_*
  environment of a LIVE session via `HarnessSessionEnv` (a read of the token
  registry, never a mint) and spawns `ao` with exactly that env — everything past
  the process boundary is production code. Requires `bin/ao`; `make e2e` builds
  it.
- `tests/workflows-overlay.spec.ts` — the workflows overlay
  (`docs/specs/workflows-system-ui/UI-SPEC.md`) driven through the REAL UI: the
  sidebar footer badge opening home and a parked gate resolving from its detail,
  the sweep stepping with j/k and auto-advancing past a receipt to all-clear,
  the discard loss preview proving nothing is destroyed before it is confirmed,
  `+ New run` starting a run, a question answered from the footer input (which
  also proves the §8 letter keys stay letters while a field has focus), the
  `/workflow` composer command appending below an in-progress draft, and a
  view-only session with every mutating affordance dead. The view-only case
  rewrites `remote` on the `/bootstrap.json` response because the harness binds
  loopback only and that bit is computed from the peer's locality — the SPA
  still receives exactly the manifest a LAN browser would, and everything
  downstream of the fetch is production code.
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
  (the fixture does this) rather than booting their own. Production project
  deletion cascades the workflow rows (D25), but `HarnessReset` still deletes
  them itself first (`DeleteProjectWorkflowRecords`): reset removes the
  generated workspace tree wholesale rather than running git worktree/branch
  destruction against whatever a spec left behind. A spec that asserts on a
  global count (the overlay's attention badge, the sweep total) depends on
  that explicit delete.
- Transport notification replay survives `HarnessReset`. Any spec whose
  backend state can produce a notification therefore declares a distinct
  no-op worker fixture identity, and each cold-activation case declares its
  own identity, so an activation for deleted test state cannot redirect or
  satisfy a later spec.
