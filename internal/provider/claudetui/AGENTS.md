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
  nested-subcall `/v1/messages` calls so only real main-loop agent
  turns (`classAgent`) surface as turns.
- `reconstruct.go` — pure SSE → stream-json envelope core. The
  `messageAssembler` replays content-block deltas into one `assistant`
  message; synthesizers emit `stream_event` / `assistant` / `result` /
  `system:init` lines. No `ProviderEvent`s here — only envelope bytes.
- `turndriver.go` — `reconstructor`: the cross-request turn state the
  pure assembler lacks — emit `system:init` once, accumulate usage
  across a turn's several requests, close the turn on a done stop_reason
  (`end_turn`/`stop_sequence`/`refusal`), and synthesize the
  interrupt result.
- `gateway.go` — the loopback reverse proxy. Reconstruction is set up
  only for `POST /v1/messages`, status 200, `classAgent`.
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

`recMu` guards the reconstructor's cross-request state (`sawInit`,
turn-usage, identity). The per-request `onSSE` path is lock-free (a
local assembler + a channel send); only `beginAgentTurn` / `endAgentTurn`
/ `interruptTurn` / `onSessionInfo` take `recMu`.

**v1 limitation:** parallel-subagent wire turns may interleave
imperfectly on the feed (two requests' envelopes mixing). Tool
completions stay correct regardless — they arrive via hooks keyed by
`tool_use_id`.

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
