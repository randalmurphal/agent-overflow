# Claude Code MITM Spike

Throwaway spike answering one question for Agent Overflow (AO): AO wants to move
from headless `stream-json` (which **bills API tokens**) to driving the
**interactive** TUI (which runs on the already-paid **Pro/Max subscription**).
**Can AO drive *interactive* `claude` and still recover the same token-level
structured stream it gets from headless?** (A cost + coverage/reliability move —
**not** a ToS workaround; headless is still permitted.)

**Finding: yes, with no binary patching and no TLS interception.** Point Claude
at a local logging **reverse proxy** via `ANTHROPIC_BASE_URL`; it captures the
full raw Anthropic Messages API SSE stream (text / thinking / tool_use deltas,
usage, stop_reason, rate-limit headers). Pair that with the `~/.claude`
transcript for clean tool results and you reconstruct a superset of the headless
`stream-json` shape — over the subscription-authed path that survives the cutoff.

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
(proxy SSE, token-level output, OAuth-authed), **hooks** (structured control +
lifecycle + tool I/O + permission gating), and **transcript** (durable backfill).
**Drive input through the PTY (text, Esc, attachment paste); gate/answer/observe
through hooks + wire — never by scraping TUI text.**

**Update (2026-05-30) — fingerprint parity.** Because the proxy *re-originates* the
upstream connection (claude→proxy is plaintext `http://`; proxy→Anthropic is a fresh
TLS connection), its outbound leg presents its **own runtime's** TLS/HTTP fingerprint
to Anthropic's edge — not claude's. A Go/OpenSSL JA3 on a "claude" OAuth session is a
detectable mismatch that could false-flag genuine subscription use. Measured fix:
open that leg from a **version-pinned Bun `fetch`** (`Bun/1.3.14`, claude's own
runtime), which reproduces both claude's TLS ClientHello *and* its HTTP/1.1 header
block byte-for-byte — not spoofed, genuinely Bun/BoringSSL. So the production shape is
**Go inbound/capture + a small Bun outbound sidecar**. Full validation, the
construction rules (preserve raw header casing; strip framing), and residual caveats
in [`FINDINGS.md`](FINDINGS.md) §12.

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
2. [`FINDINGS.md`](FINDINGS.md) — *why* the proxy architecture: the four
   interception paths tried, why the reverse proxy won, what the binary forecloses.
3. [`INTERACTIVE_DRIVING_SPEC.md`](INTERACTIVE_DRIVING_SPEC.md) — the earlier
   end-to-end driving spec. **Superseded for control operations by the coverage
   map** (its Select-widget/shift+tab driving is no longer needed); still the
   reference for the wire/transcript mechanics and §6 evidence index.

## Layout

| Path | What it is |
|---|---|
| `proxy/main.go` | The logging reverse proxy. Records request/response bodies as JSONL; redacts credential headers (auth/cookie/`*-token`/`*api-key`). The whole mechanism. |
| `ao_transform.py` | **The portable artifact.** Transforms a proxy capture of interactive `/v1/messages` into AO's event stream — the logic that ports to Go. |
| `analyze.py` | Three-way diff: proxy capture vs `~/.claude` transcript vs a headless `stream-json` reference. Backs the transform-equivalence claim (FINDINGS §11). |
| `drive_interactive.py` / `drive_multi.py` | PTY drivers: prove single- and multi-turn interactive sessions route `/v1/messages` through the proxy and can be driven + exited cleanly. |
| `probe_rewind.py` | `/rewind` revert flow — selector navigation, scope sub-choice, on-disk file restore, wire truncation, transcript fork. |
| `probe_interrupt.py` | Esc-while-working — aborted SSE (no `message_stop`), session-usable-after. |
| `probe_planmode.py` / `2` / `3` | Plan mode: launch-flag resolution (combo → bypass, not plan), end-to-end shift+tab→plan→`ExitPlanMode`→approve, and the Esc-reject path. |
| `perm_probe.py` / `perm_world.py` | Permission surface: confirm `can_use_tool` / `--permission-prompt-tool` is print-only, and that a static launch policy works interactively. |
| **`aoprobe.py`** | **Shared hook-channel harness.** Seeds an isolated `CLAUDE_CONFIG_DIR` (copied creds + pre-trusted cwd + hook settings; optional `permissions.defaultMode`/`ask` rules), forks `claude` in a PTY at the proxy, pumps + detects outcomes on the filesystem/wire. **Strips inherited `CLAUDE*`/`ANTHROPIC*` env** so the child starts in a clean (non-auto-accept) posture; detects native prompts from a **de-ANSI'd** buffer; teardown sends **Esc, never Enter** (so it can't accidentally accept an open prompt). Backs all `probe_hook_*` scripts. |
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
| **`probe_hook_steer.py`** | **Mid-turn steering** captured off the hook channel: consumed same-turn; transcript `queue-operation` (`enqueue`→`remove`) + `queued_command` attachment + wire `system` message; **`UserPromptSubmit` does NOT fire**; submit needs render-then-`\r` (paste/enter coalescing). |
| **`ja3_diff.py`** | **TLS fingerprint (FINDINGS §12):** captures each runtime's ClientHello at a raw socket; proves version-pinned **Bun 1.3.14 == claude** byte-for-byte while Go/Node/older-Bun differ. Why the proxy's outbound leg must be Bun. |
| **`probe_bun_provenance.py`** | **Provenance keystone (FINDINGS §12):** the **stock** Bun 1.3.14 release (sha256 `9fd36f87…`, build `0d9b296af`) reproduces claude's ClientHello even though claude embeds a *different* build (`521eedd6d`) → fingerprint is version-determined; AO downloads stock Bun, **no extraction** from the claude binary. |
| **`probe_tls_clients.py`** | ClientHello of each candidate **outbound** client (fetch / node:https / raw tls.connect / Bun.connect); only the **high-level** clients carry claude's OCSP(5)+SCT(18) extensions — bare sockets can't match. |
| **`probe_h1_headerforms.py`** | **HTTP/1.1 proof (FINDINGS §12):** Bun **`fetch` + a plain header object** reproduces claude's complete wire header block byte-for-byte (case-sensitive sort + regenerated framing); `node:http`/`node:https` lowercases. |
| **`probe_h1_serialize.py`** | Earlier forwarder-based h1 probe; **superseded** by `probe_h1_headerforms.py`, kept because it surfaced the `Bun.serve` ingest-lowercasing trap (the inbound read must preserve raw casing). |
| **`probe_h1_interactive.py`** | Confirms **interactive** claude emits the identical application header block as headless `claude -p` — the byte-for-byte h1 match applies to the live path AO taps. |
| `captures/cap-*.jsonl` | Raw proxy wire captures (the `--log` output) — the ground truth the transform/analysis run against. One per probe (`-headless` is the reference for the equivalence check). |
| `artifacts/ao-*` | Probe run outputs: PTY terminal logs (`*.log`) + distilled result/marks JSON. Referenced by spec §6. |
| `preload.js`, `bridge_logger.py`, `rc_probe.py` | Superseded side-investigations — the Node `--require` preload (the native Bun binary ignores it, FINDINGS §1) and the Remote Control bridge probe (FINDINGS §10). Kept for the record. |

## Re-running

```sh
go build -C proxy -o /tmp/ao-proxy .
/tmp/ao-proxy --listen 127.0.0.1:8090 --log /tmp/cap.jsonl &
# probes read AO_BASE_URL / AO_CAP_LOG; each forks its own PTY:
AO_BASE_URL=http://127.0.0.1:8090 AO_CAP_LOG=/tmp/cap.jsonl python3 probe_planmode2.py
```

Scripts default to `/tmp` for scratch + capture output; the committed
`captures/` and `artifacts/` are the frozen run that backs the spec, so re-runs
regenerate to `/tmp` rather than overwriting the record.

## Status

Per [`docs/references/spike-policy.md`](../../docs/references/spike-policy.md)
this is a **throwaway spike, not for merge to `main`** — the *learning* ports
into AO's real Claude provider package; this tree is the durable record of what
was probed and verified (proxy/PTY work `claude 2.1.150`, 2026-05-26/27;
hook-channel coverage map + permission-posture and turn-lifecycle (revert /
steering) probes `claude 2.1.158`, 2026-05-30/31; subscription OAuth
throughout).
