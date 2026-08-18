# internal/provider/claudetui/

Runs the **real Claude Code interactive TUI** in a PTY and reconstructs
AO's normalized `provider.ProviderEvent` stream from *outside* the
process. It is the third provider (alongside headless `claude` and
`codex`), additive and **full-access only**. It exists for the ~5% of
work the TUI exposes that headless can't — slash commands, the
workflows/usage UI, brand-new features — where take-control hands the
human the real terminal.

Headless `claude` stays the default. Reach for this provider only when
the interactive surface is the point.

⚠ This is a **binary-behavior** integration, not a contract. Every
signal here was probed against a pinned `claude` build. Re-probe on
version bump with the harness in `spike/claude-mitm/`. See
[`docs/architecture/claude-tui-provider.md`](../../../docs/architecture/claude-tui-provider.md)
for the full design and the build-time probe list.

## The core idea: reconstruct, don't reimplement

We never speak a protocol to the CLI. We tap two live sources, rebuild
Claude Code's `stream-json --include-partial-messages` envelopes from
them, and feed those through the **shared `claude.Parser`**. That yields
byte-identical events to the headless path with near-zero duplication —
the inner `event` of a `stream_event` envelope *is* the raw Anthropic
`/v1/messages` SSE event the parser already understands.

```
        ┌─ gateway (ANTHROPIC_BASE_URL loopback) ─ raw /v1/messages SSE ─┐
PTY      │                                                                │
claude ──┤                                            reconstruct.go ─────┤─→ feed chan ─→ claude.Parser ─→ onEvent
         │                                            (SSE→stream-json)    │   (single goroutine, serialized)
        └─ hook relay (__claude-hook subcommand) ─ hook payloads ─────────┘
```

## Two live sources (hot path)

1. **Wire** (`gateway.go` → `reconstruct.go` / `turndriver.go`): the
   per-session loopback proxy bound to `ANTHROPIC_BASE_URL`. Forwards
   Claude's traffic upstream untouched (credentials included — it must,
   or auth breaks) and tees the `/v1/messages` SSE into reconstruction:
   streaming deltas (`stream_event` passthrough), tool **starts** (the
   assembled `assistant` envelope), turn-complete (synthesized
   `result`), and turn usage (summed across a turn's requests).
2. **Hooks** (`hookrelay.go` ← `hookcmd.go`): one relay binary
   (`agent-overflow __claude-hook`) registered for a justified event
   set. Source of tool **completions** (`PostToolUse` /
   `PostToolUseFailure` → `user`/`tool_result`), session identity
   (`SessionStart` → `system:init`), compaction (`Pre`/`PostCompact`),
   and the **AskUserQuestion** answer-back.

**The transcript is NOT a live source.** `~/.claude/projects/**` lags
seconds; it is cold-resume history only. Tool results come from
`PostToolUse`, never the transcript.

## Layout

- `doc.go` — package purpose.
- `classify.go` — `classifyRequest`: drops preflight / auxiliary /
  nested-subcall / suggestion-mode `/v1/messages` calls so only real
  main-loop agent turns (`classAgent`) surface as turns. Suggestion mode
  (next-message autocomplete) is the subtle one: it carries the full tool
  set + budget like a real turn, so it's caught by the `[SUGGESTION MODE:`
  marker on its synthetic last user message, not by tools/budget.
- `reconstruct.go` — pure SSE → stream-json envelope core. The
  `messageAssembler` replays content-block deltas into one `assistant`
  message; synthesizers emit `stream_event` / `assistant` / `result` /
  `system:init` / `system:api_retry` lines. `parseRetryableStreamError`
  recognises the raw Anthropic `overloaded_error` frame that parse_stream.go
  would otherwise drop, so a transient mid-stream overload can become an
  `api_retry` (see §Errors in claude-tui-provider.md). No `ProviderEvent`s here
  — only envelope bytes.
- `turndriver.go` — `reconstructor`: the cross-request turn state the
  pure assembler lacks. Two independent turn-boundary signals mirror
  headless (do not re-fuse them into one shape heuristic):
  - **`system:init`** fires whenever a main request reopens a *settled*
    loop (`turnSettled`). Headless re-inits on every main-loop restart —
    a new user turn AND a backgrounded-task resume after the interim
    `end_turn` (see `local_agent_outlives.ndjson`, init #2). triage's
    `handleInit` opens a turn (pending send present) or re-arms the
    settled round (none). This is what unstrands turns 2+.
  - **the `user{isReplay}` echo** fires whenever an AO `Send` awaits
    confirmation (the `userEchoes` FIFO, pushed by `Send`), consuming
    triage's pending-send FIFO and stamping `provider_item_id`. Decoupled
    from `turnSettled` so a mid-turn queued steer confirms without opening
    a turn, and a backgrounded resume (no `Send`) emits none. The echo
    uuid is the app-minted `UserMessageUUID` for direct sends, or a
    `Send`-minted id for queued sends (the flush path supplies none and
    `persistDeferredUserText` needs a non-empty id).

  It also accumulates usage across a turn's several requests, closes the
  turn on a done stop_reason (`end_turn`/`stop_sequence`/`refusal`,
  flipping `turnSettled` back true), and synthesizes the interrupt
  result. Also owns subagent correlation: the Agent/Task launch registry
  and `resolveSubagentParent` (see §Subagents).

  **API retries (main loop):** when `onSSE` sees a mid-stream `error` frame
  (Claude Code's `withRetry` will re-POST), `end()` emits a `system:api_retry`
  carrying a 1-indexed `attempt` from `consecutiveAPIFailures` — reset on any
  successful completion (terminal stop_reason) or interrupt, so it mirrors
  `withRetry.ts`'s per-request loop and triage hides attempts < 4 exactly as
  headless does. A failed attempt emits NO `assistant` envelope and does NOT
  settle the loop (the retry continues the same logical request). Subagent
  error frames are not surfaced as turn-level retries.

  It also reconstructs **background-task completions** from the request
  body (`emitBackgroundCompletions`). A backgrounded command/agent's
  completion crosses the wire ONLY as a `<task-notification>` the CLI
  injects into a later `/v1/messages` body — the stream-json
  `task_updated` + `task_notification` headless emits are CLI-internal and
  never cross `/v1/messages`. Two injected shapes carry it, both handled
  by `eachTaskNotification`: a between-turns resume puts ONE notification
  on a `role:"user"` array-content message; a completion that lands while
  the agent is blocked on `TaskOutput(block=true)` is flushed as a SINGLE
  `role:"system"` `[SYSTEM NOTIFICATION - NOT USER INPUT]` message whose
  STRING content COALESCES a `<task-notification>` per just-finished task
  (the waited one AND any sibling). So we accept both injected roles
  (assistant is rejected — the model could only quote the tag) and extract
  ALL notifications, not just the first, or every task after the first
  stays "running" (confirmed 2.1.170,
  `spike/claude-mitm/probe_taskoutput_siblings.py`). For each terminal
  notification (one bearing a `<status>`; a statusless body is a
  still-running stall ping, skipped) it emits the same pair headless does,
  IN ORDER: `system/task_updated` (triage stashes the host-side exit) then
  `system/task_notification` (drains that stash → writes the
  `tool_completion` sibling at the current write head). Both pass through
  `feedReorder` untouched and the feed is FIFO, so stash-before-drain
  holds. Deduped by `task_id` (the notification recurs in every later
  body). This fires ONLY for backgrounded work — a foreground tool returns
  its result inline and injects no notification, so an inline run never
  gets a separate completion row. Field extraction + the terminal-status
  gate reuse
  `claude.ExtractAllTaskNotificationFields` / `claude.NormalizeTaskTerminalStatus`
  so the tag shape and terminal set can't drift from the shared parser.
- `compaction_capture.go` — the compaction-summarizer capture path
  `turndriver.go`'s `onSSE`/`end` hand off to: `armCompaction` (PreCompact
  arms), `beginCompactionCapture` (claims the summarizer's `classAgent`
  request), `compactionReasoningPassthrough` (forwards only its thinking
  frames live under the reserved scope), and `finalizeCompaction`
  (PostCompact emits the boundary). See §Compaction capture.
- `reorder.go` — `feedReorder`: restores the headless "tool start before
  completion" ordering that the wire+hook split can invert. A fast tool's
  `PostToolUse` hook can land its `tool_result` on the feed before the
  gateway's `end()` emits the assembled `assistant` (the sole
  `EventToolStart` source); fed in that order triage drops the completion
  and the turn-complete force-close marks the command failed. The buffer
  holds a hook tool_result until its `tool_use_id`'s start has been fed.
  A `feedLoop` local — lock-free, shared parser/triage untouched.
- `gateway.go` — the loopback reverse proxy. Reconstruction is set up
  only for `POST /v1/messages`, status 200, `classAgent`. A `classAgent`
  request carrying the `X-Claude-Code-Agent-Id` header is a subagent and
  routes to `beginSubagentTurn` instead (see §Subagents).
- `hookmap.go` — hook payload → stream-json envelope mapping
  (`PostToolUse*` → `tool_result`, `SessionStart` → identity,
  AskUserQuestion → `control_request`). Compaction hooks carry no
  envelope of their own: `Pre`/`PostCompact` drive the reconstructor's
  capture state machine (see §Compaction capture), and
  `hookPayload.CompactSummary` carries `PostCompact`'s committed summary.
- `hookrelay.go` — the per-session loopback relay endpoint +
  capability token. Observe-mode events reconstruct and return at once;
  AskUserQuestion blocks until the human answers in AO (or the window
  elapses → native TUI prompt). `Pre`/`PostCompact` dispatch to the
  reconstructor's `arm`/`finalize` compaction hooks.
- `hookcmd.go` — `RunHookChild`: the `__claude-hook` subcommand. Tiny,
  **fail-open** (any error exits 0 with no stdout = "observe, don't
  interfere"). `main.go` short-circuits to it before any other startup,
  exactly like the orphan reaper.
- `options.go` — `provider.SessionOptions` → launch `Config` (reuses
  `claude.ConfigFromOptions` for model/workdir/resume, and carries the
  two settings-owned axes — see §Prompt + tool overrides).
- `launch.go` — the PTY launch posture: full-access flags, the
  `--settings` hook registration, `--system-prompt-file` /
  `--disallowedTools`, and the env that injects the gateway base URL +
  relay url/token.
- `session.go` — the `provider.Session` impl. Owns the PTY (via
  `internal/terminal`), gateway, relay, and the single `claude.Parser`
  fed off one serialized channel.
- `session_send.go` — the send half of `Session`: the composer/send
  keystroke contract (bracketed paste, inline image path injection,
  composer clear, submit CR) and the `buildSendSteps` step builder.
- `composer_ready.go` — the cold-start composer-ready gate.
  `noteComposerOutput` scans PTY boot output for the bottom status bar
  (de-ANSI'd, ALL chrome markers required) and `awaitComposerReady` holds
  the FIRST send until the Ink composer is parked reading stdin, so the
  submit CR isn't read in the same chunk as the paste and swallowed (the
  worktree-switch / opening-message bug). Bounded-timeout fallback; binary
  behavior — re-probe on version bump (`spike/claude-mitm/`).

## Concurrency model

One `feed` channel, one `feedLoop` goroutine calling `ParseLine` — the
parser's state stays single-goroutine, mirroring the headless read loop.
Every source (`reconstructor.emit`, the relay, interrupt) only ever
*enqueues* envelope bytes. All `onEvent` emissions are serialized under
`emitMu` so the parser, proxy-error, and PTY-exit paths never interleave
into triage.

Because the two sources enqueue independently, the channel order is not
guaranteed to be the headless order. A `feedReorder` local to `feedLoop`
(`reorder.go`) corrects the one case that matters: a hook tool_result
(`EventToolComplete`) racing ahead of its wire `assistant`
(`EventToolStart`). It holds the completion until the start is fed, then
releases it — keeping `ParseLine` single-goroutine and the shared
parser/triage unchanged. Hot-path `stream_event` deltas skip it via a
byte-prefix check.

`recMu` guards the reconstructor's cross-request state (turn-usage,
`turnSettled`, identity, the subagent launch registry, the pending-echo
FIFO, the seen-background-task set). The per-request `onSSE` path is
lock-free (a local assembler + a channel send); only `beginAgentTurn`
(which runs `emitBackgroundCompletions`) / `beginSubagentTurn` /
`endAgentTurn` / `interruptTurn` / `onSessionInfo` / `queueUserEcho` (the
`Send`-side echo enqueue) take `recMu`.

Parallel subagents interleave on the feed (two requests' envelopes
mixing), and that is fine: every subagent envelope carries its own
`parent_tool_use_id`, and the parser keys streaming-block state by
`(parent_tool_use_id, index)` — so two subagents' streams never collide,
and each nests under its own Agent card. Tool completions are correct
regardless: they arrive via hooks keyed by `tool_use_id` and update the
item the parent-tagged start created.

## Subagents

A subagent (Claude's `Agent`/`Task` tool) runs its own `/v1/messages`
turns, but its requests carry **no** parent linkage in the body. The
gateway recognizes them by the `X-Claude-Code-Agent-Id` HTTP header
(absent on main-agent requests) and routes them to `beginSubagentTurn`,
which nests them under the launching Agent tool_call:

1. **Launch registry.** When a main assistant emits an `Agent`/`Task`
   `tool_use`, its `end()` records `(prompt → tool_use_id)`. The launch
   happens-before the subagent's first request (Claude needs the full
   main response, then spawns, then the subagent calls the API — a
   network round trip later), so the registry is always populated in
   time.
2. **Forward-live join by content.** A subagent's first user message
   contains its task prompt — the Agent `tool_use.input.prompt`
   delivered verbatim (confirmed substring on 2.1.170). The first
   request content-matches that against an unclaimed launch and *claims*
   it, so two parallel launches each bind to a distinct subagent.
   `X-Claude-Code-Agent-Id → parent` is cached so the subagent's later
   requests skip the match.
3. **Reconstruct nested, suppress turn artifacts.** Subagent
   `stream_event` + `assistant` envelopes carry top-level
   `parent_tool_use_id`. A subagent emits **no** `result` (it is not a
   top-level turn; one would force-close the real turn) and folds **no**
   usage into the main turn. Inner-tool completions need no special
   handling — they arrive on hooks keyed by `tool_use_id`, and the
   parser re-derives their parent from the parent-tagged start.

Why these signals: the authoritative `agent_id ↔ Agent tool_use_id` join
(`PostToolUse(Agent).tool_response.agentId`) only lands at Agent
*completion* — too late to nest live. `SubagentStart` fires early but
carries only `agent_id`, not the parent. Content-match is the only
forward-live, ordering-independent signal. Its sole failure mode is two
parallel agents with byte-identical prompts, where the binding is
arbitrary but visually equivalent (both cards run identical work). If no
launch matches, the request is forwarded **unreconstructed** rather than
mis-attributed to the main thread — the Agent card still completes via
its `PostToolUse` hook. All of this is binary behavior; re-probe with
`spike/claude-mitm/` on version bump.

## Compaction capture

The compaction summarizer is a forked agent (`querySource:'compact'`,
`maxTurns:1`) whose `/v1/messages` POST is wire-identical to a normal turn
— same model, tools, budget — so it cannot be told apart by request shape.
Left alone it would reconstruct as a phantom agent turn. Instead we **stream
its reasoning live** as its own `compaction_reasoning` row (the "compact"
tail) and commit only its summary onto the `system:compact_boundary`,
detected purely from the public `Pre`/`PostCompact` hook lifecycle (never
prompt-content markers, which churn on every CLI update).

The state machine lives on the `reconstructor` (recMu-guarded), driven by
the hook relay's `compactionHooks{arm, finalize}`:

1. **`PreCompact` → arm.** `armCompaction()` sets `compacting` and resets
   any pending capture. Auto-compaction is synchronous in Claude Code
   (`query.ts`: `await deps.autocompact(...)` resolves PreCompact →
   summarizer POST → PostCompact *before* the real-turn POST), so the
   summarizer is the **only** `classAgent` POST in the armed window —
   canonical ordering `UserPromptSubmit → PreCompact → summarizer POST →
   PostCompact → real-turn POST`. No `UserPromptSubmit` signal is needed.
2. **First armed `classAgent` POST → capture, disarm.** `gateway.handle`
   tries `beginCompactionCapture` before the normal / subagent turn paths;
   while armed it claims the request into a capture `agentRequest`
   (disarming immediately). Its `onSSE` does two things per frame: it folds
   the SSE into the assembler (for the summary fallback) AND, via
   `compactionReasoningPassthrough`, forwards ONLY the summarizer's
   **thinking** content-block frames live through `streamEventLine` tagged
   with the reserved `provider.CompactionReasoningScope`. Message / text /
   tool frames stay suppressed — no init, no summary-text deltas, no tool
   starts, no `result`. The reasoning therefore streams as parser-native
   thinking events carrying the reserved scope; everything else about the
   summarizer turn is invisible.
3. **`PostCompact` → finalize.** `finalizeCompaction(trigger, summary)`
   emits one boundary whose `compactMetadata` carries `{trigger, summary}`
   — **summary only**, because the reasoning already streamed. The hook's
   committed `compact_summary` wins; the captured SSE text
   (`pendingCompaction.summary`, assembled-text) is the fallback when the
   hook carries none.

Why the reserved scope (not a new event kind): `streamEventLine` stamps the
scope as `parent_tool_use_id` on the envelope, so the shared `claude.Parser`
emits ordinary `EventThinking` / `EventContentBlock*` carrying it — **zero
parser change** (the #1 constraint). Triage dispatches that sentinel scope
to dedicated handlers BEFORE its subagent-nesting logic. The sentinel can't
collide with a real tool_use id.

Edge cases:

- **Abort / retry.** A user typing mid-compaction aborts summarizer #1; the
  CLI fires a fresh `PreCompact` + retry. Because reasoning now streams
  per-attempt, an aborted attempt's partial reasoning DOES stream (a brief,
  sub-second tail that then settles) — accepted as a minor known limitation.
  The **summary** guarantee is preserved at the boundary: the re-arm resets
  `pendingCompaction`, so the finalized boundary's summary fallback always
  comes from the **completed** retry, never the aborted partial.
- **Failure (no `PostCompact`).** Drop the boundary silently — no summary
  row. Any reasoning that already streamed settles on its own; the
  summarizer already disarmed on capture, so the next real turn reconstructs
  normally.
- **Empty reasoning.** A thinking block with no deltas streams a
  content-block start/stop but no `EventThinking`; triage creates no row
  (handlers no-op on empty content).
- **Session-memory compaction** (`trySessionMemoryCompaction`) fires **no**
  `Pre`/`PostCompact` hooks, so it is hook-invisible here. It is gated off
  by default (`tengu_session_memory` + `tengu_sm_compact` both required) and
  treated as a documented known limitation, not handled.

Downstream: triage routes the reserved-scope thinking to a top-level
`compaction_reasoning` streaming row (the live "compact" tail, settling just
ABOVE the divider), and `handleCompaction` lifts the `summary` out of the
boundary meta into an on-demand `compaction` payload (items.meta stays a
cheap `{trigger}` blob). The frontend renders the reasoning via
`CompactionReasoning.svelte` (thinking-style 3-line tail) and the committed
summary via `CompactionDivider.svelte`. Headless `claude` and Codex emit a
boundary with no summary and stream no reasoning, so they render the plain
divider. All binary behavior — re-probe with `spike/claude-mitm/` on
version bump.

## Prompt + tool overrides

The two settings-owned axes of
[`docs/specs/prompt-tool-overrides.md`](../../../docs/specs/prompt-tool-overrides.md)
apply here, not just to headless (user decision 2026-08-18, superseding
the original exclusion). Both are ordinary PTY launch flags, the same
class as `--model` / `--effort` / `--resume`, and both were spike-verified
against claude 2.1.234 by running the real TUI under a PTY with a wire
capture:

- **`Config.SystemPrompt` → `--system-prompt-file <path>`.** Full body
  replacement, exactly as headless: the request's `system` array becomes
  [billing header, the TUI's fixed identity line "You are Claude Code,
  Anthropic's official CLI for Claude.", the file's content]. The file is
  written by the shared `claude.WriteSystemPromptFile` (0600; the prompt
  never reaches argv — `MAX_ARG_STRLEN` and `/proc/<pid>/cmdline`) and
  removed by `Session.Close`, which `NewSession`'s deferred cleanup also
  runs, so every failed-launch path drops it.
- **`Config.DisallowedTools` → one `--disallowedTools <name>` per
  entry.** The named tools' schemas are absent from the request. Note the
  CLI aliases `Task` and `Agent`: disallowing one removes both.

`ConfigFromOptions` takes the tool list RAW from `opts.DisabledTools`
rather than off `claude.ConfigFromOptions`'s merged field — that field
unions in the read-only runtime-mode strip, and `EnforcesRuntimeMode` is
false here, so every tier must stay inert (pinned by
`TestConfigFromOptionsIgnoresEveryRuntimeMode`). It still runs the shared
`claude.SanitizeDisallowedTools` argv-safety pass, so a name that is not
ONE safe CLI argument cannot reach argv on either transport.

Settings routing is provider-generic: `settings.PromptOverridesForProvider`
/ `DisabledToolsForProvider` map `claude-tui` onto the Claude lists (same
binary, like `HiddenModelsForProvider`), so the app's spawn path stamps
both axes with no provider branch and `pinSettingsOwnedAxes` keeps a
settings edit from restarting a live TUI session.

## Security boundary (load-bearing)

- **The gateway forwards credentials untouched but never logs them.**
  Production stores AO-normalized events only; raw body capture is a
  dev-only, local-only, short-retention follow-on with credential
  headers redacted. Never commit fresh raw captures.
- **The hook relay is a privileged local boundary.** Loopback-only;
  every request is checked against a per-session capability token
  (`crypto/subtle` constant-time) and a loopback-peer check. Reject
  browser/LAN-origin calls.
- **AO never collects, exposes, or logs Claude.ai credentials.** Not to
  remote clients, not to disk.

## Provider-package discipline (unchanged)

- Return normalized `ProviderEvent`s via `onEvent`. Do **not** write to
  the store or emit UI events directly — only triage persists, only
  `app.go`/transport emits.
- Do **not** leak provider-native types out of the package.
- Do **not** guess wire behavior from this repo — probe against the real
  binary. See `docs/references/spike-policy.md`.

## Take-control (the escape hatch) — not yet wired

The design (`docs/architecture/claude-tui-provider.md §Attach &
take-control`) routes everything AO can't reliably capture to a
human-driven terminal drawer: a read-only xterm view of the PTY plus a
take-control override (input lease, Refresh-on-attach, stall detector).
`session.go` already exposes the seam (`sink` for gated output fan-out;
the PTY lives in a private `terminal.Manager`). The transport RPCs +
frontend xterm drawer + provider picker that drive it are the **next
phase** — deliberately not stubbed here to avoid dead code.

Concretely out of scope until then, all handled by take-control +
stall-notify: plan mode (`ExitPlanMode`), revert/checkpoints,
resume-at-a-prior-turn, MCP auth/OAuth, and one-off native dialogs
(sensitive-path edits). AO never parses the TUI to *decide* answers — at
most it de-ANSIs coarsely to notice a stall and fetch the human.

## Testing

- `reconstruct_test.go` is the **parity harness**: it feeds reconstructed
  envelopes through the real `claude.Parser` and asserts `ProviderEvent`
  output, so the TUI path can't drift from headless.
- `session_test.go` covers the serialized feed→parser→emit wiring,
  identity capture, teardown idempotency, send-validation, and
  exit-meta. PTY write-framing / launch are validated against the real
  binary in `spike/claude-mitm/`.
- Add a reconstruction parity test in the same commit as any
  envelope-shape change.

## References

- [`docs/architecture/claude-tui-provider.md`](../../../docs/architecture/claude-tui-provider.md)
  — full design, launch posture, probe list, risks.
- `internal/provider/claude/AGENTS.md` — the shared parser and the
  stream-json shapes this provider reconstructs.
- `spike/claude-mitm/` — the probe harness and captured findings.
- `internal/terminal/AGENTS.md` — the PTY substrate.
