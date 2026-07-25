# Agent Test Harness

The harness boots the **real backend and the real SPA** headless, on an
isolated data directory, with both provider binaries pointed at
`ao-mockprovider` — so an agent (or a Playwright script) can exercise
any UI flow, reproduce streaming/rendering bugs frame-accurately, and
capture evidence, without touching real app data or a real
Claude/Codex process.

Everything below is reachable two ways, by design:

- **Checked-in Playwright specs** in `e2e/` (`make e2e`).
- **Interactively** — boot `make harness`, open the printed URL in
  Playwright MCP (or any browser), and drive the backend over the same
  WebSocket RPCs the specs use.

## Boot

```
make harness                    # build + run at a per-checkout /tmp scratch dir
bin/agent-overflow --harness --data-dir <scratch> [--mock-provider <path>] [--listen 127.0.0.1:0]
```

`--harness` is the only way the harness surface exists: the `Harness`
RPC receiver is registered on the transport exclusively by this boot
path (`main_harness.go` → `bootTransportOptions.HarnessReceiver`), so
no other mode has these methods on the wire at all.

Boot performs, in order (`prepareHarness`):

1. **Data-dir isolation.** `--data-dir` is required and refused if it
   resolves to the OS config root or the real app data dir; the root,
   its `agent-overflow` child, and the redirected home are also refused
   if any of them is a symlink (these dirs are seeded and wiped
   wholesale — a planted link would aim that at whatever it points to).
   The DB, settings, replay logs, and attachments live under
   `<dataRoot>/agent-overflow/`.
2. **HOME redirect.** `$HOME` (and `%USERPROFILE%`) point at
   `<dataRoot>/home`, so `~/.claude` scans, `~/.codex` tails, and git's
   global config are harness-local and seedable. A minimal `.gitconfig`
   is written so fixture commits work. Set `AO_HARNESS_KEEP_HOME=1` to
   opt out (e.g. replaying against real provider session files).
3. **Mock provider resolution.** `--mock-provider`, else the
   `ao-mockprovider` binary next to the running executable (where
   `make harness-build` puts it). Validated eagerly.
4. **Settings seed.** `claudeBinaryPath` and `codexBinaryPath` both
   point at the mock; the NDJSON event log
   (`observabilityEventLogEnabled`) is switched on so every session is
   recordable for wire-level replay. The mock path is additionally
   pinned at spawn-resolution time (`App.providerBinaryOverride`), so
   even an `UpdateSettings` call after boot cannot repoint a spawn at a
   real `claude`/`codex` binary.

Then the mock-provider control server starts (before `App.Start`, so
every provider spawn inherits its env), the transport comes up, and
stdout carries exactly one parseable line:

```
__AO_HARNESS__: {"url":"http://127.0.0.1:PORT/?token=...","port":PORT,"token":"...",
                 "dataRoot":"...","dataDir":"...","homeDir":"...","mockProvider":"...",
                 "pid":123,"version":"...","startupError":"only on failed boot"}
```

`url` goes straight into a browser / `page.goto()`. `token` opens the
RPC WebSocket. All subsequent logging goes to stderr.

## RPC surface

One WebSocket carries everything:
`ws://127.0.0.1:<port>/ws?token=<token>`, frames per
`internal/transport/AGENTS.md`. Call methods **by name**
(`{type:"rpc", id, method:"HarnessInfo", params:[...]}`); both the
`Harness` receiver and every bound `App` method (`CreateThread`,
`SendMessage`, ...) share the wire. The whole Harness receiver is
`LocalOnly` — a LAN-bound harness refuses it for non-loopback peers.

| Method | Purpose |
|---|---|
| `HarnessInfo()` | Identity + evidence paths (DB, event-log dir, UI trace, frontend error log). |
| `HarnessEmit(channel, payload)` | Publish a raw event on the bus — escape hatch for injecting one-off frames at the frontend. |
| `HarnessSeed(spec)` | Declarative fixtures: projects (existing path or generated git repo), threads, pre-baked turn/item history, plus project-scoped workflow definitions/profile/items. Returns created ids. |
| `HarnessReset()` | Blank slate without a reboot: set the global workflow pause and cancel every live run through the production cancel path, stop sessions, settle in-flight turns, delete the workflow run records (`DeleteProjectWorkflowRecords` — production deletion drops these too under D25, but reset drops them first so the delete has no worktrees left to walk against a spec's fixtures), delete projects through the production cascade, remove workflow config/run dirs and generated seed workspaces, and drop harness-owned state (scenario rules, active replay, in-flight recording, mock registrations). The pause is then **cleared**, not restored, so a spec that deliberately left the engine paused cannot hold every later spec's runs in the same worker. Recorded bundles survive. Reload the page after. |
| `HarnessSetScenario(spec)` | Install/replace a mock scenario rule (library `name` or inline `scenario` JSON, optional `cwd` scope). Validated at set time. |
| `HarnessClearScenarios()` / `HarnessListScenarios()` | Drop rules / list library + active rules. |
| `HarnessListMocks()` | Registered mock processes in spawn order. |
| `HarnessMockCommand(mockId, cmd)` | Drive a live mock: `advance` (release a `waitSignal`/`stall` gate), `emit` (inject wire lines, `${VAR}`-substituted), `exit` (code). |
| `HarnessRecordStart(name, threadId)` / `HarnessRecordStop()` | Capture a replay bundle: DB snapshot at start + the event-log slice recorded until stop. Start requires the thread to be idle (no turn in flight) so the snapshot/event boundary is exact; a failed stop discards the recording and frees the name. |
| `HarnessReplayBundle(name, opts)` | Restore a bundle's DB snapshot and replay its events with original timing. Refused while another replay is active (checked before the destructive restore). |
| `HarnessListBundles()` | Enumerate saved bundles. |
| `HarnessReplayStart(path, opts)` | Replay a raw event-log NDJSON file (no DB restore). |
| `HarnessReplayPause/Resume/Step/Stop/Status()` | Playback control; `Step` releases exactly one event while paused. |

`ReplayOptions`: `speed` (multiplier), `maxGapMs` (cap long recorded
gaps), `startPaused`, `threadFilter`. Status transitions push on the
`harness:replay` channel, so a test can await
`{state:"done"}` instead of sleeping. Pause takes effect even mid-gap
(no event escapes after Pause returns); a step during a pause releases
the next event immediately, skipping the recorded gap; a second Step
while one is still pending errors instead of silently coalescing.

### Seeding vs. live turns

`HarnessSeed` writes ordinary thread *completed history* — the rows the app
itself would have persisted after the fact. Workflow items are different: the
seeder writes definitions/profile files to the production project config layout
and calls `WorkflowStartRun`, the one start path every producer uses; the run
executes through the real engine and mock provider. It never inserts work-item,
phase, or unit rows directly.

The project-level workflow seed shape is:

```json
{
  "workflows": {
    "definitions": [{
      "name": "review-flow",
      "yaml": "id: review-flow\n...",
      "prompts": {"review-flow.md": "Review the change..."}
    }],
    "profile": "reliability:\n  watchdog: 100ms\n  backoff: [1ms]\n",
    "items": [{
      "workflow": "review-flow", "goal": "Review this change",
      "seeds": {"goal": "Review this change"},
      "stepMode": false, "count": 2, "target": "done"
    }]
  }
}
```

Definition `name` is a filename stem; YAML retains the authoritative workflow
id. Prompt keys are confined sibling-relative paths. `count` defaults to one,
and `target` defaults to `running`; supported targets are `running`,
`needs-human`, and `done`. A spec expands to at most 100 workflow items.
There is no queue to order against (rev 2): every run starts immediately and is
bounded by provider resource capacity. A `running` target returns as soon as the
run has started; the other two subscribe to `workflow:item-state` **before**
starting, so a run that reaches its target inside the start call cannot be
missed, and fail loudly (30s) rather than hanging if it rests somewhere else. A
fixture that must exist without executing is seeded with the global pause set
(`WorkflowSetGlobalPause`), which holds its first phase without parking it.

Live ordinary-thread behaviour (streaming, approvals, turn lifecycle) remains
the mock provider's job: create a thread, `SendMessage`, and let the scenario
stream.

**Gotcha:** `App.ListThreads` hides draft threads (no items yet) from
the sidebar. A seeded thread with no turns — or a live thread before
its first message lands — is invisible in the UI until its first
message exists. Tests that open the UI first must seed at least one
turn, or send the message before navigating.

## The mock provider (`cmd/ao-mockprovider`)

One binary impersonates both providers, sniffing argv: Claude's
`--input-format stream-json` NDJSON session (plus the `--max-turns 0`
probe), and Codex's `app-server` JSON-RPC 2.0 (plus `--version`
satisfying both version gates). Scenarios script what it streams.

### Scenarios (`internal/harness/scenario`)

A scenario is a JSON document: `onStart` steps, `turns` (each a step
list consumed per user message), and `afterTurns`
(`repeatLast` | `silent` | `exit`). Steps:

- `emit` — write wire lines (`delayBetweenMs`, or `chunkBytes` +
  `chunkIntervalMs` for partial-line stress),
- `fixture` — stream a recorded NDJSON fixture file (`fromLine`/`toLine`),
- `delayMs`, `writeFile` (real workspace mutations so diffs/git are real),
- `approval` — raise a permission request, branch `onAllow`/`onDeny`,
- `waitSignal` — block until a named `advance`,
- `stall` — hang until `advance` (or `durationMs`),
- `exit` — die with a code mid-turn.

Lines substitute `${SESSION_ID}`, `${THREAD_ID}`, `${TURN}`,
`${TURN_ID}`, `${REQUEST_ID}`, `${CWD}`.

An inbound provider interrupt aborts the active scenario in the shared engine.
It releases a blocked `waitSignal`/indefinite `stall`, skips all remaining
steps at the next boundary, reports `turn_interrupted`, and then writes the
provider-native terminal sequence: Claude's successful control ack followed by
`result{subtype:error_during_execution, terminal_reason:aborted_streaming}`;
Codex's successful RPC response followed by
`turn/completed{turn.status:interrupted}`. An interrupt received with no active
or dispatching turn is a no-op and cannot poison the next turn.

The embedded library (`internal/harness/scenario/library/*.json`)
ships ready-made scripts — `streaming-text` (Claude default),
`thinking-then-text`, `tool-call`, `tool-approval`, `file-edit`,
`session-death`, `stall-forever`, `step-gated`, `codex-basic` (Codex
default), `codex-approval`. `HarnessListScenarios` returns the current
list. With no rules set, every mock gets its provider's default — a
zero-config harness still streams a sensible reply.

### Claude framing contract

The mock's Claude adapter owns two protocol behaviours, exactly like
the real CLI, so scenario authors cannot break the app's turn
lifecycle:

1. **`system/init` is emitted per user turn** (after the user message
   arrives), never by scenarios — the app opens a logical turn only
   when init lands with a pending send registered.
2. **User envelopes echo back** with `isReplay: true`
   (`--replay-user-messages`), which resolves the pending send and
   stamps the item's provider id.

Scenarios must still frame their own assistant content: a
`stream_event message_start` carrying the assistant message id before
text/thinking blocks (that registration is what dedupes the coalesced
`assistant` envelope — without it the app renders the text twice), and
`message_stop` after. `library_test.go` enforces both invariants on
every shipped scenario, and `library_parsers_test.go` feeds every line
through the real Claude/Codex parsers.

### Control channel (`internal/harness/control`)

At spawn, the mock reads `AO_HARNESS_CONTROL` /
`AO_HARNESS_CONTROL_TOKEN` from its environment and registers over
loopback HTTP; the harness answers with its scenario (most specific
rule wins: cwd-scoped beats catch-all). Those variables are injected
into provider spawns only (`App.providerExtraEnv`), never exported
process-wide — terminals, git hooks, and other harness children don't
inherit the control credentials. The mock then long-polls for
commands and posts progress reports — `registered`, `turn_started`,
`step_started`, `step_completed`, `waiting_signal`,
`approval_pending`, `approval_decided`, `turn_interrupted`, `scenario_done`, `exiting` —
which the harness re-emits as `harness:mock` events
(`{mockId, protocol, cwd, scenario, report}`). Tests await these
instead of sleeping. A mock that reported `exiting` refuses further
`HarnessMockCommand`s (nothing would consume them). Without the env
vars (or if the harness dies), the mock falls back to scenario-file /
builtin behaviour and still works standalone.

## Record / replay bundles

The flicker-reproduction workflow:

1. `HarnessRecordStart(name, threadId)` — waits for the event-log
   drain, snapshots the DB (`VACUUM INTO`), and marks the byte offset
   in `<dataDir>/replay/<threadId>.jsonl`.
2. Drive the bug (live mock turn, UI interaction, ...).
3. `HarnessRecordStop()` — drains again and copies the event slice.
   Bundle: `<dataDir>/bundles/<name>/{db.snapshot, events.jsonl, meta.json}`.
4. `HarnessReplayBundle(name, {speed: 1})` — stops sessions, restores
   the snapshot, and re-emits the recorded events with original
   timing. Pause/step through the exact frames while watching the real
   UI (attach a trace, screenshot per `HarnessReplayStep`, ...).

The snapshot is taken at record *start*, so replay begins from the
same DB state the events originally streamed over — lazy payload loads
resolve like the original session.

**Scope: a bundle replays the wire + DB, not the filesystem.**
Workspace files and attachment bytes are not captured; rows in the
snapshot that reference them (project paths, attachment records) point
at whatever is on disk at replay time. Replay in the same harness
session before those files change — note `HarnessReset` removes
generated workspaces even though the bundle artifact itself survives.
Heavy timeline payloads (diffs, command output, thinking) live in
SQLite, so the core flicker-repro workflow is unaffected.

## e2e/ (Playwright)

`e2e/src/harness.ts` is the TS client: `launchHarness()` spawns the
binary (`$AO_HARNESS_BIN` or `<repo>/bin/agent-overflow`) on a fresh
temp data dir, parses the bootstrap line, and returns a `HarnessApp`
with `rpc(method, ...params)`, `waitForEvent(channel, predicate?)`
(scans history first — a fast backend can't win the race — and
consumes its match, so two identical waits observe two distinct
occurrences), `reset()`, and `close()`. `e2e/tests/fixtures.ts` shares one backend per
Playwright worker and resets between tests. `e2e/tests/harness.spec.ts`
covers boot, seeded rendering, a full live mock turn, frame-by-frame
`step-gated` advancement, and reset. `e2e/tests/workflows.spec.ts` covers a
two-phase chain, human gate approval, same-session question answering,
watchdog stall, and cancellation; the rest of the workflow surface is split by
concern across `workflows-rerun`, `workflows-tool`, `workflows-access`,
`workflows-fanout`, `workflows-call`, `workflows-wake`,
`workflows-automations`, `workflows-cli`, and
`workflows-overlay` (see `e2e/AGENTS.md` for what each one pins). Read
`harness.spec.ts` and `workflows.spec.ts` as references for new specs.

```
make e2e          # harness-build + playwright test
make harness      # interactive boot (Playwright MCP, manual browsers)
```

For interactive MCP sessions: run `make harness`, `page.goto` the
bootstrap `url`, and issue backend RPCs from any WS client with the
bootstrap `token` (a `node -e` one-liner importing
`e2e/src/harness.ts`'s frame shapes, or a tiny script). Evidence
accumulates under the paths `HarnessInfo` reports.

## Invariants

- Harness RPCs exist on the wire **only** under `--harness`; keep new
  harness methods on the `Harness` receiver (name-prefixed `Harness*`)
  so they inherit the registration gate and `LocalOnly` marking.
- The harness never fabricates app state through side doors: projects and
  threads run through `CreateProject`/`CreateThread`, workflow runs through
  `WorkflowStartRun`, resets through `DeleteProjectWorkflowRecords` plus the
  production delete cascade, and live
  turns through real sessions + triage. Direct store writes are reserved for
  completed ordinary-thread history—the rows production would already have
  persisted after a finished turn. If the harness needs a new capability, it
  wraps the production path.
- Claude scenarios never emit `system/init`; assistant text/thinking
  requires a prior `message_start` with the same id (enforced by
  `internal/harness/scenario/library_test.go`).
- `make e2e` must pass alongside the standard gates when touching the
  harness, the transport, the mock provider, or provider parsing.
