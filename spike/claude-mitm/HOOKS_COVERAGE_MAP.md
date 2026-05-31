# Interactive Claude Code — Complete Coverage Map (Hook-Channel Architecture)

**Status:** authoritative as of 2026-05-30. Supersedes the TUI-driving parts of
`INTERACTIVE_DRIVING_SPEC.md` (see [§Relationship](#relationship-to-the-earlier-spec)).
**Binary under test:** `claude` **2.1.158** (installed), driven in a real PTY.
**Goal restated (corrected):** AO moves from headless `stream-json`
(`claude -p --output-format stream-json`, which bills API tokens) to the
**interactive TUI**, which runs on the already-paid Pro/Max subscription. This is
*not* a ToS workaround — headless is still permitted; the move is about cost and
about not depending on a channel Anthropic is squeezing. The axis that matters is
**coverage + reliability**: recover every capability AO uses today, through a
**non-TUI tap**, without parsing/driving the Ink React tree in the terminal.

---

## TL;DR

Three taps, together a **superset** of what `stream-json` gave AO — and one of
them (hooks) is a *structured control channel* nobody documented as such for this
use:

1. **Proxy wire** (`ANTHROPIC_BASE_URL` → local logging reverse proxy): token-level
   model output (SSE), under **subscription OAuth** (confirmed live on 2.1.158 —
   `Authorization: Bearer …`, never `x-api-key`). This is the streaming text/thinking
   AO renders.
2. **Hooks** (`settings.json` `hooks` in an isolated `CLAUDE_CONFIG_DIR`): the
   structured control + lifecycle + tool-I/O channel. **PreToolUse gates and even
   *answers* tools**; lifecycle events recover subagent start/stop; payloads carry
   session/tool/agent metadata. Runs in interactive mode.
3. **Transcript / checkpoint** (`~/.claude/projects/**/<session>.jsonl`): the durable,
   authoritative history for backfill, crash recovery, and the interrupt discriminator.

**Headline result:** every capability AO relies on in `stream-json` is recoverable
in interactive mode through these taps. The single biggest win is that the
**fragile Ink-widget driving the earlier spec was built around is eliminated**:

- **Per-call permissions** → `PreToolUse` hook returns `allow`/`deny`/`ask`. No TUI
  prompt, blocks up to the hook timeout — **LIVE to 70 s**, a real human-approval
  window. **Live-confirmed.** *One bounded exception:* edits to sensitive paths
  (`.zshrc`, `.git/`, `.claude/`, `.mcp.json`, …) are **bypass-immune** — they raise
  a native dialog even on hook-`allow`. AO either **routes the prompt to the human
  then relays their choice** as a PTY digit, or **hard-denies** at the relay — it
  **never auto-confirms** (a write to these = code execution). See the bypass-immune row.
- **AskUserQuestion** → `PreToolUse` reads the full question schema *and* a hook can
  **answer it** via `updatedInput.answers` with **zero keystrokes** — for **1–4
  questions in one call**, matched by **question text, not position** (proven with a
  reverse-order probe). The Select-widget digit/arrow driving is *gone*. **Live-confirmed.**
- **Plan mode** → `PreToolUse(ExitPlanMode)` captures the plan markdown and approves
  in-band. No shift+tab cycling. **Live-confirmed** (earlier probe).
- **Tool completion — success *and* failure** → `PostToolUse` fires **only on success**;
  a non-zero exit / MCP `isError` fires the **separate `PostToolUseFailure`** hook
  (carrying `error` = exit code + stderr, `duration_ms`, `tool_use_id`). **AO must
  register both** or it never sees failed tools finish. **Live-confirmed.**
- **MCP tools** → surface through the same hooks under a **`mcp__`-prefixed** `tool_name`:
  gateable per call, success→`PostToolUse`, error→`PostToolUseFailure`. Transport-agnostic
  at the hook layer (local stdio == remote). **Name shape varies by source** — user-scope
  server `mcp__<server>__<tool>`, plugin server `mcp__plugin_<plugin>_<server>__<tool>`
  (e.g. context7) — so AO must match the `mcp__` prefix, not assume a bare server segment.
  **Live-confirmed.**
- **Background tasks / subagents** → subagent completion via the **`SubagentStop`** hook
  (`agent_id`, `agent_transcript_path`, `last_assistant_message`); backgrounded-Bash
  completion via the wire **`<task-notification>`** (correlates to the dispatch
  `backgroundTaskId`; carries status + exit code + output-file). **No hook fires at
  bg-Bash completion** — by design; recovered on the wire. `stop_task` maps to the
  in-band `TaskStop`/`KillShell` tool. **Live-confirmed** (daemon analysis source+binary).
- **Interrupt** → wire (aborted SSE, no `message_stop`) for streaming — partial text
  that already streamed is recoverable; transcript marker `[Request interrupted by user
  for tool use]` for mid-tool, where **neither `PostToolUse` nor `PostToolUseFailure`
  fires**. **Live-confirmed.**
- **Turn lifecycle (revert & steering)** → a **think-only Esc-revert is wire-detectable**
  (it aborts `/v1/messages` with the *same* no-`message_stop` + `upstream_read`-cancel
  signature as an interrupt, differing only in thinking-only content), and the reverted
  prompt is **DROPPED** from the next turn's context — not re-sent — leaving an orphaned
  transcript `user` row AO filters on backfill. **Mid-turn steering** is captured off the
  hook channel: transcript `queue-operation` (`enqueue`→`remove`) + a wire `system`
  message; **`UserPromptSubmit` does NOT fire**. Net: **no AO-relevant behavior is
  TUI-scrape-only.** **Live-confirmed** (reproduced ×2).

**The make-or-break design rules** fall out of the probes (details in
[§Critical rules](#critical-design-rules-the-make-or-break-findings)); the two that
silently break things if gotten wrong:

- **A gate hook must own its own deadline and always return an explicit decision.**
  A `PreToolUse` hook that is *killed at its timeout* doesn't force-execute — it
  **reverts to Claude's normal permission flow** (live-confirmed). In AO's gating
  posture (default mode, clean env) that drops the tool onto the **native TUI prompt
  AO can't answer → the turn gets stuck** — but *safely*: **nothing ran**, and AO sees
  no wire/hook/transcript progress, so it can detect the stall and Esc it. Only a
  permissive posture (which AO avoids by config) would **run the tool unreviewed**.
  Either way the relay must return an explicit `deny` before Claude's timeout fires —
  never let AO's gate hang until Claude kills it.
- **The workspace must be trusted before hooks fire.** Untrusted workspace → *all
  hooks silently skipped* → the gate is **bypassed entirely** (the tool falls through
  to normal permissions and AO never sees the request). AO must pre-seed/accept trust.

---

## Confidence legend

| Tag | Meaning |
|-----|---------|
| **LIVE** | Observed directly against the 2.1.158 binary in this spike (probe named). |
| **BIN** | String/symbol confirmed present in the 2.1.158 binary. |
| **SRC** | From the local source checkout (`~/repos/claude-code-source-code`, **v2.1.88** — ~70 versions stale); cross-checked against BIN where a signature exists. |
| **DOC** | Official docs (code.claude.com/docs). |
| **ARTIFACT** | Captured in this spike's `artifacts/` (e.g. `ao-interrupt-marks.json`). |

Where a claim is load-bearing and only **SRC**, it is flagged inline.

---

## Coverage map (one row per capability AO uses in stream-json)

| Capability (stream-json) | Interactive recovery | Channel | Confidence | Notes |
|---|---|---|---|---|
| Token-level assistant text / thinking | SSE on `/v1/messages` via proxy | **wire** | **LIVE** | OAuth Bearer, no `x-api-key`; `--include-partial-messages` equivalent is the raw SSE. |
| Per-call tool permission (`can_use_tool`) | `PreToolUse` → `permissionDecision: allow\|deny\|ask` | **hook** | **LIVE** (`probe_hook_permission.py`) | Blocks up to hook timeout; `allow` suppresses the TUI prompt entirely — **except** the bypass-immune set (next row). |
| **Sensitive-path edits — bypass-immune** | Edits whose path hits `DANGEROUS_FILES` (`.gitconfig .gitmodules .bashrc .bash_profile .zshrc .zprofile .profile .ripgreprc .mcp.json .claude.json`) or `DANGEROUS_DIRECTORIES` (`.git .vscode .idea .claude`) raise a **native numbered dialog** (`1.yes / 2.yes-all-session / 3.no`) **even when `PreToolUse` returns `allow`** | **hook + PTY** | **LIVE** (`probe_hook_dangerpath.py`, `probe_hook_dangerdrive.py`) | Hook can't suppress these. **Decided handling: route (a) — route-to-human, relay 1/3.** AO detects the stall (`PreToolUse` `allow` fired, no `PostToolUse`, dialog markers) and takes one route: **(a)** route the prompt to the human, then relay their choice as a PTY digit — `1` (proceed) is **validated** (`probe_hook_dangerdrive.py`); `3` (cancel) is the standard cancel affordance, not separately probed; **never auto-confirm, never `2`** (session-wide grant cascade) — these paths are immune *because* a write = code execution (`.zshrc`→next shell, `.mcp.json`/`.claude.json`→arbitrary MCP/hook command, `.git/`→git hooks); or **(b)** hard-deny at the relay. Probed via a `DANGEROUS_FILES` basename (`.zshrc`); the `DANGEROUS_DIRECTORIES` segment match is same-mechanism but SRC-only. Case-insensitive. The bg write stalls (no `PostToolUse`) until answered. |
| Structured questions (`AskUserQuestion`) | `PreToolUse` reads `tool_input.questions`; hook **answers** via `updatedInput.answers` (+`allow`) | **hook** | **LIVE** (`probe_hook_answer.py`, `probe_hook_multiq.py`) | 0 keystrokes; model gets a normal `tool_result`. **1–4 questions per call**, matched by **question text, not position**. **Eliminates Select-widget driving.** |
| **Tool completion + result (success)** | `PostToolUse` (success only) → `tool_response`, `duration_ms`, `tool_use_id` | **hook** | **LIVE** (`probe_hook_failcomplete.py` control) | Fires on exit 0; bg-Bash fires it at *dispatch* (carries `backgroundTaskId`, empty stdout). |
| **Tool completion (failure / non-zero exit / MCP `isError`)** | **`PostToolUseFailure`** → `error` (exit code + stderr), `is_interrupt`, `duration_ms`, `tool_use_id`, `tool_input` | **hook** | **LIVE** (`probe_hook_failcomplete.py`) | **Distinct event from PostToolUse — AO MUST register both** or it never sees failed tools finish. Wire `tool_result` (`is_error`) is the backup. |
| MCP tool calls + results | Same hooks; `mcp__`-prefixed `tool_name` (user-scope `mcp__<server>__<tool>`, plugin `mcp__plugin_<plugin>_<server>__<tool>`); per-call gate; success→`PostToolUse`, error→`PostToolUseFailure` | **hook** | **LIVE** (`probe_hook_mcp.py`) | Mechanics proven on local stdio (`mcp__aoprobe__ping`); plugin name shape (context7) confirmed from the live tool list. Match the `mcp__` prefix. |
| Plan approval (`ExitPlanMode`) | `PreToolUse(ExitPlanMode)` captures `tool_input.plan`; approve = `allow`, reject = `deny` | **hook** | **LIVE** (`probe_planmode*.py`) | No shift+tab. Launch with `--permission-mode plan`. |
| Subagent lifecycle (`task_started`/`parent_tool_use_id`) | `PreToolUse`/`PostToolUse` on the subagent-launch tool (named **`Agent`** in hooks, not `Task`) + inner calls tagged `agent_id`/`agent_type` + `SubagentStop` | **hook** | **LIVE** (`probe_hook_coverage.py`, `probe_hook_bgcomplete.py`) + SRC/BIN | `SubagentStop` carries `agent_id`, `agent_transcript_path`, `last_assistant_message`; fires even backgrounded (can fire >1×). |
| Background Bash lifecycle (`task_updated`/`task_notification`) | dispatch `PostToolUse(Bash)` carries `backgroundTaskId`; completion = `<task-notification>` **user message** in the next `/v1/messages` body (same `task-id` + status + exit code + output-file) | **wire** + hook (dispatch) | **LIVE** (`probe_hook_bgcomplete.py`) | **No hook fires at bg-Bash completion** (full timeline, both post-hooks registered) — recovered on the wire; correlate `backgroundTaskId`↔`task-id`. Must drive a follow-up turn to flush it. Mid-run progress: tail the output file. |
| Kill one background task (`stop_task`) | **(a)** in-band `KillShell` tool (model-mediated, inject a prompt); **(b)** direct **OS kill** — bind `backgroundTaskId→PID` by snapshot-diffing `claude`'s descendants across the `PostToolUse(Bash)` dispatch, then `SIGTERM` the subtree | **wire (tool)** / **OS** | (a) BIN+SRC; (b) **LIVE** (`probe_hook_bashpid.py`) | SDK `stop_task` is `--print`-only, gone in interactive. The bg process **is** a live descendant of `claude` (not the daemon), so the id→PID map exists; double-fork daemons + non-Linux enumeration need care. |
| Interrupt — model streaming | Aborted SSE: no `message_delta` stop_reason, **no `message_stop`**, upstream read cancels | **wire** | **ARTIFACT** (`ao-interrupt-marks.json`) | The turn-finalization rule: absence of `message_stop` + upstream cancel = interrupted. |
| Interrupt — during tool execution | Synthetic user msg `[Request interrupted by user for tool use]` (the **discriminator**) + `is_error` tool_result = `REJECT_MESSAGE` | **transcript/wire** | **LIVE** (`probe_hook_interrupt.py`) | **Neither `PostToolUse` nor `PostToolUseFailure` fires** (verified with both registered). tool_result is byte-identical to a permission-deny — only the user marker distinguishes them. |
| Image / attachment input (base64 blocks) | Write temp file → bracketed-paste the path (`\x1b[200~/abs/x.png\x1b[201~`); or OS clipboard + Ctrl+V | **PTY input** | **LIVE** (`probe_hook_attach.py`) | Yields a real `image` block (`source.type=base64`) in the transcript — exactly AO's current input. Regex accepts `.png/.jpg/.jpeg/.gif/.webp`. No PTY path accepts raw base64 — must go via file or clipboard. |
| Set permission mode mid-session | Launch flag `--permission-mode default\|acceptEdits\|plan\|bypassPermissions\|dontAsk\|auto` (full set); or hook-driven posture | **flag/hook** | BIN/DOC | The relay **is** the posture: launch once in `default`, the hook decides every call; "mode change" = the relay returning different decisions, no keystroke/relaunch. Per-call override regardless of mode — **except** bypass-immune sensitive-path edits (see that row). Only `plan` (and possibly `auto`) is behaviorally distinct. |
| Durable history / crash recovery | `~/.claude/projects/<enc-cwd>/<session>.jsonl` (relocated by `CLAUDE_CONFIG_DIR`) | **transcript** | SRC/BIN + LIVE | Authoritative; AO already has its own git-ref `internal/checkpoint/` for files. |
| Session id / cwd / permission_mode / effort | Present on **every** hook payload | **hook** | **LIVE** | `session_id`, `transcript_path`, `cwd`, `permission_mode`, plus `effort` on tool events. |

The rest of this document is the detail behind each row, the hook contract, the
critical rules, the open items, and the AO integration sketch.

---

## Architecture: the three taps

AO launches a real interactive `claude` in a PTY with three environment knobs and
one settings file:

```
CLAUDE_CONFIG_DIR=<isolated per-AO dir>     # relocates the whole .claude tree
ANTHROPIC_BASE_URL=http://127.0.0.1:<port>  # → AO's local logging reverse proxy
TERM=xterm-256color
# <CLAUDE_CONFIG_DIR>/settings.json carries AO's hooks
# <CLAUDE_CONFIG_DIR>/.credentials.json + .claude.json seed OAuth + trust
```

### Tap 1 — Proxy wire (token-level output, under subscription OAuth)

A stdlib Go reverse proxy (`proxy/main.go`) sits at `ANTHROPIC_BASE_URL` and
forwards to `https://api.anthropic.com`, logging request/response. It does **no TLS
interception** — Claude speaks plain HTTP to loopback, the proxy speaks TLS upstream.

> **Production caveat (FINDINGS §12):** "speaks TLS upstream" is where the proxy's
> own runtime fingerprint leaks. The Go proxy presents a Go/OpenSSL JA3 — a mismatch
> against a genuine claude OAuth session. For production, the **upstream leg must be a
> version-pinned Bun `fetch`** (`Bun/1.3.14`), which reproduces claude's TLS
> ClientHello + HTTP/1.1 header block byte-for-byte. `proxy/main.go` is fine for
> signal capture (this spike) but is **fingerprint-non-preserving**.

- **OAuth survives the custom base URL. LIVE-confirmed on 2.1.158:** across a probe
  run, **38 × `200` on `POST /v1/messages`**, every request carrying
  `Authorization: Bearer …` (len ≈ 115) and **no `x-api-key`**. So driving through
  the proxy does not silently fall back to API-key billing — it stays on the
  subscription. (`apiKeySource: none` is the corresponding internal signal.)
- This is where AO gets streaming assistant text and thinking (the SSE
  `content_block_delta` stream), i.e. the `--include-partial-messages` equivalent.
- **The non-200s are all benign and accounted for** (verified by correlating each
  request's path+body with its response status in the capture — not assumed):
  - **12 × `404` on `POST /v1/messages`** are each a fixed **317-byte quota probe**
    (`{"model":…,"max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`) Claude
    Code fires once per session/turn. The proxy forwards it on the **same path** it
    serves 38 × `200` on, with **no proxy error logged** — so the `404` is **upstream's
    response to that specific probe, forwarded faithfully**, not a proxy fault. Claude
    **tolerates it**: every one is immediately followed by the real `~2 KB → 200` +
    `~95 KB → 200` turn traffic. Not main-flow; nothing to handle.
  - **1 × `400`** on a real 111 KB streaming turn was **auto-retried by the Stainless
    SDK** (`X-Stainless-Retry-Count` header present) and the near-identical retry
    **through the same proxy succeeded (`200`)** — a transient upstream blip, not proxy
    corruption (a mangling proxy would fail the retry too). 1 in 53 real turns,
    self-recovered.
  - `HEAD`/`GET` `404` on `/` are connectivity probes; `context canceled` proxy errors
    are normal mid-stream teardown (interrupt / session exit). None affect message flow.

**Multi-session attribution (design note, not yet built):** one shared proxy port
can't cleanly attribute concurrent threads. The intended design is a **per-session
ephemeral loopback listener** — AO mints a `127.0.0.1:0` listener per `claude`
process, so each thread's wire is isolated by construction. This is an AO-side
implementation detail; the proxy itself is already per-process-agnostic.

### Tap 2 — Hooks (the structured control channel)

`settings.json` `hooks` map event names → matchers → command hooks. Each hook is a
process AO controls; Claude pipes a JSON payload to its stdin and reads a JSON
decision from stdout. This is the channel that recovers everything `can_use_tool`
and the SDK control protocol gave AO — and more (lifecycle, session metadata). Full
contract below.

In the spike, `hook_relay.py` is that process: it logs every payload to
`payloads.jsonl` (so we can see the exact shapes), optionally blocks, and emits a
decision read from a control file. In AO, the equivalent relay forwards the payload
over AO's existing transport to the Go side and blocks on the human/UI decision.

### Tap 3 — Transcript / checkpoint (durable backfill + the interrupt discriminator)

`<CLAUDE_CONFIG_DIR>/projects/<encoded-cwd>/<session>.jsonl` is the authoritative,
append-only history (assistant/user messages, tool_use/tool_result, thinking,
synthetic markers). Every hook payload hands AO the `transcript_path`, so AO always
knows which file backs the live thread. Uses:

- **Backfill** anything the wire didn't carry structurally (e.g. the exact
  tool_result text, the interrupt marker).
- **Crash recovery / resume** — the transcript is the source of truth per the repo's
  Core Principle 2; AO's own git-ref `internal/checkpoint/` already covers file
  state, so Claude's `/rewind` is largely redundant for AO.

---

## The hook contract (2.1.158)

> Provenance: the event list and field schemas below are drawn from the dedicated
> hook-contract investigation (binary strings + v2.1.88 source + docs) and
> **independently live-confirmed** for the events AO depends on
> (`PreToolUse`, `PostToolUse`, **`PostToolUseFailure`**, `UserPromptSubmit`,
> `SessionStart`, `SessionEnd`, `Stop`, `SubagentStop`, `Notification`) via the probes.
> Items only seen in source are tagged **SRC**.

### Events

The canonical `HOOK_EVENTS` array is present **verbatim in the 2.1.158 binary** (BIN),
30 entries:

```
PreToolUse, PostToolUse, PostToolUseFailure, PostToolBatch, Notification,
UserPromptSubmit, UserPromptExpansion, SessionStart, SessionEnd, Stop,
StopFailure, SubagentStart, SubagentStop, PreCompact, PostCompact,
PermissionRequest, PermissionDenied, Setup, TeammateIdle, TaskCreated,
TaskCompleted, Elicitation, ElicitationResult, ConfigChange, WorktreeCreate,
WorktreeRemove, InstructionsLoaded, CwdChanged, FileChanged, MessageDisplay
```

The **documented, user-configurable** subset (what AO should rely on) is the smaller
list: `PreToolUse, PostToolUse, Notification, UserPromptSubmit, Stop, SubagentStop,
SessionStart, SessionEnd, PreCompact` — **plus `PostToolUseFailure`**, which is not in
the public-docs subset but is **BIN + LIVE-confirmed** and **load-bearing**: it is the
*only* completion signal for a failed tool (see **Rule 6**).

**A caution on the other ~20 entries (advisor-flagged).** Several names that look useful
— `PermissionRequest`, `PermissionDenied`, `Elicitation`, `SubagentStart`, `Setup`,
`FileChanged` — were seen in the source's `hookSpecificOutput` *return* union, i.e. shapes
a hook **may return**, which is **not** the same as confirmed **input events that fire**
on 2.1.158. Treat them as **SRC-only / unverified-firing**; AO must not depend on them
without a live probe. (`TaskCreated`/`TaskCompleted` are the **todo/plan list** system —
they do **not** fire for `run_in_background` Bash or subagents. SRC.)

### Common stdin envelope (all events)

```jsonc
{
  "session_id": "…",
  "transcript_path": "/…/<session>.jsonl",
  "cwd": "/abs/cwd",
  "hook_event_name": "PreToolUse",
  "permission_mode": "default|acceptEdits|plan|bypassPermissions"  // when applicable
}
```
Tool events add `tool_name`, `tool_input`, `tool_use_id` (+ `effort`); subagent-inner
events add `agent_id`, `agent_type`. **PostToolUse** (success) adds `tool_response`,
`duration_ms`; **PostToolUseFailure** (failure) adds `error` (e.g. `"Exit code 3\n<stderr>"`),
`is_interrupt`, `duration_ms` — and notably **no `tool_response`**. (All LIVE-observed in
the probe payloads.)

### Exit codes

- **0** — success; stdout parsed as hook JSON (or surfaced as context).
- **2** — **blocking error**; stderr fed to the model. For PreToolUse this is a hard
  deny (equivalent to `permissionDecision: "deny"`).
- **other non-zero** — non-blocking error: logged, does **not** block. A hook
  **killed at its timeout (SIGTERM/143) lands here → control reverts to the normal
  permission flow** (it does *not* independently execute the tool — see Rule 1).

### stdout JSON (the parts AO uses)

Top-level (all optional): `continue`, `stopReason`, `suppressOutput`,
`decision: "approve"|"block"` (legacy), `reason`, `systemMessage`, and the
discriminated `hookSpecificOutput`:

| Event | `hookSpecificOutput` fields | AO use |
|---|---|---|
| **PreToolUse** | `permissionDecision: allow\|deny\|ask`, `permissionDecisionReason`, **`updatedInput`**, `additionalContext` | Gate every tool; **answer AskUserQuestion** by echoing `tool_input` + `answers` into `updatedInput` with `allow`. |
| **PostToolUse** | `additionalContext`, `updatedMCPToolOutput` | Observe completed (successful) tool I/O; inject context. |
| **PostToolUseFailure** | observe-only; payload carries `error`, `is_interrupt`, `duration_ms`, `tool_use_id`, `tool_input` | **Failure completion** — fires *instead of* PostToolUse on non-zero exit / MCP `isError`. **Register it or AO never sees failed tools finish** (Rule 6). |
| **UserPromptSubmit** | `additionalContext` (or `decision:"block"`+`reason` / exit 2 to reject) | Observe/annotate submitted prompts. |
| **SessionStart** | `additionalContext`, `initialUserMessage`, `watchPaths` | Session bootstrap. |
| **Stop / SubagentStop** | top-level `continue` / `decision:"block"`+`reason` to force continuation | Lifecycle; `SubagentStop` carries `agent_id`, `agent_transcript_path`, `last_assistant_message`. |
| **Notification** | matcher on `notification_type`; payload adds `message` | Advisory "Claude is blocked/idle" signal. `permission_prompt` value is **BIN** (newer than source). Authoritative gate is still PreToolUse. |

**Matcher key per event (SRC):** Pre/PostToolUse → tool name (regex); Notification →
`notification_type`; SessionStart → `source`; SessionEnd → `reason`; SubagentStop →
`agent_type`; PreCompact/PostCompact → `trigger`.

### Timeout / blocking semantics

- **Default 600 s (10 min).** Per-hook override via the `timeout` field, **in
  seconds**, **no upper bound / no clamp** (BIN/SRC). A gate hook can therefore hold a
  tool open while a human decides — **LIVE-confirmed to 70 s** under a configured
  `timeout: 120` (`probe_hook_longtimeout.py`: the hook slept 70 s, was **not** killed,
  and its held `allow` then applied — the tool ran). This directly answers the
  human-in-the-loop worry: **the configured `timeout` is honored; nothing imposes a
  ~30 s cap**, so a multi-minute approval window is available — set `timeout` generously
  (the relay still imposes its *own* shorter deadline per Rule 1). 70 s is the observed
  floor; multi-minute follows from the no-clamp. (Earlier `probe_hook_failopen.py` sanity
  showed 6 s.)
- **The gating hook must be synchronous.** `async:true`/`asyncRewake` hooks return
  immediately and run detached — the tool proceeds. **Do not configure AO's gate as
  async.** (SRC.)
- `defer` is print-mode-only and ignored in interactive (BIN string confirms).

### Settings injection + isolation

- Hooks live in merged settings (`user → project → local → flag → policy`); inline
  `--settings '{"hooks":{…}}'` are applied (the `flagSettings` layer). Only an
  enterprise **policy** (`disableAllHooks`, `allowManagedHooksOnly`, plugin-only
  restriction) can suppress them. (SRC, with the `--settings`→`flagSettings` parse
  step inferred — trivially confirmable.)
- **`CLAUDE_CONFIG_DIR` relocates the *entire* `.claude` tree** (credentials, projects/
  sessions, settings, CLAUDE.md, keybindings, plugins) — 150+ call sites (SRC). AO
  must seed `.credentials.json` (OAuth) **and** project trust into the isolated dir,
  or the session starts unauthenticated/untrusted. The spike harness
  (`aoprobe.seed_config`) does exactly this: copies real creds + `.claude.json`, and
  pre-sets `projects[cwd].hasTrustDialogAccepted = true`.

---

## Critical design rules (the make-or-break findings)

These are the things that, if AO gets them wrong, silently break the gate — or, for
Rule 6, silently lose completion signals. Each is backed by a live probe.

### Rule 1 — The gate hook must own its deadline and ALWAYS return a decision

**A `PreToolUse` hook that is killed at its timeout does NOT independently execute
the tool — control reverts to Claude's *normal* permission flow.** The hook simply
stops gating; whatever normal flow would do then happens. The rule matters because
*which* fall-through branch AO hits depends on its permission posture — a stuck native
prompt or an unreviewed run (below) — and the relay must never let it get there.

Evidence — `probe_hook_failopen.py`, a 2×2 matrix in **default permission mode**,
everything held constant except (a) whether a gate hook exists and (b) whether the
command is gated by an explicit `ask` rule. The `ask` rule is deterministic ground
truth for "needs approval," so the no-hook **control** proves the harness can force a
real prompt, which makes the **decider** trustworthy:

| Case | gate hook | command gated by | native prompt? | tool ran? | meaning |
|---|---|---|---|---|---|
| baseline | none | (none — `echo>file`) | **yes** | no | true default mode gates even a plain write |
| control | none | `ask: Bash(rm:*)` | **yes** | no | ask rule forces a prompt with no hook (litmus ✅) |
| **decider** | **killed @ 2 s** | `ask: Bash(rm:*)` | **yes** | **no** | **killed hook → falls through to the native prompt** |
| sanity | survives (held **6 s**) | `ask: Bash(rm:*)` | no | no | a *returned* `deny` blocks — and outranks the ask rule |

The decider is the load-bearing cell: when the gate hook was killed, `rm` hit the
**native TUI permission prompt** (from the `ask` rule), exactly as the no-hook control
did — it did **not** force-execute (`rm` target intact, no `PostToolUse`). This
matches the v2.1.88 source prediction (fall-through to `canUseTool`).

> **Correction (2026-05-30):** an earlier probe (`probe_hook_timeout.py`, now removed)
> reported "fail-open-to-**execute**." That was an artifact of two harness bugs: the
> isolated config silently inherited the user's auto-accept posture (carried
> `CLAUDE*`/auto opt-in) so *every* command auto-ran, and the teardown sent **Enter**,
> which selected the default "Yes" on the still-open prompt. With a clean child env and
> an Esc-not-Enter teardown, the true mechanism is fall-through-to-prompt, above.

**Why it's still make-or-break — but the two outcomes are asymmetric, and AO's
posture picks which one:**
- In AO's intended posture (default mode, clean env, the hook *is* the gate) a killed
  hook drops the tool onto the **native TUI prompt AO cannot answer over its non-TUI
  channel → the turn degrades to STUCK.** This is the *recoverable* branch: **nothing
  ran** (no side effects), and AO sees no wire/hook/transcript progress, so it can
  detect the hang and **Esc-interrupt** the turn.
- In a permissive posture (an `allow` rule, `acceptEdits`/`bypassPermissions`, or an
  inherited auto-accept opt-in) the same fall-through **runs the tool unreviewed** —
  the genuinely dangerous branch. AO forecloses it by construction: default mode + a
  clean env (below).

So in AO's correct configuration the worst case is **detect-and-interrupt, not silent
side effects**. The relay-owns-its-deadline rule still makes both moot — a relay that
always returns an explicit `deny` in time never reaches fall-through at all.

**AO design (unchanged in substance, now correctly motivated):**
- Configure a **generous `timeout`** (no clamp) and have the **relay impose its own
  deadline** comfortably under it. If the human hasn't decided in time, **the relay
  returns an explicit `deny`** (fail-closed) rather than hanging until Claude kills it —
  a killed hook leaves the turn stuck or the tool unreviewed, never safely blocked.
- A **returned** decision is authoritative: a `deny` blocks even a command an `ask`
  rule would otherwise prompt for (sanity case). The relay stays in control as long as
  it answers in time.
- Never configure the gate hook as `async`.
- **Spawn `claude` with a clean, curated environment** — do not pass through ambient
  `CLAUDE*`/`CLAUDECODE` vars or a carried auto-accept opt-in, and set
  `--permission-mode default` explicitly. With a clean env, default mode genuinely
  prompts for tool calls (baseline case); inherit an auto-accept posture and the gate
  is moot. (This bit the spike harness — see the Correction above.)
- Blocking while a human decides is supported — **LIVE to 70 s** under a configured
  `timeout: 120` (`probe_hook_longtimeout.py`); the configured timeout is honored, so a
  multi-minute approval window is real (no ~30 s cap is imposed). The relay's *own*
  deadline, not Claude's, is what should end the wait.

### Rule 2 — The workspace must be trusted before hooks fire

If the workspace **trust dialog has not been accepted**, **all hooks return `[]` and
are silently skipped** (`shouldSkipHookDueToTrust`, SRC) — the PreToolUse gate never
runs and **AO never sees the request**; the tool falls through to normal permissions
(native prompt → stuck, or run if permissive). For a fresh workspace driven in a PTY
this is a second way the gate silently stops gating.

**AO design:** pre-seed `projects[<cwd>].hasTrustDialogAccepted = true` in the isolated
`CLAUDE_CONFIG_DIR`'s `.claude.json` (the spike does this), or accept the trust prompt
before relying on the gate. AO already manages workspaces, so it owns this.

### Rule 3 — Interrupt: the tool_result CANNOT discriminate interrupt from deny

On a mid-tool interrupt, the synthetic `tool_result` is **byte-identical** to a normal
permission rejection. Both are `is_error: true` with `REJECT_MESSAGE`:

> "The user doesn't want to proceed with this tool use. The tool use was rejected
> (eg. if it was a file edit, the new_string was NOT written to the file). STOP what
> you are doing and wait for the user to tell you how to proceed. …"

**The only discriminator is a separate synthetic *user* message:
`[Request interrupted by user for tool use]`** (BIN + LIVE). And **neither `PostToolUse`
nor `PostToolUseFailure` fires** on an interrupted tool — verified live with **both**
registered (0 of each for the killed tool). An interrupt is therefore distinct from a
*failed completion* (which **does** fire `PostToolUseFailure` — Rule 6): the tool was
killed before producing any result, so there is no completion event on any hook at all.

**AO design:** detect interrupt-during-tool from the **transcript marker**, not the
tool_result content and not any Post\* hook. (Streaming-phase interrupt is detected on the
wire — see [§Interrupt detail](#interrupt-both-cases).)

### Rule 4 — Choose the launch permission mode deliberately

- `default` — the hook gate is the per-call decision; a *returned* `deny` blocks and
  **outranks** a static `ask` rule (sanity case). A *killed* hook reverts to the native
  permission flow (Rule 1).
- `bypassPermissions` — auto-executes everything; the hook can still observe/answer but
  the gate is moot, and the mode shows a startup acknowledgment screen. Not appropriate
  when the hook is meant to be AO's gate.
- `acceptEdits` / `plan` — situational; `plan` is how AO drives plan capture
  (`--permission-mode plan`).
- **Env matters as much as the flag:** a child that inherits an ambient auto-accept
  posture (parent `CLAUDE*`/`CLAUDECODE` vars or a carried auto opt-in) auto-runs tools
  even in nominal `default` mode. AO must spawn `claude` with a clean, curated env.

Recommended: launch in **`default`** with a **clean env**, and let the relay be the
gate, with Rule 1's self-imposed deadline so a relay timeout degrades to a *returned
deny* (block) — not a stuck native prompt or an unreviewed execution.

### Rule 5 (incidental) — 2.1.158 refuses foreground `sleep`

Discovered while building the interrupt probe: the binary blocks foreground `sleep`
commands with `<tool_use_error>Blocked: sleep … Foreground sleep is blocked; use
run_in_background: true` and redirects to the `Monitor` tool. Not an AO blocker, but it
changes model behavior around delays and explains why the first interrupt probe (which
used `sleep`) produced no tool to interrupt. The probe now uses a blocking FIFO read.

### Rule 6 — Tool completion: `PostToolUse` is SUCCESS-only; register `PostToolUseFailure` too

**`PostToolUse` fires only when a tool *succeeds*.** A tool that completes
*unsuccessfully* — a non-zero exit, or an MCP tool returning `isError` — fires the
**separate `PostToolUseFailure`** event instead. If AO registers only `PostToolUse`,
**every failed tool looks like it never finished** → the thread hangs waiting for a
completion that will never arrive on that channel. (This bit the spike: a first
failure-probe run saw *no* PostToolUse and looked like a bug, until the source revealed
the distinct event.)

Evidence — `probe_hook_failcomplete.py` (LIVE): a foreground `sh -c 'echo … >&2; exit 3'`
fired **`PostToolUseFailure` and not `PostToolUse`**, payload:

```jsonc
{ "hook_event_name": "PostToolUseFailure", "tool_name": "Bash",
  "tool_use_id": "toolu_…", "duration_ms": 95, "is_interrupt": false,
  "error": "Exit code 3\nOOPS-STDERR-7f3a",      // exit code + stderr, combined; NO tool_response
  "tool_input": { "command": "sh -c 'echo OOPS-STDERR-7f3a >&2; exit 3'", … } }
```

An exit-0 control fired `PostToolUse` (success) as expected. `probe_hook_mcp.py` confirms
the **same split for MCP tools**: a successful `ping` → `PostToolUse`; `ping` returning
`isError` → `PostToolUseFailure`. The failure info is *also* recoverable on the wire (the
next request's `tool_result` carries `is_error: true` + stderr) as a backup, but the hook
is the structured, in-band signal.

**AO design:** register **both** `PostToolUse` *and* `PostToolUseFailure` on the
completion channel and treat them as one logical "tool finished" event with a success
flag. `PostToolUseFailure.error` (`"Exit code N\n<stderr>"`) + `duration_ms` are the
failure detail AO surfaces; there is no `tool_response` on the failure event. Do **not**
read a missing `PostToolUse` as "still running" — check for `PostToolUseFailure` too. And
note neither fires on an *interrupt* (Rule 3), so the turn-level interrupt signal is
separate again.

---

## Per-capability detail

### Per-call permissions

`probe_hook_permission.py` (allow / deny, with a deliberate hook sleep):

- **allow** → tool runs, **no TUI permission prompt** appears (the hook satisfies the
  permission). `out.txt` written.
- **deny** → tool blocked; the model receives an `is_error` tool_result with the
  reason; `out.txt` absent.
- **blocking** → Claude waits on the hook for the full hook duration before proceeding
  (LIVE: 6 s block, `probe_hook_failopen.py` sanity). This is the human-approval window.
- **authoritative** → a *returned* `deny` blocks even a command a static `ask` rule
  would otherwise prompt for (sanity case) — the hook decision outranks the rules. And
  in a clean env, **default mode itself gates tool calls** (it prompted even for
  `echo > file`, `probe_hook_failopen.py` baseline), so the hook is layered on top of a
  gate that already prompts rather than one that silently allows.

This is a complete, structured replacement for the `can_use_tool` control request AO
uses in stream-json — same shape (tool name + input → allow/deny/ask with reason),
delivered before execution, blocking.

### AskUserQuestion — read AND answer, zero keystrokes (the headline)

`probe_hook_answer.py`. `AskUserQuestion` is a normal deferred tool, so it surfaces as
a `PreToolUse` event. The payload's `tool_input.questions` carries the full structure
AO needs to render its own UI:

```jsonc
{ "questions": [ {
    "question": "Which filename would you like to use?",
    "header": "Filename",
    "options": [ {"label":"alpha.txt","description":"…"}, {"label":"beta.txt","description":"…"} ],
    "multiSelect": false
} ] }
```

To **answer** it, the hook returns `allow` with `updatedInput` = the input **echoed
back intact** plus an `answers` map keyed by question text:

```jsonc
{ "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "allow",
    "updatedInput": { "questions": [ … ], "answers": { "Which filename would you like to use?": "alpha.txt" } }
} }
```

LIVE result: **0 keystrokes**, the model received
`tool_result` = *"Your questions have been answered: "Which filename…"="alpha.txt".
You can now continue with these answers in mind."* and proceeded
(*"You picked alpha.txt."*).

Why it works in interactive mode: the decision path requires `updatedInput !==
undefined` **and** `requireCanUseTool` falsy. `requireCanUseTool` is assigned only in
the subagent-fork and prompt-speculation paths (SRC) — **not** in the interactive REPL —
so the injected answers are accepted without re-prompting. (This was the load-bearing
SRC claim; now LIVE-confirmed.)

> **Gotcha (advisor-flagged, respected in the probe):** `updatedInput` must echo the
> **full** `tool_input` with `answers` *added*. A partial `{answers:…}` drops
> `questions` and the TUI re-prompts — a false negative. Always merge into the original.

**Consequence:** the entire "drive the Ink `Select`/`SelectMulti` widget with
digit/arrow/space/enter keystrokes" mechanism in `INTERACTIVE_DRIVING_SPEC.md` is
**no longer needed for AskUserQuestion**.

**Multi-question + text-keyed (LIVE, `probe_hook_multiq.py`).** A single
`AskUserQuestion` call carrying **three** questions was answered in one shot via the same
`answers` map — **0 keystrokes**, no native re-prompt, and the `tool_result` mapped every
question to its own first-option answer. Matching is by **question text, not position**: a
discriminator run that inserted the `answers` entries in **reverse** question order still
mapped each question to its correct answer (positional matching would have mis-assigned
them). **AO consequence:** AO's UI may return answers in any order, **provided each
`answers` key is the exact question string Claude sent** — echo it from
`tool_input.questions[].question`, don't reconstruct it.

### MCP tools

`probe_hook_mcp.py` (+ a minimal local stdio server `mcp_server.py` exposing one `ping`
tool). MCP tool calls are **not special at the hook layer** — they surface through the
same `PreToolUse` / `PostToolUse` / `PostToolUseFailure` events as built-in tools, with a
`tool_name` that starts **`mcp__`**. LIVE results (proven against the local stdio server,
tool named `mcp__aoprobe__ping`):

- **Gateable per call:** a relay `deny` blocks the MCP call (PreToolUse fired, no
  completion) exactly like a built-in tool.
- **Success → `PostToolUse`** (carrying `PONG:HELLO`); **error → `PostToolUseFailure`**
  (the server returned `isError`) — the same success/failure split as a non-zero Bash exit
  (Rule 6). The probe asserts this **per event** (success must land on `PostToolUse`,
  `isError` on `PostToolUseFailure`), not merely that the marker appears in some post event.

Because this is the hook layer, the mechanics are **transport-agnostic**: a local stdio
server (used here to remove network flakiness) gates/reports identically to a remote or
plugin server. **But the `tool_name` shape differs by source, and AO writes a matcher
against it:**

- **User-scope `mcpServers` entry** (like the probe's `aoprobe`) → `mcp__<server>__<tool>`
  (e.g. `mcp__aoprobe__ping`). LIVE.
- **Plugin-provided server** (like the user's `context7`, which in this setup is a
  **plugin**, not a `~/.claude.json` `mcpServers` entry) → the server segment carries an
  extra `plugin_…_` prefix: shape **`mcp__plugin_…__<tool>`** (observed as
  `mcp__plugin_context7_context7__query-docs` / `…__resolve-library-id` in the live MCP
  tool list this session). The exact `plugin_<plugin>_<server>` sub-split is *inferred*
  from a single sample where the plugin and server names coincide (`context7_context7`);
  the plugin *naming* is confirmed from that list, the hook *mechanics* above were driven
  against the local server (not context7 end-to-end). AO doesn't need the sub-split — see
  below.

**AO consequence:** match/route on the **`mcp__` prefix** (covers both shapes); do **not**
assume the segment after `mcp__` is a bare server name — plugin tools inject a
`plugin_<plugin>_` component, so a parser hard-coded to `mcp__<server>__<tool>` mis-splits
every context7 tool. With that one caveat, AO gates and observes MCP tools — including the
user's globally-configured ones, which it inherits by design — with **no MCP-specific
code**. (User-scope `mcpServers` auto-enable; no project-MCP approval prompt.)

### Plan mode

`probe_planmode*.py` + `probe_hook_special.py plan`. Launch with
`--permission-mode plan`. The model researches, then calls `ExitPlanMode`, intercepted
as `PreToolUse(ExitPlanMode)` with the **full plan markdown** in `tool_input.plan`
(+ a `planFilePath`). AO approves with `allow` (the model proceeds to execute) or
rejects with `deny`. This replaces the earlier spec's fragile shift+tab → Select
driving for plan approval.

> Note: AO's current stream-json integration deliberately **denies** `ExitPlanMode`
> with a "client captured your plan, wait" message and drives approval out-of-band.
> The same capture-then-decide pattern works here; AO can keep its UX and just swap the
> capture/decision channel to the hook.

### Background tasks & subagents

The lifecycle splits into three subsystems that do **not** share state — AO's
`task_*`/`stop_task` denote the first two, **not** the daemon:

1. **`run_in_background` Bash** (`local_bash`) — a child process of the claude process.
2. **`Task` subagent** (`local_agent`), incl. backgrounded — an in-process query loop.
3. **cc-daemon "background session"** — a *whole detached `claude` process* (own PTY,
   sessionId). On-demand only (detach/respawn/fleet/routines/remote-control); **not** in
   the `run_in_background` path. `claude stop/logs/agents` operate on **this** — they
   **silently no-op** on #1/#2. (BIN: `control.sock` NDJSON protocol, `kill {short}`
   per-session; not in v2.1.88 source.)

**Recovering the lifecycle (LIVE `probe_hook_coverage.py`, `probe_hook_bgcomplete.py` + SRC/BIN):**

- **Subagent:** the subagent-launch tool surfaces in hooks as **`Agent`** (not `Task`) —
  `PreToolUse(Agent)` at launch, `PostToolUse(Agent)` when it returns; inner tool calls
  tagged `agent_id` / `agent_type` (recovers `parent_tool_use_id` correlation);
  **`SubagentStop`** at completion with `agent_id`, `agent_transcript_path`, and
  `last_assistant_message` (= the subagent's final text — the probe saw
  `"SUBAGENT-RAN"`). Fires even when backgrounded (and can fire more than once). So hooks
  fully recover subagent start→end **with its result**.
- **Background Bash:** `PreToolUse(Bash)` at start; **`PostToolUse` fires at *dispatch*
  (tool_response `{stdout:"", …, backgroundTaskId:"<id>"}`), NOT at completion.**
  Re-verified with **both** `PostToolUse` *and* `PostToolUseFailure` registered and the
  full timeline dumped: **no hook fires when the bg command finishes** — exactly one Bash
  post-hook references the task id (the dispatch). Completion is recovered from the
  **`<task-notification>` user message** injected into the **next** `/v1/messages` request
  (carries `task-id`, `tool-use-id`, `output-file`, `status`, `summary` incl. exit code).
  Two things AO must handle:
  - **Correlate** the dispatch `backgroundTaskId` ↔ the notification's `task-id` to tie
    completion back to the originating tool call.
  - **Drive a follow-up turn.** The notification is *enqueued* and only flushed onto the
    wire on the next request — if AO never sends another turn, the completion signal never
    arrives (the probe sends a follow-up to flush it). Mid-run progress: tail the
    deterministic `output-file` path.

**Kill one task by id (`stop_task` replacement):** the SDK `stop_task` control request
is `--print`-only and gone in interactive. Two interactive routes, both now characterized:

- **In-band tool (model-mediated).** `KillShell` (`{shell_id}`) is a real model tool
  (BIN: `KillShell`, `killShellExecution`, `killShellTasksForAgent`); AO drives it by
  injecting a prompt telling Claude to kill shell `<id>`. Stable contract, but
  model-mediated — costs a turn, pollutes the transcript, the model can decline/misfire.
  **No deterministic per-task slash command exists** (BIN-grepped: `/background` is the
  run-in-background toggle, not a kill). (BIN + SRC.)
- **Direct OS kill (deterministic) — VALIDATED (`probe_hook_bashpid.py`, LIVE).**
  Previously called "fragile, no id→PID map" — that was **wrong**. The background command
  runs as a **live descendant of the `claude` process** (`claude → zsh -c wrapper →
  sleep`, confirmed at the `PostToolUse(Bash)` dispatch), so AO **can** bind
  `backgroundTaskId → PID`: the dispatch hook fires *while the process is alive* and
  carries the id, so **snapshot-diff `claude`'s descendants across the `PreToolUse →
  PostToolUse(Bash)` pair** — the newly-appeared subtree is the task — then `SIGTERM`
  that subtree (kill the wrapper's group; the leaf dies with it). The PID is **not** in
  the hook payload — it is read from the process tree (the hook, or AO directly, since AO
  owns `claude`'s pid). No cc-daemon indirection for the shell itself (the daemon
  coordinates notifications; the process is `claude`'s child). **Caveats:** a true
  double-fork daemon re-parents to init and escapes a later tree-walk (capture the PID at
  dispatch to chase it); process enumeration is `/proc` here — **Linux/WSL only**; macOS
  needs `libproc`/`proc_listchildpids`, Windows a Toolhelp snapshot.

### Interrupt (both cases)

Two code paths keyed on the abort reason — Esc = `user-cancel`, submit-over-running =
`interrupt` (both literals BIN):

- **During model streaming** (no tool running): the in-flight `POST /v1/messages` SSE
  is **aborted mid-stream** — no `message_delta` stop_reason, **no `message_stop`**,
  upstream read cancels. **ARTIFACT** `ao-interrupt-marks.json`:
  `had_message_stop:false`, `stop_reason:null`, `proxy_errors:[{stage:'upstream_read',
  error:'context canceled'}]`. **AO's turn-finalization rule:** absence of
  `message_stop` + upstream cancel ⇒ interrupted turn. **Partial text that had already
  streamed is recoverable (LIVE, `probe_hook_partialtext.py`):** the `text_delta`s reach
  the proxy *before* the Esc, so AO holds the partial assistant text by construction —
  the **wire is the primary recovery**; the probe additionally observed the partial
  persisted to the transcript (a crash-recovery bonus). Display fidelity is not at risk.
- **During tool execution** (`probe_hook_interrupt.py`, LIVE): the tool process is
  killed (side-effect did **not** complete — the FIFO probe's `&& echo LATE` never
  ran), **neither `PostToolUse` nor `PostToolUseFailure` fires** (verified with both
  registered), the `tool_result` is `REJECT_MESSAGE` (identical to a deny), and a
  synthetic user message `[Request interrupted by user for tool use]` is written —
  **the discriminator** (Rule 3).
- **Esc behavior:** for a mid-*output* interrupt the wire + transcript are the signals
  (above); a think-only **revert** carries the *same* wire signal (aborted `/v1/messages`,
  no `message_stop` + `upstream_read` cancel), differing only in that the streamed content
  was thinking-only — see the next subsection. The `user-cancel` path auto-restores the
  just-sent prompt into the composer (**LIVE-confirmed**, `probe_hook_escrevert.py`).

### Esc-revert (think-only) & mid-turn steering — turn-lifecycle observability

Two interactive behaviors AO inherits "for free" by driving the real TUI. Both LIVE on
2.1.158; these are the signals AO must reconcile its thread against.

- **Esc during think-only = revert, and it IS wire-detectable** (`probe_hook_escrevert.py`
  + `probe_hook_revertcontext.py`, LIVE, regime-verified: only `thinking` streamed before
  the Esc, no `text`/`tool_use`). The Esc **aborts the in-flight `POST /v1/messages`**: the
  captured SSE for the reverted request is `response_head 200 → thinking deltas → error
  {stage:"upstream_read", "context canceled"} → response_end`, with **no `message_stop`**
  and **no `text_delta`**. That is the *same* aborted-stream signature as a mid-*output*
  interrupt (see Interrupt above) — the **only** difference is that the streamed content
  was thinking-only. So AO detects a revert from the wire, not the TUI:

  > aborted SSE (no `message_stop` + `upstream_read` cancel) **and** streamed content was
  > thinking-only (no `text_delta`/`tool_use`) **and** no `[Request interrupted by user]`
  > transcript row **and** no `Stop` ⇒ **revert**.

  The submit-time signals still fire: `UserPromptSubmit` once at submit, and a `user` row
  written to the transcript at submit — which Esc leaves **orphaned** (no chained
  `assistant` row, no `Stop`, no synthetic interrupt row). The prompt text returns to the
  composer (LIVE).
  **Re-entry settled — DROPPED (reproduced ×2, `probe_hook_revertcontext.py`):** the
  reverted prompt does **not** re-enter the next turn. After a think-only revert, a
  follow-up prompt B's request `messages[]` is exactly `[user:B, system]`, with the
  reverted A **absent** (no separate user message; not fused into B's). Claude Code drops
  the reverted turn from its in-memory conversation. **Consequence for AO:** on revert,
  drop the message from the live thread (matches CC); but the **durable transcript keeps
  the orphan**, so on backfill/resume AO must **filter orphaned user rows** — a `user` row
  with no chained `assistant` row whose wire request was an aborted thinking-only stream.
  This refines the user's in-mem hypothesis: the revert *is* CC-side, but it is **not**
  TUI-scrape-only — the abort is on the wire, which keeps AO inside the never-scrape-ANSI
  principle.

- **Mid-turn steering IS fully capturable — just NOT on the hook channel**
  (`probe_hook_steer.py`, LIVE, reproduced ×2). A message typed while a turn runs is
  queued and **consumed in the same turn** (the agent acted on it; `Stops before
  consumption = 0`). Where it surfaces:
    - **Transcript (the durable capture point):** the queue item's lifecycle is bracketed
      by two **`queue-operation`** rows — `operation:"enqueue"` at submit (`content`, ISO
      `timestamp`) and `operation:"remove"` at pickup/consume — **plus** an `attachment`
      row of `type:"queued_command"` (`prompt`, `commandMode:"prompt"`, threaded via
      `parentUuid`). So both *submit* and *pickup* are durably observable (reproduced ×2).
    - **Wire:** injected into the running turn's next `/v1/messages` as a **`system`**
      message — `"a new message while you were working:\n<text>"` — *not* a user message.
    - **TUI:** a "queued" affordance renders.
    - **Hook: silent — `UserPromptSubmit` does NOT fire for a queued steer.** AO must not
      wait on it; pickup is instead observable off-hook via the `remove` `queue-operation`
      + the wire `system` message. **Submit timing matters:** a `\r` sent in the same
      instant as the pasted text is dropped (paste/enter coalescing) — send text, let the
      composer render, *then* `\r`.
  Since AO *originates* the steer (it relays the user's keystrokes) it already holds the
  content + submit time; the `queue-operation` row gives the durable, timestamped ordering
  for the thread.

**Discriminator — is any AO-relevant behavior TUI-scrape-only?** A Claude Code behavior is
observable off the TUI **iff it touches the conversation — on the wire or in the
transcript**; only pure-UI state (scroll position, composer cursor, which message is
highlighted, the spinner) is TUI-only, and AO renders its own UI so it never needs that.
Mapping the candidate "in-mem" behaviors against the transcript row-type catalog (`mode`,
`permission-mode`, `file-history-snapshot`, `user`, `attachment`, `ai-title`, `assistant`,
`last-prompt`, `queue-operation`, `system`):

- permission-mode toggle (shift+tab) → `permission-mode` / `mode` rows (history-traced);
- `/model` → model on the wire (and it starts a new session anyway);
- plan enter/exit → the `ExitPlanMode` tool call (PreToolUse-captured);
- mid-turn steering → `queue-operation` + wire `system` (above);
- `/compact` → explicitly out of scope.

The one behavior that is an *uncommit* rather than a history write — **think-only
Esc-revert** — was the sole candidate for "TUI-only," and it turns out to be **wire-visible
too** (the aborted `/v1/messages`). So **no behavior AO needs is TUI-scrape-only**: the
user's "some things are in-mem, TUI-only" caution holds only for pure-UI chrome AO doesn't
mirror. This is what keeps AO inside the never-scrape-ANSI principle end-to-end.

### Attachments / images

**No PTY path accepts a raw base64 `image` block** (what AO sends today). The TUI only
produces image blocks by reading bytes from the OS clipboard or a file. Two drive
strategies:

- **A — temp file + bracketed-paste the path** (recommended; **LIVE-confirmed**,
  `probe_hook_attach.py`): write the image to a temp file, send the absolute path
  wrapped in bracketed-paste markers `\x1b[200~/abs/img.png\x1b[201~`.
  `tryReadImageFromPath` reads it into a real image block — the probe observed an
  `image` content block with `source.type=base64` in the transcript user message
  (and the `[Image #1]` composer placeholder). Regex accepts
  `.png/.jpg/.jpeg/.gif/.webp`. Use bracketed paste, not char-by-char typing.
- **B — OS clipboard + Ctrl+V** (`\x16`): populate the clipboard, send Ctrl+V. On WSL2
  this needs `xclip`/`wl-paste` present (no macOS NSPasteboard fast-path).

Ingested images render as `[Image #N]` placeholders (BIN); base64 is stored out-of-band,
not in composer text.

---

## What's NOT recovered / open items / risks

Ordered by stakes. Nothing here blocks the migration; these are the honest edges.

**Two items previously listed here are now CLOSED (LIVE this round):** *multi-question
AskUserQuestion* (1–4 questions in one call, text-keyed, proven with a reverse-order
discriminator — `probe_hook_multiq.py`) and *interrupt partial-text persistence* (the wire
carries the deltas before Esc; the transcript also persisted them — `probe_hook_partialtext.py`).
The remaining edges:

1. **Mid-tool interrupt tool_result wording is gate-dependent.** The `REJECT_MESSAGE`
   content depends on the `streamingToolExecution` gate (server-controlled). It was on in
   this run (LIVE), but AO should key off the **`[Request interrupted by user for tool
   use]` marker**, not the tool_result text (Rule 3), so a wording/gate change doesn't
   break detection.
2. **bg-Bash completion is latency-bound to the next turn.** The `<task-notification>` is
   *enqueued* and only flushed on the next `/v1/messages` request (its own
   `enqueuePendingNotification` channel — it does **not** fire the `Notification` hook,
   SRC). So if AO sends no further turn, bg completion isn't observed until one is sent.
   AO already drives turns; just don't expect a push at the instant the bg command exits.
   Mid-run, the deterministic `output-file` is tail-able.
3. **`SubagentStop` can fire more than once** (observed in `probe_hook_bgcomplete.py`). AO
   should treat it idempotently — **dedupe by `agent_id`** — rather than assuming exactly
   one stop per subagent.
4. **Proxy multi-session attribution — design, not built.** The per-session ephemeral
   loopback listener (one `127.0.0.1:0` per `claude`) is the intended isolation; needs
   AO-side implementation.
5. **Version skew.** Source is v2.1.88; binary 2.1.158. Every load-bearing claim was
   checked against the binary or live-probed, but undocumented hook events and the exact
   tool_result strings can drift. Pin behavior to: documented events, the marker
   strings (BIN), and the structural shapes — not exact prose. In particular, the ~20
   non-documented `HOOK_EVENTS` entries seen only in the source *return* union
   (`PermissionRequest`, `Elicitation`, `SubagentStart`, …) are **not** confirmed to fire.

6. **This map is validated per-row, but it is not a mechanical diff of AO's
   `AllEventKinds`.** The rows above were derived from the stream-json capabilities AO is
   known to use, not by enumerating `internal/provider/events.go` +
   `internal/provider/items.go` (the exhaustiveness-test-enforced lists the Claude
   consumer actually emits). Diffing them (2026-05-31) — the 26 `Event*` kinds
   `provider/claude/*.go` emits — finds **a handful of Claude-emitted signals with no
   validated row here.** None are architectural blockers (each rides a primitive proven
   elsewhere, or a documented-but-unprobed path), but **none were exercised** and should be
   before claiming literal parity:
   - **Context compaction** (`EventCompactBoundary` / `ItemCompaction`, from
     `parse_system.go`) — a long session **will** auto-compact; AO renders the boundary +
     context-window snapshot. Recovery path is plausible (wire `system` message + the
     **documented** `PreCompact`/`PostCompact` hooks + a transcript summary record) but
     was **never probed** — no capture of a real compaction event exists. Cleanest genuine
     omission; medium stakes (it happens in every long thread).
   - **API-transport errors** (`EventError` / `ItemAPIError` on 429/529/overload, and the
     fatal `stop_reason` enums in `parse_assistant.go`) — §11 (FINDINGS) flags these as
     "capturable via response status + SSE `error` events, **not exercised on demand**."
     Capturable by design, untested.
   - **`EventAPIRetry`** — one *organic* `400→retry→200` was observed on the wire, but the
     `system.api_retry` envelope that is AO's actual `EventAPIRetry` source was not
     specifically validated. Low stakes.
   - **Lower concern (called out so they're not mistaken for gaps):** the **todo/task
     rail** (`EventTaskCreate`/`TaskUpdate`/`TodoUpdate`) is sourced from the
     `TaskCreate`/`TaskUpdate`/`TodoWrite` **tool_use + tool_result** — the *validated*
     wire/transcript tool path — **not** from the unconfirmed `TaskCreated`/`TaskCompleted`
     hooks, so their non-firing is not an AO gap (only the specific Task\* staging→apply
     parse is unexercised). **`mcp-elicitation` is a Codex-only `Kind`** (`codex/`
     emits it; `provider/claude/` never does) — **not a Claude parity item.**

---

## AO integration sketch

Maps onto the existing `provider.Session` interface (`internal/provider/session.go`)
with **no new transport back-channel** — the relay rides AO's existing transport.

- **Launch:** `claude` in a PTY, `CLAUDE_CONFIG_DIR=<isolated>`,
  `ANTHROPIC_BASE_URL=<per-session loopback proxy>`, `--permission-mode default`, with a
  **clean, curated env** (no inherited `CLAUDE*`/`CLAUDECODE` auto-accept state — Rule 1).
  Seed `.credentials.json` (OAuth) + `.claude.json` (trust for the workspace cwd) +
  `settings.json` (AO's hooks) into the config dir.
- **`Send`:** type the prompt into the PTY (bracketed-paste for safety) + Enter;
  attachments via Strategy A (temp file + bracketed-paste path).
- **`RespondToApproval`:** the `PreToolUse` relay blocks; AO renders the permission UI;
  the relay returns `allow`/`deny`/`ask` (with its own deadline → `deny`, per Rule 1).
- **`RespondToUserInput`:** for `AskUserQuestion`, the relay returns `allow` +
  `updatedInput.answers` (handles 1–4 questions; key each answer by the **exact question
  string** from `tool_input.questions[].question` — matching is text-keyed and
  order-independent). AO keeps its existing `UserInputRequest` UI — the shape matches.
- **Tool completion:** register **both** `PostToolUse` (success) **and**
  `PostToolUseFailure` (failure) — treat them as one "finished" event with a success
  flag; `PostToolUseFailure.error` carries exit code + stderr (Rule 6). MCP tools
  (`mcp__*`) flow through the same events. Never read a missing `PostToolUse` as "still
  running."
- **Plan flow:** launch (or switch) to `--permission-mode plan`; capture via
  `PreToolUse(ExitPlanMode)`; decide via `allow`/`deny`.
- **`Interrupt`:** send Esc (`\x1b`) to the PTY; finalize the turn on the wire signal
  (no `message_stop`) and/or the transcript marker. Partial streamed text is already in
  AO's hands from the wire.
- **Background/subagents:** subagent lifecycle from the **`SubagentStop`** hook (dedupe by
  `agent_id` — it can fire more than once); bg-Bash completion from the
  **`<task-notification>`** wire message — **drive a follow-up turn to flush it** and
  correlate the dispatch `backgroundTaskId` ↔ the notification `task-id`. `stop_task` by
  injecting a `TaskStop` prompt.
- **`PID` / `Close`:** the PTY child pid; `/exit` + signal on close.
- **Rendering:** streaming text/thinking from the proxy SSE; heavy payloads
  (diffs/output/thinking) stay in SQLite per AO's bounded-memory principle, backfilled
  from the transcript on demand.

The relay process is the only new moving part: a thin stdin→AO-transport→stdout bridge,
one per hook event, that AO already has the plumbing to host.

---

## Evidence index

| Claim | Source | Confidence |
|---|---|---|
| OAuth survives custom base URL on 2.1.158 | live proxy capture (`/tmp/hookcap-allow.jsonl`), path↔status correlated: 38×200 + 12×404 (317 B quota probe, tolerated) + 1×400 (SDK-retried→200) on `/v1/messages`; Bearer, no x-api-key | **LIVE** |
| PreToolUse allow/deny gates, no TUI prompt, blocks | `probe_hook_permission.py` | **LIVE** |
| AskUserQuestion read + answer via `updatedInput`, 0 keystrokes | `probe_hook_answer.py` | **LIVE** |
| AskUserQuestion multi-question (1–4) answered in one call, matched by question **text not position** (reverse-order discriminator) | `probe_hook_multiq.py` (+ `AO_MULTIQ_REVERSE`) | **LIVE** |
| Tool completion split: success→`PostToolUse`, failure (non-zero exit)→`PostToolUseFailure` (`error`="Exit code N\n<stderr>", `is_interrupt`, `duration_ms`); **register BOTH** | `probe_hook_failcomplete.py` (+ exit-0 control) | **LIVE** |
| MCP tools surface `mcp__`-prefixed (user `mcp__<server>__<tool>`, plugin `mcp__plugin_<plugin>_<server>__<tool>`): per-call gate; success→`PostToolUse`, `isError`→`PostToolUseFailure` (routing asserted per-event) | `probe_hook_mcp.py` (+ `mcp_server.py`) | **LIVE** |
| ExitPlanMode capture + approve via PreToolUse | `probe_planmode*.py`, `probe_hook_special.py plan` | **LIVE** |
| Subagent `SubagentStop` (+ `last_assistant_message`), `agent_id` tagging, launch tool named `Agent` not `Task` | `probe_hook_coverage.py`, `probe_hook_bgcomplete.py` | **LIVE** + SRC/BIN |
| bg-Bash completion is wire-only (`<task-notification>`: status, exit code, output-file); **no completion hook** (full timeline, both post-hooks registered); correlate `backgroundTaskId`↔`task-id`; needs a follow-up turn to flush | `probe_hook_bgcomplete.py` | **LIVE** |
| bg-Bash runs as a live descendant of `claude` (`claude→zsh -c→sleep`) at the `PostToolUse(Bash)` dispatch; PID **not** in payload but recoverable via /proc snapshot-diff → `backgroundTaskId→PID` bindable → deterministic per-task kill (no model/TUI). Caveats: double-fork daemons escape; /proc is Linux-only | `probe_hook_bashpid.py` | **LIVE** |
| Interrupt during tool: marker; **neither `PostToolUse` nor `PostToolUseFailure` fires**; REJECT_MESSAGE; side-effect not run | `probe_hook_interrupt.py` (FIFO, both post-hooks registered) | **LIVE** |
| Interrupt during streaming: no `message_stop`, upstream cancel | `artifacts/ao-interrupt-marks.json` | **ARTIFACT** |
| Interrupt mid-text: partial deltas captured on the wire before Esc (primary); partial also persisted to transcript (bonus) | `probe_hook_partialtext.py` | **LIVE** |
| Killed gate hook reverts to normal permission flow (→ native prompt; does NOT force-execute) | `probe_hook_failopen.py` decider | **LIVE** |
| Surviving `deny` hook held the tool 6 s then blocked; a returned deny outranks an `ask` rule | `probe_hook_failopen.py` sanity | **LIVE** |
| Gate hook holds a tool **70 s** under configured `timeout:120` (human-approval window; no ~30 s cap) | `probe_hook_longtimeout.py` | **LIVE** |
| Default mode (clean env) gates even `echo>file`; an `ask` rule forces a prompt with no hook | `probe_hook_failopen.py` baseline/control | **LIVE** |
| Child `claude` inheriting parent `CLAUDE*`/auto opt-in auto-runs tools in nominal default mode | `probe_hook_failopen.py` (env strip flipped the result) | **LIVE** |
| **Sensitive-path edits are bypass-immune**: same hook=`allow` ran a benign write but held `.zshrc` behind a native dialog (0 keystrokes). Probed a `DANGEROUS_FILES` basename (`.zshrc`); the `DANGEROUS_DIRECTORIES` segment set (`.git .vscode .idea .claude`) is same-mechanism but SRC-only | `probe_hook_dangerpath.py` | **LIVE** |
| The bypass-immune dialog is a numbered prompt **drivable via PTY**: digit `1` → held edit proceeds (PostToolUse fires, file written with exact content) | `probe_hook_dangerdrive.py` | **LIVE** |
| **Esc during think-only = revert, and it is WIRE-detectable** (regime-verified: only `thinking` streamed before Esc): the Esc aborts the in-flight `/v1/messages` — `no message_stop` + `error{upstream_read,"context canceled"}`, the *same* signature as a mid-output interrupt, differing only in thinking-only content — so revert is detected on the wire, **not** TUI-only. Submit-time `UserPromptSubmit` + an **orphaned** transcript `user` row also fire; **no `Stop`**, **no** `[Request interrupted by user]` row. Re-entry settled (×2): the reverted prompt does **not** re-enter the next turn (`messages[]`=`[user:B, system]`, A absent) → **DROPPED**; AO drops it live but must filter the orphaned transcript row on backfill | `probe_hook_escrevert.py`, `probe_hook_revertcontext.py` | **LIVE** |
| **Mid-turn steering captured OFF the hook channel**: queued steer consumed **same-turn**; lands as transcript **`queue-operation`** rows bracketing the lifecycle — `operation:enqueue` (submit) → `operation:remove` (pickup) — + a `queued_command` `attachment`, and on the wire as a **`system`** msg ("a new message while you were working:\n…"); **`UserPromptSubmit` does NOT fire** (pickup observable via the `remove` op + wire `system` msg instead); submit needs render-then-`\r` (paste/enter coalescing) | `probe_hook_steer.py` (reproduced ×2) | **LIVE** |
| 30-event HOOK_EVENTS array; payload/stdout/exit schema; timeout no-clamp; trust skip; `--settings`/`CLAUDE_CONFIG_DIR` | hook-contract investigation (binary strings + v2.1.88 source + docs) | **BIN/SRC/DOC** |
| Attachment via temp-file + bracketed-paste yields a base64 `image` block | `probe_hook_attach.py` (transcript: `image`/`source.type=base64` + `[Image #1]`) | **LIVE** |
| Clipboard+Ctrl+V strategy, regex extensions, `[Image #N]` | attachment investigation (binary + source) | **BIN/SRC** |
| cc-daemon control.sock protocol; task subsystems distinct | background-task investigation (binary + source) | **BIN/SRC** |
| 2.1.158 blocks foreground `sleep` | observed in `probe_hook_interrupt.py` v1 transcript | **LIVE** |

Probes live in `spike/claude-mitm/` (`probe_hook_*.py`, shared `aoprobe.py`,
`hook_relay.py`); the reverse proxy in `proxy/main.go`; captured wire/PTY in
`artifacts/`.

---

## Relationship to the earlier spec

`FINDINGS.md` and `INTERACTIVE_DRIVING_SPEC.md` were written before the hook channel was
characterized, around **driving the TUI** (Select widgets, shift+tab plan cycling, ANSII
reasoning). This document supersedes those driving mechanics:

- **Kept:** the proxy-wire tap, the transcript-as-source-of-truth principle, the
  interrupt wire-signal finding, the "never scrape ANSI for state" rule.
- **Replaced by hooks:** per-call permissions, AskUserQuestion (read **and answer**),
  plan capture/approval, subagent/background lifecycle, session metadata. The
  Select/SelectMulti keystroke-driving fallback is **eliminated** — the central
  "ideally not by parsing/driving react in a terminal" goal is met.
- **Newly load-bearing:** the timeout-fall-through and trust-prerequisite rules
  (Rules 1–2) — get these wrong and the gate silently stops gating (the turn sticks on
  a native prompt, or the tool runs unreviewed).
