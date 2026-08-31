# e2e/

Playwright suite for the agent test harness: real backend, real SPA,
headless, isolated data dir, mocked providers. Full harness guide:
[docs/architecture/agent-harness.md](../docs/architecture/agent-harness.md).

`tests/*.spec.ts` is the index. Each file names its subject, and every
spec's own header comment says what it proves. Do not keep a per-spec
catalogue here: the last one had drifted 11 of 44 files behind the
directory.

## Where the shared pieces live

- `src/harness.ts` is the TS client. `launchHarness()` spawns
  `bin/agent-overflow --harness` on a temp data dir, parses the
  `__AO_HARNESS__` bootstrap line, and returns a `HarnessApp` speaking the
  transport wire (RPC by method name, event push) over one WebSocket. It
  is also the reference for driving a harness from anything else, such as
  a Playwright MCP session or an ad-hoc script.
- `tests/fixtures.ts` owns the worker-scoped backend and the per-test
  `harness.reset()`.
- `tests/*-helpers.ts` and `tests/probe-wire.ts` hold the wire builders
  and seeds their spec families share. Put a new provider wire shape
  there, not inline in one spec.
- `rigs/` holds self-driving perf measurement rigs (storm, churn,
  heapsoak, coldload). They are operator tools outside every gate, and
  [rigs/README.md](rigs/README.md) has the clone-root venue, the scenario
  reinstall rules, and the storm-density caveat.

## Running

`make e2e` builds `bin/agent-overflow`, `bin/ao-mockprovider`, and the
fixed-purpose `bin/ao-harness-e2e` launcher. The launcher typechecks the
suite first (`tsc --noEmit` over `src/`, `tests/`, `scripts/`, and both
configs), because Playwright and the flow runner only STRIP types: a
typo'd property in a helper's predicate would otherwise pass an emptiness
assertion vacuously. It then runs `pnpm exec playwright test` under one
process-tree memory boundary and host-floor watchdog. The complete
two-worker gate reserves 6 GiB. `pnpm test` here uses the same launcher
through `go run`. Override the backend binary with `AO_HARNESS_BIN`.
Chromium comes from the Playwright cache
(`pnpm exec playwright install chromium` on a fresh machine).

Not everything in `tests/` runs in the gate, on purpose. A
`*.manual.spec.ts` is `testIgnore`d by `playwright.config.ts` and needs
`playwright.manual.config.ts` plus a locally generated fixture. The
`*-probe.spec.ts` instruments skip themselves unless `BOUNDARY_PROBE` is
set: they dump per-frame samples for offline analysis rather than
asserting, so they are evidence, not a gate.

## Process identity is per-platform, and every field is load-bearing

`src/harness-process.ts` builds `ProcessIdentity` / `ProcessRow` three
ways (Linux `/proc`, darwin `ps`, Windows CIM). A consumer that needs a
field the platform branch forgot does not fail — it reads `undefined` and
degrades. `captureProcessGroupMemberProof` returns `undefined` for an
identity with no `groupId`, so the Linux branch omitting `groupId`, and
the Linux row builder omitting `executable`, made every process-GROUP
reaping assertion unreachable on the one platform that runs the gate here
(fixed 2026-08-31). When adding a field, add it in every branch, and
prefer an assertion that fails on the missing value over one that skips.

## Writing specs

- **Never sleep.** Await `harness.waitForEvent('harness:mock', ...)`,
  `'harness:replay'`, `'provider:turn_completed'`, or
  `'workflow:item-state'` for backend progress, and Playwright's
  auto-waiting locators for the DOM.
- **Backend setup goes through RPCs** (`HarnessSeed`,
  `HarnessSetScenario`, `SendMessage`, ...), not the UI, unless the UI
  interaction is the thing under test.
- **Assert the precondition your assertion depends on.** A surface that
  is supposed to overflow, a fixture that is supposed to have two rows: a
  drifted fixture should fail rather than quietly stop testing anything.
- **Ask the harness RPC, not the production reader, for a negative.**
  `App.ListThreads` hides the item-less draft row several bugs create, so
  "no row exists" goes through `HarnessListThreadRows`. Turn liveness
  comes from `ListItems` statuses, never `Thread.hasIncompleteTurn`, which
  is derived against `last_read_at` and flips when the UI opens the
  thread.
- **Open the page before the session when live progress matters.** Ticks
  are in-memory UI state that no reload recovers, so gate each one behind
  a mock `waitSignal` rather than racing it.
- **A scenario reaches only the mocks that register after it is set.**
  That ordering is how one spec stages one behaviour for a run and a
  different one for the session a recovery action starts.
- Draft threads (no items yet) are hidden from the sidebar. Seed at least
  one turn, or send the first message before navigating, when a spec needs
  the thread visible.
- Each worker owns one backend. Tests share it and must leave it reset
  (the fixture does this) rather than booting their own. Production
  project deletion drops the workflow rows (D25), but `HarnessReset` still
  deletes them itself first (`DeleteProjectWorkflowRecords`): reset removes
  the generated workspace tree wholesale rather than spending a git
  worktree removal per checkout on fixtures that are about to go anyway. A
  spec that asserts on a global count (the overlay's attention badge, the
  sweep total) depends on that explicit delete.
- Transport notification replay survives `HarnessReset`. Any spec whose
  backend state can produce a notification therefore declares a distinct
  no-op worker fixture identity, and each cold-activation case declares
  its own, so an activation for deleted test state cannot redirect or
  satisfy a later spec.
- **Navigate with `harness.open(page)`, never `page.goto(harness.url)`.**
  A page URL carries a one-time ticket the first load exchanges for an
  HttpOnly session cookie, and each Playwright context is a fresh cookie
  jar, so every navigation needs a ticket of its own. `open` asks the
  running instance for one (`GET /pageurl`, session token in an
  `Authorization` header). `harness.url` is the boot URL's identity —
  origin, page marker, client id — not something to navigate to twice.
- Provider homes are seeded by writing files under
  `harness.bootstrap.homeDir`. The harness pins both `$HOME` and
  `App.credentialHomeOverride` at `<dataRoot>/home`, so a spec cannot
  reach the developer's real `~/.claude` or `~/.codex` even by accident.
- The mock provider cannot shell out, so a spec that must exercise a real
  subprocess reads a live session's `AO_*` environment through
  `HarnessSessionEnv` (a READ of the token registry, never a mint) and
  spawns the binary with exactly that env. Everything past the process
  boundary is then production code.
