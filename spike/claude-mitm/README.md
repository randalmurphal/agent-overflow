# Claude Code Local Gateway Spike

Throwaway spike answering one question for Agent Overflow (AO): can AO host a
real, local, user-authenticated interactive `claude` process and recover the
same structured signals it gets from headless `stream-json`, without turning
interactive mode into an unsupported protocol client?

**Finding: yes, with no binary patching and no TLS interception.** Point Claude
at a local logging gateway via the documented `ANTHROPIC_BASE_URL`; it captures
the full raw Anthropic Messages API SSE stream (text / thinking / tool_use
deltas, usage, stop_reason, rate-limit headers). Pair that with Claude Code
hooks and the `~/.claude` transcript for control, tool results, and durable
backfill, and AO can reconstruct a superset of the headless `stream-json` shape
for its own local UI.

**Posture update (2026-06-05).** The production interpretation is a local
desktop shell for the user's own Claude Code process, not a hosted service, not
a Claude.ai credential broker, and not an automation layer pretending to be a
human. The implementation plan is:

- use the documented local gateway path (`ANTHROPIC_BASE_URL`) for wire capture;
- use hooks, transcript tailing, and PTY input for the rest of the product
  surface;
- do **not** ship transparent TLS interception as a fallback;
- do **not** ship TLS/HTTP fingerprint parity as a product requirement;
- do **not** replicate the Claude Code remote-control cloud protocol;
- launch in `default` permission mode with the hook relay as the gate, not
  `--dangerously-skip-permissions` as the default posture.

If Anthropic ever blocks OAuth through custom base URLs, AO treats that as an
unsupported path and falls back to documented API / Agent SDK / explicit vendor
guidance, not TLS MITM or first-party protocol impersonation.

Relevant official references checked 2026-06-05: Claude Code legal/auth guidance
(`https://code.claude.com/docs/en/legal-and-compliance`), LLM gateway support
(`https://code.claude.com/docs/en/llm-gateway`), authentication precedence
(`https://code.claude.com/docs/en/authentication`), and enterprise proxy/custom
CA support (`https://code.claude.com/docs/en/network-config`). The safe reading:
gateways are documented; routing other users through Free/Pro/Max credentials,
transparent interception after a block, or first-party client impersonation is
not the product posture.

**Update (2026-05-30) — the hook channel.** A third tap makes this far stronger
than "drive PTY, watch wire": Claude Code **hooks** (`settings.json` in an
isolated `CLAUDE_CONFIG_DIR`) are a *structured control channel* that runs in
interactive mode. `PreToolUse` recovers per-call permissions **and can answer
`AskUserQuestion` with zero keystrokes**; `ExitPlanMode` capture, subagent
lifecycle, and session metadata all come through hooks. This **eliminates the
fragile Ink-widget keystroke-driving** the original spec relied on. Every
capability AO uses in `stream-json` is now **live-confirmed** recoverable on
`claude 2.1.158`.

A 2026-05-30 scenario sweep added the **completion-signal** detail and closed the
remaining unknowns: `PostToolUse` fires **only on success** — a failed tool (non-zero
exit / MCP `isError`) fires the **separate `PostToolUseFailure`**, so AO must register
both — plus live confirmation of **multi-question** answers (matched by question text,
not position), **MCP tools** through the same hooks, backgrounded **subagent + Bash
completion** signals (subagent via `SubagentStop`; bg-Bash via the wire
`<task-notification>`), **partial-text** recovery on interrupt, and a **70 s
human-approval hold** under a configured hook `timeout`.

The governing design rule: AO sees/controls Claude through three taps — **wire**
(local gateway SSE, token-level output, user's Claude Code auth), **hooks**
(structured control + lifecycle + tool I/O + permission gating), and
**transcript** (durable backfill).
**Drive input through the PTY (text, Esc, attachment paste); gate/answer/observe
through hooks + wire — never by scraping TUI text.**

**Update (2026-06-05) — fingerprint work is historical evidence, not the product
plan.** The spike validated a version-pinned Bun outbound sidecar that can mimic
Claude Code's TLS/HTTP fingerprint, but AO should not ship that as a production
requirement. A normal gateway/proxy fingerprint is consistent with the documented
gateway model; trying to preserve Claude's byte-level fingerprint makes the design
look like evasion even when the traffic is local and user-authenticated. Sections
12-13 in [`FINDINGS.md`](FINDINGS.md) remain as technical record only.

> ⚠️ Make-or-break rules: (1) a gate hook **killed at its timeout reverts to
> Claude's native permission flow** — which AO can't answer (the turn gets stuck), or
> in a permissive posture runs the tool unreviewed — so the relay must own its
> deadline and always return an explicit decision; (2) hooks are **silently skipped
> until the workspace is trusted**; (3) **`PostToolUse` is success-only** — a failed
> tool fires the separate **`PostToolUseFailure`**, so AO must register both or failed
> tools look like they never finished. All detailed in the coverage map.

## Start here

1. [`HOOKS_COVERAGE_MAP.md`](HOOKS_COVERAGE_MAP.md) — **authoritative.** The
   complete capability coverage map (hook-channel architecture), the hook
   contract, the critical design rules, open items, and the AO integration
   sketch. Every claim tagged by confidence + linked to its probe.
2. [`FINDINGS.md`](FINDINGS.md) — *why* the local gateway architecture works,
   which interception paths were tried, and which historical paths are no-go for
   production.
3. [`INTERACTIVE_DRIVING_SPEC.md`](INTERACTIVE_DRIVING_SPEC.md) — the earlier
   end-to-end driving spec. **Superseded for control operations by the coverage
   map** (its Select-widget/shift+tab driving is no longer needed); still the
   reference for the wire/transcript mechanics and §6 evidence index.

## Layout

| Path | What it is |
|---|---|
| `proxy/main.go` | The spike wire-capture gateway. Records request/response bodies as JSONL for development evidence and redacts credential headers (auth/cookie/`*-token`/`*api-key`). Production should persist AO-normalized events by default; raw body capture is dev-only, local-only, short-retention, and excluded from logs, panics, crash reports, and commits. |
| `ao_transform.py` | **The portable artifact.** Transforms a proxy capture of interactive `/v1/messages` into AO's event stream — the logic that ports to Go. |
| `analyze.py` | Three-way diff: proxy capture vs `~/.claude` transcript vs a headless `stream-json` reference. Backs the transform-equivalence claim (FINDINGS §11). |
| `drive_interactive.py` / `drive_multi.py` | PTY drivers: prove single- and multi-turn interactive sessions route `/v1/messages` through the proxy and can be driven + exited cleanly. |
| `probe_rewind.py` | `/rewind` revert flow — selector navigation, scope sub-choice, on-disk file restore, wire truncation, transcript fork. |
| `probe_interrupt.py` | Esc-while-working — aborted SSE (no `message_stop`), session-usable-after. |
| `probe_planmode.py` / `2` / `3` | Plan mode: launch-flag resolution (combo → bypass, not plan), end-to-end shift+tab→plan→`ExitPlanMode`→approve, and the Esc-reject path. |
| `perm_probe.py` / `perm_world.py` | Permission surface: confirm `can_use_tool` / `--permission-prompt-tool` is print-only, and that a static launch policy works interactively. |
| **`aoprobe.py`** | **Shared hook-channel harness.** Seeds an isolated `CLAUDE_CONFIG_DIR` (local copied config for the test harness + pre-trusted cwd + hook settings; optional `permissions.defaultMode`/`ask` rules), forks `claude` in a PTY at the proxy, pumps + detects outcomes on the filesystem/wire. **Strips inherited `CLAUDE*`/`ANTHROPIC*` env** so the child starts in a clean (non-auto-accept) posture; detects native prompts from a **de-ANSI'd** buffer; teardown sends **Esc, never Enter** (so it can't accidentally accept an open prompt). Backs all `probe_hook_*` scripts. |
| **`hook_relay.py`** | The hook command Claude runs: logs each payload, optionally blocks, returns the decision (allow/deny/ask, or answers `AskUserQuestion` via `updatedInput`). The thing AO's real relay becomes. |
| **`probe_hook_permission.py`** | PreToolUse per-call permission round-trip: allow runs (no TUI prompt), deny blocks, hook holds the tool while it deliberates. |
| **`probe_hook_answer.py`** | **AskUserQuestion answered via hook** + `updatedInput.answers`, **0 keystrokes** — the Select-widget elimination. |
| **`probe_hook_special.py`** | `ExitPlanMode` (plan capture+approve) and `AskUserQuestion` capture via PreToolUse. |
| **`probe_hook_coverage.py`** | Which hooks fire for a `Task` subagent + a backgrounded Bash (`SubagentStop`, agent_id tagging; the launch tool surfaces as `Agent`). |
| **`probe_hook_bgcomplete.py`** | **Completion signals** for a backgrounded subagent (via `SubagentStop` + `last_assistant_message`) and a backgrounded Bash (via the wire `<task-notification>` — drives a follow-up turn to flush it). Confirms **no hook fires at bg-Bash completion** (full timeline, both post-hooks registered) and correlates `backgroundTaskId`↔`task-id`. |
| **`probe_hook_bashpid.py`** | **`stop_task` feasibility:** proves a backgrounded Bash runs as a **live descendant of `claude`** (`claude→zsh -c→sleep`) at the `PostToolUse(Bash)` dispatch — so AO can bind `backgroundTaskId→PID` (process-tree snapshot-diff) and `SIGTERM` it: deterministic per-task kill, no model/TUI. PID is **not** in the hook payload. Linux/WSL `/proc`; double-fork daemons escape. |
| **`probe_hook_failcomplete.py`** | **Failure completion:** a non-zero exit fires **`PostToolUseFailure`** (not `PostToolUse`), carrying `error`="Exit code N\n<stderr>", `is_interrupt`, `duration_ms`. Proves AO must register both. |
| **`probe_hook_mcp.py`** + **`mcp_server.py`** | MCP tools through the hook channel: surface `mcp__`-prefixed (user-scope `mcp__<server>__<tool>`; plugin servers e.g. context7 `mcp__plugin_<plugin>_<server>__<tool>` — match the prefix), gate per-call (deny blocks), success→`PostToolUse` / `isError`→`PostToolUseFailure` (asserted per-event). `mcp_server.py` is a minimal local stdio server (one `ping` tool). |
| **`probe_hook_multiq.py`** | Multi-question `AskUserQuestion` (1–4 in one call) answered via the `answers` map, **0 keystrokes**; `AO_MULTIQ_REVERSE=1` inserts answers in reverse order to prove matching is **text-keyed, not positional**. |
| **`probe_hook_partialtext.py`** | Interrupt **mid-text-stream**: partial deltas reach the wire before Esc (primary recovery), and persist to the transcript (bonus). |
| **`probe_hook_interrupt.py`** | Interrupt during tool execution (FIFO-block): `[Request interrupted by user for tool use]` marker, **neither `PostToolUse` nor `PostToolUseFailure` fires**, side-effect not run. |
| **`probe_hook_failopen.py`** | 2×2 matrix (gate hook × `ask`-rule-gated cmd) proving a **killed** gate hook **falls through to the native permission prompt** (not force-execute), a surviving `deny` blocks (held 6 s) and outranks an `ask` rule, and that default mode + a clean env genuinely prompt. Supersedes the old `probe_hook_timeout.py`. |
| **`probe_hook_longtimeout.py`** | A configured hook `timeout: 120` honors a **70 s** hold (the held `allow` then applies) — the human-in-the-loop approval window; no ~30 s cap. |
| **`probe_hook_attach.py`** | Temp-file + bracketed-paste path yields a real base64 `image` content block. |
| **`probe_hook_dangerpath.py`** | **Bypass-immunity:** a `Write` to a `DANGEROUS_FILES`/`DANGEROUS_DIRECTORIES` path (`.zshrc`, `.mcp.json`, `.git/` …) is held by a **native dialog even when the PreToolUse hook returns `allow`** — the one `relay-is-posture` exception. Within-session contrast vs a benign write rules out config drift. |
| **`probe_hook_dangerdrive.py`** | Proves AO *can* mechanically **drive** that bypass-immune dialog via a PTY digit (`1`→yes); the held write then proceeds (`PostToolUse` + file written). Decision is **route-to-human-then-relay-digit** (never auto-confirm, never `2`); this only establishes mechanical viability. |
| **`probe_hook_escrevert.py`** | **Think-only Esc-revert** characterization: the Esc **aborts `/v1/messages`** (no `message_stop` + `upstream_read` cancel — the same signature as an interrupt, thinking-only content), so revert is **wire-detectable, not TUI-only**; `UserPromptSubmit` + an orphaned transcript `user` row fire at submit; no `Stop`, no `[Request interrupted by user]` row. |
| **`probe_hook_revertcontext.py`** | **Revert re-entry (settled ×2):** after a think-only revert, a follow-up prompt's request `messages[]` is `[user:B, system]` with the reverted prompt **absent** → **DROPPED** from live context (not re-sent). AO drops it live; the durable transcript keeps an orphaned `user` row AO filters on backfill. |
| **`probe_composer_clear.py`** | **Composer-clear before Send (2.1.170):** a think-only Esc-revert restores the just-sent prompt back **into the composer**, so AO's next paste-and-submit must empty it first or the leftover **fuses** with the new paste. Findings: **Ctrl-U is line-scoped** (one composer line per press); a multi-line paste **collapses to a single `[Pasted text #N +K lines]` chip**; **excess Ctrl-U on an empty composer is a no-op** (composer still accepts input). → `claudetui.Send` prepends a fixed run of 16 Ctrl-U (covers any realistic restored prompt, harmless when already empty). |
| **`probe_cold_submit.py`** | **Cold-start first-message submit (2.1.170):** the opening Send types into the composer but **does not submit**; the second message submits fine. Root cause: on a cold launch claude isn't draining stdin yet, so AO's `[paste][60ms][CR]` writes accumulate and claude reads `…\x1b[201~\r` in **one chunk** → the CR is swallowed with the paste; once warm, paste and CR are separate reads → submit. Detection is **credit-free** (local mock at `ANTHROPIC_BASE_URL` flags a `POST /v1/messages` whose body carries the prompt; excludes claude's startup `"quota"` probe). Results: send-immediately **0/3**, gate-on-`\x1b[?2004h` **0/3** (fires ~0.29 s, too early), **idle-gate 3/3** (init burst landed + stream idle ≥350 ms), two-message **msg1 0/2, msg2 2/2** (reproduces the report). → `claudetui.Send` gates the first send on `ptyReadyForSend` (≥512 B seen + ≥400 ms idle), latched. Mirror the real config's `skipDangerousModePermissionPrompt`/`bypassPermissionsModeAccepted` or the bypass-accept modal blocks the composer (not the bug). |
| **`probe_thinking_title.py`** | **Thinking-on-the-wire + title-gen classification (2.1.170, real subscription).** Drives a thinking turn + a mermaid turn through a Go capturing proxy that mirrors `gateway.go` (plain forward to `api.anthropic.com`; source in `aocap/`, built on run). **Q1 (the fix):** launched with `--thinking-display summarized` the interactive TUI puts `thinking.display:"summarized"` on its `/v1/messages` request and the response streams `thinking_delta` **text** (124 chars on a math turn); the `AO_NO_THINK_DISPLAY=1` control streams **none** (opus-4-8 defaults to `omitted` — empty thinking block + signature only, the user's symptom). `--verbose` is **not** needed (the TUI reconstructs from the wire, not stdout). → `claudetui` launch now passes `--thinking-display summarized` (mirrors headless). **Q2 (phantom):** the TUI's internal title-gen request is `n_tools=0` + system *"Generate a concise … title"* + `<session>` wrapper → `classify.go` correctly drops it as `classAuxiliary` (no leak on 2.1.170); titles came back as JSON `{"title":…}`. Disproved the "title-gen leaked the phantom assistant turn" theory. |
| **`probe_subagent_correlation.py`** | **Subagent ↔ parent Agent correlation (2.1.170, real subscription).** Drives two subagents in parallel and self-checks the five facts `claudetui` nesting relies on: (1) a subagent's `/v1/messages` carries header **`X-Claude-Code-Agent-Id`** (absent on main) — the discriminator + correlation key; (2) the **forward-live join is content**: a subagent's first user message contains its Agent `input.prompt` verbatim, matching exactly one launch (ordering-independent → `resolveSubagentParent`); (3) **`PostToolUse(Agent).tool_response.agentId`** equals the wire agent id but only at Agent *completion* (authoritative, too late to nest live); (4) **`SubagentStart` is NOT the join** — it fires early but carries only `agent_id`, no parent tool_use_id; (5) the launch tool_use is named **`Agent`** (`Task` on older builds). → `claudetui` routes header-tagged `classAgent` requests to a nesting reconstruction that tags envelopes with `parent_tool_use_id` and emits no subagent `result`. |
| **`probe_hook_steer.py`** | **Mid-turn steering** captured off the hook channel: consumed same-turn; transcript `queue-operation` (`enqueue`→`remove`) + `queued_command` attachment + wire `system` message; **`UserPromptSubmit` does NOT fire**; submit needs render-then-`\r` (paste/enter coalescing). |
| **`probe_compact.py`** | **Context compaction** (drives N short haiku turns → `/compact`, 2.1.170): closes the coverage map's one never-probed omission. The transcript carries a typed **`system/compact_boundary`** row (+ `compactMetadata` `preTokens`/`postTokens`/preserved uuids) and an **`isCompactSummary`** summary row — the *same* shape headless parses, no wire inference. **`PreCompact`** (fires before the "enough messages" check) + **`PostCompact`** (carries `compact_summary`) corroborate; raw wire shows the summarizer `POST` (distinct `max_tokens`) + `messages[]` collapse. Also surfaced the one-time **bypass-acknowledgment Select** (`--dangerously-skip-permissions` default row = "No, exit"). |
| **`probe_launchposture.py`** | **Production full-access launch posture** (2.1.170): launches with the real provider flags (`--permission-mode bypassPermissions --allow-dangerously-skip-permissions`) + the lone `AskUserQuestion` hook injected via the **`--settings` flag** (config-dir `settings.json` empty). Confirms **flag-injection works** (a flag-only hook fired) → AO can use the user's **real config** + inject the one hook, no isolated config dir. Pairs with the binary-confirmed pre-seed keys `hasTrustDialogAccepted` + **`bypassPermissionsModeAccepted`** for a clean launch. |
| **`ja3_diff.py`** | **Historical TLS fingerprint probe (FINDINGS §12):** captures each runtime's ClientHello at a raw socket; proves version-pinned **Bun 1.3.14 == claude** byte-for-byte while Go/Node/older-Bun differ. Not a production requirement. |
| **`probe_bun_provenance.py`** | **Historical provenance probe (FINDINGS §12):** the **stock** Bun 1.3.14 release reproduces claude's ClientHello even though claude embeds a different build. Useful technical evidence; not part of the product plan. |
| **`probe_tls_clients.py`** | ClientHello of each candidate **outbound** client (fetch / node:https / raw tls.connect / Bun.connect); only the **high-level** clients carry claude's OCSP(5)+SCT(18) extensions — bare sockets can't match. |
| **`probe_h1_headerforms.py`** | **Historical HTTP/1.1 proof (FINDINGS §12):** Bun **`fetch` + a plain header object** reproduces claude's complete wire header block byte-for-byte (case-sensitive sort + regenerated framing); `node:http`/`node:https` lowercases. Not a production requirement. |
| **`probe_h1_serialize.py`** | Earlier forwarder-based h1 probe; **superseded** by `probe_h1_headerforms.py`, kept because it surfaced the `Bun.serve` ingest-lowercasing trap (the inbound read must preserve raw casing). |
| **`probe_h1_interactive.py`** | Confirms **interactive** claude emits the identical application header block as headless `claude -p` — the byte-for-byte h1 match applies to the live path AO taps. |
| `captures/cap-*.jsonl` | Raw proxy wire captures (the `--log` output) — the ground truth the transform/analysis run against. One per probe (`-headless` is the reference for the equivalence check). |
| `artifacts/ao-*` | Probe run outputs: PTY terminal logs (`*.log`) + distilled result/marks JSON. Referenced by spec §6. |
| `preload.js`, `bridge_logger.py`, `rc_probe.py` | Superseded side-investigations — the Node `--require` preload (the native Bun binary ignores it, FINDINGS §1) and the Remote Control bridge probe (FINDINGS §10). Kept for the record. |

## Re-running

```sh
PRIVATE_TMP="$(mktemp -d)"
umask 077
go build -C proxy -o "$PRIVATE_TMP/ao-proxy" .
"$PRIVATE_TMP/ao-proxy" --listen 127.0.0.1:8090 --log "$PRIVATE_TMP/cap.jsonl" &

# Current production-path probes: gateway + default-mode hook relay.
AO_BASE_URL=http://127.0.0.1:8090 AO_CAP_LOG="$PRIVATE_TMP/cap.jsonl" python3 probe_hook_permission.py
AO_BASE_URL=http://127.0.0.1:8090 AO_CAP_LOG="$PRIVATE_TMP/cap.jsonl" python3 probe_hook_failopen.py
AO_BASE_URL=http://127.0.0.1:8090 AO_CAP_LOG="$PRIVATE_TMP/cap.jsonl" python3 probe_hook_failcomplete.py
```

Raw captures and transcripts are secret-bearing. Use a private temp directory
(`umask 077`) for reruns, delete it after probing, and do not commit fresh raw
captures unless they have been reviewed and intentionally frozen as spike
evidence. Historical bypass/shift-tab plan probes such as `probe_planmode2.py`
remain evidence only; they are not production-path rerun gates.

## Status

Per [`docs/references/spike-policy.md`](../../docs/references/spike-policy.md)
this is a **throwaway spike, not for merge to `main`** — the *learning* ports
into AO's real Claude provider package; this tree is the durable record of what
was probed and verified (proxy/PTY work `claude 2.1.150`, 2026-05-26/27;
hook-channel coverage map + permission-posture and turn-lifecycle (revert /
steering) probes `claude 2.1.158`, 2026-05-30/31; user's local Claude Code auth
throughout).
