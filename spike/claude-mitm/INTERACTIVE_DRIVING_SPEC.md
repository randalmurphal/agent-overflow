# Driving Interactive Claude Code from Agent Overflow — Complete Behavioral Spec

**Superseded production posture (2026-06-05).** This document is preserved as
the historical PTY-driving spec. Do not use its `bypassPermissions` /
`--dangerously-skip-permissions` default posture as the AO implementation plan.
The current plan is the documented local `ANTHROPIC_BASE_URL` gateway plus
Claude Code hooks, transcript backfill, and PTY input, launched in
`--permission-mode default` with the hook relay as the gate. Transparent TLS MITM,
TLS/HTTP fingerprint preservation, and Claude remote-control protocol replication
are out of scope for production integration. See
[`HOOKS_COVERAGE_MAP.md`](HOOKS_COVERAGE_MAP.md) for the authoritative plan.

**What this is.** A complete set of historical instructions for how Agent
Overflow (AO) drives a *real interactive* `claude` process with the user's local
Claude Code auth and handles every situation that arises, with each
behavior grounded in either the captured wire stream, the real-binary PTY
probes in this dir, or the Claude Code source (`~/repos/claude-code-source-code`,
cross-checked against binary `2.1.150`).

Read [`HOOKS_COVERAGE_MAP.md`](HOOKS_COVERAGE_MAP.md) for the current
implementation plan. This document is a historical record of the earlier
PTY-driving probes and evidence index; it is not the current "how to implement
AO" guide.

Validated 2026-05-26/27 on `claude 2.1.150` / `claude-opus-4-7`, using the
user's local Claude Code OAuth.

---

## 0. The governing model: two channels, one rule

AO sees Claude through two channels with very different reliability:

| Channel | Carries | Reliability | How AO reads it |
|---|---|---|---|
| **Wire** (proxy capture of `/v1/messages`) | the model interaction: text/thinking/tool_use deltas, tool_results (via request diff), usage, stop_reason, rate-limit headers, request `messages` history | **high** — structured JSON, version-stable shape | `ao_transform.py` logic, ported to Go |
| **Transcript / filesystem** (`~/.claude/projects/*.jsonl`, `file-history/`) | session structure, fork points, file snapshots, `toolUseResult` sidecar | **high** — structured, but debug-grade stability | tail + parse on completion |
| **PTY / TUI** (the terminal byte stream) | client-side control state: permission prompts, the rewind selector, mode indicator, trust dialog, errors the TUI renders | **low** — ANSI cursor-addressed redraws; naive scraping mangles it (proven below) | only when unavoidable, via a real terminal emulator / screen buffer |

**The rule that falls out of this: _drive_ through the PTY (keystrokes), but
_detect outcomes_ on the wire/filesystem — never by scraping TUI text.** Every
probe in this dir confirms the wire gives a clean, unambiguous signal for the
same event the TUI renders messily. Sections 3–4 apply this rule per capability.

The ANSI-mangling is not hypothetical. Stripping escapes from the *real* rewind
selector yields garbage like `Confimyouwantto restore to the point bfore you
sent thismessage` — dropped letters, collapsed spaces — because the TUI paints
with cursor moves, not linear text. AO must not build detectors on that.

**One bounded exception — the footer mode-indicator line.** The permission-mode
footer is a single fixed-format line the TUI writes synchronously with each mode
transition: **`⏵⏵ bypass permissions on`** / **`⏸ plan mode on`** /
**`accept edits on`** (each suffixed `· (shift+tab to cycle)`), or **absent** for
`default`. This is a *structured* PTY signal — one short line, stable token,
written on the transition — categorically unlike scraping wrapped message
content. AO **may** read it for the single purpose of **confirming a mode
transition** (after a shift+tab into plan, §3.8). It is the **only** sanctioned
TUI read; every other outcome stays wire/fs-detected, and even this one is a
*confirmation*, never a trigger.

---

## 1. AO capability surface (v1 cut)

Scope decision (confirmed with user): **AO is a curated host**, not a drop-in
Claude TUI. AO renders its own conversation UI from the wire/transcript and
drives only the minimal PTY surface below. Anything not listed is either
launch-config or AO's own UI; AO **never enters** the corresponding TUI context.

| # | Operation | v1? | Notes |
|---|---|---|---|
| 1 | Submit a prompt | ✅ v1 | core |
| 2 | Render the streaming turn (text/thinking/tools/results/usage) | ✅ v1 | core; wire |
| 3 | Interrupt a running turn | ✅ v1 | core |
| 4 | Revert / rewind (conversation, code, or both) | ✅ v1 | user-flagged; §3.4 |
| 5 | Permission posture | historical v1 | older static `bypassPermissions` plan; superseded by default-mode hook relay in `HOOKS_COVERAGE_MAP.md` |
| 6 | Model + thinking-effort selection | ✅ v1 | launch-config |
| 7 | Exit / shutdown | ✅ v1 | core |
| 8 | Session resume / continue | ✅ v1 | launch-config |
| 9 | Trust the workspace | ✅ v1 | launch-config pre-seed; §3.5 |
| 10 | Surface rate-limit / API / tool errors | ✅ v1 | wire |
| 11 | Image / file attachment in a prompt | ✅ v1 | AO-layer (§4) |
| 12 | Slash commands that mutate the session (`/compact`, `/clear`) | 🟡 v1.1 | drive-TUI; §3.6 |
| 13 | Plan mode + plan approval | historical v1 | older bypass + shift+tab plan; superseded by hook-based `ExitPlanMode` capture/decision |
| 14 | Compaction (auto, near context limit) | ✅ v1 detect / 🟡 drive | wire-detect in v1 |
| 15 | Mid-turn message queueing | 🟡 v1.1 | drive-TUI |
| — | Theme, tabs, history-search, transcript-view, diff-dialog, plugin UI, todo panel | ❌ out | AO renders its own; never enter these contexts |

---

## 2. Decision table — drive-TUI / AO-layer / launch-config

For each capability: **where the behavior is realized**, and the one-line
rationale. "drive-TUI" = AO sends keystrokes to the PTY. "AO-layer" = AO
implements it in its own UI/state from wire+transcript+fs. "launch-config" = set
once via CLI flags/`--settings` at spawn; never touched at runtime.

| Capability | Realized by | Rationale |
|---|---|---|
| Submit prompt | **drive-TUI** | only way to feed the model in interactive mode |
| Stream rendering | **AO-layer** (wire) | proxy SSE → `ao_transform` event stream; AO's own UI |
| Interrupt | **drive-TUI** (Esc) + wire-detect | keystroke to stop; aborted request is the signal |
| Revert/rewind | **drive-TUI** (`/rewind`) + wire/fs-detect | Claude's rewind restores files+convo correctly; reimplementing file-restore is the hard part we get for free |
| Permission posture | **superseded** | older spec used `bypassPermissions` + `--dangerously-skip-permissions`; current plan launches `--permission-mode default` and lets the hook relay decide per call (§4 note, `HOOKS_COVERAGE_MAP.md`) |
| Plan mode + approval | **superseded** | older spec used bypass launch + shift+tab + approval `Select`; current plan captures/approves `ExitPlanMode` through `PreToolUse` hooks (§3.8 note, `HOOKS_COVERAGE_MAP.md`) |
| Model / effort | **launch-config** (`--model`, settings) | no need to drive the in-TUI picker |
| Exit | **drive-TUI** (`/exit` or Ctrl-D) | clean shutdown of the child |
| Resume / continue | **launch-config** (`--resume`/`--continue`) | chosen at spawn |
| Trust workspace | **launch-config** (pre-seed `hasTrustDialogAccepted`) | avoid the first-run dialog entirely; no drive fallback in v1 (§3.5) |
| Errors / rate limits | **AO-layer** (wire) | status + SSE `error` + `anthropic-ratelimit-*` headers |
| Attachments | **AO-layer** | put paths in the prompt text; embed images as content blocks (see §4) |
| `/compact`, `/clear` | **drive-TUI** | session-mutating slash commands; detect effect on wire |
| Compaction (auto) | **AO-layer** (wire-detect) | observe the summarization request + shrunk history |
| Conversation history | **AO-layer** | AO owns its render; never use Claude's transcript view |
| Diffs | **AO-layer** | render from tool_result / fs; never enter DiffDialog |

---

## 3. Runbook — the drive-TUI capabilities

For each: the trigger, the **exact PTY input**, the **detection rule** (wire
first), and the evidence/confidence. Keystroke bytes are shown explicitly.

### 3.1 Submit a prompt  *(confidence: high — `drive_multi.py`)*
- **Input:** write the prompt text to the PTY, then `\r` (CR = `chat:submit`).
  Multi-line: see §3.7. The positional-arg form (`claude "prompt"`) is
  pre-filled but **sometimes needs an explicit `\r` to submit** — send one if no
  request appears within a few seconds of readiness.
- **Detect ready / done (wire):** a turn is a `/v1/messages` *agent* request
  (has a populated `tools` array **and** `max_tokens>1`) whose SSE ends
  `message_delta.stop_reason == "end_turn"`. Use that as "turn complete." A
  `stop_reason=="tool_use"` is mid-turn (more requests follow). `pause_turn`
  (extended thinking across calls) is **not** `end_turn`, so this never
  misfires.
- **Filter the noise (wire):** ignore `max_tokens<=1` (quota preflight),
  `tools==[]` (title/topic gen), and all-server-tools requests (a client tool's
  nested API sub-call, e.g. WebSearch→`web_search_20250305`). See `ao_transform.classify`.

### 3.2 Interrupt a running turn  *(confidence: high — `probe_interrupt.py` (streaming) + source `query.ts`/`useCancelRequest.ts` (tool-exec))*
- **Input:** single `\x1b` (Esc = `chat:cancel`). `useCancelRequest` Priority 1
  always cancels the active task first — so Esc reliably interrupts whether the
  model is streaming *or* a tool is mid-run; Priority 2 (pop a queued prompt)
  fires only when idle (useCancelRequest.ts:95).
- **Two cases, two different wire signals — AO must handle both:**
  - **(a) Interrupt during model streaming** *(probed).* The in-flight
    `/v1/messages` is **aborted** — SSE truncated with **no `message_stop` and no
    `stop_reason`**; the proxy sees the upstream read end as `context canceled`.
    **Finalize on "request aborted without `message_stop`," don't wait for a
    stop_reason.**
  - **(b) Interrupt during tool execution** *(source).* The model already
    returned (`stop_reason: tool_use`) and Claude is running the tool, so **there
    is no in-flight `/v1/messages` to abort** — signal (a) never fires. Instead
    Claude synthesizes a **tool_result for every outstanding tool_use** (content
    `"Interrupted by user"`, keeping the API's tool_use↔tool_result pairing
    valid; query.ts:1015–1029) and appends a synthetic user message
    **`[Request interrupted by user]`** / **`[Request interrupted by user for
    tool use]`** (query.ts:1047,1502). These hit the transcript immediately and
    the *next* agent request's `messages`. **AO must also treat the appearance of
    the synthetic "Interrupted by user" tool_result / `[Request interrupted by
    user…]` marker as a turn-finalization signal** — not only signal (a).
- **Esc vs. submit-interrupt (query.ts:1046,1501):** the `[Request interrupted by
  user…]` marker is emitted **only when the abort reason ≠ `'interrupt'`** — i.e.
  on a plain Esc. If the user instead *submits a new prompt over* a running turn
  (a submit-interrupt), the marker is **skipped** (the queued user message is the
  context). AO drives interrupt with Esc, so AO sees the marker; if AO ever adds
  submit-over-running (mid-turn queueing, §5), it must expect *no* marker.
- **Auto-restore behavior AO must mirror (source REPL.tsx:2106–2129, 2996;
  observed):** on cancel, any partially-streamed assistant text is first
  preserved as an assistant message, giving final order
  `[user, partial-assistant, [Request interrupted by user]]` (REPL.tsx:2124).
  Then: if the user interrupted *before any meaningful output* (e.g. during
  thinking), Claude **rewinds the interrupted user message out and restores its
  text to the input box** (`removeLastFromHistory` + `restoreMessageSync`, gated
  on empty input + no queued commands + only synthetic messages after). If
  interrupted *after* partial output, the partial turn **stays**. AO detects
  which happened by whether the next agent request's `messages` still contains
  the interrupted prompt, and reflects it in AO's composer/view.
- **Session stays usable** after interrupt (confirmed: a follow-up prompt
  produced new agent requests).

### 3.3 Exit  *(confidence: high)*
- **Input:** `/exit\r`, or `\x04` (Ctrl-D), or `\x03\x03` (double Ctrl-C). AO
  should prefer `/exit\r`, then SIGTERM the child as a backstop.

### 3.4 Revert / rewind  *(confidence: high — `probe_rewind.py` + source-verified navigation model)*

This is the most complex drive-TUI op and the one the user explicitly flagged.
**Claude's rewind restores files on disk AND the conversation**, via per-message
`file-history` snapshots (`~/.claude/file-history/{session}/{hash}@vN`). AO
drives it rather than reimplementing the file-restore.

**Two-level interaction:**

1. **Open** the selector: `/rewind\r` (slash command; alias `/checkpoint`) — or
   double-Esc when idle. *Idle-only:* the selector is gated on the agent not
   working (`openMessageSelector` guards on `!disabled`). The `/rewind` command
   is the robust open (one token vs. a chord that overloads Esc/interrupt).

2. **Level 1 — pick the message** (`MessageSelector` context). Rows are the
   **selectable user messages** in conversation order (oldest at top), plus a
   virtual empty `(current)` row appended at the **bottom**, where the cursor
   **starts** (`selectedIndex = messageOptions.length-1`, MessageSelector.tsx:67).
   - **Navigate with the literal letters `k` (up) / `j` (down)** — *not* the
     arrow-key escape sequences. An arrow sends a leading `\x1b`; a split/mistimed
     read of that byte is indistinguishable from a bare Esc, which here **cancels
     the selector**. `k`/`j` are unambiguous single bytes (keybindings.json →
     MessageSelector: `k`→up, `j`→down, `enter`→select).
   - **To revert to the k-th-most-recent selectable user message** (k=1 = latest
     user turn): send **`k` × k**, then `\r`. The cursor starts on the bottom
     `(current)` row, and each `k` moves up one *selectable* row, so k presses
     land exactly on the k-th user message from the end. AO needs only the
     offset-from-newest — never the absolute index or total length.
   - **Which rows are selectable — replicate `selectableUserMessagesFilter`
     exactly** (MessageSelector.tsx:767) so AO's offset matches Claude's list. A
     message is a selectable row iff **all** hold:
     - `type === 'user'`;
     - its first content block is **not** a `tool_result`;
     - not synthetic (`isSyntheticMessage` — interrupts/cancels);
     - `isMeta` is false;
     - not `isCompactSummary`, not `isVisibleInTranscriptOnly`;
     - its text contains **none** of these wrapper tags: `<local-command-stdout>`,
       `<local-command-stderr>`, `<bash-stdout>`, `<bash-stderr>`,
       `<task-notification>`, `<tick>`, `<teammate-message>` (constants in
       `src/constants/xml.ts`).

     In short: count only *genuine user-authored prompts* — skip tool results,
     synthetic/interrupt markers, meta, compaction summaries, and the
     command-output/notification breadcrumbs.

3. **Level 2 — pick the scope** (a `Select` submenu). Both the option set and the
   initial cursor depend on whether **code can be restored** for that message
   (`canRestoreCode` — a `file-history` snapshot exists for it; MessageSelector.tsx:344):
   - **code changed** → `[both, conversation, code, summarize, nevermind]`,
     cursor pre-seeded on **`both`** (`defaultFocusValue='both'`, index 0).
   - **no code changed** → `[conversation, summarize, nevermind]`, cursor
     pre-seeded on **`conversation`** (index 0).
   - **Index 0 is always the maximal restore available**, so:

   | AO intent | L2 keystrokes | Prediction needed? |
   |---|---|---|
   | **Restore code + conversation** (common case, default) | `\r` | ✅ none — `\r` always accepts the index-0 maximal restore |
   | **Restore conversation only**, code *did* change | `j` `\r` (→ index 1) | yes (below) |
   | **Restore conversation only**, no code change | `\r` (already index 0) | ✅ none |
   | **Restore code only** | `j` `j` `\r` (→ index 2; exists only when code changed) | yes (below) |

   Esc at L2 goes **back to Level 1**, not full-close (MessageSelector.tsx:248).

   **Predicting `canRestoreCode` (only for the conversation-only / code-only rows).**
   The snapshot↔message map lives only in Claude's in-memory `FileHistoryState`
   (on disk, `file-history/` is keyed by file-path-hash, not message id), so AO
   **predicts from the wire**: a turn changed code iff it contains an `Edit`,
   `Write`, `MultiEdit`, or `NotebookEdit` tool_use. Use **only those four** —
   *not* `Bash`. The error direction is then safe-by-construction: AO can only
   *under*-predict (the rare internal `sed`-style bash in-place edit also
   snapshots), whose worst case is "`\r` restored code too" on a
   conversation-only intent — a broader revert, still recoverable. Counting
   `Bash` as a code change would *over*-predict, and on the 1-option menu `j`
   would land on **`summarize`** (a different, destructive action) — so don't.

- **Detect the outcome (wire + fs — NOT by scraping the selector):**
  - **Wire (primary):** the rewind took iff
    `next_agent_request.messages.length < last_pre_rewind_agent_request.messages.length`
    (verified `9 → 5` rewinding past one turn).
  - **Transcript:** rewind **forks** — a **new session `.jsonl`** appears
    (truncated) and the **original is preserved** (verified: original 30 lines
    kept; fork began at 23). AO's session tracking must follow the fork's new id.
  - **Filesystem:** the tool-edited files are restored to the snapshot (verified:
    `foo.txt` MODIFIED→ORIGINAL on disk).
  - **Mis-land guard:** the wire/fs oracle is AO's safety net — if AO drove to the
    wrong row, the observed truncation length / restored files won't match AO's
    target, and AO can re-`/rewind` to correct.
- **Caveat AO must surface to the user:**
  **"Rewinding does not affect files edited manually or via bash"** — only files
  Claude changed via its Edit/Write/MultiEdit/NotebookEdit tools are
  snapshotted/restored. Verbatim from the TUI; load-bearing — show it in AO's
  revert UI (it's a real footgun).
- **Summarize rows (`summarize`, `summarize_up_to`): out of scope for v1.** The
  keystroke tables above never navigate to them. If AO later exposes "summarize
  from here," it's a separate v1.1 feature with its own wire signature (a
  summarization request / `isCompactSummary` message), not part of revert.

### 3.5 Trust the workspace  *(launch-config only in v1; confidence: high — source-verified pre-seed)*
The first-run "Do you trust the files in this folder?" dialog blocks the initial
prompt. The user declined a drive-TUI fallback for v1, so AO **pre-seeds trust at
launch** and never drives this dialog.
- **Pre-seed:** before spawning, set `hasTrustDialogAccepted: true` for the
  project path in the global config's `projects` map —
  `config.projects[<git-root-or-cwd>].hasTrustDialogAccepted = true`.
  `checkHasTrustDialogAccepted` (config.ts:697) walks cwd→parents, so trusting
  the git root or any ancestor suffices and the dialog never appears.
- **If trust can't be pre-seeded:** AO surfaces a setup error. The symptom is the
  §4 stall — the initial prompt never produces a `/v1/messages` request because
  the dialog is blocking. AO does **not** answer the dialog by keystroke in v1.

### 3.6 Session-mutating slash commands `/compact`, `/clear`  *(v1.1; confidence: medium)*
- **Input:** `/compact\r` or `/clear\r`.
- **Detect (wire):** `/clear` → the next agent request's `messages` array resets
  to ~1 (history dropped). `/compact` → a summarization request, then subsequent
  requests carry the compacted summary instead of full history. AO observes both
  on the wire; it should not parse the TUI's compaction UI.

### 3.7 Multi-line input  *(v1.1; confidence: low — confirm before relying)*
- The TUI accepts pasted multi-line text and a continuation key for newlines.
  AO should prefer **bracketed-paste** (wrap the text in `\x1b[200~` … `\x1b[201~`)
  to inject a whole multi-line prompt atomically rather than emulating per-line
  continuation. *Probe before shipping (see §5).*

### 3.8 Plan mode + plan approval  *(v1 — user-selected; confidence: high — end-to-end verified on `2.1.150`, `probe_planmode{,2,3}.py`)*

**Superseded for production.** The current plan captures `ExitPlanMode` through
`PreToolUse`, renders the plan in AO, and returns `allow`/`deny` through the hook
relay. The bypass + shift-tab + `Select` runbook below is historical probe
evidence only.

Plan mode lets Claude research and present a plan **without executing workspace
changes**, then asks the user to approve before edits run. The user pulled plan
+ approval into v1.

**Historical bypass-era entry was subtle — and the "obvious" launch flag was
wrong (verified):**

- **Historical probe launched in bypass, NOT in plan — passed
  `--dangerously-skip-permissions` only.**
  Do **not** add `--permission-mode plan` at launch:
  - `--permission-mode plan --dangerously-skip-permissions` resolves to
    **bypass, not plan**. `--dangerously-skip-permissions` pushes
    `bypassPermissions` into `orderedModes` *first*; the resolver picks the first
    non-disabled mode and `break`s (permissionSetup.ts:725, 777–795). *Verified:*
    `probe_planmode.py` combo run wrote `foo.txt` (Write executed) under
    "bypass permissions on".
  - `--permission-mode plan` **alone** does enter plan — but then
    `bypassPermissions` is **unavailable** (gated on the dangerous flag,
    useReplBridge.tsx:437), so the approval idx0 degrades to "Yes, auto-accept
    edits" (→`acceptEdits`) and post-approval `Bash` prompts, breaking the
    no-dialog posture. *Verified:* `probe_planmode.py` planonly run refused to
    write ("plan mode on", `foo.txt` absent).
  - So bypass-availability and plan-active are **orthogonal**: the launch flag
    makes bypass available for the *whole session*; plan is entered at *runtime*.
- **Enter plan at runtime via shift+tab, confirmed by the footer signal.** Send
  **`\x1b[Z`** (shift+tab) to cycle the mode. From bypass the cycle is
  **bypass → default → acceptEdits → plan** (getNextPermissionMode.ts:38–79):
  **3** shift+tabs when auto-mode is unavailable (auto, if present, inserts one
  extra step). **Do not blind-count** — after each shift+tab read the **footer
  mode-indicator line** (§0's one sanctioned PTY read) and stop when it shows
  **"plan mode on"**. *Verified:* `probe_planmode2.py` reached plan in 3
  shift+tabs; auto unavailable on this account.
- **Detect plan ready (wire):** the planning turn ends by calling the
  **`ExitPlanMode` / `ExitPlanModeV2` tool** (messages.ts:3287). The **plan text
  is in that tool_use's `input`**; AO renders the plan from there (also persisted
  to **`~/.claude/plans/*.md`** — an fs backup). AO never scrapes the TUI plan view.
  - **Plan mode still runs read-only tools — don't treat plan as "no tools."**
    Read-only `Bash`/`Read`/`Grep` and plan-file edits **execute** during planning
    (*verified:* a `Bash(ls)` ran and returned output); only **workspace-mutating**
    tools (`Write`/`Edit`/`MultiEdit`) are suppressed — the model may *emit* a
    `Write` tool_use that the permission system blocks (no file appears, no success
    tool_result). The universal rule holds: **tool_use-emitted ≠ tool-executed;
    the fs / following tool_result is the truth.**
- **Approve (drive-TUI `Select`; `\r`).** The option list is **conditional**
  (`buildPlanApprovalOptions`, ExitPlanModePermissionRequest.tsx:674), but **index
  0 is invariably the approve-and-continue-in-bypass row** under AO's config.
  Verbatim render on `2.1.150` (`probe_planmode2.py`; this account has
  `showUltraplan` on, hence the 4th row):

  | idx | Label (verbatim from binary) | Value → mode | AO action |
  |---|---|---|---|
  | 0 | **"Yes, and bypass permissions"** | `yes-accept-edits-keep-context` → **bypassPermissions** (line 431, bypass available) | **Approve → `\r`** |
  | 1 | "Yes, manually approve edits" | `yes-default-keep-context` → `default` | unused (AO can't answer per-call prompts) |
  | 2 | "No, refine with Ultraplan on Claude Code on the web" | `ultraplan` | unused (web hand-off) |
  | 3 | "Tell Claude what to change" (text-input row) | `no` (+ feedback) | reject-with-feedback — but use Esc (below) |

  - **Approve = `\r` (zero navigation).** idx0 → `bypassPermissions`; the
    tool_result is "User has approved your plan…" (ExitPlanModeV2Tool.ts:483) and
    execution proceeds **auto-approved under bypass**. *Verified end-to-end*
    (`probe_planmode2.py`): footer flipped to "bypass permissions on", `foo.txt`
    written, **no permission prompt**.
  - **Plan mode is one-shot per approval.** After `\r` the session is in bypass
    and plan mode is **over** — it does *not* persist the way the reject path keeps
    plan active. A "plan the next change too" workflow must **re-enter plan with a
    fresh shift+tab cycle after every approval**; don't assume the second planning
    request runs in plan mode.
  - **idx0 has two preconditions AO controls/verifies.** `showClearContext` must
    stay **false** — it is `settings.showClearContextOnPlanAccept ?? false`
    (line 137); a true value prepends a "clear context" row at idx0, so **AO never
    enables it**. And **auto-mode must be unavailable** — if available, idx0
    becomes "Yes, and use auto mode" (→`auto`, not bypass). AO **self-checks after
    `\r`**: read the footer — **if it is not "bypass permissions on," idx0 didn't
    mean bypass**; shift+tab back to bypass and surface the anomaly.
- **Reject / keep planning = single `Esc` (index-independent).** Esc cancels the
  approval `Select` → `toolUseConfirm.onReject()` with no feedback, **staying in
  plan mode with no mode change** (handleCancelRef, ExitPlanModePermissionRequest.tsx:521–531).
  The dialog closes, the TUI shows "User rejected Claude's plan," and the session
  is immediately usable — the user's next prompt re-plans. *Verified*
  (`probe_planmode3.py`): after Esc the footer was still "plan mode on", no file
  written, and a follow-up prompt produced a fresh `ExitPlanMode`.
  - **Why Esc, not a `j`-counted reject row:** the reject input row is **last**,
    and its index moves with `showUltraplan` / `showClearContext` (idx 3 here, idx
    2 without ultraplan). Blind counting is fragile; Esc is invariant. (A future
    reject-*with*-feedback can type into the input row then `\r`; v1 rejects via
    Esc and lets the user re-prompt.)
  - **Abandon planning entirely (not just this plan).** Esc only *rejects the
    plan*; it leaves the session **in plan mode**. To leave planning altogether,
    after Esc closes the dialog send **one shift+tab** — from plan (bypass
    available) the cycle is **plan → bypass** directly (getNextPermissionMode.ts:55–62),
    landing back in the launch posture. Confirm via the footer (§0), don't assume.
- **Plan mode is per-process state (reliability).** The mode lives in the running
  CLI. A provider crash, `--resume` after disconnect, or any relaunch **resets to
  the launch mode (bypass)** and loses the runtime plan toggle. AO's plan-mode UX
  is bound to the live process; on restart AO must re-enter plan via shift+tab if
  the user is still mid-plan.
- **Posture-degraded guard (shared with §4):** if the `bypassPermissions`
  killswitch is active (org/Statsig/settings, permissionSetup.ts:778), bypass is
  unavailable everywhere — launch falls back to `default`, and the approval idx0
  degrades to `acceptEdits`. AO can't read the TUI's downgrade notice, so it
  detects the **observable consequence** (a `tool_use` with no following
  tool_result and no further request — a blocking prompt) and surfaces "permission
  posture not as expected."

---

## 4. AO-layer responsibilities (implement from wire / transcript / fs)

For each capability AO owns, the data source and the state AO maintains.

| Capability | Reads from | State AO maintains |
|---|---|---|
| **Turn stream → AO events** | wire SSE (`ao_transform` logic) | per-turn assembled assistant message; token-level deltas for live render |
| **tool_result reconstruction** | wire: diff agent request N+1 `messages` vs N; corroborate with transcript `toolUseResult` | map tool_use_id → result (+ `is_error`) |
| **Thinking** | wire `thinking` block — **signature only, no content** in adaptive mode (same as headless; not a gap) | show indicator + token count; no reasoning text exists to show |
| **system:init** (model, tools) | wire request body | session_id/cwd/mcp/permissionMode come from AO's own launch knowledge or transcript — **not on the wire** |
| **usage / cost** | wire `message_delta.usage` summed | cost = derive from a pricing table (notional under subscription) |
| **rate limits / errors** | wire response headers `anthropic-ratelimit-*`; SSE `error`; HTTP status (429/529) | surface as user-facing state |
| **conversation history / scrollback** | AO's own event log + transcript | AO's render; never Claude's transcript view |
| **diffs** | tool_result content / fs | AO renders; never enter DiffDialog |
| **attachments** | AO composes the request | images as `image` content blocks in the submitted message; file refs as paths in prompt text (Claude reads them) |
| **revert outcome** | wire (messages shrink) + transcript (fork file) + fs (restored files) | follow the forked session id; reconcile AO's thread to the truncation |
| **compaction** | wire (summary request, shrunk history) | mark the compaction point in AO's thread |

**Permissions section superseded for production.** This older spec assumed
interactive mode had no programmatic per-call interception and selected
`bypassPermissions` to avoid native prompts. The hook work invalidated that
assumption. Current AO launches with `--permission-mode default`, strips ambient
auto-accept env, and uses `PreToolUse` hooks to return explicit `allow`/`deny`/
`ask` decisions. Do **not** use `--dangerously-skip-permissions` as the default
AO posture. Do **not** use `--bare` (it forces API-key-style auth and forecloses
the user's Claude Code OAuth path — see FINDINGS §2).

---

## 5. Decisions, deferrals, and things to probe

**Historical decisions from this older spec (superseded where noted):**
- **Superseded:** the older v1 posture selected `bypassPermissions`
  (`--dangerously-skip-permissions`) to avoid native prompts. Current AO uses
  `--permission-mode default` plus the hook relay.
- **Superseded:** the older plan-mode path launched in bypass, shift-tabbed into
  plan, and drove the approval `Select`. Current AO captures and decides
  `ExitPlanMode` through `PreToolUse`.
- **Revert** — drive `/rewind`; navigation is now concrete (mirror
  `selectableUserMessagesFilter`, count from the newest, use `k`/`j`); the L2
  scope sub-choice (code+conversation / conversation-only) is handled, with the
  `canRestoreCode` prediction rule and a wire/fs mis-land guard (§3.4).
- **Caveat surfaced:** AO shows "manual/bash edits are not reverted" in its
  revert UI.

**Deferred — explicit v1 non-goals (don't implement without a scope talk):**
- **`/compact` / `/clear`** (§3.6) — easy to drive + wire-detectable; v1.1 on
  product priority.
- **Mid-turn message queueing** — Claude queues prompts typed while it works;
  v1.1. Note the coupling: a queued command suppresses the interrupt
  auto-restore (§3.2), and a *submit-interrupt* skips the synthetic
  `[Request interrupted by user]` marker — so adding queueing changes interrupt
  detection.
- **MCP-server OAuth/auth prompts** — *not* selected for v1. AO's v1 assumes MCP
  servers are pre-authenticated at launch; a first-use auth prompt would block
  and AO surfaces it as a setup error (same stall detector as §4) rather than
  driving the dialog.
- **Trust-dialog drive fallback** — *not* selected for v1. Trust is pre-seeded at
  launch (§3.5); if it can't be, that's a setup error AO surfaces — AO does not
  drive the first-run trust prompt in v1.
- **Login / mid-session re-auth** — *not* selected for v1. Subscription OAuth
  tokens are long-lived, but a mid-session expiry/re-login prompt is a known v1
  gap: AO surfaces it (auth error on the wire = 401 / SSE `error`) for the user
  to resolve out-of-band; it does not drive the login flow.

**Historical open probes from this older spec, not production gates:**
- **Multi-line input** (§3.7) — confirm bracketed-paste injects a multi-line
  prompt cleanly on `2.1.150`.
- **Late-interrupt partial-content persistence** (§3.2 case a) — source says the
  partial turn stays; confirm on the wire with an interrupt *after* text deltas
  (the probe here interrupted during thinking).
- **Interrupt-during-tool-execution wire shape** (§3.2 case b) — source-derived
  (synthetic "Interrupted by user" tool_result + `[Request interrupted by user…]`);
  confirm by interrupting during a slow `Bash` and inspecting the next request's
  `messages` if this path becomes load-bearing.

---

## 6. Evidence index (this dir)

| Claim | Evidence |
|---|---|
| Wire stream → headless-equivalent AO events; byte-identical tool_results | `ao_transform.py`; FINDINGS §11 |
| Multi-turn submit + turn detection | `drive_multi.py` |
| Rewind restores files on disk; scope sub-choice; wire truncation `9→5`; transcript fork | `probe_rewind.py` → `artifacts/ao-rewind-marks.json`, `artifacts/ao-rewind-pty.log` |
| Interrupt = aborted request (no `message_stop`); session usable after | `probe_interrupt.py` → `artifacts/ao-interrupt-marks.json` |
| Rewind file-history + auto-restore semantics | source: `fileHistory.ts` (`fileHistoryCanRestore`:399), `commands/rewind/index.ts`, `REPL.tsx` (onCancel:2106, partial-preserve:2124) |
| Selector navigation: cursor starts at bottom; `k`/`j` keys; `selectableUserMessagesFilter` | source: `MessageSelector.tsx` (selectedIndex:67, moveUp/Down:257, filter:767); `keybindings.json` (MessageSelector) |
| `canRestoreCode` = tracked-edit tools only (not bash) | source: `fileHistory.ts`; tracked by `FileEditTool`/`FileWriteTool`/`NotebookEditTool` (+ bash `sed` edge in `BashTool.tsx`:392) |
| Interrupt: two cases; synthetic markers; Esc-vs-submit | `probe_interrupt.py` (streaming); source `query.ts` (abort:1015, markers:1047/1502), `messages.ts` (207-209, 545), `useCancelRequest.ts`:95 |
| Plan-mode entry: combo `plan`+`skip-perms` → **bypass** (not plan); `plan` alone → **plan**. *(Both verdicts probed via FS ground truth. The further "plan-alone leaves bypass unavailable at the approval dialog" claim is **source-confirmed, not driven to the dialog** — the probe stopped at the refusal, never reaching approval under plan-alone.)* | `probe_planmode.py` → `artifacts/ao-planprobe-result.json` (combo wrote `foo.txt`; planonly didn't); resolver: `permissionSetup.ts` (orderedModes push:725/740, first-valid+break:777–795); bypass-gated-on-flag: `useReplBridge.tsx`:437 (not exercised under plan-alone) |
| Plan-mode end-to-end: bypass launch → 3× shift+tab → plan (footer-confirmed) → `ExitPlanMode` → `\r` idx0 → **back to bypass**, file written, no prompt; auto unavailable on this account | `probe_planmode2.py` → `artifacts/ao-planprobe2-result.json`, `artifacts/ao-planprobe2.log` (approval dialog verbatim) |
| Plan-approval **reject = single Esc** → stays in plan, no file, session re-plannable; option indices shift with `showUltraplan`/`showClearContext` so reject is not index-driven | `probe_planmode3.py` → `artifacts/ao-planprobe3-result.json`; source: `ExitPlanModePermissionRequest.tsx` (handleCancel:521–531, `no`-needs-feedback:476–479, buildPlanApprovalOptions:674–746, showClearContext:137) |
| Plan-mode tool semantics: read-only `Bash`/plan-file edits **run**, workspace mutations suppressed (tool_use-emitted ≠ executed) | `probe_planmode3.py` (plan turn ran `Bash(ls)`, `Write` suppressed); `probe_planmode.py` planonly (model refused `foo.txt`) |
| Plan mode: ExitPlanMode tool_use signal, value→mode mapping, approved tool_result, mode cycle, bypass flag/killswitch | source: `ExitPlanModePermissionRequest.tsx` (ResponseValue:50, value→mode:431, options:674), `ExitPlanModeV2Tool.ts` (restore:361, approved msg:483), `getNextPermissionMode.ts` (cycle 38–79), `permissionSetup.ts` (bypass flag:725, killswitch:778), `messages.ts`:3287 |
| Footer mode-indicator line = bounded structured PTY signal (`⏵⏵ bypass permissions on` / `⏸ plan mode on` / `accept edits on`) | `probe_planmode2.py`/`probe_planmode3.py` PTY logs (footer read drives shift+tab confirmation) |
| Trust pre-seed via `projects[path].hasTrustDialogAccepted` | source: `config.ts`:697 (`checkHasTrustDialogAccepted` walks cwd→parents) |
| TUI interaction-context map | `~/.claude/keybindings.json` |
| Permissions print-only; static policy works interactively | FINDINGS §11; `perm_world.py`; `probe_rewind.py` (acceptEdits) |

> Spike branch `spike/claude-mitm`; **not for merge**. Per
> `docs/references/spike-policy.md`, port the *learning* (this spec + the
> transform rules), not the throwaway Python.
