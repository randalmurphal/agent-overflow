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
  `system:init` lines. No `ProviderEvent`s here — only envelope bytes.
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
  compaction boundary, AskUserQuestion → `control_request`).
- `hookrelay.go` — the per-session loopback relay endpoint +
  capability token. Observe-mode events reconstruct and return at once;
  AskUserQuestion blocks until the human answers in AO (or the window
  elapses → native TUI prompt).
- `hookcmd.go` — `RunHookChild`: the `__claude-hook` subcommand. Tiny,
  **fail-open** (any error exits 0 with no stdout = "observe, don't
  interfere"). `main.go` short-circuits to it before any other startup,
  exactly like the orphan reaper.
- `options.go` — `provider.SessionOptions` → launch `Config` (reuses
  `claude.ConfigFromOptions` for model/workdir/resume).
- `launch.go` — the PTY launch posture: full-access flags, the
  `--settings` hook registration, and the env that injects the gateway
  base URL + relay url/token.
- `session.go` — the `provider.Session` impl. Owns the PTY (via
  `internal/terminal`), gateway, relay, and the single `claude.Parser`
  fed off one serialized channel.

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
FIFO). The per-request `onSSE` path is lock-free (a local assembler + a
channel send); only `beginAgentTurn` / `beginSubagentTurn` /
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
