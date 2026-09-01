# Cursor provider

Status: design, signed off 2026-08-31. Nothing implemented yet. `(Qn)`
tags are the brainstorm session's decision ids and carry no other
meaning. This is a living spec: the Gap table and Spike backlog get
filled in as spikes against the real CLI land, and settled wire detail
migrates out of here into `docs/references/acp-wire.md` and
`docs/references/cursor-wire.md` once those exist.

## Goal

Cursor (`agent`, alias `cursor-agent`) as a third full-parity provider
beside Claude Code and Codex, driven over ACP (the Agent Client
Protocol, agentclientprotocol.com). Parity means everything the wire
exposes, surfaced with the same UX quality as claude/codex, with an
explicit gap table for what the wire genuinely lacks. No fabricated
approximations of missing features (Q6).

## Approach

Two phases, refactor first (Q1), the whole effort queued behind the
remote-access work (Q4).

**Phase A — seam tightening, zero behavior change.** The provider seam
already has real shared primitives (`provider.Session`,
`ApprovalRegistry`, provider-blind triage/store/transport/discussion,
the frontend `PROVIDER_DEFINITIONS` catalog). What remains is ~68
backend and ~18 frontend switch sites. Phase A:

- converts repeated identical-shape `switch provider` dispatch
  (settings env building/validation, sessionimport scan/tail, scattered
  `internal/app` sites) into per-provider function tables. Genuinely
  divergent branches (fork semantics, runtime-mode flags, subagent
  models) stay explicit per Core Principle 6.
- moves frontend provider conditionals that are feature gates into
  `PROVIDER_DEFINITIONS`; fixes latent not-claude-means-codex bugs
  (e.g. the two-way ternary at `frontend/src/lib/stores/events.ts:236`).
- adds an exhaustiveness tripwire: a test that every dispatch table
  covers every `ProviderID`, so a new provider cannot silently miss a
  site.

Phase A lands alone and green so any regression is unambiguously the
refactor's.

**Phase B — the provider.** Two new packages plus the standard
integration inventory from `docs/architecture/how-to.md` §"Add a New
Provider Adapter":

- `internal/provider/acp/` — full ACP v1 client, spec-shaped (Q5).
  Implements the whole spec surface even where cursor doesn't use it
  yet: JSON-RPC 2.0 over NDJSON stdio, initialize/capability
  negotiation, session new/load/prompt/cancel, `session/update`
  streaming (text, `agent_thought_chunk`, `ToolCallUpdate` with the
  `ToolKind` enum and diff content), `session/request_permission`,
  session modes and `session/set_config_option`/`set_model`, and the
  agent-calls-client surface: `fs/read_text_file`,
  `fs/write_text_file`, `terminal/*`. Future ACP agents (Grok, Gemini
  CLI, OpenCode) ride this layer free, but none is built in this
  effort.
- `internal/provider/cursor/` — cursor specifics: spawn/probe/auth,
  session lifecycle on the acp package, the private extension methods
  (`cursor/task` subagent notifications, `cursor/ask_question`,
  `cursor/create_plan`, `cursor/update_todos`,
  `list_available_models`), the transcript importer, and satellite
  config/catalog code (mirroring the `claude*`/`codex*` satellite
  packages where warranted).

## Key decisions

- Two-phase; Phase A alone and green first (Q1).
- Queued behind remote-access; ACP's client-side `fs/*` + `terminal/*`
  server surface is classified under the transport authz rules
  (`internal/transport/internalmethods.go`) from day one (Q4).
- Full-spec ACP package, cursor extensions layered separately (Q5).
  ACP is an external spec, so this is not the forced provider
  abstraction Core Principle 6 bans.
- ACP is the sole live session surface. Headless `-p
  --output-format stream-json` is for one-shot text generation
  (matching the claude/codex ephemeral-textgen pattern) and spike
  cross-checks only. Approvals don't work headless, and one session
  cannot be driven over two surfaces.
- No fabrication: features the wire lacks go in the Gap table, but only
  after config/access routes are exhausted (Q6).
- Discovery/enablement identical to claude/codex: probed and available
  when the binary is on PATH (Q7).
- Session import scrapes cursor's on-disk transcripts best-effort,
  rendered as-if-streamed. The no-file-scraping rule binds live
  operation only; import from disk is exempt by design (Q11).
- Accounts: `agent login` and `CURSOR_API_KEY` both first-class (Q12).
- MCP passthrough via `session/new` `mcpServers` + `.cursor/mcp.json`,
  with a tripwire for cursor's known silent-ignore failure mode
  (accepted the list, spawned nothing; regressed once in early 2026).
- Capability/version gating at `initialize`. The CLI self-updates on a
  roughly monthly churn cycle with date-based versions; never assume
  last month's surface, degrade gracefully.
- Wire expertise is a deliverable: `docs/references/acp-wire.md`
  (spec-level, provider-neutral) and `docs/references/cursor-wire.md`
  (extensions and divergences, spike-verified) get authored alongside
  the parser, same as the claude/codex wire references.
- Parser performance gets the same treatment as claude/codex: bounded
  memory, tool-call update coalescing (ACP agents can emit an update
  per redraw of a terminal tool), and `session/load` replay bursts
  streamed through triage, never buffered whole.

## Verified wire facts (pre-spike)

From cursor docs, the ACP spec, and t3-code's implementation
(`~/repos/t3-code`, drives cursor via `cursor-agent acp` through a
shared ACP runtime):

- Protocol: ACP v1 stable (spec package 1.7.0, 2026-08; v2 in draft).
  JSON-RPC 2.0, NDJSON, stdio.
- Flow: `initialize` → `authenticate` (`methodId: "cursor_login"`) →
  `session/new` | `session/load` → `session/prompt` →
  `session/update` notifications → `session/cancel`.
- `session/load` replays full history over the stream before its RPC
  response returns; t3-code races it with an idle gate (2s gap, 90s
  cap).
- Mid-turn steering: a prompt sent while one is in flight folds into
  the same turn (no concurrent/queued prompts).
- Sessions on disk are owned by the CLI; the id is the only resume
  token a client needs. Spike-verified location (CLI 2026.08.25):
  `~/.cursor/acp-sessions/<sessionId>/` holding `meta.json` plus a
  SQLite `store.db` (WAL). The JSONL location circulating in
  reverse-engineering posts does not apply to ACP sessions.
- Wire-native session enumeration exists: `sessionCapabilities.list`
  is advertised at `initialize`, and `session/list` returns
  `{sessions: [{sessionId, cwd, title, updatedAt}]}`. Scope is global
  (spike-verified: full list from an unrelated cwd, with per-row cwd
  for filtering), though not exhaustive — headless-created sessions
  were absent, so the importer diffs `~/.cursor/acp-sessions/` dirs
  against the list and falls back to store.db for the delta. With
  `session/load`'s replay, import is wire-first (enumerate + load).
- RuntimeMode mapping (decided): AO's tiers map to cursor's session
  mode plus spawn flags — read-only tiers → `mode: "plan"`;
  default/approval tiers → `mode: "agent"` with
  `session/request_permission` driving AO's approval registry;
  full-access tier → `mode: "agent"` + `--force`. `ask` mode is not
  used by AO (plan covers read-only). Cursor gets
  `EnforcesRuntimeMode` semantics equivalent to Claude's flag-based
  tier, enforced provider-side per mode.
- Auth lives in the CLI (`agent login`, or `CURSOR_API_KEY` /
  `--auth-token`). Nothing works unauthenticated.
- Permission modes: `agent` / `plan` / `ask`, plus sandbox and
  force/yolo config. Unanswered `session/request_permission` blocks
  tool execution indefinitely (no documented timeout).
- t3-code gaps that are client omissions, not wire limits: subagent
  visibility (`cursor/task` exists), MCP passthrough, thinking stream
  (`agent_thought_chunk` spike-confirmed streaming), slash-command
  advertisement (`available_commands_update` spike-confirmed: fires
  after `session/new` with builtin skills and the user's own skills).
- Spike-confirmed absences (CLI 2026.08.25): `session/fork` and
  `session/resume` both return `-32601 Method not found` (only
  `session/load` exists); no usage data of any kind was emitted across
  a full real turn. Rewind/checkpoints remain interactive-CLI only.
- First-turn capture facts (2026-08-31, `gpt-5.4-nano`): `initialize`
  advertises `promptCapabilities` image=true,
  embeddedContext=false, audio=false and `mcpCapabilities` http+sse.
  `session/new` returns modes (`agent`/`plan`/`ask`), the model
  catalog, and `configOptions` in one response; model ids use a
  structured grammar `name[k=v,...]` (e.g. `composer-2.5[fast=true]`,
  `gpt-5.4-nano[reasoning=medium]`), and the ACP catalog includes
  cheap tiers (`gpt-5.4-nano`, `gpt-5-mini`, `gemini-2.5-flash`).
  Tool-call lifecycle: `tool_call` (pending) →
  `tool_call_update` (title/rawInput/locations) → `in_progress` →
  `completed` with `content: [{type: "diff", ...}]`. Wire quirks the
  parser must survive: `toolCallId` embeds a literal newline joining
  two ids (`call_…\nctc_…`), and diff `oldText`/`newText` carry
  diff-header junk (`"-- /dev/null"`, `"++ b/<path>"` prefix line). A
  `session_info_update` kind carries title changes;
  `session/prompt` resolves with `{stopReason: "end_turn"}`. A file
  edit in `agent` mode ran with no permission request.
- `session/load` replay (spike-confirmed): the burst arrives before
  the RPC response and is a coalesced canonical transcript — consecutive
  thought/message chunks merged into single updates (101 live thought
  chunks replayed as 1), ordering and tool calls preserved; the
  response then carries current modes/models/configOptions with the
  session's model intact.
- Permissions (spike-confirmed): shell `execute` in `agent` mode fires
  `session/request_permission` with `toolCall` context and options
  `[{optionId: "allow-once", kind: "allow_once"}, allow-always,
  reject-once]`; a file edit prompts nothing. `plan` mode is enforced
  (edit request refused, zero tool calls) and switching emits
  `current_mode_update`. `session/set_config_option` takes `configId`
  (not `configOptionId`; wrong keys return `-32603` with zod-style
  detail) and returns the full refreshed configOptions.
- Execute output: completed `tool_call_update` carries
  `rawOutput: {exitCode, stdout, stderr}`.
- MCP passthrough (spike-confirmed WORKING on 2026.08.25): an
  `mcpServers: [{type: "http", name, url, headers}]` entry in
  `session/new` is connected immediately (initialize →
  notifications/initialized → tools/list over streamable HTTP) and
  tools are callable. Tool-call updates title as `"<server>: <tool>"`,
  `kind: "other"`, `rawInput: {providerIdentifier, toolName, args}`,
  but completed `rawOutput` is only `{success: true}` — the MCP result
  content is not surfaced in the tool call, only in the model's prose.
- Subagents (spike-confirmed): the parent turn shows a `tool_call`
  (`kind: "other"`, `rawInput: {_toolName: "task", prompt, description,
  subagentType}`) that completes with `rawOutput: {durationMs,
  isBackground}`, and a single `cursor/task` notification arrives at
  completion carrying `{toolCallId, description, prompt, subagentType,
  model, agentId, durationMs}`. No live child streaming and no child
  transcript on the wire; the child's findings only surface in the
  parent's prose.
- Client fs/terminal: cursor never calls `fs/*` or `terminal/*` even
  when the client advertises them — file and shell work is all
  agent-side. Implement them in `internal/provider/acp` for spec
  completeness (other ACP agents use them), expect zero cursor
  traffic.
- Image prompts work: `{type: "image", data: <base64>, mimeType}`
  content block accepted and seen by the model (spike: described a
  1px PNG).
- Turn control (wave-3 spikes, 2026-08-31): `session/cancel` resolves
  the in-flight `session/prompt` with `stopReason: "cancelled"`; the
  session stays usable and keeps the partial progress in context. A
  second `session/prompt` while one runs does NOT error and does NOT
  fold: cursor implicitly cancels the running turn (first prompt
  returns `cancelled`) and starts the new one with partial progress in
  context — mid-turn steering is cancel+resend semantics, simpler than
  t3-code's fold description.
- Resume: `session/load` then `session/prompt` works — replay, then
  live turns with full context. This is the AO resume path and it
  holds.
- Crash/limbo recovery: killing the process with a permission request
  unanswered leaves nothing stuck — a later `session/load` replays
  user + partial agent text and drops the never-approved tool call.
  Unanswered permission requests block the turn indefinitely until
  then.
- Error taxonomy: unknown-session `session/load` → `-32602 Invalid
  params` with `data.message`; `session/prompt` on unknown session →
  `-32603 Internal error` with `data.details` (inconsistent detail
  key); invalid model → `-32602` naming the value; malformed params →
  `-32603` with zod-style issue arrays.
- Extension methods (wave-3): `cursor/create_plan` is a real
  agent→client request `{toolCallId, name, overview, plan(markdown)}`,
  and todo lists arrive as the ACP-core `plan` update
  (`entries: [{content, priority, status}]`) — refusing create_plan
  degrades gracefully. `cursor/ask_question` is DORMANT on 2026.08.25:
  the model reports having no such tool in every mode (agent/plan/ask,
  with and without `_meta.parameterizedModelPicker`); implement the
  handler per the t3-code shape but expect no traffic until a CLI
  update revives it.
- Subagent permissions: a child's shell command bubbles a normal
  `session/request_permission` on the parent session; child output
  returns only in parent prose. `subagentType` was
  `{custom: {unspecified: {}}}` even for a named delegation.
- Cadence (nano turn, quantified): updates arrive in micro-bursts —
  median inter-chunk gap 0ms, p90 2ms, pauses up to ~3s — with tiny
  payloads (median thought chunk 4 chars; 101 chunks totalled 436
  chars). Parser-side coalescing is mandatory, not an optimization.
- Unreachable edge cases, resolved from spec vocabulary rather than
  observation (t3-code's cursor handling is generic passthrough, so
  only the vocabulary is borrowable): the ACP `StopReason` enum is
  `end_turn | max_tokens | max_turn_requests | refusal | cancelled`
  (first two observed; all five handled); auth expiry is spec error
  `-32000 authRequired` (handling is ours: one `authenticate` retry,
  then user-facing "run `agent login`"); rate limits have no cursor
  surface anywhere — Grok's ACP extension proves agents invent custom
  error codes (`-32003`) and non-spec stopReasons (`"rate_limit"`), so
  the parser must treat unknown stopReasons and error codes as
  non-fatal, user-visible errors with raw payload captured for the
  wire doc.
  Claude-Code-shaped events (`system`/`init` with
  `permissionMode`/`session_id`, `user`, `assistant` message chunks,
  `result`), and `result` carries `usage: {inputTokens, outputTokens,
  cacheReadTokens, cacheWriteTokens}` plus `duration_ms`,
  `request_id`. Usage is real but headless-only. Headless requires
  workspace trust (`--trust`); ACP sessions never prompt for trust.

## Gap table (living)

| Feature | Wire status | Resolution |
|---|---|---|
| Token usage per turn | none over ACP; headless `result.usage` proves the backend tracks it (S1+S13) | GAP over ACP; re-probe for `usage_update` on CLI updates |
| Thread fork | `session/fork` → Method not found (S2) | GAP: fork disabled in the provider catalog for cursor |
| Rewind/checkpoints | no ACP method documented | gap unless a spike finds a route |
| Non-image attachments | `embeddedContext: false` advertised at initialize | path-in-prompt; images native and spike-verified (S5) |
| Subagent child streaming | `cursor/task` fires only at completion; no child transcript on the wire (S8) | launch row + completion metadata only; no live child view |
| MCP tool result content | completed update carries `{success: true}` only (S3) | render call + args + success; result text stays in model prose |
| `cursor/ask_question` | dormant on 2026.08.25 — model has no such tool in any mode | implement handler (t3-code shape), expect no traffic; re-probe on CLI updates |
| Queued sends | second prompt mid-turn cancels the running turn (no queue, no fold) | AO steer maps to cancel+resend; a queue would be client-side fiction |

## Integration inventory (Phase B)

Per the how-to playbook and the claude-tui precedent: new provider id
`cursor` in `internal/provider` kinds + `Capabilities` switch;
registration in `spawnProviderSession` (`internal/app/app_session.go`);
new arms in the Phase A dispatch tables (settings, sessionimport,
workflowhost, mcpapp, provideraccountapp); SQLite CHECK-constraint
widen per the `rebuildProvidersV10SQL` pattern; frontend
`PROVIDER_IDS` + `PROVIDER_DEFINITIONS` entry; `ao-mockprovider` gains
an argv-sniff branch (`acp` verb) and a real ACP adapter speaking the
harness control protocol; kerneltest poisoning grows a cursor arm so
`make go-test` can never touch the live `~/.cursor` login;
`import-corpus-smoke` gains a cursor corpus root; provider-smoke gains
a cursor leg.

## Non-goals

- No second ACP agent (Grok/Gemini/OpenCode) in this effort.
- No cursor TUI variant (no claudetui analog).
- No synthetic usage numbers, fake subagent views, or emulated rewind.
- No mid-turn correction flow (repo-wide deferred item).

## Success criteria

- [ ] Phase A: `make verify` green, byte-identical claude/codex
      behavior, exhaustiveness tripwire in place
- [ ] Cursor thread live: streaming, thinking, tool calls + diffs,
      approvals, interrupt, mid-turn steer, in-session model switch,
      MCP servers, subagent visibility via `cursor/task`
- [ ] Importer round-trips a real cursor corpus through
      `import-corpus-smoke`, rendering close to as-if-streamed
- [ ] Mock ACP adapter + scenario coverage; cursor provider-smoke leg
- [ ] `docs/references/acp-wire.md` + `cursor-wire.md` authored and
      spike-verified

## Migration/removal

| Old | New | Action |
|---|---|---|
| Repeated identical-shape `switch provider` sites | per-provider function tables | MIGRATE |
| Two-way ternaries assuming claude-else-codex | catalog/table lookup | DELETE |
| Divergent branches (fork, runtime modes, subagents) | unchanged | KEEP — divergence is real (Principle 6) |

## Testing strategy

Phase A's oracle is the existing suites plus the exhaustiveness
tripwire. Phase B is mock-first: the ACP mockprovider adapter drives
harness/e2e with no account and no tokens; parsers get unit tests
written against the wire references; real-CLI verification is
provider-smoke plus a generated import corpus. Spikes run outside the
repo per `docs/references/spike-policy.md`, against the developer's
real authenticated CLI, cheapest models first (`composer-2.5`
workhorse at $0.50/$2.50 per 1M; `cursor-grok-4.6-low` only where a
second model is needed; never `-fast` variants or `auto`).

## Spike backlog

Environment: real `agent` CLI (2026.08.25+, Team plan), isolated spike
dir at `~/spikes/cursor-acp/` (rpc.mjs driver + `cap-*.jsonl`
captures). Each spike's captured wire traffic seeds the wire reference
docs. Spike models: `gpt-5.4-nano` / `composer-2.5[fast=true]`.

| ID | Question | Status |
|---|---|---|
| S1 | Does `agent acp` emit `usage_update` (or any usage data)? | CLOSED 2026-08-31: none observed on a full turn; re-probe each CLI update |
| S2 | Is `session/fork` implemented? | CLOSED 2026-08-31: `-32601`, same for `session/resume`; only `session/load` |
| S3 | MCP passthrough reality | CLOSED 2026-08-31: works end-to-end (connect at session/new, tools/list, tools/call); result content thin, see Gap table |
| S4 | Slash/custom commands over ACP? | CLOSED 2026-08-31: `available_commands_update` after `session/new`, builtin + user skills |
| S5 | Image content-block send shape | CLOSED 2026-08-31: base64 image block accepted and seen by the model |
| S6 | `session/load` replay burst: shape, ordering, fidelity | CLOSED 2026-08-31: coalesced canonical replay before RPC response; see Verified wire facts |
| S7 | On-disk session format (importer fallback input) | PARTIAL 2026-08-31: `~/.cursor/acp-sessions/<id>/` meta.json + SQLite (`blobs` + `meta`); blobs are raw model-conversation JSON, some compressed; full schema only matters if wire-first import proves insufficient |
| S8 | `cursor/task` subagent lifecycle | CLOSED 2026-08-31: completion-time notification only, no child streaming; see Verified wire facts |
| S9 | Thinking stream presence | CLOSED 2026-08-31: `agent_thought_chunk` streams (per-model coverage still worth sampling) |
| S10 | Permission-request shapes per mode | CLOSED 2026-08-31: execute prompts (allow-once/always, reject-once), edits don't; plan mode enforced; `ask` untested but same config surface |
| S11 | Capability advertisement vs reality | CLOSED 2026-08-31: every advertised capability checked out on 2026.08.25 (list, load, image, MCP http); re-verify after CLI updates |
| S12 | Does cursor call `terminal/*`/`fs/*` when advertised? | CLOSED 2026-08-31: never; all file/shell work is agent-side |
| S13 | Headless `stream-json` event shapes | CLOSED 2026-08-31: Claude-Code-shaped stream, `result.usage` present; needs `--trust` |
| S14 | Interrupt (`session/cancel`) semantics | CLOSED 2026-08-31: `stopReason: "cancelled"`, session continues with context |
| S15 | Mid-turn steer | CLOSED 2026-08-31: implicit cancel + new turn; see Verified wire facts |
| S16 | Load-then-continue (resume path) | CLOSED 2026-08-31: works, full context after replay |
| S17 | Error shapes (bad session/model/params) | CLOSED 2026-08-31: prompt typed errors; see taxonomy |
| S18 | Extension methods live behavior | CLOSED 2026-08-31: create_plan + core `plan` update real; ask_question dormant |
| S19 | Pending-permission across client death | CLOSED 2026-08-31: clean replay, pending call dropped |
| S20 | Subagent child permission routing | CLOSED 2026-08-31: bubbles on parent session |
