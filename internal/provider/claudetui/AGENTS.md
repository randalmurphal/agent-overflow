# internal/provider/claudetui/

Runs the real Claude Code interactive TUI in a PTY and reconstructs AO's
normalized `provider.ProviderEvent` stream from OUTSIDE the process. The
third provider, additive and full-access only. Headless `claude` stays
the default; reach for this one when the interactive surface is the
point.

Full design, launch posture, signal-recovery details, and the build-time
probe list live in
[`docs/architecture/claude-tui-provider.md`](../../../docs/architecture/claude-tui-provider.md).
The stream-json shapes this package reconstructs are in
`internal/provider/claude/AGENTS.md` and
[`claude-wire.md`](../../../docs/references/claude-wire.md).

**This is binary behavior, not a contract.** Every signal here was probed
against a pinned `claude` build. Re-probe on version bump with
`spike/claude-mitm/`, and never guess a shape from this repo.

## Reconstruct, do not reimplement

We never speak a protocol to the CLI. Two live sources are rebuilt into
Claude Code's `stream-json --include-partial-messages` envelopes and fed
through the SHARED `claude.Parser`, which yields byte-identical events to
the headless path: the inner `event` of a `stream_event` envelope IS the
raw Anthropic `/v1/messages` SSE event the parser already understands.

```
        gateway (ANTHROPIC_BASE_URL loopback), raw /v1/messages SSE
PTY                                                                \
claude                          reconstruct.go (SSE to stream-json) -> feed chan -> claude.Parser -> onEvent
                                                                    /
        hook relay (agent-overflow __claude-hook), hook payloads
```

Two sources, two jobs. The WIRE carries streaming deltas, tool STARTS
(the assembled `assistant` envelope), turn-complete, and turn usage. The
HOOKS carry tool COMPLETIONS (`PostToolUse`), session identity
(`SessionStart`), compaction (`Pre`/`PostCompact`), and the
AskUserQuestion answer-back.

**The transcript is NOT a live source.** `~/.claude/projects/**` lags
seconds; it is cold-resume history only. Tool results come from
`PostToolUse`, never the transcript.

## Concurrency model

One `feed` channel, one `feedLoop` goroutine calling `ParseLine`, so the
parser's state stays single-goroutine exactly as it does on the headless
read loop. Every source (`reconstructor.emit`, the relay, interrupt) only
ENQUEUES envelope bytes. All `onEvent` emissions are serialized under
`emitMu` so the parser, proxy-error and PTY-exit paths never interleave
into triage.

- Because the two sources enqueue independently, channel order is not
  headless order. `feedReorder` (`reorder.go`, a `feedLoop` local) fixes
  the one case that matters: a hook `tool_result` racing ahead of its
  wire `assistant`, the sole `EventToolStart` source. Fed in that order,
  triage drops the completion and turn-complete marks the command failed.
  The buffer holds the completion until its `tool_use_id`'s start has
  been fed. Hot-path `stream_event` deltas skip it via a byte-prefix
  check. Keep the fix here, local and lock-free: the shared parser and
  triage stay unchanged.
- `recMu` guards the reconstructor's cross-request state (turn usage,
  `turnSettled`, identity, the subagent launch registry, the pending-echo
  FIFO, the seen-background-task set). The per-request `onSSE` path is
  lock-free.
- Parallel subagents interleaving on the feed is fine and needs no
  serialization: every subagent envelope carries its own
  `parent_tool_use_id`, and the parser keys streaming-block state by
  `(parent_tool_use_id, index)`.

## Turn boundaries are two independent signals

Do not re-fuse them into one shape heuristic (`turndriver.go`).

- **`system:init`** fires whenever a main request reopens a SETTLED loop
  (`turnSettled`). Headless re-inits on every main-loop restart, a new
  user turn AND a backgrounded-task resume after the interim `end_turn`.
  This is what unstrands turns 2 and later.
- **The `user{isReplay}` echo** fires whenever an AO `Send` awaits
  confirmation (the `userEchoes` FIFO). Decoupled from `turnSettled` so a
  mid-turn queued steer confirms without opening a turn, and a
  backgrounded resume with no `Send` emits none. The echo uuid must be
  non-empty: `persistDeferredUserText` needs one, so a queued send whose
  flush path supplies none gets a `Send`-minted id.

## Subagent nesting joins by content, and has to

A subagent's `/v1/messages` requests carry NO parent linkage in the body.
The gateway recognizes them by the `X-Claude-Code-Agent-Id` header and
content-matches the subagent's first user message (which contains the
Agent `tool_use.input.prompt` verbatim) against an unclaimed launch in
the registry, then caches `agent_id -> parent`.

That indirection is not a shortcut. The authoritative join
(`PostToolUse(Agent).tool_response.agentId`) only lands at Agent
COMPLETION, too late to nest live, and `SubagentStart` carries only
`agent_id`, never the parent. Content-match is the only forward-live,
ordering-independent signal available. Its sole failure mode is two
parallel agents with byte-identical prompts, where the binding is
arbitrary but visually equivalent. On no match the request is forwarded
UNRECONSTRUCTED rather than mis-attributed to the main thread; the Agent
card still completes via its `PostToolUse` hook.

A subagent emits no `result` (one would force-close the real turn) and
folds no usage into the main turn.

## Compaction capture

The summarizer's POST is wire-identical to a normal turn, so it cannot be
told apart by request shape. Detection is purely the public
`Pre`/`PostCompact` hook lifecycle, never prompt-content markers, which
churn on every CLI update. The state machine and its edge cases
(abort/retry, missing `PostCompact`, empty reasoning, hook-invisible
session-memory compaction) are documented in
[`claude-tui-provider.md §Compaction`](../../../docs/architecture/claude-tui-provider.md).

The one constraint to hold while editing here: reasoning streams through
`streamEventLine` stamped with the reserved
`provider.CompactionReasoningScope` in the `parent_tool_use_id` slot, so
the shared parser emits ordinary `EventThinking` / `EventContentBlock*`
and needs ZERO parser change. A new event kind for this would break that.
Message, text and tool frames from the summarizer stay suppressed; only
the committed summary reaches `system:compact_boundary`.

## Background completions come from the request body

A backgrounded command or agent's completion crosses `/v1/messages` ONLY
as a `<task-notification>` the CLI injects into a later request body. The
stream-json `task_updated` + `task_notification` pair headless emits is
CLI-internal and never crosses the wire.

- Accept both injected roles (`user` array-content between turns, and the
  `role:"system"` `[SYSTEM NOTIFICATION - NOT USER INPUT]` flush while
  the agent blocks on `TaskOutput`). Reject `assistant`: the model could
  only be quoting the tag.
- Extract ALL notifications per body, not just the first. The blocked
  flush COALESCES one per just-finished task, so stopping at the first
  leaves every sibling stuck "running".
- Emit the same pair headless does, IN ORDER: `system/task_updated`
  (triage stashes the host-side exit) then `system/task_notification`
  (drains that stash). Both pass through `feedReorder` untouched and the
  feed is FIFO, so stash-before-drain holds.
- Field extraction and the terminal-status gate reuse
  `claude.ExtractAllTaskNotificationFields` /
  `claude.NormalizeTaskTerminalStatus`, so the tag shape and terminal set
  cannot drift from the shared parser.

## Runtime mode is inert here

`EnforcesRuntimeMode` is false: approvals live inside the real TUI.
`ConfigFromOptions` therefore takes the disallowed-tool list RAW from
`opts.DisabledTools` rather than off `claude.ConfigFromOptions`'s merged
field, which unions in the read-only mode strip. Every tier must stay
inert, pinned by `TestConfigFromOptionsIgnoresEveryRuntimeMode`. The list
still runs through the shared `claude.SanitizeDisallowedTools`, so a name
that is not ONE safe CLI argument cannot reach argv on either transport.

The two settings-owned axes of
[`prompt-tool-overrides.md`](../../../docs/specs/prompt-tool-overrides.md)
apply here as ordinary PTY launch flags (`--system-prompt-file`,
`--disallowedTools`), spike-verified against 2.1.234. Note the CLI
aliases `Task` and `Agent`: disallowing one removes both. This session
has NO live-update surface, so `reconcileSettingsOwnedAxes` pins both
axes rather than converging them.

`Config.AdditionalDirs` → `--add-dir` is the third flag shared with the
headless launch, stamped by the app with the attachments root so a Read
of an attached file never prompts. Rationale and the commander
verification are in `claude/AGENTS.md` §Spawn posture; keep the two
emitters identical.

## Take-control

Take-control is WIRED, end to end: `attach.go` here (`AttachTerminal`
plus the `*TerminalAttachment` a caller gets back — `Release`,
`SetControl`, `HoldsControl`, `WriteInput` — and the session-wide
`TerminalReplaySnapshot`, `HasTakeControl`, `ResizeTerminal`,
`RefreshTerminal`), the seven `ProviderTerminal*` App methods in
`app_claudetui_terminal.go` (all `//ao:scope terminal:operate`), and
`frontend/src/lib/components/takecontrol/`. Design is
[`claude-tui-provider.md §Attach & take-control`](../../../docs/architecture/claude-tui-provider.md).

Rules that live in this package:

- **A claim belongs to the client that made it.** `AttachTerminal`
  returns a `*TerminalAttachment`, and everything a client armed is given
  back through that one handle. The app mints one per CONNECTION and
  registers its `Release` with the socket's teardown, so a client that
  dies mid-take-control gives the lease back on its own. Before this the
  lease was one session-wide `bool`: a dead socket left it held and every
  `Send` on the thread was refused until the session restarted, and a
  second attach displaced the first viewer's sink while either detach
  stripped the other's lease.
- **The input lease is the arbitration, and it has ONE holder.**
  `WriteInput` is refused unless the CALLING attachment holds the lease,
  so neither a read-only attach nor a second pane watching over the
  holder's shoulder can inject keystrokes; `Send` is refused while any
  attachment holds it, so AO's driver and the human never interleave into
  the composer. Acquiring while another attachment holds it is REFUSED,
  never taken — seizing it would put two humans in one composer.
  Releasing without holding it is always a no-op, because the client's
  own detach and its socket's teardown both run that path and cannot
  order themselves against each other.
- **Fan-out is gated on attach and REFCOUNTED; the ring buffer never is.**
  The TUI repaints constantly. Attaching only starts the live tee, and a
  session nobody is attached to loses nothing because `internal/terminal`
  keeps buffering. One tee serves every attachment, because the app
  answers a chunk with one thread-keyed broadcast frame every attached
  client already receives — a tee per attachment would emit each chunk
  once per client and every client would render the duplicates. It is
  torn down when the LAST claim goes, so one pane closing never blinds
  another.
- **Repaint through `RefreshTerminal`.** A freshly attached xterm shows a
  stale Ink frame. Never call `Process.Resize` directly, which bypasses
  `resizeMu`.
- PTY width never affects the normalized event stream. Reconstruction is
  wire and hook sourced.

Deliberately out of scope, all routed to take-control plus stall-notify:
plan mode (`ExitPlanMode`), revert/checkpoints, resume-at-a-prior-turn,
MCP auth and OAuth, and one-off native dialogs such as sensitive-path
edits. AO never parses the TUI to DECIDE an answer; at most it de-ANSIs
coarsely to notice a stall and fetch the human.

## Security boundary

- **The gateway forwards credentials untouched and never logs them.**
  Production stores AO-normalized events only. Raw body capture is
  dev-only, local-only, short-retention, with credential headers
  redacted. Never commit fresh raw captures.
- **The hook relay is a privileged local boundary.** Loopback-only, with
  a per-session capability token checked in constant time
  (`crypto/subtle`) plus a loopback-peer check. Reject browser and
  LAN-origin calls.
- The hook child (`hookcmd.go`) is FAIL-OPEN: any error exits 0 with no
  stdout, which the CLI reads as "observe, do not interfere".

## Provider-package discipline

Return normalized `ProviderEvent`s through `onEvent`. Do not write to the
store, do not emit UI events, do not leak provider-native types out of
the package. Only triage persists and only `app.go` and transport emit.

## Testing

- `reconstruct_test.go` is the PARITY harness: it feeds reconstructed
  envelopes through the real `claude.Parser` and asserts `ProviderEvent`
  output, so this path cannot drift from headless. Add a parity test in
  the same commit as any envelope-shape change.
- `session_test.go` covers the serialized feed-parser-emit wiring,
  identity capture, teardown idempotency, send validation, and exit meta.
- PTY write framing and launch are validated against the real binary in
  `spike/claude-mitm/`, not in Go tests.
