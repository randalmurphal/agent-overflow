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
- `harness.rpc('MethodName', ...)` calls bound methods by NAME STRING, so
  no compiler connects these call sites to the Go signature. Changing a
  bound method's parameters must sweep `e2e/tests` and `cmd/ao-harness`
  for that name (the dispatcher rejects a wrong arity with `bad_params`,
  which is 26 red specs, not a build error — 2026-08-31, the `ListItems`
  `inlinePreviews` param). `make e2e` is the gate that catches it; run it
  before merging any bound-signature change.
- `tests/*-helpers.ts` and `tests/probe-wire.ts` hold the wire builders
  and seeds their spec families share. Put a new provider wire shape
  there, not inline in one spec. `offhost-helpers.ts` also owns
  `answered(outcome, why)`: a wire-level spec that wants the PAYLOAD of a
  call needs the outcome union narrowed, and `expect(outcome.ok).toBe(true)`
  narrows nothing, so reading `.result` after it fails the launcher's
  typecheck rather than the assertion.
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

## Owning processes

`src/harness-process.ts` is the only place that reads the process table,
and everything it produces is evidence for a kill. It builds
`ProcessIdentity` / `ProcessRow` three ways (Linux `/proc`, darwin `ps`,
Windows CIM); a consumer that needs a field the platform branch forgot
does not fail — it reads `undefined` and degrades, so when adding a
field, add it in every branch, and prefer an assertion that fails on the
missing value over one that skips. Two rules keep the evidence real:

- **An identity carries its process group on Unix.** Escalation after
  the group leader exits authenticates through a surviving member proof,
  and `captureProcessGroupMemberProof` declines any identity without a
  `groupId` — so a platform branch that omits the field silently disarms
  teardown instead of failing loudly (the Linux branch did exactly that,
  fixed 2026-08-31). A row only becomes a proof once its executable
  resolves; on Linux that link is read per candidate, never per row,
  because the memory watchdog sweeps every row on a cadence.
- **Sweep `/proc` by name.** `readdir` with `withFileTypes` lstats the
  entries procfs leaves untyped, so a process exiting mid-scan raises
  ENOENT out of the whole scan; the watchdog reads that as a backend
  fault and takes the run down with it. Numeric names plus per-process
  reads already guarded against disappearance are enough.

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
- **An assertion that nothing happened waits for the thing that would
  have.** Emptiness is true before the work starts, so a spec that checks
  it without first waiting on a SETTLED rendered state is racing what it
  is about, and wins often enough to look green — two runs in three, for
  "a view-only device spends no refusal", which was passing over four
  real refusals (2026-08-31). Wait on the state the guarded path
  produces, and assert the capture itself saw traffic, so a broken probe
  reads as a failure rather than as a clean bill.
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
- **Every page this backend hands out shares ONE ui_state bucket.** Pane
  layout persists under the `client:<id>` scope and `/pageurl` answers with
  the same client id every time, so a second page BOOTS INTO the panes the
  first one opened — and then watches their threads. A spec that needs a
  client with no panes, or with a different set, opens it BEFORE any other
  page opens one; `HarnessReset` clears ui_state, so the first page of each
  test is the only one that can boot bare.
  `transport-watch-badge-carriers.spec.ts` turns on that ordering.
- **A spec boots its OWN backend only for state `harness.reset()` cannot
  undo**, and then owns everything downstream of it. The LAN bind and the
  canonical domain both PERSIST to the settings file and REBIND the
  listener, so borrowing the worker fixture's instance hands the next
  spec a rebound backend. Such a spec is `test.describe.serial` with its
  own `beforeAll`/`afterAll`, restores the settings it wrote, and — when
  its legs need different browser LAUNCH arguments, since
  `--host-resolver-rules` is process-wide — owns its browsers too.
  `harness-remote-device-lifecycle.spec.ts`,
  `harness-passkey-lifecycle.spec.ts` and
  `harness-provider-signin.spec.ts` are the three, and each header argues
  its own constraints where they bite. Read the passkey one before
  writing any WebAuthn case: the three requirements a page has to satisfy
  at once (secure context, a DOMAIN relying party, a non-loopback peer)
  admit exactly one shape, and Chromium's virtual authenticator has a
  ceiling the header names rather than stages around. The sign-in spec is
  the other kind of unresettable state: it ADOPTS provider accounts,
  which live in the account store rather than in anything
  `HarnessReset` clears.
- Otherwise each worker owns one backend. Tests share it and must leave
  it reset (the fixture does this) rather than booting their own. Production
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
  satisfy a later spec. That population is now EVERY spec that runs a
  turn: the event mapping (`internal/app/app_notification_mapping.go`)
  raises a `notification:send` when a top-level turn comes to rest, fails,
  or opens an approval, and withdraws it when the thread resumes. A spec
  asserting on notification traffic must therefore filter by thread id or
  kind rather than by "the next send".
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
