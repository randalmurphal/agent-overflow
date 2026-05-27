# Claude Code MITM Spike — Findings

**Goal.** Anthropic is cutting subscription (Pro/Max OAuth) auth from the SDK /
headless mode (`claude -p --output-format stream-json`). Interactive mode keeps
subscription auth. We want to drive Claude in interactive mode but still recover
the same **token-level structured stream** we get from headless, for Agent
Overflow's own UI.

**Bottom line.** It works, and *not* by modifying the binary. Pointing Claude at
a local logging **reverse proxy** via `ANTHROPIC_BASE_URL` captures the full
raw Anthropic Messages API SSE stream — token-level text/thinking/tool_use —
with **no TLS interception and no binary patching**. Pair that with the
`~/.claude` transcript for clean tool results and you can reconstruct (a
superset of) the headless `stream-json` shape.

Date: 2026-05-26. Binary: Claude Code `2.1.150` (Bun v1.3.14 standalone ELF).

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
- **Version drift.** SSE shape and the `[1m]` alias quirk are version-specific
  (`2.1.150`). Re-validate on upgrades.
- **PTY automation brittleness.** See §7.4.

## 9. Recommended path for Agent Overflow

1. Spawn `claude` in a PTY (interactive), env `ANTHROPIC_BASE_URL` → embedded Go
   proxy, `--dangerously-skip-permissions` (or configured `allowedTools`).
2. Render the UI from the **proxy SSE** (token-level), filtering quota/title
   calls; reconstruct `tool_result` from request diffs or read the transcript.
3. Use the proxy's `end_turn` as the turn-complete signal to gate the next
   prompt write.
4. Keep the transparent-MITM fallback ready in case base-URL-with-OAuth is shut.

A Go reverse proxy fits the existing stack and could later live under
`internal/` (it does not violate the triage-and-pipe principle — it's a
capture/transport shim, not an orchestration engine).

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
```

> Spike branch `spike/claude-mitm`; not for merge to `main`. Per
> `docs/references/spike-policy.md`, port the *learning* (proxy capture +
> reconstruction rules), not this throwaway code.
