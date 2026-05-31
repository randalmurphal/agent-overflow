# Claude Code MITM Spike — Findings

**Goal.** AO moves from headless `stream-json` (`claude -p --output-format
stream-json`), which **bills API tokens**, to driving the **interactive** TUI,
which runs on the already-paid Pro/Max subscription. We want to drive Claude in
interactive mode but still recover the same **token-level structured stream** we
get from headless, for Agent Overflow's own UI. (A cost + coverage/reliability
move — **not** a ToS workaround; headless is still permitted.)

**Bottom line.** It works, and *not* by modifying the binary. Pointing Claude at
a local logging **reverse proxy** via `ANTHROPIC_BASE_URL` captures the full
raw Anthropic Messages API SSE stream — token-level text/thinking/tool_use —
with **no TLS interception and no binary patching**. Pair that with the
`~/.claude` transcript for clean tool results and you can reconstruct (a
superset of) the headless `stream-json` shape.

Date: 2026-05-26 … 2026-05-31. Binary: Claude Code `2.1.150` → `2.1.158` (Bun
v1.3.14 standalone ELF). §1–11 are the capture/signal story; **§12** validates the
outbound TLS+HTTP fingerprint (version-pinned Bun `fetch`); **§13** validates the
Go-inbound + Bun-sidecar topology end-to-end (streams + fingerprint + gzip handling).

---

## 1. The binary forecloses the preload approach

- `~/.local/share/claude/versions/2.1.150` is a **238 MB Bun v1.3.14 compiled
  standalone ELF**, not a Node script.
- Empirically confirmed: it **ignores `NODE_OPTIONS=--require`** and
  **`BUN_INSPECT_PRELOAD`**. So the original `preload.js` (a Node `--require`
  hook) cannot load into it. It only works against the npm `cli.js` under real
  Node — a distribution we've migrated off and which is subject to the same
  subscription cutoff. **`preload.js` is out of scope.**
- `BUN_INSPECT` *is* compiled in (the debugger tried to start), so a CDP
  attach is a viable in-process path — but we didn't need it.

## 2. Four interception paths — and which won

| Path | Mechanism | Binary mod? | Granularity | Verdict |
|------|-----------|-------------|-------------|---------|
| **Reverse proxy** (chosen) | `ANTHROPIC_BASE_URL=http://127.0.0.1:PORT` | none (env) | token-level | ✅ simplest, proven |
| Transparent MITM | `HTTPS_PROXY` + `NODE_EXTRA_CA_CERTS` | none (env) | token-level | ✅ fallback (see §7) |
| Bun inspector | attach CDP via `BUN_INSPECT` | none (env) | anything | viable, unused |
| Node preload | `NODE_OPTIONS=--require` on npm `cli.js` | n/a | anything | ❌ native binary ignores it |
| Transcript tail | read `~/.claude/projects/*.jsonl` | none | per-message | ✅ structural backbone |

The binary heavily respects proxy/CA env (`HTTP(S)_PROXY`,
`CLAUDE_CODE_HTTP(S)_PROXY`, `NODE_EXTRA_CA_CERTS`) and honors
`ANTHROPIC_BASE_URL`. Auth is subscription OAuth (`~/.claude/.credentials.json`
→ `claudeAiOauth`), and the proxy run reported `apiKeySource: "none"` — i.e. the
captured stream is the **subscription-authed** path, exactly what survives the
cutoff.

**Closed door — do not reach for `--bare`.** The natural instinct to suppress
interactive noise (hooks, auto-memory, prefetches, the auxiliary calls in §7) is
the `--bare` "Minimal mode" flag — but the `2.1.150` `--help` is explicit that it
makes *"Anthropic auth … strictly `ANTHROPIC_API_KEY` or `apiKeyHelper` via
`--settings` (OAuth and keychain are never read)."* So `--bare` **forecloses
subscription auth entirely**: it would force the very API-key path the cutoff
pushes us off. The subscription path requires the normal (non-`--bare`) launch.

## 3. Proven architecture

```
                ANTHROPIC_BASE_URL=http://127.0.0.1:8090
  claude (TUI) ───────────────────────────────────────►  ao-proxy  ──► api.anthropic.com
       ▲                                                     │ (logs every request body
       │ PTY keystrokes (prompt, Enter)                      │  + streamed SSE response)
       │                                                     ▼
   driver / app                                       cap.jsonl  ── your UI renders from this
```

- The OAuth bearer is sent as a header regardless of base URL, so a plain
  HTTP reverse proxy transparently forwards it. **No cert, no CA, no TLS
  termination required** for the `http://` base-URL form.
- Confirmed identical transport in **headless and interactive** — interactive
  routed `HEAD /` + `POST /v1/messages` through the proxy the same way.

## 4. What the MITM gives you (token-level)

Raw `/v1/messages` SSE, captured verbatim:

```
message_start → content_block_start(text|thinking|tool_use)
  → content_block_delta (TOKEN-LEVEL: text_delta / thinking_delta / input_json_delta)
  → content_block_stop → message_delta(stop_reason, usage) → message_stop
```

Plus, in the **request bodies**, the full context Claude sends each turn:
system prompt blocks, the entire `tools` array (names + JSON schemas), and the
complete `messages` history. That's *more* than headless exposes.

## 5. Tool results: reconstructed from the next request (not a discrete event)

Tool *results* never appear as SSE events. They show up embedded in the **next**
request body. Reconstruction = diff request N+1's `messages` against N; the
newly appended `tool_result` blocks are the outputs of the turn that just ran.

Verified end-to-end (interactive run, prompt = `echo spike-interactive-99999`):

```
AGENT_TURN 1  tools=29  → SSE blocks=[thinking, tool_use]  stop=tool_use
AGENT_TURN 2  tools=30  → SSE blocks=[text]                stop=end_turn
  diff(req2.messages, req1.messages):
     + tool_result for toolu_01WFK9mhbTTw: 'spike-interactive-99999'
```

The `~/.claude` transcript closes this gap directly: it records
`user:[tool_result]` plus a richer record-level `toolUseResult` sidecar at
turn completion. **Recommended: MITM for token streaming + transcript for clean
tool results / structure.**

## 6. Three-way mapping: headless event → where to source it

| Headless `stream-json` event | From MITM | From transcript |
|------|------|------|
| `system:init` (session_id, tools, model, cwd, mcp) | not in stream; tools/model/system live in the **request body**; cwd/session_id known to launcher | early records (`sessionId`, `cwd`, `version`, `gitBranch`) |
| `stream_event:message_start` | ✅ SSE `message_start` | — (per-message) |
| `stream_event:content_block_*` (deltas) | ✅ **token-level** SSE | — |
| `assistant:text\|thinking\|tool_use` | assemble from deltas | ✅ `assistant` record `message.content` |
| `user:tool_result` | diff req N+1 vs N | ✅ `user` record + `toolUseResult` |
| `stream_event:message_delta` (usage) | ✅ SSE `message_delta.usage` | `message.usage` on assistant record |
| `result` (usage, duration, num_turns) | usage ✅; `total_cost_usd` must be derived from pricing (notional for subscription) | `system:turn_duration` record |
| `rate_limit_event` | response headers (`anthropic-ratelimit-*`) | — |

Net: **everything the headless stream carries is recoverable.** `system:init`
and `result` aren't single wire events but are assembled from the request body,
launcher knowledge, and `message_delta` usage.

## 7. Interactive-specific gotchas (must handle)

1. **Auxiliary API calls.** Interactive fires calls headless doesn't, and you
   must filter them from the real turn:
   - **quota preflight**: `max_tokens:1`, body `[{user: "quota"}]`. It uses the
     `claude-opus-4-7[1m]` alias and the raw API **404s** it
     (`not_found_error: "model: claude-opus-4-7[1m]"`). Claude tolerates this.
   - **title/topic generation**: a `tools:0` text turn.
   - The **real agent turn** is identifiable by a populated `tools` array
     (29–30 tools here) and the user's actual message.
2. **`HEAD /` probe** to the base URL on startup (404 from upstream root; fine).
3. **First-run trust dialog** ("Is this a project you trust?") blocks the prompt
   in an untrusted cwd. Run in a trusted dir or answer it (default = Enter).
4. **Input/control is the hard part, not output.** Output capture is solved by
   the proxy. But in interactive mode you still drive *input* through the TUI:
   submitting prompts, answering permission prompts, interrupting. Our PTY
   driver needed a trust handler and a submit nudge (the positional prompt was
   pre-filled, not auto-sent). Turn completion is cleanly detectable **from the
   proxy** (an `end_turn` after a `tool_use` turn) — use that as the app's
   "turn done" signal rather than scraping the TUI. Permissions: bypass with
   `--dangerously-skip-permissions` / `allowedTools`, or parse+answer the TUI
   prompt (fragile).

## 8. Risks / fragilities

- **`ANTHROPIC_BASE_URL`-with-OAuth could be blocked.** The same crackdown could
  refuse custom base URLs under OAuth (token-exfiltration hardening). *Currently
  the door is wide open*: the headless probe ran with `ANTHROPIC_BASE_URL` pointed
  at the local HTTP proxy and OAuth active (no API key), and `system:init`
  reported `apiKeySource:"none"` with a clean exit — the subscription path flowed
  through a custom HTTP base URL with no warning or refusal on `2.1.150` (2026-05).
  The risk is forward-looking, not present. If it does shut, fall
  back to the **transparent MITM**: `HTTPS_PROXY` + a CA in `NODE_EXTRA_CA_CERTS`.
  The binary explicitly supports this ("TLS-intercepting firewall… set
  `NODE_EXTRA_CA_CERTS`"), and it's hard to block without breaking enterprise
  proxies. `proxy/main.go` already has a `--tls-cert/--tls-key` HTTPS-listener
  path for this; you'd add on-the-fly cert generation for `api.anthropic.com`.
- **TLS/HTTP fingerprint of the re-originated connection.** A re-originating proxy
  (the chosen path *and* the transparent-MITM fallback) presents its own runtime's
  JA3 + HTTP signature to Anthropic's edge, not claude's — a detectable mismatch
  against a "claude" OAuth session. **Resolved** by opening the outbound leg from a
  version-pinned Bun `fetch` (claude's own runtime), which reproduces both layers
  byte-for-byte — and the matching Bun is the **stock GitHub release** (a different
  build of `1.3.14` than claude embeds still matches, so the fingerprint tracks the
  version, not the build), so AO just downloads it; no extraction from the claude
  binary. Full validation, rules, and residual caveats in **§12**.
- **Version drift.** SSE shape and the `[1m]` alias quirk are version-specific
  (`2.1.150`); the TLS/h1 fingerprint is pinned to the embedded **Bun** version
  (`1.3.14`, §12). Re-validate both on upgrades.
- **PTY automation brittleness.** See §7.4.

## 9. Recommended path for Agent Overflow

1. Spawn `claude` in a PTY (interactive), env `ANTHROPIC_BASE_URL` → embedded
   capture proxy, `--dangerously-skip-permissions` (or configured `allowedTools`).
   The proxy's **outbound** leg must be version-pinned Bun `fetch` for fingerprint
   parity (see §12); the Go side handles the plaintext inbound read + capture.
2. Render the UI from the **proxy SSE** (token-level), filtering quota/title
   calls; reconstruct `tool_result` from request diffs or read the transcript.
3. Use the proxy's `end_turn` as the turn-complete signal to gate the next
   prompt write.
4. Keep the transparent-MITM fallback ready in case base-URL-with-OAuth is shut.

A Go reverse proxy fits the existing stack and could later live under
`internal/` (it does not violate the triage-and-pipe principle — it's a
capture/transport shim, not an orchestration engine). **One caveat from §12:** the
upstream-facing leg cannot be Go — to avoid false-flagging genuine subscription
traffic, the connection to Anthropic must be opened by a version-pinned Bun `fetch`
(claude's own runtime), so the production shape is Go inbound/capture + a small Bun
outbound sidecar.

---

## 10. ADDENDUM — "Remote Control" reframes the whole problem (validated)

`claude --help` surfaced `--remote-control`. Running it end-to-end against the
real cloud (under subscription) and reading the bridge + worker debug logs
revealed the actual mechanism. **This changes the spike's premise** and partly
supersedes §9. Validated 2026-05-26 on `2.1.150`.

### What Remote Control actually is — a cloud-mediated relay, NOT a local RPC

```
claude.ai/code or mobile
        ⇅  (WebSocket)
  api.anthropic.com  /v1/code/sessions/<cse_id>          ← cloud session ("work")
        ⇅  (SSE GET .../worker/events/stream  +  POST .../worker/events)
  local headless worker:  claude --print --sdk-url .../v1/code/sessions/<id>
        --session-id <id> --input-format stream-json --output-format stream-json
        --replay-user-messages --verbose
```

Verbatim from the bridge debug log (`--debug-file`):

```
[bridge:session] Spawning sessionId=… sdkUrl=https://api.anthropic.com/v1/code/sessions/cse_…
[bridge:session] Child args: --print --sdk-url …/v1/code/sessions/cse_… --session-id cse_…
                 --input-format stream-json --output-format stream-json --replay-user-messages …
[bridge:ws] sessionId=… <<< {"type":"system","subtype":"hook_started",…}   ← stream-json relayed
```

And from the worker's own debug log:

```
SSETransport: SSE URL  = https://api.anthropic.com/v1/code/sessions/cse_…/worker/events/stream
SSETransport: POST URL = https://api.anthropic.com/v1/code/sessions/cse_…/worker/events
SSETransport: Connected   ·   CCRClient: initialized, epoch=1
```

Flow: the `remote-control` process authenticates with **subscription OAuth**,
calls a `/v1/code/work` API (`[bridge:api] GET …/work/poll`) to mint a session
`cse_<id>` + a per-session **accessToken**, then spawns a **headless
`claude --print --output-format stream-json` worker** pointed at the session's
`/worker/events` endpoint with that token. The worker streams stream-json to the
cloud; the cloud relays to claude.ai/mobile. `--spawn worktree` isolates each
session in a git worktree (matches AO's model); `--capacity N` (default 32)
allows concurrent sessions.

### The headline: subscription headless stream-json runs *today* — via a different endpoint

The mode presumed killed (`claude --print --output-format stream-json` under
subscription) is exactly what Remote Control runs *right now*. The difference is
the endpoint and auth: **not** direct `/v1/messages` with the raw OAuth bearer,
but `/v1/code/sessions/<id>/worker/events*` with a bridge-minted per-session
token, via a `/v1/code` "work" API.

**Caveat — this is what works today, not necessarily what survives the cut.**
The premise driving this spike is a change *next month*. We do not know whether
the `/v1/code/sessions` channel stays open to a **third-party-driven** worker
after the cut. It could be the path Anthropic consolidates around, or the next
thing gated (e.g., minted token bound to first-party client attestation /
Trusted-Devices, strings for which exist in the binary). Treat "subscription
headless survives, repackaged" as *plausible and currently real*, not proven for
our use after the cutoff.

### What this means for capture (token-level)

- The worker emits **stream-json**. Its spawn args **omit
  `--include-partial-messages`**, which would normally mean per-message (not
  token-level) granularity — **but the actual stream was not observed**: this
  run's session was idle (no prompt sent), so the only relayed events were
  `hook_started`/`hook_response` + 2 `system` transcript lines. Confirming
  token-vs-message granularity requires driving a real turn (not yet done).
- **`ANTHROPIC_BASE_URL` will NOT redirect the worker** — `--sdk-url` is an
  absolute URL. The §1 reverse-proxy trick doesn't reach this traffic. Only the
  **transparent MITM** (`HTTPS_PROXY` + CA; the worker loads system + bundled CAs
  and honors mTLS env, debug shows `configureGlobalMTLS` + 293 system CAs) can
  intercept `/v1/code/sessions/<id>/worker/events*`.
- Two turnkey-ish capture points exist without TLS work: the bridge
  `--debug-file` already logs every relayed message as `[bridge:ws] <<< {…}`,
  and the bridge writes `/tmp/bridge-transcript-<cse_id>.jsonl` per session
  (both debug-grade, not stable APIs).

### Corrections to earlier assumptions in this addendum's first draft

- `control.sock` (the `/tmp/cc-daemon-<uid>/<id>/` Unix socket, op set
  `ping/dispatch/lease/attach/subscribe/reply/kill/…`) is a **separate**
  subsystem — a local terminal-multiplexer/supervisor for interactive
  background sessions and `detach`. It is **not** what Remote Control's bridge
  uses; in `--spawn same-dir` the bridge spawned the worker directly and no
  `control.sock` was created. The earlier "local RPC gives structured I/O with
  no MITM" hope conflated the two subsystems.
- `LOCAL_BRIDGE=1` did **not** redirect the bridge to `ws://localhost:8765`
  (the byte-logger saw nothing); it connected to the production cloud. The
  local-bridge dev hook needs a different trigger (unconfirmed).

### Candidate architectures for AO, ranked safest → most aggressive

Risk here means two distinct things, both worth weighing: **fragility** (how
likely a Claude Code release silently breaks it) and **posture** (how close it
sits to reverse-engineering / re-implementing a first-party client Anthropic
controls — the ToS-sensitive axis). They don't move together: option A is the
most fragile but the least aggressive; option D is the most durable contract but
the most aggressive posture.

- **A. Parse the bridge `--debug-file` / transcript JSONL** *(least aggressive;
  most fragile).* Purely passive observation of a process AO already launched —
  no network interception, no protocol replay, nothing leaves the machine. But
  it reads a debug-grade surface with no stability contract; format churns
  release to release, and granularity is whatever the bridge logs (no
  `--include-partial-messages` control). Fastest to prototype, first to break.
- **B. Run `claude remote-control` + transparent-MITM the worker.** Same posture
  as §1's proxy approach — intercepting AO's *own* machine's outbound HTTPS via
  a locally-installed CA — just pointed at the worker's
  `/v1/code/sessions/.../worker/events` SSE instead of `/v1/messages`. Token
  granularity only if we can influence worker args (`--include-partial-messages`);
  otherwise per-message. Inherits §1's forward-looking risk (TLS pinning / attestation).
- **C. Run `claude remote-control`, be the cloud client.** Let the bridge spawn
  workers; AO connects to `api.anthropic.com /v1/code/sessions/<id>` as the
  "remote UI" (what claude.ai/code does over WS). Structured first-party I/O —
  but it means impersonating Anthropic's own remote frontend and reversing its
  WS client protocol, and every byte routes through Anthropic's cloud. Different
  category from B: not intercepting our own traffic, but speaking as a client we
  aren't.
- **D. Replicate the bridge** *(most aggressive; most stable contract).* Do what
  `remote-control` does ourselves: subscription OAuth → `/v1/code/work` mint →
  spawn our own headless stream-json worker (with `--include-partial-messages`)
  → read its stdout directly. Cleanest token-level capture, no MITM, and the
  worker's stdout *is* a documented stream-json contract — but it rebuilds the
  exact first-party client Anthropic is gating, against an undocumented,
  ToS-sensitive, version-coupled `/v1/code` work API.

The granularity gap (token vs message) collapses into this choice: **B and D
give `--include-partial-messages` control (token-level); A and C do not** (A
takes what the debug log emits; C takes what the cloud relays).

Docs: https://code.claude.com/docs/en/remote-control . Spike artifacts:
`bridge_logger.py`, `rc_probe.py` (PTY driver that enables RC + probes the
socket), captured worker/bridge debug logs in `/tmp/rc-debug*.log`.

---

## 11. Full validation of the interactive + proxy path (signals + interaction)

Validated 2026-05-26 on `2.1.150` / `claude-opus-4-7`, subscription OAuth
(`apiKeySource:"none"`). Method: drive real interactive sessions through the
logging proxy, build the actual `proxy_sse → AO_event` transform AO will ship
(`ao_transform.py`), and check it against a headless
`--output-format stream-json --include-partial-messages` reference of the *same*
prompt. The bar is **semantic parity with headless**, not byte-equality.

**Headline: the transform reproduces headless's event stream from the proxy
capture.** Transforming the proxy capture of the *same request* that produced a
headless run's stdout yields an **identical event multiset** (e.g. 38==38
`input_json_delta`, 2==2 `tool_use`, 2==2 `tool_result`), and reconstructed
`tool_result` content is **byte-identical** (7275==7275, 7700==7700 chars).
Driving interactive instead of headless yields the same signal classes (only
counts differ, because interactive ran more turns).

### Signal coverage (every class AO needs)

| Signal | Source from proxy / transcript | Status |
|---|---|---|
| assistant text (token-level) | SSE `content_block_delta.text_delta` | ✅ exact |
| thinking | SSE `thinking` block — **signature only, no content** | ✅ but see note |
| tool_use (+ streamed args) | SSE `tool_use` block + `input_json_delta` | ✅ exact |
| parallel tool_use in one turn | multiple `tool_use` blocks per assistant msg | ✅ confirmed (2× Read) |
| tool_result (+ `is_error`) | **diff** next agent request's `messages` array | ✅ byte-identical; transcript `toolUseResult` corroborates |
| usage (input/output/cache) | SSE `message_delta.usage` (+ per-iteration) | ✅ |
| stop_reason | SSE `message_delta.stop_reason` | ✅ end_turn / tool_use |
| result (cost/turns) | derived: sum usage; cost needs pricing table | ✅ derivable |
| rate limits | response headers `anthropic-ratelimit-*` | ✅ (proxy redaction fixed, below) |
| WebSearch / WebFetch | **client tools** → ordinary `tool_use` + reconstructed result | ✅ |
| session_id, cwd, mcp, permissionMode | **NOT on the wire** → transcript or AO's own knowledge | ⚠ off-wire |
| **per-call permission prompts** | **not available in interactive** (see below) | ❌ real gap |

### Two findings that change the picture

1. **Thinking is signature-only — but so is headless.** With `thinking:
   {type:"adaptive"}` (the 2.1.150 default at "max effort"), the API returns a
   thinking block carrying a `signature_delta` and **zero `thinking_delta`** —
   the reasoning *content* is never sent. The **headless reference shows the
   exact same thing**. So this is a model/API property of adaptive thinking, not
   an interactive limitation: AO can show a thinking indicator + token count but
   not the reasoning text — and loses nothing relative to the headless bar.

2. **Permissions are the one place interactive is strictly weaker than
   headless.** The clean per-call "ask" surface (`can_use_tool` control_request)
   rides the stream-json control channel, which is `--print`-only.
   `--permission-prompt-tool` **did not redirect a permission request to a named
   MCP tool in interactive mode** (tested: a `Write` in default mode showed
   Claude's *own* TUI prompt; the binary's `"(passed via --permission-prompt-tool)
   not found"` validation error never fired). That observation is consistent with
   both "flag ignored interactively" and "flag parsed but falls back to the TUI
   when the tool is unresolvable" — distinguishing them needs a real MCP `decide`
   tool (not run). Either way, a host driving interactive mode has **no
   programmatic per-call
   permission interception** — only static policy (`--permission-mode` +
   `--allowedTools`/`--disallowedTools` + `--settings permissions`, all of which
   *do* work interactively) or scraping the TUI prompt. AO must choose a
   permission posture up front; it cannot natively render each request as forge
   does over the SDK.

### Auxiliary-call filtering (the transform must do this)

A real interactive turn is surrounded by noise the proxy also sees. The
classifier keys on the request body:
- `max_tokens<=1` → **quota preflight**, skip.
- `tools==[]` → **auxiliary** (title/topic generation), skip.
- all tools are server tools (`<name>_<8digits>`, e.g. `web_search_20250305`) →
  **nested sub-call**: a client tool (WebSearch) making its *own* API call to the
  native server-side tool. The proxy captures it but headless stdout does not —
  skip. (This case crashed the first transform; it's why you build against real
  captures.)
- otherwise → **agent** main-loop turn (note: a pure-text follow-up turn still
  has tools *available*, so it classifies as agent — it is **not** dropped).

### Interaction mechanics

- **Multi-turn driving works.** `drive_multi.py` submits a queue of prompts into
  the live TUI, detecting each turn's completion from the proxy SSE (an agent
  request whose response ends `stop_reason==end_turn`), then types the next
  prompt. Validated 2-turn (parallel-Read turn → pure-text follow-up) with clean
  exit.
- **Turn detection is robust to `pause_turn` by construction**: it fires only on
  `end_turn`. A `pause_turn` (extended thinking spanning multiple `/v1/messages`
  calls) is *not* counted complete, so the driver waits for the real `end_turn` —
  no misfire. (Not triggered on demand; safe by the keying logic.)
- API-transport errors (429/529/overload) are capturable via response status +
  SSE `error` events (not exercised on demand). **Tool-level** errors *were*
  exercised: a failing WebSearch produced an `is_error` `tool_result`, recovered
  correctly by the diff.

### Proxy redaction bug fixed (found while validating rate limits)

`redactHeaders` matched the bare substring `"token"`, which over-redacts
`anthropic-ratelimit-*-input-tokens-*` headers AO needs. Tightened to redact only
real credential headers (`authorization`, `x-api-key`, `cookie`, `set-cookie`,
`*-{access,refresh,session,auth}-token`). Verified: OAuth/api-key/cookies still
redacted; rate-limit token headers now pass through.

### Bottom line

The interactive + local-proxy path delivers **headless-equivalent structured
output** for every signal AO renders, with the transform proven against a
headless reference. The single substantive gap is **dynamic per-call
permissions**, which interactive does not expose programmatically — a product
decision (curated allow-list + permission mode, or surface Claude's own prompt),
not a blocker for the output stream.

> **Superseded (see `HOOKS_COVERAGE_MAP.md`).** This "single substantive gap"
> conclusion predates the hook-channel characterization. The narrow finding here
> stays true — `--permission-prompt-tool` does **not** redirect a permission
> request interactively — but the broader conclusion that interactive offers *no*
> programmatic per-call interception is **wrong as of the hook work**: a
> `PreToolUse` hook returns `allow`/`deny`/`ask` per call (LIVE-confirmed,
> `probe_hook_permission.py`). Dynamic per-call permissions are recovered via a
> *different* mechanism (hooks, not the print-only control channel) — **not a
> gap.** The validated-vs-unvalidated edges are catalogued in
> `HOOKS_COVERAGE_MAP.md#whats-not-recovered--open-items--risks`.

---

## 12. TLS/HTTP fingerprint preservation — the proxy's outbound leg (validated)

Validated 2026-05-30 on `2.1.158` (Bun **v1.3.14** embedded), subscription OAuth.
Probes: `ja3_diff.py`, `probe_bun_provenance.py`, `probe_tls_clients.py`,
`probe_h1_serialize.py`, `probe_h1_headerforms.py`, `probe_h1_interactive.py`.

### Why this matters (and why it isn't evasion)

The chosen architecture (§3) **re-originates** the upstream connection: claude→proxy
is plaintext `http://` (so the proxy can read everything), and proxy→Anthropic is a
**fresh TLS connection the proxy opens**. That outbound connection carries the TLS
fingerprint (JA3/JA4) and HTTP/1.1 header signature of **whatever runtime opened
it — not claude's**. The current spike proxy is Go, so Anthropic's edge (Cloudflare,
which exposes JA3 to origins) sees Go's fingerprint, not claude's.

This is not about defeating a control. The traffic *is* genuine subscription
traffic — same OAuth bearer, same request bodies, same endpoint — merely relayed
through a local capture shim AO runs for its own UI. The goal is that routing
legitimate usage through that shim does not make it **look** anomalous (a Go/OpenSSL
JA3 hitting a "claude" OAuth session is a trivially detectable mismatch that could
false-flag the user). Reproducing claude's fingerprint keeps genuine usage looking
genuine. We do it by **originating the connection from the same runtime claude uses
(Bun/BoringSSL)** — not by spoofing/uTLS/eBPF. It genuinely *is* a Bun connection.

### SSLKEYLOGFILE is a dead end (don't try to decrypt in place)

The tidy alternative — let claude keep its own TLS and just log the session keys —
does **not** work. The `SSLKEYLOGFILE` string is **absent** from both the claude and
bun binaries; setting it produces **no keylog file**; and the statically-linked
BoringSSL precludes an `LD_PRELOAD` shim. There is no in-place key extraction. The
re-originating proxy is the only viable capture, which is why the outbound
fingerprint has to be handled.

### Version pin: `Bun/1.3.14` (loose coupling, but it IS coupled)

The fingerprint is **runtime-version-specific**. Detect claude's embedded Bun with
`grep -aoE 'Bun/[0-9]+\.[0-9]+\.[0-9]+' <binary> | head -1`. All four on-disk builds
(2.1.154→2.1.158) embed the single string `Bun/1.3.14` — so the coupling is *loose*
(Bun revs move slower than claude revs), but it is real: **Bun 1.3.11 already
differs** (it adds extension `65037`/ECH-GREASE → different fingerprint). The proxy
must run a Bun pinned to claude's embedded version, re-checked on claude upgrades.

### Provenance: a STOCK Bun download matches — no extraction from claude needed

The integration-gating question is *where the pinned Bun comes from*. Answer
(`probe_bun_provenance.py`, measured 2026-05-30): **the official stock release is
sufficient — AO downloads it, it need not carve Bun out of the claude binary.**

- The Bun used in every fingerprint test this spike, `/tmp/bun1314/bun`, is **byte-identical
  to the official GitHub release** `bun-v1.3.14/bun-linux-x64.zip`
  (both sha256 `9fd36f87e4b90b07632b987a2e4ec81ca15a62c81bf983190cea6d715be2ad74`,
  both revision `1.3.14+0d9b296af`). A *fresh, independent* re-download was verified
  to produce the same ClientHello `7513169a…`.
- **claude embeds a *different build* of the same version** — revision
  `1.3.14+521eedd6d` (vs the stock release's `0d9b296af`). The two builds were already
  compared head-to-head in the Layer-1 table and **matched** (`7513169a…`). So the
  ClientHello is determined by the **Bun minor version** (BoringSSL version + default
  `SSL_CTX`, which Anthropic does not customize), **not** the exact build hash. This is
  why a stock download is safe: it need not be bit-for-bit claude's build, only the
  same `1.3.x` line.
- **Integration consequence:** ship/download stock Bun pinned by version, choosing the
  stock release whose version equals claude's embedded `Bun/x.y.z` string, re-checked
  on claude upgrades. No binary-extraction step, no dependence on claude's internal
  layout. Pin the **per-platform** sha256 for supply-chain integrity — the `9fd36f87…`
  above is the **linux-x64** binary's; macOS (`darwin-arm64`/`x64`) and Windows ship
  *different* binaries with *different* shas, so pin each (ideally cross-checked against
  Bun's own published `SHASUMS`, not just a locally computed one).

### Layer 1 — TLS ClientHello: Bun 1.3.14 is byte-identical to claude

`ja3_diff.py` captures each runtime's real ClientHello at a raw socket and compares a
GREASE-stripped, extension-as-set normalized fingerprint (BoringSSL shuffles
extension order per-connection, so the *set* is the stable identity; claude+bun were
each run twice to confirm intra-client stability):

| Runtime | normalized fp | ext set (GREASE-stripped) | ciphers | ALPN | vs claude |
|---|---|---|---|---|---|
| **claude 2.1.158** (embeds Bun `…521eedd6d`) | `7513169a…` | `5,10,11,13,16,18,23,35,43,45,51,65281` | 17 | h1 | baseline |
| **Bun 1.3.14 stock** (`…0d9b296af`) | `7513169a…` | *identical* | 17 | h1 | ✅ **MATCH** (diff build, same fp) |
| Bun 1.3.11 (older) | `dd31589e…` | `…+65037` (ECH) | 17 | h1 | ❌ differs |
| Node 24 (undici/OpenSSL) | `cda9e5b8…` | `…22…`, no `5`/`18` | 52 | h1 | ❌ differs |
| Go 1.25 (spike proxy) | `8a6d0b37…` | `…50…`, no `16` | 13 | none | ❌ differs |

So a **version-pinned Bun** proxy reproduces claude's ClientHello byte-for-byte;
Go, Node, and even a one-minor-version-off Bun do not.

### Layer 2 — HTTP/1.1 headers: a SEPARATE code path, also reproduced by `fetch`

A matching ClientHello only proves the proxy uses Bun's default TLS config — it does
**not** prove the HTTP client serializes headers like claude. That (casing,
ordering, auto-headers, framing) is a distinct code path and was measured end to end.

claude's `POST /v1/messages` application headers are emitted in a **case-sensitive
ASCII sort** with **original casing preserved** — well-known headers Title-Cased
(`Accept, Authorization, Content-Type, User-Agent`), then `X-*` Title-Cased
(`X-Claude-Code-Session-Id, X-Stainless-*`), then lowercase `anthropic-*`, then
`x-app` — followed by framing (`Connection, Host, Accept-Encoding, Content-Length`).

The result (after correcting a confound — see below):

- **Bun `fetch`, given claude's headers as a plain object with original casing,
  reproduces claude's COMPLETE wire header block byte-for-byte** — application
  headers in the exact case-sensitive sort *and* framing regenerated in claude's
  exact order, down to `Accept-Encoding: gzip, deflate, br, zstd`
  (`probe_h1_headerforms.py`, "FULL wire block match: True"). fetch sorts
  case-sensitively and preserves the casing it is handed.
- **`node:http`/`node:https` LOWERCASES header names in every construction form**
  (object, array, pairs, `setHeader`). So `node:https` — despite matching the
  ClientHello — **cannot** reproduce claude's h1 and is **not** the outbound client.
- **Interactive == headless.** `probe_h1_interactive.py` PTY-drives the *interactive*
  TUI and diffs its request against `claude -p`: the 17-header application block is
  **identical in name, order, and casing**. The byte-identical h1 match proven
  against headless applies to the live interactive path AO taps. No interactive-only
  header to anticipate.

**The confound worth remembering:** an earlier forwarder test (`probe_h1_serialize.py`)
appeared to show fetch *mangling* headers. That was `Bun.serve` lowercasing names on
**ingest** before fetch ever saw them — fetch faithfully preserved the
already-lowercased names. fetch was never the problem; the *ingest* was.

### Why only high-level clients match the ClientHello

`probe_tls_clients.py`: bare `tls.connect` and native `Bun.connect` — which *would*
let you write claude's exact h1 bytes raw — produce a **different** ClientHello: they
omit ext `5` (OCSP `status_request`) and `18` (SCT), which only Bun's **high-level**
clients (`fetch`, `node:https`) enable. `requestOCSP:true` does **not** add them.
So the raw-socket "write exact bytes" path is a dead end for fingerprint parity.
`fetch` is the one client that reproduces **both** layers.

### Architecture rules (forced by the measurements)

1. **Outbound leg = version-pinned Bun `fetch`**, fed claude's headers as a **plain
   object with original casing**. Reproduces both the ClientHello and the h1 block.
2. **The inbound read must preserve claude's raw header casing.** This is a real
   trap: **Go's `net/http` canonicalizes** names to Title-Case
   (`anthropic-version`→`Anthropic-Version`) and **`Bun.serve` lowercases** them —
   *both* corrupt the casing fetch would otherwise reproduce. The proxy must capture
   claude's header names from the **raw request bytes** (as the probes' `_parse`
   does), not via a normalizing HTTP-server parser.
3. **Strip hop-by-hop/framing headers before forwarding** (`Host, Connection,
   Content-Length, Accept-Encoding`, …); fetch regenerates them, and because it is
   the same Bun fetch, the regenerated framing matches claude's.

### Two ways to inject this (decision for the integration phase)

- **(Recommended) Go proxy + Bun outbound sidecar.** AO's proxy stays Go (fits the
  stack; handles the plaintext inbound read with raw-casing capture, logging, and the
  SSE relay back to claude); a small **version-pinned Bun** child process performs
  the `fetch()` to Anthropic and streams the response back to Go. The Bun dependency
  is isolated to the one fingerprint-bearing leg. Go ↔ Bun can talk over a local
  socket/pipe.
- **Whole-Bun proxy.** The entire reverse proxy is one Bun program. Simpler process
  topology, but it (a) introduces a Bun runtime into AO's Go/Wails stack and (b) must
  **still** avoid the `Bun.serve` ingest-lowercasing trap (read the raw inbound bytes,
  don't trust `req.headers` for casing).

Either way the §3 `http://`-base-URL design is unchanged and correct — only the
**runtime of the outbound leg** moves from Go to Bun. `proxy/main.go` (Go) is
**fingerprint-NON-preserving** by construction; it is fine for signal validation (it
proved the SSE/transcript story) but is not the production upstream client.

### Residual caveats (measured honestly)

- **Platform: only `linux-x64` was measured.** Every capture here — claude's baseline
  *and* the stock Bun — is the Linux build, on WSL2. AO is a multi-platform desktop app
  (Wails ships macOS/Windows/Linux); macOS-claude is a Mach-O Bun and Windows another
  build, each paired with a *different* stock Bun binary. The version-determined logic
  (same BoringSSL version + uncustomized default `SSL_CTX`) predicts the per-platform
  claude-vs-stock-Bun ClientHello match holds on `darwin-arm64`/`darwin-x64`/`win-x64`
  too — but that is **inference, not measurement**, and it's the same inference class
  that misfired earlier this spike. **Re-run `ja3_diff.py` + `probe_bun_provenance.py`
  on each target platform during integration** before relying on the match there.
- **SNI (ext 0) was omitted in measurement** because probes connect to `127.0.0.1`
  (an IP literal — RFC forbids SNI), on *every* leg, so the comparison stays
  apples-to-apples. Production connects to the hostname `api.anthropic.com`, which
  adds an **identical** `server_name` to both claude and the proxy's fetch (same
  hostname, same Bun) — parity holds by construction, but the real-hostname
  ClientHello was not directly captured (would need MITM of claude's genuine
  outbound).
- **ALPN/HTTP version — *offer* proven, *negotiated* protocol to eyeball.** What is
  measured and solid is the ALPN **offer**: claude advertises `http/1.1` **only**, and
  Bun fetch advertises the identical `alpn=['http/1.1']`. The offer is **client-fixed**
  — it is the same bytes regardless of destination — so this part holds against the
  real origin by construction (it's in the ClientHello that already matches). What the
  spike did **not** exercise is the **negotiated** protocol against a real *h2-capable*
  origin: both probes hit a local h1-only sink, so there was no h2 on the table to
  decline. Because the offer is h1-only, a compliant origin *must* negotiate h1 — but
  integration should confirm the production fetch to `api.anthropic.com` in fact stays
  on HTTP/1.1 (and re-measure if a Bun upgrade ever widens the offer to `h2,http/1.1`).
- **TLS session resumption is unmeasured.** All captures are fresh full handshakes.
  Keep-alive reuse sends no new ClientHello; a *resumed* new connection sends a
  different (PSK-bearing) hello. Both sides are BoringSSL so resumption behavior is
  expected to align, but the proxy's connection lifecycle differs from claude's and
  this was not exercised.
- **`NODE_EXTRA_CA_CERTS` fallback (§8) is documented, not live-tested**, and being a
  re-originating MITM it inherits the *same* outbound-fingerprint requirement — the
  Bun-fetch outbound leg applies there too.

---

## 13. Tier-0 architecture gate — the Go-inbound + Bun-outbound topology streams while preserving the fingerprint (validated)

§12 proved the *pieces*: Bun 1.3.14 reproduces claude's ClientHello and h1 block,
and recommended the **Go proxy + Bun outbound sidecar** topology. It left one
load-bearing hinge unproven: does that topology **stream SSE incrementally**
(token-level UI must survive) **while carrying claude's fingerprint through the
Go→Bun handoff**? This section closes that gate with a staged build — isolate each
hop, then the full hermetic topology, then real subscription turns. Verdict up
front: **GO.** The topology streams end-to-end and preserves the fingerprint, and
the real turn surfaced a response-side requirement §12 didn't state (gzip).

### The components (throwaway validation prototypes — not production)

- `tier0_sink.py` — hermetic origin: raw-parses the inbound h1 block (casing
  intact, via `probe_h1_serialize._parse`) and emits a chunked SSE stream of N
  events DELAY apart, each flushed immediately. Plain TCP on `127.0.0.1` — no real
  traffic, no tokens.
- `tier0_sidecar.js` — the outbound leg: Bun.serve receives a JSON **envelope**
  (`{url, method, headers:[[name,value]…], body}`; claude's headers ride in the
  *body* so Bun.serve's ingest-lowercasing never touches their casing), rebuilds a
  **plain headers object** (casing preserved, fetch re-sorts), calls the
  version-pinned Bun `fetch`, and streams `Response(up.body)` straight back.
- `tier0_proxy.go` — the claude-facing leg: accepts **raw** (not `net/http`, which
  canonicalizes), hand-parses the request preserving claude's header casing, strips
  the framing fetch regenerates, envelopes the rest to the sidecar, and relays the
  response chunk-by-chunk with an immediate flush each chunk.
- `probe_tier0_stream.py` (hops a+b), `probe_tier0_full.py` (full hermetic),
  `probe_tier0_real.py` (real origin, prompt env-overridable).

### Streaming — incremental through all three hops

Q1 ("does it stream?") is really three hops: (a) Bun `fetch` *reads* the upstream
SSE incrementally, (b) Bun.serve *writes* a `Response(stream)` body without
buffering-until-close, (c) Go reads Bun.serve incrementally. Discriminator = the
**arrival span** (last − first chunk): streamed ⇒ span ≈ (N−1)·DELAY; buffered ⇒
span ≈ 0.

- **(a) and (b) isolated** (`probe_tier0_stream.py`, 8 events × 100 ms): bare
  `bun fetch` span **699 ms** / 8 reads; minimal Bun.serve span **702 ms** / 8
  reads. Hop (b) — the one §12 couldn't vouch for — adds essentially zero buffering
  (first arrival 3.8 ms vs fetch's 5.2 ms). No stdout-pipe fallback needed.
- **(c) + end-to-end hermetic** (`probe_tier0_full.py`): the Go flush-relay's
  `response_chunk` span **701 ms** over 8 chunks; the synthetic client received
  **8/8** SSE events intact through de-chunk/re-chunk.
- **End-to-end real, the strong proof** (`probe_tier0_real.py`): the span **scales
  with generation time**, which buffering (constant ≈0 span regardless of length)
  cannot produce:

  | prompt | main-response chunks | span |
  |---|---|---|
  | one word (`PONG`) | 6 | 366 ms |
  | 40 numbered lines | 6 | 809 ms |
  | ~500-word essay | **36** | **13,786 ms** |

  A 14-second span across 36 chunks is live, token-by-token streaming.

### Fingerprint — survives the Go→envelope→fetch handoff

`probe_tier0_full.py` drives a synthetic claude request (claude's 17 app headers +
its own framing, synthetic bearer) through the real path and checks the sink:

- **h1 block** at the sink == `CLAUDE_FULL` exactly — the 17 app headers in claude's
  case-sensitive order, then fetch's regenerated framing. (Q2 i.)
- **Casing survived on both ends**: the sidecar's *handed-to-fetch* names (input
  side) == `CLAUDE_APP`, and the sink's app block (output side) == `CLAUDE_APP`. The
  raw-read → envelope → object-rebuild chain never lowercased. (Q2 ii.)
- The **TLS ClientHello is invariant under forwarding** — it's whatever Bun's fetch
  opens, already proven == claude by `ja3_diff.py`; the sidecar sits above TLS and
  cannot regress it. Tier-0 does **not** re-measure TLS, and shouldn't.

### Content-encoding — Anthropic gzips the SSE; the outbound leg MUST decompress (a fourth rule)

This is the finding the hermetic sink could not surface and the real turn did.
Because §12 rule 3 strips `Accept-Encoding` and lets fetch regenerate it, the
fingerprint-preserving leg necessarily **advertises** claude's full
`Accept-Encoding: gzip, deflate, br, zstd` — and so **receives compressed
responses**. Across all real turns, **every `200` SSE response came back
`content-encoding: gzip`** (`text/event-stream; charset=utf-8`), as did the `404`
JSON error body. Handling, all validated by correct rendering ×3:

- Bun `fetch` **auto-decompresses, and this is not luck — it follows from rule 3.**
  Because the Go proxy strips `Accept-Encoding` (`stripForward`), the sidecar never
  sets that header itself, so `fetch` *owns* it: fetch advertises the encodings, so
  fetch decompresses them. A client inflating what it offered is the contract, not a
  happy accident. And it decompresses **incrementally**: the essay's 36 chunks spread
  over 13.8 s prove the gzip stream is inflated progressively (not
  buffer-then-inflate-at-close), so streaming survives compression.
- The sidecar's `STRIP_RESP` drops the now-stale `Content-Encoding`/`Content-Length`
  (and framing) before relaying, so claude receives coherent **identity** SSE
  (claude offered gzip but accepts identity — a server may always decline to
  compress).

The Go-only `proxy/main.go` dodges this entirely by stripping `Accept-Encoding`
outbound to force plaintext — which is *precisely why it is fingerprint-NON-
preserving*. The fingerprint path cannot take that shortcut, so:

> **Architecture rule 4 (addendum to §12).** Because rule 3 hands `Accept-Encoding`
> ownership to `fetch`, the response arrives decompressed for free; the sidecar's
> *only* added duty is to strip the now-stale `Content-Encoding`/`Content-Length`
> before relaying, so claude never double-decodes. Verified for **gzip**; see caveats
> for `br`/`zstd`.
>
> **Integration note (not now).** Rule 4 moves the decompression boundary into the
> sidecar, so AO's token capture (§1–11) consumes the **post-sidecar plaintext** at
> the Go inbound layer — which already sees the stream incrementally (hop c). Capture
> survives the topology change; the transform reads identity SSE, never gzip.

### The `404` pattern is claude's own model-fallback, not topology-induced

Every real turn showed statuses `[404, 404, 200, 200, …]`. Neither 404 is a topology
bug; both are claude's own behavior, and both reproduce identically in the older
Go-fingerprint `proxy/main.go` capture (`/tmp/ao-cap-danger.jsonl`):

- **`HEAD / → 404`** is claude's base-URL connectivity probe (`404 ×10` in the old
  cap).
- **`POST /v1/messages?beta=true → 404`** is a **1M-context model probe + fallback.**
  In *both* captures, every 404 POST carries `"model":"claude-opus-4-8[1m]"` in its
  request body and the origin answers `not_found_error: model: claude-opus-4-8[1m]`;
  claude then re-issues with `"model":"claude-opus-4-8"` (no `[1m]`) and gets `200`.
  The 404 is keyed to what claude puts in its *own request body*, which the topology
  relays verbatim — so it would occur on genuine direct claude too; the transport is
  transparent to it.

The turn completes regardless (`Stop` fires, the answer renders). (Honest scope: both
caps share the proxied path via `ANTHROPIC_BASE_URL`; we didn't snapshot a fully
un-proxied direct-claude session. But since the 404 is driven by claude's request
body rather than the transport, that gap doesn't change the conclusion.)

### What this discharges from §12's residual caveats

- **SNI / real hostname** — the real turns point the sidecar's fetch at
  `api.anthropic.com` (hermetic probes used `127.0.0.1`, which omits SNI), so the
  real-hostname ClientHello is now *exercised* on the wire. But a served turn is
  **not** evidence the fingerprint is *right*: the old Go-fingerprint `proxy/main.go`
  — whose ClientHello and h1 block do **not** match claude — also got `200`s (`31×`
  in the same cap). Acceptance discriminates working plumbing from broken plumbing
  and nothing finer; it can't even show "not soft-flagged," since a mismatched client
  is served too. Fingerprint *fidelity* rests entirely on the **hermetic byte-match**
  (sink == `CLAUDE_FULL`) and the **§12 ClientHello invariant** — the real turn adds
  reach, not fidelity. The hello itself is still not captured (would need to MITM
  claude's genuine outbound).
- **ALPN/negotiated protocol** — the turns succeeded against the real *h2-capable*
  origin; since Bun fetch's offer is h1-only (§12), h1 is what gets negotiated. The
  "negotiated protocol against a real origin" gap §12 flagged is now exercised
  (though the negotiated value wasn't separately logged).

### Residual caveats (Tier-0)

- **Only `gzip` was exercised.** Anthropic chose gzip from claude's offered set;
  Bun fetch auto-decompresses gzip/deflate/br, but **`zstd` decompression by Bun
  fetch is unverified**. If the origin ever serves `br`/`zstd`, confirm Bun inflates
  it and that `STRIP_RESP` still yields coherent bytes.
- **Prototypes, not production.** One request per connection, `Connection: close`,
  no keep-alive/auth/LAN-block/reconnect. They close the *gate*; they are not the AO
  proxy. Integration is a separate, **currently-unauthorized** step.
- **`linux-x64` only** (carried from §12) — re-run the fingerprint probes per target
  platform during integration.
- The real-hostname ClientHello and the negotiated ALPN value are **exercised but not
  captured** — and (see SNI above) a served turn is not evidence they are *correct*;
  correctness rests on the hermetic byte-match + §12 invariant, not on the real origin.

### Verdict

**Go/no-go: GO.** The §12-recommended Go-inbound + Bun-outbound topology streams SSE
token-by-token end-to-end (hermetic *and* real, span scaling to 13.8 s) and carries
claude's h1 fingerprint + casing intact through the handoff. Fingerprint fidelity
comes from the hermetic byte-match plus the §12 ClientHello invariant; the live origin
served all three real turns, which confirms the path *reaches and works* — not that
the fingerprint is right (a served turn can't show that). The one new requirement —
auto-decompress + strip stale encoding headers (rule 4), itself a consequence of
rule 3 — is implemented and validated. Nothing here authorizes AO integration; it
confirms the architecture is sound to integrate when that work is scoped.

---

## Repro

Artifacts in this dir:
- `proxy/main.go` — stdlib-only logging reverse proxy (own module; never
  touches the parent build). `go build -C proxy -o /tmp/ao-proxy .`. Redacts
  only credential headers (not rate-limit `*-tokens-*`).
- `ao_transform.py` — **the portable artifact**: `proxy_sse → AO_event`
  transform (classify → drop preflight/auxiliary/nested → token-level
  passthrough + assembled messages + tool_result diff + derived result), plus
  `compare()` for semantic-parity checking. This is the logic to port to Go.
- `drive_multi.py` — multi-turn PTY driver (prompt queue; SSE-based turn
  detection; clean exit).
- `drive_interactive.py` — original single-turn PTY driver.
- `perm_world.py` / `perm_probe.py` — permission-surface probes (proved
  `--permission-prompt-tool` is a no-op in interactive mode).
- `analyze.py` — earlier three-way comparison (superseded by `ao_transform.py`
  but kept for the static mapping table).
- `preload.js` — the original Node-preload attempt; **dead against the native
  binary**, kept only as a record.

Fingerprint probes (§12), runnable independently (need claude on `PATH` + a Bun
1.3.14 at `/tmp/bun1314/bun` — the **stock** linux-x64 release, sha256 `9fd36f87…`:
`curl -sL -o /tmp/b.zip https://github.com/oven-sh/bun/releases/download/bun-v1.3.14/bun-linux-x64.zip && unzip -o /tmp/b.zip -d /tmp/bun1314_stock && cp /tmp/bun1314_stock/bun-linux-x64/bun /tmp/bun1314/bun`):
- `ja3_diff.py` — captures each runtime's TLS ClientHello at a raw socket; proves
  Bun 1.3.14 == claude and Go/Node/older-Bun differ. Writes `/tmp/ja3_results.json`.
- `probe_bun_provenance.py` — **provenance keystone**: the stock release reproduces
  claude's ClientHello even though claude embeds a *different* build of 1.3.14 →
  fingerprint is version-determined, AO downloads stock Bun, no extraction needed.
- `probe_tls_clients.py` — ClientHello of each candidate **outbound** client
  (fetch / node:https / raw tls.connect / Bun.connect); only the high-level clients
  match (carry OCSP+SCT).
- `probe_h1_headerforms.py` — **the h1 proof**: which Bun construction form
  reproduces claude's header serialization (answer: `fetch` + plain object), and a
  full-17-header byte-for-byte round-trip.
- `probe_h1_serialize.py` — earlier forwarder-based h1 probe; **superseded** by
  `probe_h1_headerforms.py`, kept because it surfaced the `Bun.serve`
  ingest-lowercasing trap (a real architecture lesson).
- `probe_h1_interactive.py` — confirms interactive claude emits the identical
  header block as headless `claude -p`.

Tier-0 topology probes (§13), runnable independently (need the stock Bun above +
`go build -o /tmp/tier0_proxy tier0_proxy.go`):
- `tier0_sink.py` / `tier0_sidecar.js` / `tier0_proxy.go` — the hermetic origin,
  the Bun outbound fingerprint sidecar, and the Go raw-inbound proxy.
- `probe_tier0_stream.py` — isolates the two Bun streaming hops (fetch-read,
  serve-write) against the sink.
- `probe_tier0_full.py` — full hermetic topology: h1 block == `CLAUDE_FULL`, casing
  survives the handoff, the Go flush-relay streams, body integrity.
- `probe_tier0_real.py` — one real subscription turn through the topology
  (`TIER0_REAL_PROMPT`/`TIER0_REAL_MARK` override the prompt); proves e2e render,
  streaming that scales with output, and surfaces the gzip content-encoding.

```bash
# 1. capture (headless, fast transport check)
/tmp/ao-proxy --listen 127.0.0.1:8090 --log /tmp/cap.jsonl &
ANTHROPIC_BASE_URL=http://127.0.0.1:8090 \
  claude -p "…" --output-format stream-json --include-partial-messages --verbose

# 2. interactive capture + transcript
AO_BASE_URL=http://127.0.0.1:8090 AO_CWD=<trusted-dir> \
  python3 drive_interactive.py

# 3. compare
python3 analyze.py /tmp/cap.jsonl ~/.claude/projects/<proj>/<session>.jsonl /tmp/ref-headless-stream.jsonl

# 4. fingerprint validation (§12) — needs stock Bun 1.3.14 at /tmp/bun1314/bun
python3 ja3_diff.py              # TLS ClientHello: Bun 1.3.14 == claude
python3 probe_bun_provenance.py # stock download matches; no extraction from claude
python3 probe_h1_headerforms.py # HTTP/1.1: Bun fetch reproduces claude's headers
python3 probe_h1_interactive.py # interactive == headless header block

# 5. Tier-0 topology validation (§13) — Go-inbound + Bun-sidecar streams + fingerprint
go build -o /tmp/tier0_proxy tier0_proxy.go
python3 probe_tier0_stream.py   # Bun fetch-read + Bun.serve-write both stream
python3 probe_tier0_full.py     # hermetic: h1 block + casing survive, flush-relay streams
python3 probe_tier0_real.py     # real turn: e2e render, incremental SSE, gzip observed
```

> Spike branch `spike/claude-mitm`; not for merge to `main`. Per
> `docs/references/spike-policy.md`, port the *learning* (proxy capture +
> reconstruction rules), not this throwaway code.
