# internal/provider/claude/

Wraps the Claude Code CLI. One process per active thread, NDJSON over
stdio both ways.

The wire is documented once, in
[`docs/references/claude-wire.md`](../../../docs/references/claude-wire.md):
every envelope shape and subtype, the `tool_result` variant catalog (E1
to E9), the model-fallback family, permission notices, the task
lifecycle, `system/init.capabilities`, cross-session messaging, the
session-JSONL rules, and the captured fixtures that pin all of it. Read
it before adding or changing parser logic, and add new wire findings
there rather than here. This guide carries only what that reference
cannot: the rules that break something in THIS package.

## Spawn posture

Each flag below is unconditional, or conditional in a way that is easy
to "clean up" by mistake.

- `--forward-subagent-text` goes on every spawn. Without it an INLINE
  awaited agent (`run_in_background: false`) has its prose, thinking and
  final answer dropped before they reach the parent stream, so its card
  and pane show tool rows and nothing else.
- `Config.SystemPrompt` rides `--system-prompt-file <path>`, never an
  argv value. A long rendered prompt would fail every spawn with E2BIG
  (`MAX_ARG_STRLEN`) and would sit in `/proc/<pid>/cmdline`.
  `WriteSystemPromptFile` writes the 0600 temp file; `Close` and every
  failed-spawn path remove it, so `buildArgs` takes the PATH and never
  reads `cfg.SystemPrompt` itself.
- `--settings` (`inlineSettingsForCLI`) is the ONLY delivery route for a
  setting the CLI reads from its settings file rather than a flag. The
  CLI resolves `policySettings > flagSettings > userSettings` and
  reapplies its own settings `env` block over the inherited environment
  at init, so an env-backed axis handed to the subprocess as a plain
  variable is silently discarded. Every name that block carries is in
  `provider.ReservedEnvNames`, precisely so a user's custom environment
  cannot set a value the CLI would then ignore.
- Structured output is session-sticky: `Config.OutputSchema` goes out as
  inline JSON on `--json-schema` at spawn. Per-turn
  `provider.SendOptions.OutputSchema` is deliberately ignored by Claude.
- `WriteSystemPromptFile`, `RemoveSystemPromptFile` and
  `SanitizeDisallowedTools` are exported for
  `internal/provider/claudetui`, which passes the same two flags on its
  PTY launch. One writer and one argv-safety pass keep the two Claude
  transports from drifting on the temp file's mode and removal contract
  or on what counts as one safe CLI argument. `mergeDisallowedTools`
  stays unexported: the read-only mode strip it unions in is
  headless-only, because `EnforcesRuntimeMode` is false on claude-tui.
- Put a spawn-time-only axis on `Config` and NOT on
  `provider.SessionOptions`. `PlanLiveUpdate` diffs
  `ConfigFromOptions(prev)` against `ConfigFromOptions(next)`, so a field
  the options struct cannot carry is structurally "next sessions only",
  with no live-update branch and no reconcile pin needed for it.

## Restarting a session is the last resort

A restart costs the user their live process and their in-flight turn.
Route a config change to the live path first (`live_update.go`), and
only return "restart" when no wire form exists. This is a user ruling,
not a preference.

- `PlanLiveUpdate` reports what a live session can adopt; `ApplyLiveUpdate`
  applies it. Model and permission mode ride control_requests; effort and
  fast mode ride the CLI's own uuid-stamped `/effort` and `/fast` command
  sends, whose confirmation arrives later as an `EventCommandResult`
  carrying that uuid (`app_claude_live_config.go` settles those).
  Context-window changes ride the `[1m]` marker on `set_model`. Extended
  thinking rides `set_max_thinking_tokens`.
- `ApplyLiveUpdate` validates EVERY axis before ANY side effect,
  including refusing the command axes while the transcript still needs
  the resume-at repair, so a restart-bound update never half-applies. Its
  `preSend` hook fires between validation and the first wire write: the
  caller registers pending-confirmation state there, before the CLI can
  answer.
- The one direction with no wire form is returning thinking to the CLI's
  own choice: `max_thinking_tokens: null` is accepted and does nothing,
  so `PlanLiveUpdate` reports that as a restart.
- **Switching provider accounts needs no session restart.** The CLI
  resolves its credential from disk per request, so a live session adopts
  a swapped canonical credential from its next request onward. Never
  restart sessions to apply an account switch. Consequences and the
  re-billing gap are in `internal/provideraccounts/AGENTS.md`.

## Credentials

The CLI owns OAuth for the parser and session path. Nothing that talks
to the subprocess touches credentials.

- `login.go` is the one place that DRIVES an OAuth exchange, and it
  still writes no credential itself: the CLI does, into an isolated
  `CLAUDE_CONFIG_DIR` the caller cut, and the account layer decides
  where it goes from there. It runs on the headless control channel
  rather than `claude auth login`, because that command owns the browser
  and the URL and reports completion only by exiting — unusable on a
  host with no browser, and worse than unusable for a person signing in
  from a phone. Three rules bite: one flow is live at a time and a new
  `claude_authenticate` supersedes the previous one; ONE rejected
  callback burns the flow, so the recovery is a fresh link and never a
  re-prompt; and `claude_oauth_wait_for_completion` pends unbounded with
  no keepalive, so the deadline is ours and a supersede must abandon the
  wait rather than leak the goroutine holding it. Wire shapes:
  `docs/references/claude-wire.md` § The sign-in control channel.
- `ratelimits_probe.go` is the other exception: it reads the OAuth bearer
  out of a selected native credential path to query Anthropic's OAuth
  usage endpoint (Claude only emits `utilization` on the wire above the
  warning band). It is read-only on the credential file and never writes
  back. Poll cadence and triggering live in `app_claude_ratelimits.go`;
  the endpoint's 429 throttle is per-bearer and shared across machines,
  so the poll runs only when a turn completed since the last one.
- `rotation.go` is the rule that keeps the account probe from destroying
  the login it is probing. The CLI fires its OAuth refresh at startup as
  a DETACHED task and answers `initialize` from cached local state
  without awaiting it, so the probe's answer says nothing about whether
  the rotation reached disk — and under `--max-turns 0` the CLI exits on
  stdin EOF, which is what `Close` does first. Anthropic retires the old
  refresh token the moment the request is processed, so the gap is a
  permanently dead login, not a retry. `ProbeConfig.ReadCredential` arms
  a watch when the credential is at or inside the CLI's five-minute
  proactive-refresh buffer; teardown then holds until the credential
  actually changes. Measurements, the failure rates, and why a fixed
  delay is not a fix are on `rotationWatch`.
- `ratelimits_probe.go` — out-of-band HTTP probe of Anthropic's OAuth usage
  endpoint. Reads a bounded, regular native credential file, preserves every
  dynamically returned limit bucket, and falls back to
  `anthropic-ratelimit-unified-*` headers for older servers. Triggered from
  `internal/providerlifecycleapp` through `app_provider_services.go` (startup,
  plus a 2-minute poll that runs only
  when a turn completed since the last poll — the endpoint's 429 throttle
  is per-bearer and shared across machines, so turn completion marks
  activity rather than probing); emits go through the standard
  `provider:usage` channel.
- `mcpstatus.go` — ephemeral MCP status fetcher (`MCPStatusFetcher`,
  driven by `claude mcp list`) plus the `system/init` → unified status
  projectors (`MCPStatusFromRaw`, `MCPStatusFromListLine`) consumed by
  `internal/mcpstatus` via the shared `Fetcher` interface.
  `system/init.mcp_server_errors` (2.1.237) is the second half of that
  projection and lands on `SessionInfo.MCPServerErrors`: servers whose
  config entry the CLI REFUSED are absent from `mcp_servers[]`
  entirely, so without this array a rejected server is
  indistinguishable from one that was never configured. The two arrays
  never name the same server, which is why `ingestClaudeInitMCPStatus`
  can loop both without a collision, and every Put it makes from the
  error array carries a non-empty `Error` — the mcpstatus merge matrix
  stores those verbatim and only ever carries a prior explanation
  forward onto an error-LESS ephemeral fetch, so nothing has to be
  forced.
  Version floor worth knowing when reading old reports: before 2.1.221
  a `--mcp-config` server was not connected before the first
  print-mode turn, so a session's first turn ran with none of them
  available and `system/init` said so. AO does not gate on this — the
  supported-version floor is above it — but a stale bug report
  describing "MCP servers missing on the first message only" is that,
  not this projection.
  `sanitizeChildStderr` lives here too for bounding child-process
  stderr in user-facing errors.
- `sessionfork/` — subpackage. The fork transform over an existing
  transcript, plus the shared reading surface (`TranscriptTypes`,
  `ParseTranscript`, `ResolveParent`, `ResolveLogicalParent`,
  `SessionIDFromPath`) that both the live resume path and the importer
  read transcripts through. Has its own subarea guide.
- `sessionimport/` — subpackage. Read-only reader for
  `~/.claude/projects/…` behind session import: the lite lister (a stat
  plus two 64 KB reads per file, no parse), the conversation DAG and its
  leaf enumeration, the subagent join, and the transcript →
  `internal/importir` event projection. Spawns nothing, writes nothing,
  and never resolves the Claude home itself. It deliberately does NOT
  reuse `claudeBranchIndex` — that answers "which chain will
  `claude --resume` accept" and returns exactly one; import needs every
  leaf, and bending the live resume path to answer both is not worth the
  blast radius (invariant 28). Has its own subarea guide.

## Session, resume, and fork

- Resume uses `--resume <session-ref>`, with `--resume-session-at` for a
  cold resume at a chosen leaf. `sessionleaf*.go` reconstructs that leaf:
  `claudeLeafTracker` picks it in FILE order, `claudeBranchIndex`
  validates the pick against what the CLI's resume will actually accept
  (the active branch, run through a conservative mirror of the CLI's
  deserialization filters), and repairs a rejected pick to the deepest
  surviving row. `ResumeAtOnActiveBranch` is the exported spawn-time
  validator. See invariant 28.
- Row admission for that branch walk IS `sessionfork.TranscriptTypes`,
  one exported set shared with the fork transform. Do not copy it.
- **Sidechain rows never advance the leaf, and the two feeds spell
  "sidechain" differently.** A transcript row on disk carries
  `isSidechain`; the live stdout wire carries top-level
  `parent_tool_use_id` and never `isSidechain`, on `user` envelopes (a
  subagent's tool_results) exactly as on `assistant` ones. BOTH ingest
  paths gate on `parent_tool_use_id`. Without the `user` gate the
  tracker's leaf became a sidechain uuid for the whole duration of a
  Task, and every consumer resolves that leaf against the FILE, where the
  fork transform and the branch walk have already filtered sidechains
  out, so the lookup could only miss.
- Fork is replay-based for a tail fork and JSONL-slicing for an anchored
  one. The fork PIN mechanism (`pending_fork_session_ref` /
  `pending_fork_resume_at`, the live-source cut, who clears the pair)
  lives in
  [`recovery.md`](../../../docs/architecture/recovery.md) and
  [`providers.md`](../../../docs/architecture/providers.md). Read it
  there; do not re-derive it from this package.
- `sessionfork/` owns the JSONL transform plus the shared transcript
  reading surface. `sessionimport/` is a read-only reader for session
  import that deliberately does NOT reuse `claudeBranchIndex`: that
  answers "which chain will `claude --resume` accept" and returns exactly
  one, while import needs every leaf. Both have their own subarea guides.

## Parser rules

The parser is split by wire-envelope type so each NDJSON shape has one
owner; `ParseLine` in `parser.go` reads the envelope's `type` and
dispatches. Parser state is single-goroutine, driven by the read loop.

- Method names are part of the contract. `take*` / `consume*` read and
  clear state: use them only where there is exactly one lifecycle owner
  for the value, and document that owner on the method. `peek*`, `has*`,
  `is*` and `lookup*` are read-only. When a second same-boundary reader
  appears for a `take*` value, add a `peek*` companion rather than
  smuggling reads through the consuming method.
- State that may span multiple future envelopes needs an explicit
  cleanup point: `parseResult`, `Close`, or bounded map eviction.
- `system/init` is re-emitted before EVERY turn, so init handling must be
  idempotent and must not re-fire one-shot work. Anything new hung off
  `EventInit` needs the same treatment as `noteCapabilities` (replace
  under the live-state lock, log once, return early on an empty array).
  Regression: `TestParseSystem_RepeatedInitIsIdempotent`.
- Capabilities land on `provider.SessionInfo.Capabilities` and on the
  session behind `HasCapability` / `Capabilities()`. Nothing is keyed on
  them yet. When something is: light up a newer path on PRESENCE, and
  never refuse an older path on absence, because the stream-json engine
  under-reports what it implements.
- Absence is never a denial anywhere on this wire. `fast_mode_state`,
  `fast_mode_disabled_reason` and an absent `commands` / `tasks` key all
  mean "no signal", never "off" or "empty". An EMPTY array, by contrast,
  is usually a real replacement value; check the wire reference per
  envelope before treating one as the other.
- In `transcript_mirror`, `attributionSkill` is attribution, not ownership.
  It labels both inline-skill main work and forked-skill work. Only an
  `isSidechain:true` row carrying that attribution proves a direct command
  fork; `isSidechain:false` must never open a projector or parent later main
  activity beneath the command.

## Lifecycles this package drives

- Every `tool_use` produces a matching `EventToolComplete`. Universal
  invariant, all seven `user` `tool_result` variants included. See
  [`turn-lifecycle.md`](../../../docs/architecture/turn-lifecycle.md).
- `result` is authoritative for the turn's accounting payload, but it is
  NOT the only source of `EventTurnComplete`: `parse_stream.go` emits a
  typed `provider.SoftRoundCloseMeta` turn-complete on a "model has
  stopped" `stop_reason` with a null `parent_tool_use_id`, so the working
  indicator clears even when the CLI withholds `result` (which it does
  whenever a `local_agent` subagent is still in flight). A trailing
  `result` folds its payload in later via `persistLateTurnPayload`. See
  [`invariants.md §27`](../../../docs/architecture/invariants.md#27-soft-round-close-from-message_deltastop_reason-is-wire-typed).
  Re-lighting the indicator on a parent-content resume is triage's job,
  not the parser's; no parser change is needed for it.
- Wire usage values are SESSION-CUMULATIVE (`modelUsage`,
  `total_cost_usd`), and `modelUsage` is the only subagent-inclusive
  source. `usage_accounting.go` keeps a per-process snapshot and emits
  per-turn DELTAS. Cost is wire-reported only: there is no client-side
  pricing table here.
- Do NOT derive turn activity from item state. Do NOT emit lifecycle
  state from `task_notification`. Do NOT rewrite `tool_use_id` between
  start and complete. All three are enforced by
  [`invariants.md`](../../../docs/architecture/invariants.md).

## Cross-session messaging

Wire detail is in claude-wire.md §Cross-session messaging. What shapes
this package:

- **Off states the refusal.** `claudeCrossSessionInbound` returns
  `refuse` when the setting is off, so every Claude spawn carries the key
  and the `--settings` flag is always present. AO's gate is not the only
  one: remote GrowthBook flags can bind the inbox for a user who never
  enabled it here, and an absent key plus those flags means a
  class-matching peer auto-delivers into a thread nobody opted in. The
  env gate cannot express this, because the CLI checks the override for
  truthiness and falls through to the flag when unset.
- **`hold` is never emitted.** A held message waits for an approval a
  headless session cannot present. `internal/settings` refuses `hold` at
  the save and resolves an enabled-but-empty policy to `accept`.
- **Renaming is `/rename`, never `rename_session`.** The control request
  moves the session TITLE and leaves the peer registry alone, so it would
  report success while every peer kept addressing the old name.
  `RenamePeerSession` sends `/rename <name>` as an ordinary stdin user
  message with a client-minted uuid, which is what lets triage's
  pending-send correlator consume the send instead of stranding it.
- **The inbox setting rides `SessionOptions`; the peer NAME does not.**
  The inbox binds once during CLI setup, so a settings change converges
  by restart and `PlanLiveUpdate` sees it. The name is live-changeable
  for free, so it is stamped past `ConfigFromOptions` (`app_session.go`)
  and converges through `RenamePeerSession`, which is what keeps it from
  queueing a restart nobody needs.
- **A peer-started turn is a uuid we never minted.** `session_peer.go`
  holds the issued-command-uuid ledger. The classifier is fail-safe in
  one direction only: an unknown session or an overflowed ledger reads as
  OURS, because mislabelling a peer turn costs a missing label while
  mislabelling the user's own turn puts "from another Claude session" on
  a message they typed.

## Responsibility boundary

- Belongs here: NDJSON parse and marshal for every shape the CLI emits,
  per-session correlation maps, approval response encoding, binary
  probing, session spawn/read/signal.
- Does not: SQLite writes or `app.Event.Emit`, cross-thread coordination
  or retry policy, provider-agnostic event shapes (those live in
  `internal/provider/`).

## Anti-patterns

- Do NOT silently drop an NDJSON line. An envelope `type` no `ParseLine`
  case claims reaches `warnUnknownEnvelopeType`, which logs it once per
  type per parser lifetime and keeps reading, bounded at 64 distinct
  types. Log-and-continue is the design: a CLI release adding an envelope
  must not break a live session.
- Do NOT let a parse error kill the read loop. Log with enough context to
  reproduce and keep reading. There is a regression test; keep it
  passing.
- Do NOT kill the session when a control_request fails to ack. Surface
  the wrapped error. A kill also reaps backgrounded tasks, which inverts
  the documented foreground-only interrupt behaviour and masks the CLI
  bug.
- Do NOT touch UI shapes before adding parser and round-trip tests.
- Do NOT guess a wire shape from this repo. Spike against the real CLI
  ([`spike-policy.md`](../../../docs/references/spike-policy.md)), then
  land the finding in claude-wire.md and refresh the fixture from a new
  `AGENT_OVERFLOW_DEBUG=provider` run in the same commit.

## Extension points

- New NDJSON shape: pick the matching `parse_*.go` (or add one), add a
  round-trip test in the same commit, then wire the event type in shared
  `provider/` types.
- New approval Kind: extend `approvals.go` plus
  `provider.ApprovalRequest`, then wire the frontend branch. See
  `docs/architecture/how-to.md#add-a-new-approval-kind`.

## References

- Upstream SDK: `@anthropic-ai/claude-agent-sdk`.
