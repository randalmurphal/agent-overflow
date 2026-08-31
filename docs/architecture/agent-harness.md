# Agent Test Harness

The harness boots the **real backend and the real SPA** headless, on an
isolated data directory, with both provider binaries pointed at
`ao-mockprovider`. An agent (or a Playwright script) can therefore
exercise any UI flow, reproduce streaming/rendering bugs
frame-accurately, and capture evidence, without touching real app data
or a real Claude/Codex process.

Everything below is reachable two ways, by design:

- **Checked-in Playwright specs** in `e2e/` (`make e2e`).
- **Interactively**: boot `make harness`, open the printed URL in
  Playwright MCP (or any browser), and drive the backend over the same
  WebSocket RPCs the specs use.

On Windows the harness has its own shell: **`make harness-wsl`** runs
the real launcher and WebView2 window (`--profile harness`) against this
same isolated backend. The backend rides the `--soak` wire flag, the
launcher-owned historical name for "isolated launcher-shell instance".
**`make soak`** is that shell plus the soak preset (`--autopilot`:
seeded threads and a never-ending streaming turn, left running for
hours). **`make perf-wsl`** is a third shell for destructive renderer A/B
runs. It owns `~/.agent-overflow-perf`, its own WebView2 profile, and CDP
9226, so reset/reload/interrupt cannot land on the harness or soak by
profile collision. See [soak-rig.md](soak-rig.md).

## Boot

```
make harness                    # build + run at a per-checkout /tmp root (reused)
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
   wholesale, so a planted link would aim that at whatever it points to).
   On unix the same three are refused if they are not owned by the
   running uid or are group/world-writable, and a root the harness
   CREATES is stamped 0700 past the umask: `make harness` derives a
   predictable /tmp path from the checkout, and a stranger who creates
   it first at 0777 hands the boot a `$HOME` on a tree they still
   control. A writable `$HOME` is a writable `.gitconfig`, which is
   their `core.pager` running as you. The symlink check catches the link
   they might plant; this catches the directory. Windows is a no-op
   (POSIX mode bits do not map onto its ACLs).
   On macOS the root-owned `/var` and `/tmp` aliases normalize to their
   `/private/...` identity before managed-run, capture, comparison, and
   retention paths are frozen. Symlinks below those OS aliases remain visible
   and are still refused.
   The DB, settings, replay logs, and attachments live under
   `<dataRoot>/agent-overflow/`.
2. **HOME redirect.** `$HOME` (and `%USERPROFILE%`) point at
   `<dataRoot>/home`, so `~/.claude` scans, `~/.codex` tails, and git's
   global config are harness-local and seedable. A minimal `.gitconfig`
   is written so fixture commits work. Set `AO_HARNESS_KEEP_HOME=1` to
   opt out, which is **child-process widening only**. What the flag changes is
   what SPAWNED processes see: `$HOME` stays real, so a provider CLI (or
   `ao-mockprovider`, or git) launched by the harness resolves the real
   trees. What it does NOT change is where the BACKEND resolves provider
   state: every `~/.claude` / `~/.codex` path the backend builds goes
   through `App.providerHome()`, which returns
   `App.credentialHomeOverride` (always `<dataRoot>/home`) in both
   modes. That covers the credential surface (account slots, canonical
   credential, orphan prune), the session-import scan, the MCP config
   writers (`~/.claude.json`, `~/.codex/config.toml`), the Claude
   memory-directory mkdir, the authenticated rate-limit probe's
   credential read, and every `sessionfork` locate/fork/relocate, so a
   keep-home run can never read, write, or prune the developer's real
   provider state from inside the backend.
   `TestAppLayerResolvesProviderHomesThroughOneSeam`
   (`app_provider_home_test.go`) is what keeps that true: it fails on a
   bare `os.UserHomeDir()` in `app_*.go`, `internal/provider/`,
   `internal/claudeconfig` or `internal/codexconfig` outside a
   reasoned allowlist. Before the seam, only the credential surface and
   the import scan honoured the pin and eight other sites read `$HOME`
   directly. (And before the pin existed at all, a keep-home run with a
   reused data dir handed the boot prune a foreign keep-set aimed at the
   real home, the 2026-07-29 incident class.)
3. **Single-instance lock.** An OS-held advisory lock on
   `<dataDir>/harness.lock` (`flock` on unix, `LockFileEx` on Windows),
   taken before ANY write into the root and held for the process's
   lifetime. Two backends on one data root open the same SQLite file and
   the second's `publishInstance` overwrites the first's registry row, so
   every tool then points at the wrong backend. The refusal names the
   holder's pid and boot mode. It lives in the backend rather than in a
   launcher because every entry point has to be covered: `make harness`
   and the wails3 dev harness path boot directly, and `ao-harness up`'s
   registry pre-check is skippable and TOCTOU. Because the kernel drops
   the lock when the process dies however it dies, a crashed boot leaves
   the next one free with no stale-pid reaping.
4. **Mock provider resolution.** `--mock-provider`, else the
   `ao-mockprovider` binary next to the running executable (where
   `make harness-build` puts it). Validated eagerly.
5. **Settings seed.** `claudeBinaryPath` and `codexBinaryPath` both
   point at the mock; the NDJSON event log
   (`observabilityEventLogEnabled`) is switched on so every session is
   recordable for wire-level replay. Measurement caveat: that writer is
   OFF by default in production, so every perf/bench number a harness
   produces includes per-event NDJSON serialization a user's run
   doesn't pay. The mock path is additionally
   pinned at spawn-resolution time (`App.providerBinaryOverride`), so
   even an `UpdateSettings` call after boot cannot repoint a spawn at a
   real `claude`/`codex` binary.

Then the mock-provider control server starts (before `App.Start`, so
every provider spawn inherits its env), the transport comes up, and
stdout carries exactly one parseable line:

```
__AO_HARNESS__: {"url":"http://127.0.0.1:PORT/?t=TICKET&cid=...","port":PORT,"token":"...",
                 "dataRoot":"...","dataDir":"...","homeDir":"...","mockProvider":"...",
                 "pid":123,"version":"...","clientId":"...",
                 "startupError":"only on failed boot"}
```

`url` goes straight into a browser / `page.goto()` (it already carries
`&cid=`), and `token` opens the RPC WebSocket. All subsequent logging goes
to stderr.

**`url` opens ONE browser session.** The `?t=` on it is a one-time page
ticket; the load that spends it receives an HttpOnly cookie that carries
every later request from that browser, including the WebSocket upgrade. A
caller that navigates again — a reload, or a second cookie-less browser
context, which is what every Playwright test gets — asks the running
instance for a fresh URL instead of reusing this string:

```
GET http://127.0.0.1:PORT/pageurl      Authorization: Bearer <token>
```

`ao-harness open`/`info`/`attach`/`up` and the e2e rig's
`HarnessApp.open()` already do this; `harnessclient.Bootstrap.PageURL` is
the Go helper. Reusing a spent ticket is not a wedge — the page simply
gets the transport's ordinary refusal — but it is a blank window.

`clientId` is the instance's durable UI-state identity, resolved under its
OWN `--data-dir` and therefore never the developer's. It is reported
separately as well as threaded onto `url` because a caller that builds its
own page URL (the Windows launcher opening a WebView2 window on a `--soak`
backend, a Playwright run pointing at the instance) must attach the SAME
id, or the frontend's per-client `ui_state` bucket changes identity on
every port churn and every persisted UI preference reads back as unset.

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
headless (`make harness-window`, `make soak-window`; GUI builds only,
since the `nogui` WSL payload refuses the flag at boot). Versus the
ordinary desktop boot: no single-instance registration, no updater, a
window titled `Agent Overflow (harness · <instance-id>)`, and, on linux,
`XDG_{DATA,CACHE,CONFIG}_HOME` pointed at `<dataRoot>/home/xdg/*` so the
webview's cookies, localStorage, IndexedDB replica and shader caches
stay inside the data root. Under WSLg the window lands on the Windows
desktop. Full contract: `docs/specs/testing-harness.md` §1-§2.

### What an isolated boot does NOT stub

The premise is that everything except the provider processes is
production code, so three things that look like they could be stubbed
are not:

- **OS notifications.** `newIsolatedProviderApp` installs the real
  `transportNotificationSender`, the same one `runHeadless` does.
  `HarnessNotify` therefore SUCCEEDS and the send is observable on
  `notification:send`; presentation is the subscriber's job (the Windows
  launcher under `--soak`, nobody under a headless `--harness`, where the
  sender logs one line and returns nil). It used to install a refusal
  stub, which meant the e2e spec covering the notification pipe asserted
  the stub's error string and never executed the emission at all.
- **pprof.** Still opt-in via `AGENT_OVERFLOW_PPROF`, but a BARE enable
  (`1`/`true`) binds an ephemeral loopback port on an isolated boot
  instead of `pprofserve`'s fixed `127.0.0.1:6363`. Isolated boots are
  the one shape deliberately run N-at-a-time (a soak beside your own app,
  a harness per checkout), and the variable is usually INHERITED from a
  `make dev` shell rather than chosen, so the second instance's listener
  used to fail to bind and log an error for a port nobody asked it to
  claim. An explicit `host:port` is honoured verbatim.
- **Frontend assets.** `--harness` / `--soak` honour
  `FRONTEND_DEVSERVER_URL` even in a production-stamped binary (an
  isolated boot is already an explicit operator act). That variable is
  EXPORTED by `make dev`, so a harness launched from that terminal
  silently serves an unminified, HMR-instrumented bundle, and every
  number a perf run or a soak then produces describes a build nobody
  ships. The boot logs a loud `WARNING:` line naming the dev server when
  this is actually happening.

## RPC surface

One WebSocket carries everything. The receiver implementation lives in
`internal/harnessrpc`; isolated boot registers it explicitly as
`main.Harness` with receiver-level `LocalOnly` policy:
`ws://127.0.0.1:<port>/ws?token=<token>`, frames per
`internal/transport/AGENTS.md`. Call methods **by name**
(`{type:"rpc", id, method:"HarnessInfo", params:[...]}`); both the
`Harness` receiver and every bound `App` method (`CreateThread`,
`SendMessage`, ...) share the wire. The whole Harness receiver is
`LocalOnly`: a LAN-bound harness refuses it for non-loopback peers.

The table below documents the stable harness methods, not the complete wire
surface. `HarnessListMethods` is generated by the running backend and includes
every App plus Harness method. Use `ao-harness rpc --list` for that full list.

| Method | Purpose |
|---|---|
| `HarnessInfo()` | Identity + evidence paths (DB, event-log dir, UI trace, frontend error log), the instance's `clientId`, `soakAutopilot` (`"off"` / `"arming"` / `"armed"` / `"failed: <reason>"`, where the autopilot arms on a goroutine that starts *after* the instance is published as a soak, so without the latch a soak that never armed looks identical to a working one), and `assetsFreshness`: the boot's embedded-bundle verdict (`match` / `stale` / `unknown` / `dev-server`). The binary embeds `frontend/dist` at BUILD time, so a `vite build` followed by a not-rebuilt harness binary silently serves the previous bundle; boot compares the embed against the adjacent on-disk dist, warns loudly on `stale`, and `ao-harness health` flags it. |
| `HarnessListMethods()` | Every method name reachable on the wire, sorted: the App's bindings and the Harness surface in one array of bare wire names. Lets a caller check an instance has the RPC it is about to call instead of discovering a version mismatch as an opaque `method_not_found`. |
| `HarnessEmit(channel, payload)` | Publish a raw event on the bus: the escape hatch for injecting one-off frames at the frontend. |
| `HarnessListThreadRows()` | Every non-archived thread ROW, drafts included. `App.ListThreads` hides a row until it has an item or a content-carrying draft, so this is the only read that can prove a row was *not* created (or read back what a just-materialized one was bound to). |
| `HarnessSeed(spec)` | **Strictly decoded** (unknown fields refused, positions reported, since a mistyped `treads:` used to seed nothing and return success). Declarative fixtures: projects (existing path or generated git repo), threads, pre-baked turn/item history, project-scoped workflow definitions/profile/items, and `providerHome` files, which are slash-separated relative paths written under the harness-owned provider home (`<dataRoot>/home`, never the real one, even under `AO_HARNESS_KEEP_HOME`), for `.claude.json` MCP config, skills, settings, or a `.claude/projects/...` transcript paired with a thread's `sessionRef`. Returns created ids and the home paths written. |
| `HarnessReset()` | Blank slate without a reboot: set the global workflow pause and cancel every live run through the production cancel path, stop sessions, settle in-flight turns, delete the workflow run records (`DeleteProjectWorkflowRecords`: production deletion drops these too under D25, but reset drops them first so the delete has no worktrees left to walk against a spec's fixtures), delete projects through the production cascade, remove workflow config/run dirs and generated seed workspaces, remove the provider trees under the harness-owned home (`.claude`, `.claude.json`, `.codex`: seeded `providerHome` fixtures plus the transcripts mocks wrote, which would otherwise leak into the next test's import scan), drop the cached session-import scan (its dedup is a projection of the rows just deleted, and nothing but a finished import run invalidates it), clear persisted UI view state (`ui_state` rows name entity ids: the workflows overlay stack persists work-item ids, and a surviving row makes the next test's fresh page restore a selection onto deleted rows), and drop harness-owned state (scenario rules, active replay, in-flight recording, mock registrations). The pause is then **cleared**, not restored, so a spec that deliberately left the engine paused cannot hold every later spec's runs in the same worker. Recorded bundles survive. Reload the page after. |
| `HarnessSetScenario(spec)` | Install/replace a mock scenario rule (library `name` or inline `scenario` JSON, optional `cwd` and `sessionRef` scopes, described in "Scoping a scenario" below). Validated at set time. |
| `HarnessClearScenarios()` / `HarnessListScenarios()` | Drop rules / list library + active rules. |
| `HarnessListMocks()` | Registered mock processes in spawn order, dead ones pruned by a PID probe (30s grace so a just-exited mock's terminal reports still land). Each row carries `openGate` (the `waitSignal` gate the mock is currently blocked on, empty when none) and `pendingAdvances` (advances buffered for gates that have not opened yet), the state a stuck `advance` await is diagnosed from. |
| `HarnessClearThreadProviderCursor(threadId)` | Fault injection for an idle thread: clear AO's durable provider cursor without touching the mock process or transcript, so recovery must choose a fresh thread. Refuses an active turn or an already-empty cursor. |
| `HarnessMockCommand(mockId, cmd)` | Drive a live mock: `advance` (release a `waitSignal`/`stall` gate), `emit` (inject wire lines, `${VAR}`-substituted), `exit` (code). |
| `HarnessRecordStart(name, threadId)` / `HarnessRecordStop()` | Capture a replay bundle: DB snapshot at start + the event-log slice recorded until stop. Start requires the thread to be idle (no turn in flight) so the snapshot/event boundary is exact; a failed stop discards the recording and frees the name. |
| `HarnessReplayBundle(name, opts)` | Restore a bundle's DB snapshot and replay its events with original timing. Refused while another replay is active (checked before the destructive restore). |
| `HarnessListBundles()` | Enumerate saved bundles. |
| `HarnessReplayStart(path, opts)` | Replay a raw event-log NDJSON file (no DB restore). |
| `HarnessReplayPause/Resume/Step/Stop/Status()` | Playback control; `Step` releases exactly one event while paused. |
| `HarnessUIQuery(spec)` | Ask the attached frontend bridge a question and wait up to 10s for the answer, but only ~250ms when NO client is connected to the event bus at all, since there is nothing to wait for and a headless script's twenty probes should not cost four minutes. A connected client with no bridge in its bundle is indistinguishable from a slow one and still waits the full timeout. See "Frontend bridge and perf" below. |
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

- It is **empty on a session's first spawn** (there is nothing to resume
  yet), so a session-scoped rule binds only RESUMED sessions. The sequence
  is: start the turn (which matches the cwd-scoped or catch-all rule), read
  `sessionRef` off the thread row, install the session-scoped rule, then
  `StopSession` + `StartSession` so the app respawns with `--resume`.
  `e2e/tests/harness.spec.ts` walks exactly that.
- It is **always empty for Codex**, whose app-server resumes a thread
  through the `thread/resume` JSON-RPC method on an already-registered
  process rather than through a launch flag, so `HarnessSetScenario`
  refuses `sessionRef` on a codex scenario at set time. Scope Codex
  mocks by `cwd`.

### Seeding vs. live turns

`HarnessSeed` writes ordinary thread *completed history*, the rows the app
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
the sidebar. A seeded thread with no turns (or a live thread before
its first message lands) is invisible in the UI until its first
message exists. Tests that open the UI first must seed at least one
turn, or send the message before navigating.

## The mock provider (`cmd/ao-mockprovider`)

One binary impersonates both providers, sniffing argv: Claude's
`--input-format stream-json` NDJSON session (plus the `--max-turns 0`
probe), and Codex's `app-server` JSON-RPC 2.0 (plus `--version`
satisfying both version gates). Scenarios script what it streams.
Protocol strictness is deliberate on both adapters: Claude
`control_request`s are acked with the real CLI's wire keys per subtype
(pinned against the 2.1.237 binary), and a Codex request the mock does
not implement answers JSON-RPC `-32601` rather than silence, so
`codex.IsMethodUnsupported` fallbacks are reachable under the harness.

**Known limitation: auth is always healthy.** The mock never models
login state: both adapters present a logged-in, non-expired account, and
no scenario step can drive logout, token expiry, or an auth error shape.
Auth/credential UX is testable only against real CLIs
(`make provider-smoke`).

### Scenarios (`internal/harness/scenario`)

A scenario is a JSON document: `onStart` steps, `turns` (each a step
list consumed per user message), and `afterTurns`
(`repeatLast` | `silent` | `exit`). Steps:

- `emit`: write wire lines (`delayBetweenMs`, or `chunkBytes` +
  `chunkIntervalMs` for partial-line stress, or `coalesce` for the
  opposite: every line in ONE stdout write, the multi-envelope read a
  fast provider produces),
- `fixture`: stream a recorded NDJSON fixture file (`fromLine`/`toLine`),
- `delayMs`, `writeFile` (real workspace mutations so diffs/git are real),
- `approval`: raise a permission request, branch `onAllow`/`onDeny`;
  Claude requests may name `toolUseId` and `agentId` (the subagent's
  task id), the correlation fields that scope a prompt to an agent's
  card (omit them for a main-agent prompt),
- `waitSignal`: block until a named `advance`,
- `stall`: hang until `advance` (or `durationMs`),
- `repeat`: run a nested step list `count` times, or forever when
  `count <= 0`,
- `exit`: die with a code mid-turn.

Lines substitute `${SESSION_ID}`, `${THREAD_ID}`, `${TURN}`,
`${TURN_ID}`, `${REQUEST_ID}`, `${CWD}`, and inside a `repeat` body
`${ITER}` (1-based iteration).

Two scenario-level knobs sit beside the step lists. `startupDelayMs`
delays the first frame that proves the provider is up (Claude's first
`system/init`, Codex's `initialize` response), once per process and
capped at 30s, so the app's cold-start window is drivable. `providerVersion`
downgrades the version the mock claims (Codex `initialize` userAgent,
Claude `system/init.claude_code_version`), which is what every
per-method version gate reads; the default is above every gate, so this
is the only way a spec exercises a gate's fails-closed branch. It does
not reach `--version`, the account probe, or one-shot text generation.
Those invocations answer and exit before a scenario loads.

An unbounded `repeat` must contain a pacing step among its direct
children (`delayMs > 0`, `stall`, `waitSignal`, `approval`, or an `emit`
with `delayBetweenMs > 0`). Validation rejects a hot loop. Its body runs
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
list. With no rules set, every mock gets its provider's default, so a
zero-config harness still streams a sensible reply.

General-purpose scripts: `streaming-text` (Claude default),
`thinking-then-text`, `tool-call`, `tool-approval`, `file-edit`,
`file-edit-diff`, `session-death`, `stall-forever`, `step-gated`,
`soak-background-agents` (three async `local_agent` subagents streaming
forever; see [soak-rig.md](soak-rig.md)), `codex-basic` (Codex
default), `codex-approval`.

The two file-edit scripts are not interchangeable. `file-edit` writes a
file and answers with a plain-string `tool_result`, so triage extracts
no diff and the card renders a disabled header — the shape most tools
actually produce. `file-edit-diff` answers with the real Edit
`tool_use_result` (`filePath` + a two-hunk `structuredPatch`), which is
the only library script that makes triage persist an inline diff
PAYLOAD, so it is the one that exercises diff rows, expand-to-load, and
the `collapseDiffPreviews` default. Its claim is pinned by
`internal/triage/scenario_file_edit_diff_test.go`, which drives the
scenario's own lines through the real parser and Router.

Bench scripts (`bench-burst-stream`, `bench-active-stream`, `bench-giant-turn`,
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
hold. That ledger is fed exclusively by an HTTP 429 from Anthropic's OAuth
usage endpoint (`claude.ProbeRateLimits`), which is out of band from both
stdio streams, so no scenario can reach it.

Codex wire-shape fixtures, each written against a specific behaviour
the typed stream alone cannot express. Read the scenario's own
`description` before changing one, it names the regression it pins:

| Scenario | What it reproduces |
|---|---|
| `codex-collab-two-deliveries` | One child answers TWICE in a single parent turn, both envelopes stamped with the same passthrough `turn_id`. Keying delivery identity on that turn id collapses them onto one row. |
| `codex-collab-parallel-children` | Two children spawned in one parent turn, both answering into it. Each `FINAL_ANSWER` must land on its own completion row linked to the correct spawn. |
| `codex-collab-progress-message` | A `Message Type: MESSAGE` progress delivery in its ENCRYPTED form (plaintext header block plus an `encrypted_content` tail) followed by the real `FINAL_ANSWER`. A single-text-block parser drops the progress beat entirely. |
| `codex-collab-send-message-queueonly` | `send_message` (QueueOnly): the parent messages a running child, no child turn starts, and the typed wire is indistinguishable from `followup_task`. The raw function-call name is the only verb evidence. |
| `codex-collab-reload-after-unload` | A terminal child re-loaded by `followup_task`, which re-fires the SAME spawn activity. The repeated ownership registration must not mint a second spawn event. |
| `codex-steer-while-running` | A turn that parks mid-stream so sends reach it through `turn/steer`. The mock answers each steer with the `userMessage` echo carrying the `clientUserMessageId` back as `clientId`, the identity every codex pending send is registered by. |
| `codex-revert-paginated` | A thread on a current app-server that takes the paginated history AO asks for at >= 0.148, so edit-and-resend cuts it in place with `thread/revert`. |
| `codex-revert-legacy` | The same thread against an app-server reporting `codex_cli_rs/0.147.0`: legacy history for life, so edit-and-resend must fall back to `thread/fork` and repoint at a new provider thread id. |

### Claude framing contract

The mock's Claude adapter owns two protocol behaviours, exactly like
the real CLI, so scenario authors cannot break the app's turn
lifecycle:

1. **`system/init` is emitted per user turn** (after the user message
   arrives), never by scenarios. The app opens a logical turn only
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
`assistant` envelope, and without it the app renders the text twice), and
`message_stop` after. `library_test.go` enforces both invariants on
every shipped scenario, and `library_parsers_test.go` feeds every line
through the real Claude/Codex parsers.

### Control channel (`internal/harness/control`)

At spawn, the mock reads `AO_HARNESS_CONTROL` /
`AO_HARNESS_CONTROL_TOKEN` from its environment and registers over
loopback HTTP; the harness answers with its scenario (most specific
rule wins: cwd-scoped beats catch-all). Those variables are injected
into provider spawns only (`App.providerExtraEnv`), never exported
process-wide, so terminals, git hooks, and other harness children don't
inherit the control credentials. The mock then long-polls for
commands and posts progress reports (`registered`, `turn_started`,
`user_input`, `step_started`, `step_completed`, `waiting_signal`,
`advance_released`, `advance_buffered`, `approval_pending`,
`approval_decided`, `turn_interrupted`, `history_cut`, `fixture_error`,
`session_config`, `scenario_done`, `exiting`),
which the harness re-emits as `harness:mock` events
(`{mockId, protocol, cwd, scenario, report}`). Tests await these
instead of sleeping.

The advance pair makes gate handshakes assertable instead of inferred:
`advance_released` (gate named in `report.gate`) fires when an advance
actually opens a gate, either immediately or later when a buffered
advance is consumed by the gate opening. `advance_buffered` fires when an advance
matched nothing and was parked (`report.openGate` names the gate that
WAS open and didn't match, empty when none); its `detail` is empty for a
real buffering and marks a DISCARD when the per-turn buffer was full.
`fixture_error` reports a step that could not do its job (unreadable
fixture file, rejected `writeFile`). Without it such a turn is a silent
provider with evidence only in the mock's stderr. `session_config`
posts once per mock with the permission/sandbox configuration the app
actually launched it with. `scenario_done` is per TURN: every turn that
runs its step list to completion reports it for its own 1-based turn
number, repeated (`repeatLast`) turns included. Await it with the turn
you drove, not just its first occurrence. `user_input` additionally carries `report.input` and
`report.sessionRef`: the exact text the adapter received and the Claude
session/Codex thread that received it. This is the assertion surface for both
prompt selection and session routing; neither can be recovered reliably from
the stored transcript or a Codex app-server process id. A mock that reported `exiting` refuses further
`HarnessMockCommand`s (nothing would consume them). Without the env
vars (or if the harness dies), the mock falls back to scenario-file /
builtin behaviour and still works standalone.

## Record / replay bundles

The flicker-reproduction workflow:

1. `HarnessRecordStart(name, threadId)` waits for the event-log
   drain, snapshots the DB (`VACUUM INTO`), and marks the byte offset
   in `<dataDir>/replay/<threadId>.jsonl`.
2. Drive the bug (live mock turn, UI interaction, ...).
3. `HarnessRecordStop()` drains again and copies the event slice.
   Bundle: `<dataDir>/bundles/<name>/{db.snapshot, events.jsonl, meta.json}`.
4. `HarnessReplayBundle(name, {speed: 1})` stops sessions, restores
   the snapshot, and re-emits the recorded events with original
   timing. Pause/step through the exact frames while watching the real
   UI (attach a trace or query semantic geometry per `HarnessReplayStep`, ...).

The snapshot is taken at record *start*, so replay begins from the
same DB state the events originally streamed over, and lazy payload
loads resolve like the original session.

**Scope: a bundle replays the wire + DB, not the filesystem.**
Workspace files and attachment bytes are not captured; rows in the
snapshot that reference them (project paths, attachment records) point
at whatever is on disk at replay time. Replay in the same harness
session before those files change. Note that `HarnessReset` removes
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
`Harness` receiver: one flag, two surfaces, no way to have one without
the other). The SPA reads it in `lib/transport/harnessMode.ts` and
`lib/stores/harnessBridge.ts` arms on it, LAZILY and in two steps: the flag
arms only the cheap event subscription, the first `harness:ui-query`
triggers the dynamic import, and the document-wide mutation observer is
installed only by a query that reports settledness (`viewport`). It is
then DISARMED again as soon as nothing needs it: a few seconds' linger
past the last such query so a settle poll keeps one continuous history,
and immediately at BOTH ends of a perf run (`HarnessPerfStart` clears
whatever the bench's own thread-open polling left armed;
`HarnessPerfStop` does not wait out the linger); the next `viewport`
re-arms it transparently, mid-run included. A soak that is never queried therefore streams for hours
with no observer allocating a MutationRecord per delta, and a perf run or
a bench workload measures a renderer carrying no observer either unless
the caller explicitly asked the page whether it had settled. The rig
must not perturb the renderer it exists to watch, least of all while it
is taking the numbers. The consequence is honest, not free: a freshly
armed clock has no history, so that query's `settled` reads false until
the observer has a settle window of it. A view-only remote session never
arms at all (`harness:ui-query` is loopback-only, so it could never
receive a query). An ordinary boot
reads a boolean and stops; the bridge modules are their own rolldown
chunk that a normal page never fetches (`architecture.test.ts` bans any
static import of `lib/harness/` outside the store's one dynamic door).
That also means a production binary can serve a harness with no frontend
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
| `globals` | Whitelisted read of a diagnostic global. A name outside the whitelist errors; a whitelisted name this build did not install answers `{unavailable: true}`. `__paneGeometry` and `uiTrace.recent` are genuinely absent in a harness build, because `make harness` builds with `UI_TRACE` unset. `__aoMemoryReport` and `__aoRevealDrain` are installed in EVERY build (main.ts) precisely because the things that read them (a memory probe, a measurement window's end) run against a harness binary. |
| `perf` | Meter control. Driven by `HarnessPerf*`, not by a test directly. |
| `open` | Mount a thread in a pane, `newPane` for a new one beside the others. The ONLY mutating kind, and it exists for one door: the plain open already has an out-of-page spelling (`notification:activated`, the channel an OS-notification click rides) and deliberately keeps using it, but `openThreadInNewPane` is reached in-page only (ctrl-click on a sidebar row, the thread context menu, a builtin command), so a shell driver has no other way to reach it. It calls that same function; it does not mint a pane of its own. Answers `{opened, threadId, paneId, newPane}`, or an error naming THIS PAGE's thread registry when the id is not in it (a thread the backend has but the page has not listed is a real state and reads very differently from a typo). |
| `reload` | Navigate the page. `HarnessReset`'s contract ends with "reload the page after" (the SPA is holding rows that no longer exist), and nothing outside a browser could do that. The answer is sent BEFORE the reload (a short deferred timeout, capped at 5s), because the socket the reply rides is about to drop; a caller treats a failed reload query as inconclusive and re-probes the bridge. |

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
`runtime/metrics`, reads the backend's own RSS and its owned WebKit helpers
through `internal/procrss` (`/proc` on Linux; process table + responsible
process ownership + libproc on macOS), pulls one frontend sample
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
series as one report.

**The frame gap cannot answer a budget question; busy time can.** Under a
vsync-locked compositor the gap between rAF callbacks has one value (a
3ms tick and a 9ms tick both read ~16.7ms), so a gap histogram can never
say whether the work FITS a 6ms frame budget at 165Hz, and LoAF starts
reporting at 50ms, far too late to see it. The `busy` meter measures the
other quantity, per tick: `performance.now()` at rAF-callback entry, a
task posted through ONE MessageChannel reused for the whole run, and
`performance.now()` again in its handler. What lands in between is that
tick's remaining callbacks plus style, layout and paint: "time until the
main thread could service a cheap task", which is what a budget is
written against. One measurement is in flight at a time: a probe the next
tick overtook is DROPPED and counted (`busy.dropped`), never charged to
whichever tick it lands in. Both meters ride the one rAF loop and either
arms it alone, so `--meter busy` pays for no frame histogram and
`--meter frames` posts no probe.

`busy` reports p50/p95/max/mean over a quarter-millisecond histogram
(finer than the frame one because resolving work below a frame is the
point), plus, per budget in `budgetsMs` (`HarnessPerfSpec.budgetsMs`,
`ao-harness perf start --budgets` / `bench --budgets`, default `6,8,16`),
the share of measured ticks that fit it. Those fit counters are exact,
incremented at record time rather than derived from the buckets, because
they are the figure a `--baseline` gates on. A run that measured no tick
answers `ticks: 0` and 0%, never 100%: "every tick fit" is a claim a
meter that never armed has not earned, and every consumer down the chain
(the bench aggregate, the `perf stop` rendering, the watch columns)
reads `ticks` to tell an unmeasured run from a flawless one.

`busy.worst` is the histogram's complement: the run's eight most expensive
ticks, descending, each with `atMs` (the tick's rAF-callback entry on the
page clock) beside the summary's one `timeOriginMs`, which is what turns
those into wall-clock instants a trace or a log can be opened at. A
percentile says what the distribution was; this says where to look. It is
maintained as a bounded insertion into two preallocated number arrays with
an early exit, because it rides the hot meter and an instrument that
allocates per tick is measuring itself. Like `--trace`'s call-site ranking
it is EVIDENCE and is deliberately absent from the bench aggregate.

**`HarnessReset` stops any active perf run.** It
holds a sampler goroutine AND a set of armed in-page meters, and the
caller reloads the page after a reset, so nothing else would ever disarm
them. That stop query is bounded at 2s rather than the caller-facing 10s:
on the reset path the page is usually already gone, and a bench repeat
would otherwise pay ten seconds per repeat waiting for a reply that is
never coming.

**Every perf spec carries its run id**, `start` included. Two pages can
be attached at once, and a page that never armed THIS run would otherwise
win the first-reply race with an error; stamped with the id, it declines
instead and the armed page answers. A bridge that ignores the field
behaves exactly as it did before.

**A run has a duration ceiling** (`maxDurationMs`, default 30 minutes;
zero means the default, negative means none). Past it the sampler
self-finishes, logs loudly, and PARKS its report: `HarnessPerfStatus`
reports `endedRunId` and the next `HarnessPerfStop` hands the report over
once. An abandoned `perf start` therefore costs a bounded run and a
collectable report rather than a goroutine sampling until the instance
dies. The page carries its own belt to that suspender: meters self-disarm
after five minutes without a collect (the backend collects at least once
a second while a run lives, so a silent backend means the run is gone),
and a late collect/stop answers with a clear self-disarmed error. The
worst frame in a sample (`maxFrameMs`) is per-window (reset at each
collect, as are `maxBusyMs`, `meanBusyMs`, `busyTicks` and
`busyDropped`), while the report's `frames.maxMs` and `busy.*` stay
run-wide; and an
unknown meter name refuses the whole arm, naming the valid set, instead
of silently arming nothing. A bad BUDGET does not refuse, because unlike
a meter name it cannot narrow the run to nothing: the page sorts,
deduplicates and drops non-positive entries, and an empty list falls back
to the default set. (`ao-harness` is stricter at its own edge and refuses
a `--budgets` entry it cannot parse, since a silently shortened budget
list is a gate quietly not being enforced.)

`internal/procrss` matches webview processes by name PREFIX because Linux
truncates `/proc/<pid>/status`'s `Name:` at 15 characters. On macOS it joins
ordinary descendants with their macOS responsible-process sets before reading
libproc RSS; that join is what finds WebKit/Chrome helpers whose PPID is
launchd. Unsupported platforms return `ErrUnsupported` and the RSS series is
simply absent.

**The renderer series takes a sample only when a child actually
matched.** `webviewRssBytes.count == 0` means "not measurable from this
process", which is the NORMAL answer on the Windows/WSL topology, where
WebView2 is the launcher's child and never appears in the backend's
subtree, and `HarnessPerfStatus.webviewRssMeasurable` says so directly.
Recording a zero on an unmatched tick would report a renderer that used
no memory, which is a different and false claim. `SampleAll` is the
sibling that takes every owned process whatever it is named, which is what a
whole-tree figure needs: an empty prefix list cannot express that, since
prefix matching skips a process it does not recognise by design.

## Driving an instance from a shell (`bin/ao-harness`)

`cmd/ao-harness` is the same surface for a human or an agent at a
terminal: `up` / `down` / `list` / `info` / `open` / `attach`, `seed`, `reset`,
`rpc <Method> [json]`,
`threads` / `items` / `send`, `scenario`, `mock`, `events tail|await|count`,
`record` / `bundles` / `replay`, `logs`, a read-only `db`, the
bridge-backed `ui` / `perf` / `monitor` / `bench` / `health`, managed `run`,
offline `compare` / `postmortem`, and the two
CDP-backed instruments `profile` and `bench --trace`. `make
harness-build` builds it alongside `bin/agent-overflow`, and it finds the
backend binary as its own sibling, so a fresh checkout needs no
configuration.

It resolves which instance to talk to from the registry above: an
explicit `--instance <id|idPrefix|dataRoot>`, else the single live row, else this
worktree's default data root. Two live instances is an error listing the
candidates rather than a guess.

The generated CLI reference is [docs/references/ao-harness.md](../references/ao-harness.md). It lists the descriptor tree and points to `rpc --list` for the instance-specific method catalog. The reusable half is `internal/harnessclient`: bootstrap discovery
(instance file or a spawned backend's stdout line), the WS client with
the same consume-on-match `WaitForEvent` semantics as
`e2e/src/harness.ts`, detached launch, and file tailing. A Go test that
needs a real instance imports that rather than re-implementing the
frames. Details, the registry prune rule, and the one refusal an operator
may override (`down --force`, for a row whose data root no longer claims
any instance):
[cmd/ao-harness/AGENTS.md](../../cmd/ao-harness/AGENTS.md).

The bridge-backed commands need a page open on the instance, and
`attach` is the unattended way to get one: it hosts the instance URL in a
headless Chromium, waits for that page to register and answer the bridge,
then either holds it open until SIGINT/SIGTERM or, with `--detach`,
prints the pid and returns (stop it by killing that pid). A wait that
runs out of its wall-clock `--timeout` fails and kills the browser group
rather than reporting a page that is not there. The browser is resolved
in three links, and the chosen one is printed: `--browser` or
`$AO_HARNESS_BROWSER`, then the Chrome-for-Testing already managed by the
built-in browser (a path lookup, never a download), then a Chromium-family
binary on `PATH`. `--devtools-port`
additionally exposes CDP, which is what `profile` and `bench --trace`
need. A headless-shell page hosts the bridge faithfully but is not the
production rendering engine on any platform, so treat its perf numbers as
comparable to other headless runs, not to a WebView2 or WebKitGTK window.

The monitor CLI is typed at its edge. `monitor start`, `heartbeat`, `overlap`, `status`,
`collect`, `stop`, `cleanup`, and `last` send only the finite monitor query
union. Each operation resolves and verifies one exact attached page through
`HarnessInfo`, refuses ambiguous or late page IDs, and bounds returned JSON by
the same `--full` / `--file` output controls as other frontend queries. `status`
is a live collection that leaves the run active. `cleanup` is an explicit
single-run stop used during teardown; page unload also stops all active runs in
the bridge and retains their results.

Use `run --plan` for a fresh disposable workload. The command requires an
absent or empty root, applies the host memory ceiling, and preserves failed
roots for inspection. In addition to the governor reservation and watchdog,
the shared launcher installs a hard per-run boundary before exec: Linux uses a
private cgroup v2 (`memory.max`, swap disabled, OOM group kill). macOS has no
usable memory rlimit, so its native application-responsibility ceiling and
host-floor watchdog are the enforceable boundary. On Windows,
`ao-harness up --window` and
the WSL launcher put the native launcher/WebView2 tree in a memory-limited
Job Object. The WSL backend gets an inherited `RLIMIT_DATA` and an exact
`/proc` identity watchdog, since a Windows Job cannot cross into the WSL
kernel. A missing Linux delegation falls back to inherited `RLIMIT_DATA` with
a 100ms detached watchdog and visible evidence. The ordinary `bench` command
deliberately operates on a selected borrowed
instance and resets it. `up --soak` starts only the soak backend mode; the
Windows launcher is started by `make soak`.

Windowed make targets do not launch the backend binary directly. They call
`ao-harness up --window` and remain in the foreground until the instance
closes, with a teardown trap for Ctrl-C. The macOS bundle helper creates the
per-run WKWebView bundle, arranges an in-place responsibility-disclaimed exec,
and then delegates to that same supervisor path. On exit it removes the
generated bundle and its exact bundle-id-scoped user-Library WebKit state.
On Windows, the launcher Job Object bounds its WebView2 tree, while the WSL
backend receives an inherited Linux address-space limit and an exact-identity
watchdog. One Job Object cannot account for both Windows and WSL namespaces.
The launcher writes `logs/harness-containment.json` in the WSL data root only
after both boundaries and the watchdog are armed.

### The state-clone repro rig

Every fixture above is synthetic, which is the point and also the
limit. Threads seeded from a spec have the sizes and shapes whoever wrote
the spec imagined, so a stall that only appears at the developer's own
scale, or with the exact item mix one real turn produced, has nowhere to
happen. Two verbs close that gap from opposite ends: `clone` brings the
DATA over, `scenario from-thread` brings one thread's BEHAVIOUR back onto
the mock wire.

`ao-harness clone --from <real dataDir>` builds a harness data root from
a copy of a real app data dir and stops. It prints the `up` line rather
than booting. The source app may be RUNNING, so the database is never
file-copied: the source is opened read-only and snapshotted with
`VACUUM INTO`, which yields one consistent file with the WAL folded in.
Nothing is ever opened writable on the source side. The TARGET copy is
then scrubbed of everything resume- or identity-shaped: each thread's
`session_ref` / `pending_fork_session_ref` / `pending_fork_resume_at`,
`thread_import_state`'s source ids (rows kept, identity emptied), and
`ui_state` wholesale, that last one being the stale client-scoped restore
state `HarnessReset` already had to fix once. Only `attachments/` comes
across besides the database; settings, provider accounts, replay bundles,
traces, logs and the instance file are all left behind. The target must
pass `up`'s own refusals plus three more: no live instance holds it, it
is not the source or a parent of it, and an existing database needs
`--force`.

That the result is SAFE to boot is structural rather than a promise from
the scrub: credentials live under the provider home, which an isolated
boot redirects to `<dataRoot>/home`, and provider binary paths are
re-pointed at the mock on every harness boot regardless of what any
settings file says. What the clone DOES carry, verbatim, is real
conversation content.

**The privacy rule, then, is one sentence: a clone lives in its target
root and is never committed anywhere.** Not to this repo, not to a
gist, not into a bug report. The verb prints that line itself every
time it runs.

`ao-harness scenario from-thread --thread <sel> [--turns N]` is the other
half. It reads the booted instance's own store read-only and rebuilds the
thread's last N turns as a mock scenario document: streamed text and
thinking cut into deltas at the recorded `payload_chunks` boundaries (so
the replay has the original stream's shape, not a re-chunking of the
final text), tool calls and completions in recorded order with their
pairing intact, app-internal kinds skipped and counted. It replays
ASSISTANT work only (a real `send` is what opens each Turn), so it
finishes by printing the drive recipe. The Codex leg covers what the
scenario library demonstrates (agent-message streaming and the turn
envelope) and REFUSES reasoning and tool items, naming the wire facts the
store does not record; a guessed dialect would be worse than a refusal.

End to end:

```
ao-harness clone --from ~/.config/agent-overflow
ao-harness up --data-dir <root the clone printed> --window
ao-harness scenario from-thread --thread last --turns 3 --set
ao-harness send --thread <id> --wait '<the recorded prompt>'
```

Now the perf verbs above (`perf`, `bench --trace`, `profile`) run
against the real thread list, the real thread sizes, and a turn that
actually happened.

### Bench workloads

`ao-harness bench <workload>` is a soak that ENDS: it seeds its own
fixture, arms the perf meters, drives a scripted load, and writes
`<dataDir>/bench/<workload>-<timestamp>.json`. Six workloads:
`burst-stream` (chunked text-delta flood), `giant-turn` (225 items in one
turn), `subagent-fanout` (three bounded async subagents),
`multi-pane-stream` (three panes each flooding at once),
`active-multi-pane` (six panes mounted, four streaming paced rich Markdown),
and `many-threads` (30 seeded threads, then a switch storm). The finite
provider workloads wait on `provider:turn_completed` for their thread. The
active workload instead runs until its requested duration and then interrupts
its four turns cleanly. The mock's own
`scenario_done` fires when the mock stopped WRITING, upstream of parse,
triage, persist and render, so it would time a shorter pipeline than the
one under test.

#### The window ends at drain-empty, not at turn completion

Turn completion is where the WIRE stops. What a reader watches is the
reveal queue handing the text over afterwards, and under the mock
providers that outlives the turn by an order of magnitude: a burst-stream
turn closes in about a second and keeps revealing for ten or more. Every
measurement window here (every bench workload and `profile`) therefore
continues past `provider:turn_completed` until the reveal queue is empty,
then takes its settle beat, then stops the meters.

**Baselines taken before this change read shorter.** `duration.ms` in
particular now covers the drain, so an old report compared against a new
one shows a large "regression" on that metric that is purely the window
moving. Re-take the baseline; do not tune to it.

The drain signal is deliberately NOT the bridge's settledness machinery.
`perf start` / `perf stop` force-disarm the document-wide MutationObserver
precisely so a run measures a renderer with no observer on it, and
re-arming one to detect quiet would perturb the experiment it is timing.
It is cheap store state instead: `window.__aoRevealDrain()`
(`frontend/src/lib/utils/revealDrainProbe.ts`, whitelisted in the bridge's
globals table) folds the pane registry into `{panes, draining, smoothers,
boundaries}`: one SvelteMap size and one nullable field per pane, no DOM
walk. The CLI polls it at 250ms and needs two consecutive empty readings,
because a drain empties BETWEEN rows and a single reading taken in that
gap would close the window mid-stream.

It is a READ, and that is a rule rather than an implementation detail: the
reveal queue's behaviour is unchanged by the presence of a reader.
Nothing in the measurement path may skip, rush or pop the drain.

Three degradations, none of which fails a run: a page that does not
install the probe (an older build) answers `unavailable`, a bridge whose
whitelist has never heard of it answers with a query error, and a drain
still going after 60s ends the window anyway. All three print one operator
note and the window ends at turn completion instead.

#### multi-pane-stream

Three seeded threads, one pane each, all three streaming the burst-stream
flood at once. A single streaming pane is not the shape a heavy session
takes: three panes revealing simultaneously share one main thread, one
style and layout pass and one frame budget, and the per-pane work that
looks free in isolation is what saturates a tick here.

The first pane opens the way every other workload's does. The other two go
through the bridge's `open` query kind with `newPane`, which calls the
app's own `openThreadInNewPane`, the function a ctrl-click on a sidebar
row reaches. That door has no event channel (it is an in-page gesture
only), which is why it is the one page move a bench makes through the
bridge rather than over the wire; minting a pane harness-side would put a
pane nobody ships on the screen. The pane opens happen in the workload's
PREPARE step, before the meters and the trace are armed: mounting three
timelines is setup, not the thing being measured.

#### active-multi-pane

Six panes stay mounted while four seeded threads stream
`bench-active-stream` concurrently. The wire cadence stays near the smoother's
reader-facing reveal rate instead of the 1ms flood cadence. Each turn starts
with ordinary rich Markdown and a tool pair, then repeats headings, emphasis,
inline code, a link, Unicode, a table, a fenced code block, and a quote until
the runner interrupts it. This is the normal six-open/four-active workload.

The default duration is 30 seconds. `--duration` can lengthen it and refuses
values below 30 seconds. The runner samples each active assistant's rendered
text length and timeline scroll height every 30 seconds at most. Every text
sample must grow, and the final timeline must be taller than the first. A
provider timer over a static or glitched DOM therefore fails instead of
passing as a sustained load. The report stores those readings under
`runs[].visibleProgress`.

All four completion waits are parked before the sends. A completion before the
deadline fails the run. At the deadline the runner calls the production
`InterruptTurn` RPC for all four concurrently, waits for all four completion
events, drains the reveal queue, and settles. Every error path also interrupts
every turn whose send was attempted.

It uses the same production pane-open path and the same pre-measurement
mount boundary as `multi-pane-stream`. The separate workload preserves the
old three-pane flood baseline while covering the four-pane target.

`many-threads` has no scenario. It drives each switch by emitting
`notification:activated`, which the SPA routes through
`parseNotificationTarget` and `applyNotificationActivated` into
`openThreadInPane`, the same function a sidebar click reaches. That
keeps the workload honest (30 real timeline unmounts and 30 real
bounded-window loads out of SQLite for one tiny RPC each) while being
explicit about what it does NOT cover: the sidebar row's own hit-testing
and hover.

A report doubles as a baseline. `--baseline` takes either a previous
report (its `aggregate` p50 becomes the reference under a default 25%
budget) or a hand-written `metrics` budget, and drift exits 3. There is
no default baseline, so a bench never becomes a gate by accident.

`bench --trace --cdp <endpoint>` adds a Chromium timeline trace around
each repeat and reports the JS call sites that FORCED layout or style
recalculation: `UpdateLayoutTree` / `Layout` events carrying
`args.beginData.stackTrace`, grouped by their top frame. The stack is the
signal rather than a heuristic over it: the engine's own end-of-frame
pass has no JS stack to carry, so only a script-triggered invalidation
gets one, and stackless events are counted separately as engine-scheduled.
The recording starts after the reload and thread open (so it covers the
workload, not the mount) and ends after the meters stop (so draining the
trace is not itself main-thread work on the page being measured). The
merged top 15 land in the report's `trace`, outside `aggregate`, because
a call-site ranking is evidence, not a metric to gate on. Needs a devtools
endpoint; see the section below.

### CPU profiling one turn

`ao-harness profile --thread <sel> --scenario <name>` records a V8
sampling profile (100µs) of ONE scripted turn and writes
`<dataDir>/profiles/profile-<timestamp>.cpuprofile`. It attaches the
debugger, settles, opens the thread through the same `notification:activated`
path `ui open` uses, settles again, arms the profiler, installs the
scenario, sends, waits for `provider:turn_completed`, and stops. It never
reloads the page: a reload would profile the mount instead of the turn.

The rollup it prints splits sampled time three ways. FLUSH is Svelte
running queued effects (`flush_queued_root_effects`, `flush_queued_effects`,
`process_effects`, `update_effect`, `update_derived`, `execute_derived`,
`update_reaction` anywhere in a sample's ancestry). MARKING is the write
side (`internal_set` / `mark_reactions`), which fires from any state write
INCLUDING from inside an effect, so it is checked first and wins wherever
it appears, because charging a dirty-walk inside a flush to "flush
execution" is the exact misattribution the split exists to prevent.
Everything else is `other`.

### The two CDP verbs need a Chromium page

`profile` and `bench --trace` are the only harness commands that bypass
the frontend bridge, because a CPU profile and a timeline trace are
Chromium instruments no bridge can synthesize. They speak the Chrome
DevTools Protocol through `internal/cdpclient` against an endpoint named
by `--cdp` (port, `host:port`, `http://host:port`, or a `ws://` page url)
or by `$AO_CDP_URL` / `$AO_CDP_PORT`.

**WebKitGTK serves no DevTools protocol.** A Linux `make harness-window`
instance answers every other command in this CLI and can serve neither of
these two; the refusal says so rather than timing out. What DOES serve one:
the Windows WebView2 shells, on the loopback ports `appidentity.DevToolsPort`
assigns per mode (dev 9223, soak 9224, harness 9225, perf 9226), an external Chrome or Edge started with
`--remote-debugging-port`, and a Playwright-driven headless Chromium.
An absent endpoint exits 2; an unreachable one exits 1. Page selection
prefers the target on the instance's own origin, falls back to the only
page, and refuses ambiguity with a candidate list. Profiling whichever
tab a listing happened to put first yields plausible numbers about the
wrong document.

For a launcher-hosted WSL backend, run a Windows build of `ao-harness` and
name the data root through `\\wsl.localhost\\<distro>\\...`; Windows can then
reach both the backend and WebView2 loopback ports. Attach accepts the
authenticated WebSocket even though the Linux PID does not exist in Windows'
process namespace. Destructive lifecycle commands retain PID validation and
must run in the backend's namespace.

The busy meter is gateable the same way, through metric names the
aggregate carries alongside the frame ones: `busy.p50Ms`, `busy.p95Ms`
and `busy.maxMs` (lower is better), plus one `busy.fitPct.<budget>ms` per
budget the run carried: `busy.fitPct.6ms`, `busy.fitPct.8ms`,
`busy.fitPct.16ms` by default, HIGHER is better, so a floor is written
`{"busy.fitPct.6ms": {"min": 90}}`. The fit vocabulary is derived from
the reports rather than fixed here, because `--budgets` is a run-time
flag; repeats that disagree contribute the union, each budget folded over
only the repeats that measured it.

### Health rollup

`ao-harness health` is the generalized `make soak-check`: process
liveness and uptime, new `frontend-errors.jsonl` lines, ui-trace oracle
triggers (`timeline.margin.diverge`, `timeline.reasoning.tailJump`, not
the continuous `timeline.row.resize` tracker), new backend stderr, the
process tree's RSS via `procrss.SampleAll`, database size, mock liveness,
replay state, any armed perf run, and embedded-asset freshness (a
`stale` or `dev-server` verdict from `HarnessInfo.assetsFreshness` is
warn: the instance works, but every measurement describes a bundle
nobody ships). One line per concern with an
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
(scans history first, since a fast backend can't win the race, and
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
(`session-import-fixtures.ts`), the seeding pattern for anything that reads
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
  completed ordinary-thread history, the rows production would already have
  persisted after a finished turn. If the harness needs a new capability, it
  wraps the production path.
- Claude scenarios never emit `system/init`; assistant text/thinking
  requires a prior `message_start` with the same id (enforced by
  `internal/harness/scenario/library_test.go`).
- `make e2e` must pass alongside the standard gates when touching the
  harness, the transport, the mock provider, or provider parsing.
