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

A third consumer reuses the same isolation with a different shell: the
**soak rig** (`--soak`, `make soak`) runs the real Windows launcher and
WebView2 window against mocked providers for hours.
See [soak-rig.md](soak-rig.md).

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
   opt out (e.g. replaying against real provider session files) —
   **read-only widening only**: the credential surface (account slots,
   canonical credential, orphan prune) stays pinned under
   `<dataRoot>/home` via `App.credentialHomeOverride` in both modes, so
   a keep-home run can read the real session trees but can never write
   to or prune the real `~/.claude` / `~/.codex` credential state.
   (Before that pin, a keep-home run with a reused data dir handed the
   boot prune a foreign keep-set aimed at the real home — the
   2026-07-29 incident class.)
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

The same payload is written to `<dataDir>/harness-instance.json` (0600)
once the backend is ready, plus a token-free discovery row at
`<user cache dir>/agent-overflow/harness-instances/<instance-id>.json`
(`internal/harness/instanceinfo`). That is how a tool attaches to an
instance whose stdout it never had; both files are removed on graceful
shutdown, and a row whose pid is gone is stale. The instance id is the
first 8 hex chars of the SHA-256 of the canonical data root.

### Windowed mode (`--window`)

`--harness --window` / `--soak --window` boot exactly the backend above
and then open the real Wails webview window on it instead of waiting
headless (`make harness-window`, `make soak-window`; GUI builds only —
the `nogui` WSL payload refuses the flag at boot). Versus the ordinary
desktop boot: no single-instance registration, no updater, a window
titled `Agent Overflow (harness · <instance-id>)`, and — on linux —
`XDG_{DATA,CACHE,CONFIG}_HOME` pointed at `<dataRoot>/home/xdg/*` so the
webview's cookies, localStorage, IndexedDB replica and shader caches
stay inside the data root. Under WSLg the window lands on the Windows
desktop. Full contract: `docs/specs/testing-harness.md` §1-§2.

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
| `HarnessListThreadRows()` | Every non-archived thread ROW, drafts included. `App.ListThreads` hides a row until it has an item or a content-carrying draft, so this is the only read that can prove a row was *not* created (or read back what a just-materialized one was bound to). |
| `HarnessSeed(spec)` | Declarative fixtures: projects (existing path or generated git repo), threads, pre-baked turn/item history, plus project-scoped workflow definitions/profile/items. Returns created ids. |
| `HarnessReset()` | Blank slate without a reboot: set the global workflow pause and cancel every live run through the production cancel path, stop sessions, settle in-flight turns, delete the workflow run records (`DeleteProjectWorkflowRecords` — production deletion drops these too under D25, but reset drops them first so the delete has no worktrees left to walk against a spec's fixtures), delete projects through the production cascade, remove workflow config/run dirs and generated seed workspaces, drop the cached session-import scan (its dedup is a projection of the rows just deleted, and nothing but a finished import run invalidates it), and drop harness-owned state (scenario rules, active replay, in-flight recording, mock registrations). The pause is then **cleared**, not restored, so a spec that deliberately left the engine paused cannot hold every later spec's runs in the same worker. Recorded bundles survive. Reload the page after. |
| `HarnessSetScenario(spec)` | Install/replace a mock scenario rule (library `name` or inline `scenario` JSON, optional `cwd` and `sessionRef` scopes — see "Scoping a scenario" below). Validated at set time. |
| `HarnessClearScenarios()` / `HarnessListScenarios()` | Drop rules / list library + active rules. |
| `HarnessListMocks()` | Registered mock processes in spawn order. |
| `HarnessClearThreadProviderCursor(threadId)` | Fault injection for an idle thread: clear AO's durable provider cursor without touching the mock process or transcript, so recovery must choose a fresh thread. Refuses an active turn or an already-empty cursor. |
| `HarnessMockCommand(mockId, cmd)` | Drive a live mock: `advance` (release a `waitSignal`/`stall` gate), `emit` (inject wire lines, `${VAR}`-substituted), `exit` (code). |
| `HarnessRecordStart(name, threadId)` / `HarnessRecordStop()` | Capture a replay bundle: DB snapshot at start + the event-log slice recorded until stop. Start requires the thread to be idle (no turn in flight) so the snapshot/event boundary is exact; a failed stop discards the recording and frees the name. |
| `HarnessReplayBundle(name, opts)` | Restore a bundle's DB snapshot and replay its events with original timing. Refused while another replay is active (checked before the destructive restore). |
| `HarnessListBundles()` | Enumerate saved bundles. |
| `HarnessReplayStart(path, opts)` | Replay a raw event-log NDJSON file (no DB restore). |
| `HarnessReplayPause/Resume/Step/Stop/Status()` | Playback control; `Step` releases exactly one event while paused. |
| `HarnessUIQuery(spec)` | Ask the attached frontend bridge a question and wait up to 10s for the answer. See "Frontend bridge and perf" below. |
| `HarnessUIQueryReply(id, result)` | The bridge's answer path. Called by the page, not by a test. A reply for an id with no waiter is dropped silently. |
| `HarnessPerfStart(spec)` / `HarnessPerfStop()` / `HarnessPerfStatus()` | Arm, stop and inspect a perf run. Stop returns one report folding the in-page meters and the Go-side samples. |

`ReplayOptions`: `speed` (multiplier), `maxGapMs` (cap long recorded
gaps), `startPaused`, `threadFilter`. Status transitions push on the
`harness:replay` channel, so a test can await
`{state:"done"}` instead of sleeping. Pause takes effect even mid-gap
(no event escapes after Pause returns); a step during a pause releases
the next event immediately, skipping the recorded gap; a second Step
while one is still pending errors instead of silently coalescing.

### Scoping a scenario

A rule may name a `cwd`, a `sessionRef`, both, or neither. Every selector
it declares must match; the narrowest matching rule wins
(`sessionRef` > `cwd` > catch-all, and a rule naming both beats either
alone). Ties keep the earliest rule installed. Setting a rule with the
same three selectors REPLACES it.

`sessionRef` is matched against the mock's registration `ResumeRef`, which
is argv-derived and read once at process start. That bounds it in two ways
a test has to plan around:

- It is **empty on a session's first spawn** — there is nothing to resume
  yet — so a session-scoped rule binds only RESUMED sessions. The sequence
  is: start the turn (which matches the cwd-scoped or catch-all rule), read
  `sessionRef` off the thread row, install the session-scoped rule, then
  `StopSession` + `StartSession` so the app respawns with `--resume`.
  `e2e/tests/harness.spec.ts` walks exactly that.
- It is **always empty for Codex**, whose app-server resumes a thread
  through the `thread/resume` JSON-RPC method on an already-registered
  process rather than through a launch flag — so `HarnessSetScenario`
  refuses `sessionRef` on a codex scenario at set time. Scope Codex
  mocks by `cwd`.

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
- `approval` — raise a permission request, branch `onAllow`/`onDeny`;
  Claude requests may name `toolUseId` and `agentId` (the subagent's
  task id), the correlation fields that scope a prompt to an agent's
  card — omit them for a main-agent prompt,
- `waitSignal` — block until a named `advance`,
- `stall` — hang until `advance` (or `durationMs`),
- `repeat` — run a nested step list `count` times, or forever when
  `count <= 0`,
- `exit` — die with a code mid-turn.

Lines substitute `${SESSION_ID}`, `${THREAD_ID}`, `${TURN}`,
`${TURN_ID}`, `${REQUEST_ID}`, `${CWD}`, and inside a `repeat` body
`${ITER}` (1-based iteration).

An unbounded `repeat` must contain a pacing step among its direct
children (`delayMs > 0`, `stall`, `waitSignal`, `approval`, or an `emit`
with `delayBetweenMs > 0`) — validation rejects a hot loop. Its body runs
**unreported**, so a forever-loop cannot flood the control channel or the
event bus with per-step reports; an interrupt still aborts it at the next
step boundary.

An inbound provider interrupt aborts the active scenario in the shared engine.
It releases a blocked `waitSignal`/indefinite `stall`, skips all remaining
steps at the next boundary, reports `turn_interrupted`, and then writes the
provider-native terminal sequence: Claude's successful control ack followed by
`result{subtype:error_during_execution, terminal_reason:aborted_streaming}`;
Codex's successful RPC response followed by
`turn/completed{turn.status:interrupted}`. An interrupt received with no active
or dispatching turn is a no-op and cannot poison the next turn.

The embedded library (`internal/harness/scenario/library/*.json`)
ships ready-made scripts. `HarnessListScenarios` returns the current
list. With no rules set, every mock gets its provider's default — a
zero-config harness still streams a sensible reply.

General-purpose scripts: `streaming-text` (Claude default),
`thinking-then-text`, `tool-call`, `tool-approval`, `file-edit`,
`session-death`, `stall-forever`, `step-gated`,
`soak-background-agents` (three async `local_agent` subagents streaming
forever; see [soak-rig.md](soak-rig.md)), `codex-basic` (Codex
default), `codex-approval`.

Bench scripts (`bench-burst-stream`, `bench-giant-turn`,
`bench-subagent-fanout`) are the load workloads, and their one shared
difference from the soak scripts is that they TERMINATE: each ends with a
`result` envelope so a bench can wait on turn completion instead of a
wall clock. See the bench section below.

Usage-limit scripts, one per provider: `usage-limit-claude` (a
`rate_limit_event` with `status: "rejected"` plus an `assistant` envelope
carrying the `rate_limit` error enum) and `usage-limit-codex` (an `error`
notification with `codexErrorInfo: "usageLimitExceeded"` and
`willRetry: false`, then `turn/completed` with `status: "failed"`). Both
drive the same downstream decision: `provider.FailureReasonUsageLimit`,
which parks a workflow run as `OutcomeProviderUsageLimited` rather than
failing it. Neither can arm `internal/usagebackoff`'s durable per-account
hold — that ledger is fed exclusively by an HTTP 429 from Anthropic's OAuth
usage endpoint (`claude.ProbeRateLimits`), which is out of band from both
stdio streams, so no scenario can reach it.

Codex wire-shape fixtures, each written against a specific behaviour
the typed stream alone cannot express — read the scenario's own
`description` before changing one, it names the regression it pins:

| Scenario | What it reproduces |
|---|---|
| `codex-collab-two-deliveries` | One child answers TWICE in a single parent turn, both envelopes stamped with the same passthrough `turn_id`. Keying delivery identity on that turn id collapses them onto one row. |
| `codex-collab-parallel-children` | Two children spawned in one parent turn, both answering into it. Each `FINAL_ANSWER` must land on its own completion row linked to the correct spawn. |
| `codex-collab-progress-message` | A `Message Type: MESSAGE` progress delivery in its ENCRYPTED form — plaintext header block plus an `encrypted_content` tail — followed by the real `FINAL_ANSWER`. A single-text-block parser drops the progress beat entirely. |
| `codex-collab-send-message-queueonly` | `send_message` (QueueOnly): the parent messages a running child, no child turn starts, and the typed wire is indistinguishable from `followup_task` — the raw function-call name is the only verb evidence. |
| `codex-collab-reload-after-unload` | A terminal child re-loaded by `followup_task`, which re-fires the SAME spawn activity. The repeated ownership registration must not mint a second spawn event. |
| `codex-steer-while-running` | A turn that parks mid-stream so sends reach it through `turn/steer`. The mock answers each steer with the `userMessage` echo carrying the `clientUserMessageId` back as `clientId` — the identity every codex pending send is registered by. |
| `codex-revert-paginated` | A thread on a current app-server that takes the paginated history AO asks for at >= 0.148, so edit-and-resend cuts it in place with `thread/revert`. |
| `codex-revert-legacy` | The same thread against an app-server reporting `codex_cli_rs/0.147.0`: legacy history for life, so edit-and-resend must fall back to `thread/fork` and repoint at a new provider thread id. |

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
3. **Each accepted user envelope appends a minimal provider transcript** under
   the harness's redirected `.claude` home. This is durable mock context, not
   app state: restarting the backend against the same data root exercises the
   production cold-resume preflight without reading a real provider home.

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
`user_input`, `step_started`, `step_completed`, `waiting_signal`,
`approval_pending`, `approval_decided`, `turn_interrupted`, `scenario_done`, `exiting` —
which the harness re-emits as `harness:mock` events
(`{mockId, protocol, cwd, scenario, report}`). Tests await these
instead of sleeping. `user_input` additionally carries `report.input` and
`report.sessionRef`: the exact text the adapter received and the Claude
session/Codex thread that received it. This is the assertion surface for both
prompt selection and session routing; neither can be recovered reliably from
the stored transcript or a Codex app-server process id. A mock that reported `exiting` refuses further
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

## Frontend bridge and perf

Screenshots are a bad instrument for an agent: they cost tokens, they
cannot be diffed, and they answer "what does this look like" when the
question is "what is on screen". The bridge answers the second question
in text.

`/bootstrap.json` carries `harness: true` in `--harness` / `--soak`
boots (`internal/transport/server.go`, set where `main.go` registers the
`Harness` receiver — one flag, two surfaces, no way to have one without
the other). The SPA reads it in `lib/transport/harnessMode.ts` and
`lib/stores/harnessBridge.ts` arms on it. An ordinary boot reads a
boolean and stops; the bridge modules are behind a dynamic import, so
they are their own rolldown chunk that a normal page never fetches. That
also means a production binary can serve a harness with no frontend
rebuild.

The protocol is request/reply over the same WebSocket:

1. `HarnessUIQuery(spec)` assigns an id, emits `harness:ui-query
   {id, spec}` and parks on a waiter keyed by that id.
2. The page answers with `HarnessUIQueryReply(id, result)`. A `result` of
   `{"error": "..."}` surfaces as the RPC's error, so a refusal reads as
   a failed call rather than a successful empty one.
3. First reply wins. Several attached frontends, a late reply after the
   10s timeout, and a duplicate are all the same case: the id has no
   waiter, and the reply is dropped silently.

`harness:ui-query` is `RetentionEphemeral` on purpose. It is a
DIRECTIVE, not a state frame: replaying a ring's worth of them to a
reconnecting client would re-run queries whose waiters are long gone.
`harness:perf` is the opposite and keeps the full ring, because a sample
is a point in a series and a watcher that reconnects mid-run wants what
it missed.

Query kinds, versioned `v: 1`:

| kind | Answers |
|---|---|
| `viewport` | The semantic snapshot: per pane, the mounted timeline rows with `{itemId, kind, role, status, streaming, badge, rect, textHead}`, scroll position, open overlays by accessible name, the active thread id, and `settled` (no DOM mutation for 300ms). |
| `element` | `{count, first:{rect, visible, clipped, text, aria}}` for a CSS selector. A malformed selector errors; one that matches nothing answers `count: 0`. |
| `globals` | Whitelisted read of a diagnostic global. A name outside the whitelist errors; a whitelisted name this build did not install answers `{unavailable: true}` — `__paneGeometry` and `uiTrace.recent` are genuinely absent in a harness build, because `make harness` builds with `UI_TRACE` unset. |
| `perf` | Meter control. Driven by `HarnessPerf*`, not by a test directly. |
| `reload` | Navigate the page. `HarnessReset`'s contract ends with "reload the page after" — the SPA is holding rows that no longer exist — and nothing outside a browser could do that. The answer is sent BEFORE the reload (a short deferred timeout, capped at 5s), because the socket the reply rides is about to drop; a caller treats a failed reload query as inconclusive and re-probes the bridge. |

The snapshot reads the DOM through attributes the components declare,
never through class names: `[data-pane-id]`, `[data-ui-surface="chat"]`,
`[data-testid="message-timeline-scroll"]`, `[data-row-index]`,
`[data-item-id|-kind|-role|-status]`, `[data-testid="indicator"]`,
`[role="dialog"]`, `[data-popover]`. Only the three `data-item-*`
attributes were added for it. Extend that list rather than
pattern-matching a class, and put the attribute on the element that owns
the concept.

"Visible rows" means the rows the virtualizer has MOUNTED, each flagged
`inViewport`. For a virtualized list that is the honest reading: the
mounted window is what the DOM contains, the intersecting subset is what
a human sees, and most timeline bugs live in the difference.

**Perf runs are backend-clocked.** `HarnessPerfStart` arms the in-page
meters through one ui-query, then samples on its own ticker (default
1000ms, floor 250ms): each tick reads Go heap/goroutines through
`runtime/metrics`, reads the backend's own RSS and its WebKit children's
from `/proc` (`internal/procrss`, linux only), pulls one frontend sample
with a `perf/collect` query, and emits both halves as one `harness:perf`
frame. Two reasons for one clock rather than a page-side push: a reader
correlating a frame stall against the Go heap gets one timeline instead
of two drifting ones, and a page that cannot answer becomes a labelled
`frontendError` on a frame that still arrives, rather than silence
indistinguishable from a healthy idle run.

The frontend SUMMARY is computed page-side, because percentiles need the
whole distribution: frame times fold into a fixed 1ms-bucket histogram
(constant memory over an hours-long soak) plus the exact max.
`HarnessPerfStop` collects that summary and returns it beside the backend
series as one report. **`HarnessReset` stops any active perf run** — it
holds a sampler goroutine AND a set of armed in-page meters, and the
caller reloads the page after a reset, so nothing else would ever disarm
them.

`internal/procrss` matches webview processes by name PREFIX because the
kernel truncates `/proc/<pid>/status`'s `Name:` at 15 characters
(`WebKitWebProce`, never `WebKitWebProcess`). Off linux `Sample` returns
`ErrUnsupported` and the RSS series is simply absent. `SampleAll` is the
sibling that takes EVERY descendant whatever it is named, which is what a
whole-tree figure needs: an empty prefix list cannot express that, since
prefix matching skips a process it does not recognise by design.

## Driving an instance from a shell (`bin/ao-harness`)

`cmd/ao-harness` is the same surface for a human or an agent at a
terminal: `up` / `down` / `list` / `info`, `seed`, `rpc <Method> [json]`,
`threads` / `items` / `send`, `scenario`, `mock`, `events tail|await|count`,
`record` / `bundles` / `replay`, `logs`, a read-only `db`, and the
bridge-backed `ui` / `perf` / `bench` / `health`. `make
harness-build` builds it alongside `bin/agent-overflow`, and it finds the
backend binary as its own sibling, so a fresh checkout needs no
configuration.

It resolves which instance to talk to from the registry above: an
explicit `--instance <id|dataRoot>`, else the single live row, else this
worktree's default data root. Two live instances is an error listing the
candidates rather than a guess.

The reusable half is `internal/harnessclient`: bootstrap discovery
(instance file or a spawned backend's stdout line), the WS client with
the same consume-on-match `WaitForEvent` semantics as
`e2e/src/harness.ts`, detached launch, and file tailing. A Go test that
needs a real instance imports that rather than re-implementing the
frames. Details and the registry prune rule:
[cmd/ao-harness/AGENTS.md](../../cmd/ao-harness/AGENTS.md).

### Bench workloads

`ao-harness bench <workload>` is a soak that ENDS: it seeds its own
fixture, arms the perf meters, drives a scripted load, and writes
`<dataDir>/bench/<workload>-<timestamp>.json`. Four workloads:
`burst-stream` (chunked text-delta flood), `giant-turn` (225 items in one
turn), `subagent-fanout` (three bounded async subagents), and
`many-threads` (30 seeded threads, then a switch storm). The first three
are `bench-*` entries in the scenario library and finish on
`provider:turn_completed` for their thread — the mock's own
`scenario_done` fires when the mock stopped WRITING, upstream of parse,
triage, persist and render, so it would time a shorter pipeline than the
one under test.

`many-threads` has no scenario. It drives each switch by emitting
`notification:activated`, which the SPA routes through
`parseNotificationTarget` and `applyNotificationActivated` into
`openThreadInPane` — the same function a sidebar click reaches. That
keeps the workload honest (30 real timeline unmounts and 30 real
bounded-window loads out of SQLite for one tiny RPC each) while being
explicit about what it does NOT cover: the sidebar row's own hit-testing
and hover.

A report doubles as a baseline. `--baseline` takes either a previous
report (its `aggregate` p50 becomes the reference under a default 25%
budget) or a hand-written `metrics` budget, and drift exits 3. There is
no default baseline, so a bench never becomes a gate by accident.

### Health rollup

`ao-harness health` is the generalized `make soak-check`: process
liveness and uptime, new `frontend-errors.jsonl` lines, ui-trace oracle
triggers (`timeline.margin.diverge`, `timeline.reasoning.tailJump` — not
the continuous `timeline.row.resize` tracker), new backend stderr, the
process tree's RSS via `procrss.SampleAll`, database size, mock liveness,
replay state, and any armed perf run. One line per concern with an
ok/warn/red marker; red exits 3, warn exits 0.

Every file concern is since-last-check through
`<dataDir>/health-cursor.json`, which stores each file's size beside its
offset so a rotation (uitrace's size cap, `up`'s stderr truncation) is
detected rather than silently skipping or over-reading. `--watch` appends
timestamped lines with no clear-screen, so an hours-long watch is
greppable evidence.

## e2e/ (Playwright)

`e2e/src/harness.ts` is the TS client: `launchHarness()` spawns the
binary (`$AO_HARNESS_BIN` or `<repo>/bin/agent-overflow`) on a fresh
temp data dir, parses the bootstrap line, and returns a `HarnessApp`
with `rpc(method, ...params)`, `waitForEvent(channel, predicate?)`
(scans history first — a fast backend can't win the race — and
consumes its match, so two identical waits observe two distinct
occurrences), `reset()`, and `close()`. `e2e/tests/fixtures.ts` shares one backend per
Playwright worker and resets between tests. The returned client also exposes
`stop()` (graceful, preserving the data root), `crash()` (SIGKILL, also
preserving it), and `close()` (stop plus owned-root cleanup). Restart specs can
therefore launch a second backend over the first one's durable state and prove
cold provider recovery instead of simulating it inside one process.
`e2e/tests/harness.spec.ts`
covers boot, seeded rendering, a full live mock turn, frame-by-frame
`step-gated` advancement, and reset. `e2e/tests/workflows.spec.ts` covers a
two-phase chain, human gate approval, same-session question answering,
watchdog stall, and cancellation; the rest of the workflow surface is split by
concern across `workflows-rerun`, `workflows-tool`, `workflows-access`,
`workflows-fanout`, `workflows-call`, `workflows-wake`,
`workflows-automations`, `workflows-cli`, and
`workflows-overlay`, and `workflows-resume` (same-context, missing-context, and
cold-backend-restart behavior for both providers; see `e2e/AGENTS.md` for what
each one pins).
`e2e/tests/session-import.spec.ts` drives session import end to end against
hand-written provider homes written into the harness's redirected `HOME`
(`session-import-fixtures.ts`) — the seeding pattern for anything that reads
`~/.claude` or `~/.codex`. Read
`harness.spec.ts` and `workflows.spec.ts` as references for new specs.
`e2e/tests/harness-bridge.spec.ts` covers the frontend bridge: the viewport
snapshot's row ids matching what the backend seeded, element and globals
queries, a perf run streaming frames and stopping with a two-sided report,
reset disarming a run, and the no-page timeout plus its dropped late reply.

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
