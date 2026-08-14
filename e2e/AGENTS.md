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
  (claude plan phase, codex units), a mixed-provider fan-out whose failed
  unit parks the run until `WorkflowRetryUnit` repairs it in place, and the
  same fan-out with BOTH units failed — the usage-limit shape — repaired by one
  `WorkflowRetryFailedUnits` call (D33).
- `tests/workflows-access.spec.ts` — `access` enforced at the provider session
  (§9, D22), asserted from the launch config each mock observed: one run per
  provider whose read-only phase is followed by a write phase, so both sessions
  share a worktree and any difference in permission mode / sandbox came from
  `access` alone. The Go unit tests cover the mapping; this covers the wiring
  surviving to argv and thread-start params.
- `tests/workflows-call.spec.ts` — call edges (§3a, D18/D35): a call phase
  whose child run lands in the *caller's* worktree rather than provisioning one,
  completing the parent on the child's declared outputs; a bounded
  self-call recursion terminating inside its declared `max_depth`; and a
  call-bound fan-out unit whose child runs in that unit's own sub-worktree
  instead — the one place a child's workspace is not its caller's, because
  isolation is introduced by fan-out (§9).
- `tests/workflows-wake.spec.ts` — thread binding and disposal (§5, D17/D23): a
  run bound to a chat thread waking it with its declared outputs when it rests,
  a pause interrupting a held turn and a resume continuing as turn 2 of the very
  same mock session, and a discard preview naming a unit checkout's uncommitted
  file before the discard removes every checkout and branch in the tree. The
  discard case reads and writes the run's real worktrees from Node, which is the
  only way to assert on work that exists nowhere else.
- `tests/workflows-resume.spec.ts` — provider-agnostic prompt/session recovery:
  Claude and Codex both prove that a valid cursor receives a short continuation
  on the same provider session, while a deliberately cleared cursor starts a
  new AO thread/provider session with the full authored prompt. It also proves
  a cold backend restart for both providers and Codex's authoritative
  `thread/resume` rejection after a retained cursor, including the superseded
  unsent attempt and automatic full-prompt reconstruction.
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
- `tests/workflows-cli.spec.ts` — the CLI execution surface (§5, D15/D17, D30)
  driven by the REAL `bin/agent-overflow` binary as a subprocess. The CLI is the
  app binary dispatched by verb, so this spec proves the entry dispatch too:
  an interactive session starting a run that binds to its thread, `run wait` /
  `run output` / `run list`, one-shot `run start --wait`, and the credential
  dying with its session. Then a phase holding `grants: [start-run, introspect]`
  starting a child run, the identical call surfacing the prior start instead of
  firing twice, a row-level refusal on a run it did not start, a typed
  `grant_required` refusal for `agent-overflow schedule`, and `ListThreads`
  coming back `method_not_found` for a scoped token. The mock provider has no
  exec step and cannot shell out, so the spec reads the AO_* environment of a
  LIVE session via `HarnessSessionEnv` (a read of the token registry, never a
  mint) and spawns the binary with exactly that env — everything past the
  process boundary is production code. One case resolves the command the way a
  session does: by bare name, with PATH set to *only* `<dataDir>/bin`, which
  passes exactly when boot published the canonical-name symlink. Requires
  `bin/agent-overflow`; `make e2e` builds it.
- `tests/workflows-overlay.spec.ts` — the workflows overlay
  (`docs/specs/workflows-system-ui/UI-SPEC.md`) driven through the REAL UI: the
  sidebar footer badge opening home and a parked gate resolving from its detail,
  the sweep stepping with j/k and auto-advancing past a receipt to all-clear,
  the discard loss preview proving nothing is destroyed before it is confirmed,
  `+ New run` starting a run, a question answered from the footer input (which
  also proves the §8 letter keys stay letters while a field has focus), the
  `/workflow` composer command completing from the slash menu and expanding at
  send time (the provider's received text carries the block, the transcript
  carries only the typed words — D31), and a view-only session with every
  mutating affordance dead. The view-only case
  rewrites `remote` on the `/bootstrap.json` response because the harness binds
  loopback only and that bit is computed from the peer's locality — the SPA
  still receives exactly the manifest a LAN browser would, and everything
  downstream of the fetch is production code.
  Two cases cover the run map's scroll contract (RUN-MAP §9), which only a real
  engine can prove: a reader who wheels UP disengages follow (escape is
  event-sourced — a programmatic scroll would not), keeps their position while
  the frontier advances, and gets it back only by clicking the chip; and a
  reader parked at the tail of a long map whose EARLIER rows grow under a real
  engine step — held on the mock's signal, so the growth happens when the spec
  releases it — stays on the same line to within a pixel while `scrollTop`
  moves by exactly what the document gained (§9.7 compensation). Both assert
  their preconditions (the surface really overflows; the growth really is above
  the viewport top), because a fixture that drifts should fail rather than
  quietly stop testing anything.
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
  `callChildren` is the call-linkage read (§3a) and goes through
  `WorkflowGetRunMap`: ONE name — `WorkflowRunMapRun` — for a run of the map,
  root included, because "child" is what the helper's filter produces and not a
  kind of run. It costs a whole-tree read per call (root plus every descendant,
  each frozen definition decoded server-side), so read it once and filter
  rather than calling it per candidate parent; it normalises the `omitempty`
  linkage numbers so a spec compares numbers rather than sometimes `undefined`;
  and it THROWS on a refusal (§4.2) instead of letting an id that names no run
  satisfy the `toHaveLength(0)` most callers assert.
- `tests/session-import.spec.ts` — session import through the REAL UI: the
  sidebar trigger opening the lazy modal, rows from BOTH providers with the
  provider segment / search / clear-filters narrowing them, a two-session
  import settling into threads whose imported history renders (including the
  Bash row's on-demand output payload), a multi-leaf Claude transcript landing
  as one thread per branch whose same-id divergent thinking payloads remain
  isolated through SQLite, transport, expansion, and repeated frontend-cache
  thread switches, the dedup that empties the catalogue on the next open, and
  "Check for Provider Updates" appending what the transcript grew on
  disk. The progress strip, the per-row outcome stamps and the Retry CTA are
  pinned on a run that FAILS a session — a clean run closes its own surface in
  milliseconds, so asserting the strip there would be a race; the happy path
  awaits the `session-import:progress` frames on the wire instead (the
  terminal frame carries no per-row detail, so `threadIds` is read off the
  row's own frame).
- `tests/session-import-fixtures.ts` — the hand-written provider homes that
  spec seeds. The harness already pins `$HOME` **and**
  `App.credentialHomeOverride` at `<dataRoot>/home`, and session import
  resolves its homes through that override, so seeding is just writing files
  under `harness.bootstrap.homeDir` — a harness process cannot reach the
  developer's real `~/.claude` / `~/.codex`. Claude transcripts are JSONL
  rows (linear + tool call, multi-leaf + subagent join, and one that lists
  cleanly but fails the writer); the Codex thread index is a `state_5.sqlite`
  built with `node:sqlite` in the same schema subset the Go fixtures use, in
  `journal_mode=DELETE` because the reader opens it `immutable=1` and would
  not see a WAL. The workspace the sessions record is a `HarnessSeed`'d git
  repo, so reset owns its cleanup.
- `tests/notifications.spec.ts` — OS-notification pipe: `HarnessNotify`'s
  typed degraded send error, cold activation through transport replay and
  the pre-hydration queue, and the `none`-target no-op log.
- `tests/boundary-probe.spec.ts`, `tests/reveal-drain-probe.spec.ts`, and
  `tests/reveal-slide-probe.spec.ts` —
  opt-in **instruments**, not assertions: they drive a real turn and dump
  per-frame samples (rAF gaps, reveal rate, mounted row count, scroll
  trace; the slide probe adds think-clamp geometry through the line-slide
  FLIP — box growth, layout re-pack, and the animated advance — for its
  incident-exact and paced control
  wire shapes) next to the test results for offline analysis. All sit
  behind the shared `BOUNDARY_PROBE` env gate and `test.skip` themselves
  in `make e2e`; they also want a WebKit browser
  (`pnpm exec playwright install webkit`) and a `UI_TRACE=1` harness
  build. All three share their wire shapes, seed/collect driver, and
  trace folds in `tests/probe-wire.ts`. Run one with
  `BOUNDARY_PROBE=1 pnpm exec playwright test <name>`.
- `tests/freeze-repro.manual.spec.ts` + `tests/freeze-repro-probe.ts` — the
  renderer main-thread freeze driver. It is a `*.manual.spec.ts`, which the
  base config `testIgnore`s: it needs a locally generated fixture that is
  verbatim real session content (gitignored, never committable) and it
  deliberately loads ~950 items into one pane, so it launches its OWN harness
  instead of borrowing the worker's. `scripts/generate-freeze-repro.mjs` reads
  the incident turns out of the local store READ-ONLY and writes
  `fixtures/freeze-repro/{seed.json,scenario.json,manifest.json}`: the earlier
  turns become completed `HarnessSeed` history, the dense turns become a
  Claude mock scenario replaying each item in recorded order with
  thinking/text deltas cut at the REAL `payload_chunks` boundaries, tool_use +
  tool_result pairs carrying the recorded output (file-edit results rebuilt as
  `tool_use_result` so their diff payload comes back byte-identical), and
  backgrounded Bash launches completed through the
  `task_started`/`task_updated`/`task_notification` trio. Liveness is measured
  OUT OF BAND (probes fired on a Node timer, never awaited in sequence) because
  a wedged main thread stalls every locator and evaluate alike; both CDP
  capture channels are armed before the replay for the same reason. Run it with
  `pnpm test:freeze-repro` (`playwright.manual.config.ts`); evidence lands in
  `fixtures/freeze-repro/evidence-<timestamp>/`.

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
  deletion drops the workflow rows (D25), but `HarnessReset` still deletes them
  itself first (`DeleteProjectWorkflowRecords`): reset removes the generated
  workspace tree wholesale rather than spending a git worktree removal per
  checkout on fixtures that are about to go anyway. A spec that asserts on a
  global count (the overlay's attention badge, the sweep total) depends on
  that explicit delete.
- Transport notification replay survives `HarnessReset`. Any spec whose
  backend state can produce a notification therefore declares a distinct
  no-op worker fixture identity, and each cold-activation case declares its
  own identity, so an activation for deleted test state cannot redirect or
  satisfy a later spec.
