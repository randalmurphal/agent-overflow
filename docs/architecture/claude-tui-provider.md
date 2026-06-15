# Claude TUI Provider

A third provider that runs the **real Claude Code interactive TUI** in a PTY
and reconstructs AO's normalized event stream from outside the process, instead
of speaking the headless stream-json protocol. It exists for the ~5% of work the
TUI exposes that headless cannot — slash commands, the workflows UI, the usage
UI, and any new feature that lands in the TUI before (or without ever) reaching
the `--output-format stream-json` surface.

> **Status: design.** This document is the build plan. It is grounded in the
> `spike/claude-mitm/` investigation (see that directory's `README.md` and
> `HOOKS_COVERAGE_MAP.md`). Spike code is throwaway — we port the *learnings*,
> not the Python. Items marked **[validate-at-build]** are confirmed in
> principle but must be re-probed against the installed binary before the code
> that depends on them ships.

## Scope

**This provider is additive. Headless (`internal/provider/claude`) stays the
default and is the better choice 95% of the time** — it gives us a clean typed
protocol, deterministic approvals, and no PTY to babysit. The TUI provider is
the escape hatch for what headless structurally cannot reach.

In scope:

- Launch the literal `claude` TUI in full-access mode under AO, in a PTY.
- Reconstruct the same `provider.ProviderEvent` kinds the headless provider
  emits — by **reusing the headless parser** (see below), fed from two live
  sources: the gateway wire and a small hook relay.
- Drive turns: send a prompt, interrupt.
- A read-only terminal drawer that renders the live TUI, plus an intentional
  **take-control** override so the user can drive the real session directly to
  unbrick something, then hand back to AO.
- Resume the whole session (v1).

Explicitly **out of scope** — these route to the take-control escape hatch, not
to bespoke AO handling (see [Out of scope](#out-of-scope--the-escape-hatch)):

- Revert / checkpoints (no clean external primitive; confirmed not needed).
- Plan mode (`ExitPlanMode` interception).
- Resume-at-a-prior-turn (only useful for revert).
- Driving MCP auth / OAuth login flows.
- Sensitive-path and other one-off TUI dialogs.

## The core idea: reconstruct stream-json, reuse the headless parser

Headless Claude Code hands AO **one stdio protocol**: NDJSON in, NDJSON out,
every signal typed and synthesized by the CLI. The TUI gives us none of that —
its only output is ANSI bytes painting an Ink UI, and its only input is
keystrokes.

The decisive realization from grounding: **Claude Code's `stream-json
--include-partial-messages` envelope can be reconstructed from outside the
process**, and the existing `internal/provider/claude` `Parser` consumes that
envelope. Concretely:

- The inner `event` of a `stream_event` envelope **is the raw Anthropic
  `/v1/messages` SSE event** — exactly what the gateway sees on the wire.
- The `user`-with-`tool_result` envelope is exactly what a `PostToolUse` hook
  payload reconstructs (`tool_response` is the same object the CLI writes as the
  transcript's `toolUseResult`).
- `system:init`, `result`, `compact_boundary`, and `rate_limit_event` envelopes
  are small synthetic shapes assembled from wire + hook data.

So the TUI provider does **not** reimplement the ~150 KB of ProviderEvent
transform logic. It runs a thin **reconstruction layer** that emits stream-json
envelopes and feeds them through `claude.NewParser()` — the same parser the
headless provider uses. Result: **byte-identical `ProviderEvent`s**, identical
triage and frontend handling, near-zero duplication, and automatic parity as the
headless parser evolves. `claudetui` importing `claude` is sibling
Claude-to-Claude reuse (two transports for one wire format), not a forced
cross-provider abstraction (Core Principle 6).

```
                          ┌─────────────────────────────────────────────┐
   AO (claudetui.Session)  │             the real `claude` TUI            │
                          │        (Ink app in a PTY, full-access)        │
  ┌──────────┐ keystrokes  │                                              │
  │  Send /   ├────────────▶ stdin ─┐  ANTHROPIC_BASE_URL=127.0.0.1:NN    │
  │ Interrupt │            │        │  CLAUDE hooks → relay subcommand     │
  └──────────┘            │        ▼                                      │
                          │   ┌──────────┐  /v1/messages SSE              │
                          │   │ gateway  ├──────────────────▶ api.anthropic│
   ① wire SSE  ◀──────────┼───┤ (loopback│◀──────────────────            │
                          │   │  proxy)  │                                │
   ② hook payloads ◀──────┼── PostToolUse / PostToolUseFailure / Session- │
                          │   Start / Pre+PostCompact / AskUserQuestion   │
                          │                                              │
                          └───┬──────────────────────────────────────────┘
                              │
              reconstruct.go  │  raw SSE  ─┐
              hookrelay.go    │  hook JSON ─┤── synthesize stream-json envelopes
                              ▼            │   {stream_event, assistant, user,
                       ┌─────────────┐     │    result, system, rate_limit_event}
                       │ claude.Parser│◀────┘   (one instance, serialized feed)
                       └──────┬───────┘
                              │ provider.ProviderEvent  (identical to headless)
                              ▼
                    onEvent → triage → store → frontend

   (transcript: COLD-RESUME ONLY — read history on resume; never a live signal)
   (PTY bytes → terminal drawer for read-only view + take-control)
```

The two live sources run continuously and independently of input mode. That is
what makes take-control safe: when the human grabs the PTY, the wire and hook
taps never stop capturing, so AO resumes from a consistent state the moment
control is handed back.

## The two live sources (and why the transcript is not one)

The transcript (`~/.claude/projects/**/<session>.jsonl`) is **written with
multi-second lag at times** — fine as a durable record, wrong as a live signal.
Sourcing tool completion from it would leave every tool stuck "running" for
seconds after it finished. So the hot path uses only the two low-latency
sources, and the transcript is reserved for cold resume (reading history when a
session is resumed, where latency is irrelevant).

| Source | Mechanism | Carries (→ reconstructed envelope) |
|---|---|---|
| ① **Wire** | Per-session loopback reverse proxy on `ANTHROPIC_BASE_URL`, parses `/v1/messages` SSE | streaming text + thinking deltas, content-block boundaries (→ `stream_event`); tool **starts** + durable text + usage (→ assembled `assistant`); turn-complete (→ synthesized `result`); rate-limit headers (→ `rate_limit_event`); background-Bash completion (`<task-notification>` in the next request body); the interrupt marker |
| ② **Hooks** | One relay command (a hidden AO subcommand) registered via `--settings` for ~6 events | tool **completions** — `PostToolUse` (`tool_response`) + `PostToolUseFailure` (`error`) (→ `user`/`tool_result`); session identity — `SessionStart` (→ `system:init`); compaction — `PreCompact`/`PostCompact` (→ `system:compact_boundary`); AskUserQuestion answer-back |

Wire and hooks are complementary, neither redundant:

- The wire has streaming granularity and tool **starts**, but `/v1/messages`
  never carries tool **results** as a discrete event (they ride in the *next*
  request's `messages[]`) and never carries the CLI's synthesized envelopes.
- The hooks deliver tool **completions** synchronously and structured
  (`tool_response` == the transcript's `toolUseResult`, with **zero lag**), plus
  session identity and compaction.

One `claude.Parser` instance per session consumes both, serialized through a
single feed channel, so a tool **start** (wire `assistant`) and its **completion**
(hook `user`) correlate by `tool_use_id` inside the shared parser state — exactly
as they do in headless.

**Start-before-completion ordering is not free here.** Headless gets it for
nothing: stdout serializes the `assistant` line before the `user` line
in-process. The TUI's two sources can invert it. The gateway forwards each SSE
chunk to the CLI *before* teeing it to reconstruction (`gateway.stream`), and the
assembled `assistant` envelope — the sole source of `EventToolStart` — is emitted
only at request `end()`. A fast tool (e.g. `echo`) therefore runs and its
`PostToolUse` hook enqueues the `user`/tool_result (`EventToolComplete`) on the
feed channel *before* `end()` enqueues the `assistant`. Fed in that order, triage
drops the completion (no launch row yet) and the turn-complete force-close marks
the orphaned running tool_call errored — a successful command rendered as failed,
its output lost. `reorder.go` (a `feedLoop` local, lock-free) closes the gap: it
holds a hook tool_result until the `assistant` carrying its `tool_use_id` has been
fed, then releases it. The shared parser and triage stay untouched — this is a
TUI-only reconstruction-ordering correction.

### Two wire-only exceptions (no hook fires)

Both confirmed LIVE in the spike with **both** post-hooks registered:

- **Background Bash completion.** `PostToolUse(Bash)` fires at *dispatch*
  (carries `backgroundTaskId`, empty stdout), **not** at completion. The
  completion arrives only on the wire as a `<task-notification>` user message in
  the next `/v1/messages` body (same `task-id` + status + exit code +
  output-file). Correlate `backgroundTaskId` ↔ `task-id`; a follow-up turn
  flushes it. This is the same data the headless `task_notification` path
  consumes.
- **Interrupted tool.** Neither `PostToolUse` nor `PostToolUseFailure` fires. The
  discriminator is the synthetic `[Request interrupted by user for tool use]`
  user message on the wire, plus an `is_error` `tool_result` byte-identical to a
  permission-deny. See [Interrupt](#interrupt--validate-at-build-on-21170).

## The hook relay

One relay **command** — a hidden AO subcommand (`agent-overflow __claude-hook`,
mirroring the existing `__reap` orphan-reaper sidecar pattern) — is registered
for every captured event via the `--settings` flag layer. Claude runs it on each
hook; it reads the payload on stdin and forwards to AO's per-session loopback
relay endpoint. Two modes:

- **Observe mode** (`PostToolUse`, `PostToolUseFailure`, `SessionStart`,
  `PreCompact`, `PostCompact`, and optionally `SubagentStop` / `Notification`):
  POST the payload **fire-and-forget**, exit 0 immediately. Must **not** block —
  these fire on every tool completion and the agent loop waits on the hook
  process; AO's handler reads the body, queues it, and returns at once.
- **Answer mode** (`PreToolUse` matcher `AskUserQuestion`): POST **and block**
  until AO returns the human's selection, then emit the answer (intended
  blocking — the spike confirmed Claude honors a generous hook `timeout`, no
  ~30 s cap, so a multi-minute approval window is available).

### Justified hook set

The surface is principled, not maximal — one relay binary, registered per event:

| Event | Why it's a hook (not wire) | Tier |
|---|---|---|
| `PreToolUse:AskUserQuestion` | only way to answer without a keystroke | required |
| `PostToolUse` | tool **success** completion, zero-lag `tool_response` | required |
| `PostToolUseFailure` | tool **failure** completion; *no* `tool_response`; fires *instead of* PostToolUse — register both or failed tools never complete | required |
| `SessionStart` | session identity (`session_id`/`transcript_path`/`cwd`) at launch; `session_id` is not on the wire | clean win |
| `PreCompact` / `PostCompact` | explicit compaction signal vs fragile wire-structural inference | clean win |
| `SubagentStop` / `Notification` | richer subagent metadata; "blocked/idle" advisory feeding the stall detector | enhancement (deferrable) |

### AskUserQuestion answer-back (mechanism)

The relay returns (confirmed in `probe_launchposture.py`):

```json
{ "hookSpecificOutput": {
    "hookEventName": "PreToolUse", "permissionDecision": "allow",
    "updatedInput": { "...the original tool_input, echoed INTACT...",
                      "answers": { "<question text>": "<chosen option label>" } } } }
```

Two load-bearing properties: echo the **full** `tool_input` (a partial
`updatedInput` makes the TUI re-prompt), and key answers by **question text**,
not position (the `reverse_answers` discriminator proved text-keyed consumption).
This surfaces as AO's existing `EventUserInputRequest` and resolves through the
same `UserInputResponse` path the headless provider uses — no new frontend
branch.

### Security: the relay is a privileged local boundary

- Loopback-only endpoint, authenticated by a **per-session capability token**
  minted at launch and handed to the hook via env. Reject any call without the
  exact token, and any call whose origin is a browser/LAN peer.
- The relay's App-bound methods → `internal/transport/internalmethods.go`
  `LocalOnlyMethods` (refused from non-loopback peers).
- Hook payloads and wire bodies are secret-bearing; nothing credential-bearing
  is logged. AO must never collect Claude.ai credentials nor expose them to
  `--connect` remote clients.

## Launch posture

Launch the user's **real** Claude Code with full-access flags and the relay
hooks. **[validate-at-build]** — confirmed working in `probe_launchposture.py`
against 2.1.170.

### Config: use the real `~/.claude` (strategy B)

Point Claude at the user's actual config directory — inherits native trust,
authentication (OAuth; **never** `--bare`, which forecloses OAuth), and
remembered acceptances. We do **not** manage an isolated config dir; we only
*add* the relay hooks, via the flag layer.

### Flags + env

```
ANTHROPIC_BASE_URL=http://127.0.0.1:<gateway-port>   # per session
AO_CLAUDE_HOOK_URL=http://127.0.0.1:<relay-port>     # per session
AO_CLAUDE_HOOK_TOKEN=<capability token>              # per session
claude \
  --permission-mode bypassPermissions \
  --allow-dangerously-skip-permissions \
  --settings '<JSON registering the relay command for the captured events>'
```

The `--settings` flag is the `flagSettings` layer, which **merges** with the
user's existing hooks rather than clobbering them — confirmed: a hook present
only in `--settings` fires even when the config-dir `settings.json` has none.

### Clean-launch pre-seeds

Two config keys gate full-access launch; if absent, Claude shows one-time
dialogs whose default rows are *decline*. When the user has used bypass mode
before, their real config already has them; the provider ensures both:

| Key | Effect |
|---|---|
| `hasTrustDialogAccepted` | suppresses the per-directory trust prompt |
| `bypassPermissionsModeAccepted` | suppresses the one-time bypass-mode Select |

Org killswitch: if `disableBypassPermissionsMode` is set, the binary refuses
full-access launch. Detect it and surface user-facing state ("this provider
needs full-access mode, which your organization has disabled"), don't crash.

### Why full-access collapses the approval surface

`bypassPermissions` deletes per-call permission gating entirely — there is no
`can_use_tool` round trip in the TUI. The only interactive gate that remains
*and that AO wants to drive programmatically* is AskUserQuestion, which is why
that is the one **blocking** hook.

## Signal recovery details

### Auxiliary-call filtering (wire)

Not every `/v1/messages` POST is a real agent turn. Classify and **drop** the
auxiliaries so they don't surface as phantom turns (the `is_agent()` predicate
from `probe_compact.py`, ported to Go):

| Call type | Discriminator | Action |
|---|---|---|
| Quota preflight | `max_tokens <= 1` | drop |
| Title / topic generation | `tools == []` | drop |
| Nested server-tool sub-call | tools all dated server tools (e.g. `web_search_20250305`) | fold into parent, don't surface |
| Suggestion-mode autocomplete | last user message starts with `[SUGGESTION MODE:` | drop |
| **Real agent turn** | populated `tools` **and** `max_tokens > 1` **and** not suggestion-mode | reconstruct → parser |

⚠ **Suggestion mode is the exception that breaks "tools + budget = real turn."**
Claude Code's next-message autocomplete fires a `/v1/messages` request carrying
the *full* tool set and `max_tokens`, so by tools/budget alone it is
indistinguishable from a main-loop turn — but its response is the model's
*prediction of what the user will type next*. Surfacing it renders a phantom
assistant turn (LIVE: "Do that again", "the IPC flow as a sequence diagram").
The only discriminator is the synthetic last user message, which opens with
`[SUGGESTION MODE: Suggest what the user might naturally type next into Claude
Code.]`. Matching the `[SUGGESTION MODE:` bracket prefix (the stable, structural
part) drops it. Found via the `AGENT_OVERFLOW_DEBUG=provider` classify log; the
gateway still forwards it upstream untouched so the TUI's ghost-text works.

### Turn boundaries (wire)

Per agent request, stream each SSE event as a `stream_event` envelope (live
deltas + the soft turn-complete that `parse_stream.go` already emits on
`message_delta.stop_reason ∈ {end_turn, stop_sequence, refusal}`). At response
end, assemble the complete `assistant` envelope (the only source of
`EventToolStart`) and feed it. When the final `stop_reason` is a "done" reason,
synthesize a `result` envelope (cumulative usage → priced turn-complete); on
`stop_reason: tool_use`, emit **no** result — the model is mid-turn and the next
request's tool_results continue it.

### Tool completion (hooks)

- `PostToolUse` → synthesize `{type:"user", message:{content:[{type:"tool_result",
  tool_use_id, content}]}, tool_use_result: <tool_response>}`. The
  `tool_use_result` sibling gives `parse_user.go` the rich enrichment
  (`exit_code`, `structuredPatch`, `task`) for free.
- `PostToolUseFailure` → same shape with `is_error:true` and `content` from
  `error`; `tool_response` is absent on failure.
- Background-Bash dispatch `PostToolUse` (carries `backgroundTaskId`) is the
  launch placeholder; the **completion** is the wire `<task-notification>`
  (see §Background completions).

### Background completions (wire)

A backgrounded command or agent (`Bash run_in_background:true`, async `Agent`)
finishes *after* its launching turn settled, so its completion can't ride that
turn's hooks. Headless learns of it from two CLI-internal stream-json events —
`system/task_updated` (host process exit) then `system/task_notification` (the
agent-facing summary + `output_file`). **Neither crosses `/v1/messages`.** The
only thing that does is the `<task-notification>` XML the CLI injects as a user
message into the *next* request body so the model can react to it
(`LocalShellTask.tsx` / `LocalAgentTask.tsx`).

So `turndriver.emitBackgroundCompletions` scans each agent request body and, for
every terminal `<task-notification>` it hasn't seen, reconstructs **both**
headless events, in order:

1. `system/task_updated` `{patch:{status}}` → `EventBackgroundTaskTerminal`.
   triage *stashes* this as the host-side exit — it does **not** write a chat
   row (invariant 21: `task_notification` is not a completion source, and a lone
   `task_updated{completed}` only stashes).
2. `system/task_notification` `{status, output_file, summary}` →
   `EventBackgroundTaskNotification`. triage *drains* the stash from step 1 and
   writes the `tool_completion` sibling at the current write head, reading
   `output_file` for the command output.

Feeding only the notification would never complete the tool: with no stashed
terminal there is nothing to drain. Both envelopes are `system` / string-`user`
lines that pass `feedReorder` through untouched, and the feed channel is FIFO, so
stash-before-drain is guaranteed.

Discriminators, all matching headless:

- **Terminal vs stall.** A completion carries `<status>` (`completed` / `failed`
  / `killed`); a stall ping (command blocked on input) omits it, and a statusless
  body is skipped — the task is still running. `NormalizeTaskTerminalStatus` is
  the shared gate.
- **Backgrounded vs inline.** Only backgrounded work injects a
  `<task-notification>`; a foreground tool returns its result inline as a
  tool_result in the same turn. An inline run therefore never produces a separate
  completion row — the desired UX.
- **Dedup by `task_id`.** The notification persists in conversation history and
  recurs in every later request body; a per-session seen-set reconstructs it once.

Field extraction reuses `claude.ExtractAllTaskNotificationFields`, so the tag
shape stays drift-free with the shared parser's synthetic-XML path.

### Compaction — transcript no longer required

The wire shows the structural signature (summarizer call at `max_tokens:20000`
vs the normal 32000, then `messages[]` collapse) and `preTokens` is the
`message_delta.usage` top-level the existing `advisor_pretokens_correlation`
fixture already correlates. `PostCompact` is the explicit "it completed" signal.
Synthesize a `system:compact_boundary` envelope so AO's existing
`compact_boundary` / `ItemCompaction` parser handles it. (The transcript carries
the richest typed `compactMetadata`, but only matters on cold resume.)

### Interrupt — **[validate-at-build]** on 2.1.170

No control-ack exists on the TUI path (the headless `interruptAcked` correlation
is unavailable). Re-derive from the taps and **synthesize** the shape
`parse_result.go` already classifies: a `result` with
`subtype:"error_during_execution"` + `errors:["...interrupted..."]` →
`detectInterrupted` returns true → `stop_reason:"interrupted"`. The trigger is
the wire/transcript synthetic `[Request interrupted by user for tool use]`
marker (mid-tool) or an agent request that closes without `message_stop`
(pre-output). This is the one signal whose 2.1.170 TUI behavior is not yet
confirmed; probe before the interrupt code ships.

### Errors

A mid-stream API error arrives as a wire `error` SSE event **after** HTTP 200
(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`).
Crucially, it is NOT all one user-facing `EventError` — it splits by whether
Claude Code's own `withRetry` (`withRetry.ts`) retries it:

- **Retryable — `overloaded_error` only.** `withRetry` retries a *status-less*
  mid-stream error ONLY when its message matches `overloaded_error` (its
  `shouldRetry` string-match; the SDK drops the 529 status mid-stream). Every
  other status-less mid-stream type falls through to "do not retry." So the
  reconstruction (`reconstruct.parseRetryableStreamError` → `agentRequest.end`)
  synthesizes the `system.api_retry` envelope headless emits for `overloaded_error`
  and nothing else, with a 1-indexed `attempt` counted per logical request
  (`reconstructor.consecutiveAPIFailures`, reset on any terminal stop_reason or
  interrupt — mirroring `withRetry`'s per-request loop). It flows through the
  shared parser's `EventAPIRetry` and triage's `apiRetryHideAttemptsBelow=4` gate,
  so the first three attempts are hidden exactly as in the real TUI. A failed
  attempt emits **no** assistant envelope (headless discards a failed attempt's
  partial output on retry) and does not settle the loop.
- **Terminal (auth / billing / invalid_request)** — surfaces through the
  existing `assistant.error` enum / `result{is_error}` path as a fatal
  `EventError`. These arrive pre-stream (non-200), not as a mid-stream frame.
- **Non-overload mid-stream error** — a non-`overloaded_error` `type:"error"`
  frame after 200 is NOT modeled as a retry (that would be a perpetual hidden
  `api_retry` that never settles). It falls through `onSSE`'s passthrough so any
  trailing terminal envelope still reaches the parser. Whether the API ever sheds
  such a frame, and whether a trailing terminal envelope follows it, is unprobed
  binary behavior — synthesizing a terminal `EventError` for this case is a
  spike-gated follow-on (build-probe #2). It is believed empty in practice
  (non-retryable errors arrive pre-stream as non-200).

The TUI's retry aborts its own gateway request, which cancels the inbound
context. `gateway.stream` treats a canceled-context read/write failure as the
client going away (retry, or user Esc) and does **not** surface it — the prior
`upstream read: context canceled` banner was the symptom that made the
2026-06-14 overload incident look like a gateway crash. A genuine upstream read
error (context still live) still surfaces with its real message.

Regression coverage: `reconstruct_apiretry_test.go` (attempt counting +
suppression + reset, tool_use-stop reset, partial-output discard, subagent ignore,
non-retryable fall-through, interrupt-vs-errored ordering) and
`gateway_stream_test.go` (abort suppression vs. real error). The two Blocking-fix
cases (non-retryable misclassification, interrupt phantom retry) are RED without
their fix.

## Driving input

Two operations, both via PTY `Write`. No widget driving beyond these — anything
else is the escape hatch.

- **Send(prompt):** type the prompt (bracketed-paste for multi-line /
  attachments), then submit. The spike found a **paste/enter coalescing**
  hazard: a `\r` sent in the same instant as the text is dropped. Type, wait for
  the composer to render, *then* send `\r` — key off observed composer render,
  not a blind sleep.
- **Interrupt:** send `Esc` (not `Ctrl-C`, which kills the process). Confirm the
  fingerprint per the interrupt section.

## Attach & take-control

The terminal drawer is the second reason this provider exists. It reuses
`internal/terminal` wholesale.

The provider spawns `claude` through a `terminal.Session` — PTY master, the
256 KiB ring buffer for replay, output fan-out, `Resize`, and `Refresh` for
free. The provider owns this session (distinct from user-opened shell
terminals).

Two writers exist for one PTY — the provider's input driver and the human —
arbitrated by an **input lease**:

| Mode | Lease holder | Frontend |
|---|---|---|
| Default | provider driver | renders the PTY **read-only** (xterm.js, replay + live) |
| Take-control | human | provider driver **paused**; keystrokes via `Session.Write` |

Handing control back re-asserts the provider lease; because the taps never
paused, AO reconciles and resumes cleanly.

- **Repaint on attach.** An Ink TUI in a freshly-attached xterm shows stale
  content until forced to redraw. Call `terminal.Session.Refresh` (the
  serialized shrink→pause→restore winsize nudge) on attach. Never call
  `Process.Resize` directly — it bypasses `resizeMu`.
- **Output fan-out gating.** The TUI repaints constantly; its PTY output is
  high-volume. The ring buffer always captures it (for replay), but **frontend
  fan-out is gated on attach state** — don't stream the live ANSI firehose to a
  closed drawer.
- **Stall detector + notify.** AO doesn't promise to handle every TUI prompt. It
  pre-seeds the common gates and routes everything else to take-control: when a
  turn is in flight but the taps show no expected progress for a threshold (or a
  `Notification` hook fires "blocked/idle"), AO emits "Claude needs you in the
  terminal" and offers take-control. Coarse de-ANSI'd prompt-marker matching is
  allowed only to **trigger** the notify, never as control input — AO never
  parses the TUI to decide answers, it just notices it's stuck and gets the
  human.

## Package structure & registration

New package `internal/provider/claudetui/`, implementing `provider.Session` and
emitting the existing `provider.ProviderEvent` kinds via the reused parser — **no
new event kinds, no provider-native types leaked out**.

```
internal/provider/claudetui/
  doc.go            one-line purpose
  AGENTS.md (+ CLAUDE.md symlink)
  session.go        provider.Session impl: lifecycle, owns PTY + gateway + relay
                    + the single claude.Parser and serialized feed; Send /
                    Interrupt / RespondToUserInput / PID / Close
  gateway.go        per-session loopback proxy; classify + stream /v1/messages SSE
  reconstruct.go    raw SSE → stream-json envelopes (stream_event passthrough,
                    assemble assistant, synthesize result / system:init /
                    rate_limit_event) — ao_transform.py logic, real-time
  hookrelay.go      loopback relay endpoint + capability token; hook payload →
                    envelope (PostToolUse* → user/tool_result; SessionStart →
                    system:init; Pre/PostCompact → compact_boundary;
                    AskUserQuestion → block + answer-back)
  launch.go         config pre-seeds, full-access flags, killswitch, --settings,
                    env (ANTHROPIC_BASE_URL / hook url+token)
  options.go        provider.SessionOptions → claude launch flags
  probe.go          binary discovery / version gate (reuse claude.Probe)
  attach.go         take-control lease, Refresh on attach, stall detector
  transcript.go     COLD-RESUME ONLY history read (deferrable past v1)
  *_test.go         reconstruct parity, hook-map, classify, launch-flag tests
```

The relay subcommand lives at the repo root alongside the other hidden
subcommands (`__reap`): `agent-overflow __claude-hook` reads stdin, posts to
`AO_CLAUDE_HOOK_URL` with `AO_CLAUDE_HOOK_TOKEN`, blocks only for AskUserQuestion.

Registration (the seam already anticipates a third provider —
`spawnProviderSession` comment, `app_session.go:298-301`):

1. `internal/provider/kinds.go`: add `ClaudeTUI ProviderKind = "claudetui"`.
2. `app_session.go` `spawnProviderSession`: add a `case
   string(provider.ClaudeTUI):` arm that builds the config and calls
   `claudetui.NewSession`, wiring the PTY (via `internal/terminal`), gateway
   listener, and relay — analogous to the Codex arm's handler wiring.
3. The `session` wrapper struct gains a `claudetui` typed pointer; its
   interface-dispatch sites pick it up via the shared `provider.Session`.
4. The relay's App-bound methods → `LocalOnlyMethods`.
5. `docs/references/claude.md` / `providers.md`: document the TUI path.

Provider-package discipline (unchanged): the package returns normalized
`ProviderEvent`s via `onEvent`; it does **not** write to the store or emit UI
events directly. Only triage persists; only `app.go`/transport emits.

## Out of scope — the escape hatch

The design principle: **AO does not try to perfectly handle every TUI
interaction.** It handles the turn loop and the signals it can reliably capture,
pre-seeds the common gates, and routes everything else to take-control. That
keeps the provider robust against TUI churn — when Claude Code changes a dialog
or adds a feature, AO notices the stall and hands the user the real terminal
rather than breaking.

Concretely out of scope, all handled by take-control + stall-notify: revert /
checkpoints; plan mode (`ExitPlanMode`); resume-at-a-prior-turn; MCP auth / OAuth
login; sensitive-path and other one-off dialogs (a sensitive-path edit raises a
native numbered dialog even when `PreToolUse` allows — relay the human's choice
as a PTY digit, never auto-confirm).

## Build-time probe list

Re-probe against the installed binary (current 2.1.170; pin and re-probe on
bump). Use the spike harness in `spike/claude-mitm/`.

1. **Interrupt classification on 2.1.170 (TUI path).** Confirm the synthetic
   `[Request interrupted by user for tool use]` marker (mid-tool) and the
   no-`message_stop` close (pre-output) reliably distinguish an interrupted turn,
   and that the synthesized `error_during_execution` result classifies correctly.
2. **API-error shape via inject-test.** Gateway upstream → a stub returning 429 /
   500 / overloaded. Confirm the split in §Errors: a mid-stream `overloaded_error`
   frame (after HTTP 200) reconstructs to `system.api_retry` (suppressed under
   attempt 4), and a terminal pre-stream error normalizes to `EventError`. The
   2026-06-14 incident confirmed the mid-stream/overload arm against a live
   `overloaded_error`; the synthetic inject-test still owes the 429/500 arms.
3. **End-to-end AskUserQuestion via the production relay** (not the canned-answer
   spike): loopback token round-trip, 0-keystroke answer-back.
4. **Tool-result fidelity:** confirm `PostToolUse.tool_response` reconstructs a
   `user` envelope that `parse_user.go` turns into the same `EventToolComplete`
   (content + `exit_code`/`structuredPatch`) the headless path produces.
5. **Clean full-access launch** against the real config (no stray acceptance
   dialogs) — re-confirm the two pre-seeds and the killswitch behavior.

## Risks & open questions

- **Version drift.** Every signal here is binary behavior, not a contract.
  Mitigation: pin the probed version, gate launch on a version check, keep the
  stall detector as the catch-all when a signal silently changes shape.
- **Reconstruction fidelity.** The wire→envelope and hook→envelope reconstruction
  must produce envelopes the shared parser accepts exactly. Mitigation:
  table-driven parity tests feeding reconstructed envelopes through the real
  `claude.Parser` and asserting the `ProviderEvent` output.
- **Feed serialization & ordering.** Wire and hook feeds hit one non-concurrent
  parser; a tool **start** (wire) must precede its **completion** (hook). The
  causal order holds (a tool can't complete before it starts), but the single
  feed channel must preserve it. Mitigation: one channel, one parser goroutine;
  ordering asserted in tests.
- **Hook spawn overhead.** `PostToolUse` spawns a relay process per tool
  completion. Acceptable (the user's own hooks already pay this; observe-mode is
  fire-and-forget), but noted.
- **Multi-session attribution.** Each session needs its own gateway port, relay
  port + token, and PTY. Everything keyed by AO thread ID at construction; no
  process-global state.

## References

- [`spike/claude-mitm/README.md`](../../spike/claude-mitm/README.md) and
  [`HOOKS_COVERAGE_MAP.md`](../../spike/claude-mitm/HOOKS_COVERAGE_MAP.md) — the
  investigation and the full per-signal coverage map (wire / hook / transcript).
- [`providers.md`](providers.md) — the existing two-provider process model.
- [`how-to.md#add-a-new-provider`](how-to.md#add-a-new-provider) — the
  new-provider playbook this follows.
- [`invariants.md`](invariants.md) — transport boundary, `LocalOnlyMethods`,
  dev-watcher exclusions.
- `internal/provider/claude/` — the headless provider whose parser this reuses.
- `internal/terminal/` — the PTY substrate for launch, attach, and take-control.
