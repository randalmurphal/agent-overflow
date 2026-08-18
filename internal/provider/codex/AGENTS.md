# internal/provider/codex/

Wraps `codex app-server`. One process per active thread, JSON-RPC 2.0
over stdio.

## Layout

- `session.go` — process lifecycle, constructor, send/steer/interrupt,
  close, dynamic-tool handler registration, and the shared `Session`
  state struct. Everything the read loop touches is either owned by the
  read-loop goroutine alone (`usageAcct`) or guarded — `mu` for the
  mutable session state, `eventMu` for emission, and an atomic for the
  root Codex thread id (see below).
- `jsonrpc.go` — JSON-RPC request/response/notification writes,
  pending response correlation, read loop, and raw line dispatch.
- `account.go` — shared `account/read` decoding plus the cached app-server
  identity used to verify which saved login a live process is serving.
- `identity_probe.go` — fresh-process `account/read` probe used to detect
  native credential changes without a rate-limit request or model turn.
- `launch.go` — shared app-server CLI arguments. Agent Overflow pins Codex to
  its native `auth.json` credential store so account switching is a
  deterministic atomic file replacement even where Codex `auto` mode would
  otherwise choose an OS keyring.
- `session_notifications.go` — notification pre-processing, child
  routing, classifier invocation, event enrichment, turn-state tracking,
  and final event emission.
- `child_routing.go` — bounded/deadlined fail-closed quarantine for
  notifications and server requests from not-yet-owned child threads.
- `collab_rehydrate.go` — bounded read-only descendant-history traversal and
  active-child subscription recovery on root resume.
- `session_rollout_notifications.go` — narrow tailing of the active
  Codex rollout file for detached-child mailbox notifications that
  resumed app-server sessions do not expose as raw events.
- `server_requests.go` — server-initiated request dispatch:
  approvals, MCP elicitation, structured user input, and dynamic tools.
- `turn_input.go` — outbound `turn/start` / `turn/steer` user-input
  payload shaping.
- `interactive_requests.go` — pending approval/user-input tracking,
  dedupe, response claiming, interrupt/close drain, and lost-prompt
  resolution events.
- `plan_buffer.go` — proposed-plan delta buffering and fallback
  completion content.
- `raw_tool_calls.go` — raw `rawResponseItem` function-call tracking,
  `write_stdin`/`wait_agent`/`spawn_agent` enrichment, and event-log
  redaction for stdin payloads.
- `collab_agents.go` — spawned child-thread parent mapping, agent path
  mapping, receiver metadata enrichment, wait-target preservation, and
  child metadata refresh.
- `session_helpers.go` — thread start/resume params, sandbox policy,
  approval/user-input metadata builders, and route-field parsing.
- `session_probe.go` — `thread/read` liveness probe used on reopen to
  reconcile stale running rows. `session_probe_testhelpers.go` hosts
  the fake probe reply harness.
- `session_fork.go` — the `thread/fork` RPC wrapper (`Fork` / `ForkAt`);
  a `lastTurnId` cut (Codex >= 0.143) is the history-truncation
  primitive that replaced the deprecated `thread/rollback`.
- `session_background.go` — the three background-terminal RPCs:
  `ListBackgroundTerminals` (paginated enumeration with a cursor-progress
  guard), `TerminateBackgroundTerminal` (per-process stop; `terminated:
  false` means "matched nothing", not an error), and the thread-wide
  `CleanBackgroundTerminals`. All three need `experimentalApi`; all three
  sit above AO's 0.143 provider floor, so there is no runtime capability
  probe.
- `thread_settings.go` — `thread/settings/updated` reconciliation.
  `ThreadSettings` is what Codex IS running; the `LiveUpdate` block on
  Session is what AO will ASK FOR next turn. They are deliberately
  separate — see the type doc before merging them.
- `thread_settings_push.go` — the `thread/settings/update` RPC
  (`PlanThreadSettingsPush` / `PushThreadSettings`) plus the echo
  expectation it arms and the `serviceTier` write planner `turn/start`
  shares. Strictly ADDITIVE over the `turn/start` overrides: model,
  effort and fast mode were already next-turn-effective without it, so
  every failure path degrades to exactly that. Runtime-mode axes are
  deliberately NOT pushed — see the file's own doc block.
- `account_usage.go` — `account/usage/read`: the wire shape
  (`AccountUsage`, every summary field a pointer because absence is not
  zero), `Session.ReadAccountUsage` for a live connection, and
  `AccountUsageFetcher` for the ephemeral-process fallback.
  `classifyAccountUsageError` is the split between "this account/binary
  has nothing to report" (`ErrAccountUsageUnavailable`) and a real
  failure. Caching lives in `internal/codexusage`.
- `notification_catalog.go` — the pinned catalogue of upstream
  notification methods, the derived `optOutNotificationMethods` sent at
  initialize, the shared `codexInitializeParams` builder, and the
  per-session protocol-drift log.
- `protocol.go` — top-level notification dispatch plus small shared
  notification helpers. `classifyNotification` returns the `handled`
  flag `ClassifyNotification` drops; that flag is what drives both the
  drift log and the opt-out derivation.
- `protocol_item.go` — `item/*` lifecycle, tool completion, item type
  normalization, and tool-result content extraction.
- `protocol_turn.go` / `protocol_thread.go` — turn lifecycle,
  thread/account/model notifications, and token usage normalization.
- `usage_accounting.go` — per-turn token accounting derived from the
  cumulative `thread/tokenUsage/updated` totals (Codex has no per-turn
  usage signal and no USD cost on the wire). Tracks
  accounted-vs-latest cumulative per session, handles the resume
  baseline (skips the first post-resume turn when no pre-turn seed
  arrived) and the ContextWindowExceeded sentinel that destroys the
  cumulative; `attachTurnUsage` stamps the delta onto the parent
  turn-complete meta in `updateNotificationState`. Wire `inputTokens`
  includes `cachedInputTokens` — the normalized delta splits them.
  `cacheWriteInputTokens` is a separate billed class on its own
  cumulative axis and lands on `TokenUsage.CacheCreationInputTokens`,
  the same field the Claude adapter fills; app-servers predating the
  field omit the key and it reads as zero.
- `protocol_json.go` — JSON navigation, compact/pretty JSON, retry-count,
  and flexible wire-shape helpers.
- `protocol_rate_limits.go` — rate-limit event normalizer.
- `approval.go` — sandbox approval-method translation into the shared
  `ApprovalRequest` Kind. MCP elicitation lands here too as
  `Kind: "mcp-elicitation"`; the dispatch lives in `server_requests.go`'s
  server-request handler.
- `subagent_notifications.go` — `<subagent_notification>` XML-tag parser
  for detached-child-agent terminal signals in raw mailbox carriers,
  rollout `response_item` records, and legacy standalone user-message
  carriers. Pure parsing (regex + JSON decode), no Session state.
- `session_review.go` — `review/start` (`StartReview`) and
  `thread/compact/start` (`CompactThread`). `ReviewTarget` is a closed
  union with unexported fields and four validating constructors, so the
  four variants' different required payloads cannot be mixed and the zero
  value refuses to marshal rather than defaulting to reviewing something.
  `ReviewStarted.Detached` is derived from the RETURNED `reviewThreadId`
  versus the session's own thread — never from the requested delivery,
  which is what upstream's own TUI routes on. A detached review's
  notifications land on a thread this session does not own and are
  quarantined fail-closed; surfacing them needs the returned id registered
  with the routing tables first.
- `skills.go` — `skills/list` (`Session.ListSkills` for a live connection,
  `SkillsFetcher` for the ephemeral fallback), the wire→`codexskills.Skill`
  projection, and the `skills/changed` side channel. Every cwd must be
  absolute and the list must be non-empty: upstream resolves a relative
  path against the ANSWERING process's cwd and an empty list against that
  process's own directory, so both would mean different things on a live
  session than on a throwaway fetcher. Caching lives in
  `internal/codexskills`.
- `oneshot_client.go` — the shared spawn + `initialize`/`initialized` +
  sequential request/response client behind the thread-less reads
  (`model/list`, `skills/list`). `oneshotSpec.Experimental` and
  `KeepNotifications` are per-caller because a throwaway process asking
  for more than it uses changes what the server emits.
- `disabled_tools.go` — the curated tool-toggle vocabulary the settings
  UI offers (`ToggleWebSearch` … `ToggleToolSuggest`) and the table
  mapping each id onto the `config` override keys that turn the tool off
  (`web_search: "disabled"`, `tools.update_plan.enabled: false`, …).
  Keys stay DOTTED and flat — codex expands them into nested TOML itself
  (`config/src/overrides.rs`), so pre-nesting them here would be a second
  encoding of the same thing. `buildThreadParams` merges the result, which
  is what makes `thread/resume` carry the toggles too: a cold resume that
  omitted them rebuilds the thread with the tools back in the request. An
  id this build does not know is skipped with a log line — the list is
  settings data that outlives any one AO version — never a fatal. The id
  set is mirrored by the frontend's toggle switches
  (`frontend/src/lib/utils/promptOverrides.ts`), and
  `TestDisabledToolTogglesMatchTheFrontendMirror` parses that file —
  renaming an id here without the frontend fails the test, not the user.
- `options.go` — `SessionOptions → Config` hydration, binary probe.
- `models.go` — `model/list` paging plus the `codexModel → provider.ModelInfo`
  projection. **`serviceTiers` is the model's whole tier menu, not a fast-tier
  list.** Upstream ships `flex` on the same enum
  (`ServiceTier::Flex`, `codex-rs/protocol/src/config_types.rs`) and its own
  fixtures carry `{id:"batch",name:"slow"}`, so "the model declares a tier"
  never means "the model is fast-capable" — a fast-mode turn sent onto `flex`
  or `batch` runs SLOWER. `codexFastModeTier` identifies the fast entry by
  either of upstream's own two anchors, `id == "priority"`
  (`ServiceTier::Fast.request_value()`, what `ModelPreset::supports_fast_mode`
  matches) or `name == "fast"` (`SPEED_TIER_FAST`, what the TUI's
  `current_model_fast_service_tier` matches), and carries the matched entry's
  id, name and description onto `ModelInfo.FastModeTier`. That is what makes an
  upstream rename inert in either direction: the id AO sends and the label the
  composer shows both come off the wire, with `priority`/`Fast` surviving only
  as the fallback for the deprecated `additionalSpeedTiers` path. Adding a
  third anchor is a wire question — confirm it in codex-source first.
- `mcpstatus.go` — ephemeral MCP status fetcher (`MCPStatusFetcher`,
  drives `mcpServerStatus/list` via an inline JSON-RPC client) plus
  the wire-shape projectors (`MCPStatusFromList`,
  `MCPStatusFromNotif`) consumed by `internal/mcpstatus` via the
  shared `Fetcher` interface. Backs both the inactive-thread fallback
  and the `mcpServer/startupStatus/updated` /
  `mcpServer/oauthLogin/completed` notification paths. Three rules:
  - **A list response describes SETTLED attempts.** Every call builds a
    fresh connection set (threadId only selects config, it never reads a
    loaded thread's manager) and awaits each pending client's startup
    before answering, so `MCPStatusFromList` can never return
    `StatusStarting` — only a startup notification can. Its liveness
    signal is `serverInfo` presence, which MCP makes mandatory in a
    successful `initialize` and codex echoes at every detail level;
    tool count is the safety net, not the signal. No config-shaped
    field (command/args/env/headers) is decoded here — only
    `MCPServerInfo`'s name/version.
  - **`MCPStatusFromNotif` takes the whole `MCPStartupUpdate`** so a
    failure carrying `failureReason: "reauthenticationRequired"`
    resolves to needs-auth (which surfaces the existing Sign in action)
    instead of a dead error string. It must never be *depended* on: a
    revoked-but-structurally-intact refresh token fails with
    `failureReason: null` deterministically (see
    [`codex-wire.md`](../../../docs/references/codex-wire.md)), so the
    plain failed state has to be actionable on its own.
  - **Startup updates are retained per session.**
    `Session.MCPStartupStates` (state on `session.go`, written in
    `session_notifications.go`) keeps the last update per server name,
    last-write-wins, independent of whether an observer is registered.
    `app_mcp_thread.go` merges it over the list so a thread reports the
    lifecycle it watched — with the cause string a probe cannot carry —
    while the list stays the membership answer and the reconciler,
    because delivery is lossy. Two rules bound the merge:
    - **Only TERMINAL retained states (`TerminalFailure`: failed /
      cancelled) outrank the settled list.** The list awaits pending
      startups before answering, so it is always the newer observation
      for a non-terminal retained state — a retained "starting" latching
      over a connected probe is exactly the incident shape this exists
      to prevent. Unrecognized future states defer to the list too.
    - **AO-initiated restarts forget first.** Every path that asks Codex
      to restart a server (OAuth success, enable/disable toggle,
      Reconnect) calls `ForgetMCPStartupState(name)` before the reload:
      the retained failure describes a run AO just invalidated, and a
      fresh startup round only arrives at the next turn boundary — so
      without the forget, a fixed server would keep reporting the dead
      run's failure until then.
    Retention itself is bounded (`session_notifications.go`): names over
    256 bytes drop the whole update loudly, error strings clamp
    rune-safe at 2 KiB, and the map caps at 128 names (known names still
    update at cap; new ones log and skip retention but still reach the
    live handler).
- `rollout/` — subpackage. Read-only reader for Codex's own on-disk state:
  the `state_5.sqlite` thread index (session listing) and rollout JSONL
  (parse → `internal/importir` events) behind session import. Spawns
  nothing, writes nothing, and never resolves the Codex home itself. Has its
  own subarea guide.

## Methods we call

- `thread/start`, `thread/resume`, `thread/fork` (optionally cut at a
  `lastTurnId` anchor — the revert/fork history primitive; upstream
  deprecated `thread/rollback` and AO no longer calls it).
  The handshake start/resume (`buildThreadParams`) always names
  `approvalsReviewer`, and `verifyApprovalsReviewerEcho` reads it back off
  the response before the session is handed out: the params struct has no
  `deny_unknown_fields`, so a codex without the field would start a
  `user`-reviewer thread on a successful-looking response, silently
  downgrading the `auto` runtime mode. An absent echo reads as `"user"`
  (upstream's field is non-`Option`, so silence can only be the drop) and a
  mismatch fails the session with a user-facing error rather than running
  unreviewed. The mid-life reconcile resumes (`session_probe.go`,
  `collab_rehydrate.go`) send no overrides on purpose — Codex ignores
  overrides on a resume of a loaded thread and a divergent one arms its
  shutdown-and-cold-resume branch. See
  [`codex-wire.md §approvalsReviewer`](../../../docs/references/codex-wire.md).
  `thread/fork` names none of the config axes — model, sandbox, approval
  policy and reviewer all come back as config defaults on the forked thread —
  and that is safe for the same reason for all four: nothing executes on a
  fork until a `turn/start`, which re-asserts every axis. Do not special-case
  the reviewer here without doing the same for the sandbox.
- `thread/read` — on-reopen liveness probe. Called by `Session.Probe`
  to fetch the current `thread.status.type` so the app-layer reconciler
  can flip stale running background tool rows.
- `thread/backgroundTerminals/list` / `.../terminate` / `.../clean` —
  enumerate, stop one, stop all model-initiated background PTYs
  (`session_background.go`). `terminate` joins on the `processId` the
  item meta already carries.
- `thread/settings/update` — `#[experimental]` upstream, so it rides the
  `capabilities.experimentalApi` every AO session handshake already sets.
  Pushes a model / effort / `serviceTier` change into Codex's own thread
  state BETWEEN turns (`thread_settings_push.go`), and gets the
  authoritative
  `thread/settings/updated` echo back instead of assuming. It is an
  addition to the `turn/start` overrides below, never a replacement:
  those already made the change next-turn-effective, so a codex that
  refuses the method degrades to exactly the previous behavior. Two
  rules the code enforces:
  - **Never mid-turn.** The app layer gates the call on the thread being
    idle (`app_session_config.go#threadTurnInFlight`). The RPC's value is
    the echo and the absence of a restart, not mutating a running turn.
  - **Runtime-mode axes are not routed through it.** `approvalPolicy`,
    `sandboxPolicy` and `approvalsReviewer` stay on `turn/start` by
    deliberate design (see RuntimeMode in the parent guide); pushing
    them here would also arm the echo check against fields the
    handshake already verifies.
  Its params are the same double-option shape as `turn/start`'s: an
  omitted key means "unchanged", an explicit `null` clears to the config
  default. That is why turning fast mode OFF sends `serviceTier: null`
  rather than omitting it, and why the clear is scoped to a tier AO
  itself asserted — a user's `config.toml` tier must survive.
- `account/usage/read` — Codex's account-level token report (lifetime /
  peak-day / streak / longest-turn totals plus ~12 months of daily
  buckets). Global (`serialization: None`), so it needs no thread
  context: a live session answers it in one round trip and
  `AccountUsageFetcher` starts a short-lived app-server when none is.
  Absence is a state, not a failure — an API-key login has no usage
  profile at all.
- `skills/list` — which skills are visible from a set of directories.
  Global (`serialization: global_shared_read("config")`), not
  `#[experimental]`, since 0.73.0, so a live session answers it for ANY
  workspace in one round trip and `SkillsFetcher` starts a short-lived
  app-server when none is. Skills replaced custom prompts, which upstream
  removed in 0.118 — there is no legacy method to fall back to. The
  request always names absolute directories; see `skills.go` for why the
  wire's empty-`cwds` default is never used.
- `review/start` — Codex's built-in code review, on one of four targets
  (`uncommittedChanges` / `baseBranch` / `commit` / `custom`) delivered
  `inline` (default) or `detached`. Not experimental, since 0.59.0. The
  response's `reviewThreadId` is the routing authority for everything the
  review emits.
- `thread/compact/start` — compact this thread's context now. Not
  experimental, since 0.96.0; response body is empty. The boundary arrives
  as the `contextCompaction` item, NOT on the response, and the older
  `thread/compacted` notification is deprecated — both feed the same
  `EventCompactBoundary`.
- `turn/start`, `turn/steer`, `turn/interrupt` — deliver, steer, and stop
  user turns. `turn/start` carries per-turn config overrides (`model`,
  `effort`, `serviceTier`, `approvalPolicy`, `sandboxPolicy`,
  `approvalsReviewer` — upstream
  documents each as applying "for this turn and subsequent turns"),
  which is how a mid-session model / effort / fast-mode / runtime-mode
  change lands without a session restart (`live_update.go`).
  `provider.SendOptions.OutputSchema` is sent as `outputSchema` on every
  schemaed `turn/start`; it is per-turn and never sticky. The final completed
  agentMessage text is JSON-parsed into the turn-complete structured payload.
  `turn/steer` takes NO config fields, so an in-flight turn can never
  be reconfigured.
- Sandbox/approval method family — `file/write`, `file/delete`,
  `file/mkdir`, `command/execute`, etc. These arrive as requests we
  must respond to.

## Notifications we handle

⚠ **Authoritative wire reference**:
[`docs/references/codex-wire.md`](../../../docs/references/codex-wire.md).
Read that before adding or changing handler logic — it has the
canonical JSON-RPC examples, pinned citations into codex-source
and CodexMonitor, and the rules for how `item/*`, `turn/*`, and the
collab-agent lifecycle interact.

Summary:

- `turn/started`, `turn/completed` — **turn lifecycle**. Authoritative
  turn-complete signal; emit
  `EventTurnComplete` which triage forwards as
  `provider:turn_completed` to the frontend.
- `item/started`, `item/completed` — **tool lifecycle**. Every tool
  produces both (one-shot upsert pattern). Items carry their own
  `status` field (`inProgress | completed | failed | ...`) directly
  on the wire.
- `thread/status/changed` — session-level state.
- `thread/settings/updated` — Codex's authoritative config echo.
  Reconciled into the session's observed snapshot
  (`thread_settings.go`), never into the requested turn config, and
  never emitted as a transcript event.
- `model/safetyBuffering/updated` — the response is held while OpenAI
  reviews the turn. Emits a notification row on `showBufferingUi: true`
  only; the clear edge is handled but silent.
- `skills/changed` — side channel, not transcript content. Upstream types
  it as an EMPTY struct and documents it as an invalidation signal, so it
  carries no cwd, no scope and no skill name: the App layer drops the whole
  `internal/codexskills` cache rather than pretending to narrow the scope.
  Claimed in `sessionSideChannelNotifications`, which is also what keeps it
  out of the initialize opt-out list.
- Rate-limit, model-reroute, reasoning-delta, thread-rename,
  thread-compact events.
- `error` notifications (user-facing state, not log entries).

**We tell Codex what not to send.** `initialize` carries
`capabilities.optOutNotificationMethods`, computed as the complement of
what this package consumes (`notification_catalog.go`): Codex drops those
per connection before serializing them
(`codex-rs/app-server/src/transport.rs`). The list is derived, never
hand-written — adding a classifier case or a side-channel handler removes
the method from the opt-out automatically. The short-lived clients (login,
probes, model list, MCP status) opt out of the whole catalogue except the
notifications they explicitly name.

A method the catalogue does not know is never opted out; it arrives, no
classifier claims it, and the per-session drift log names it once. That
pairing is the whole scheme: known-and-ignored is silent because it never
arrives, and unknown is loud.

## Lifecycles we drive

- **Tool lifecycle** — every `item/*` fires `EventToolStart` /
  `EventToolComplete` for the item's own id. Status comes from the
  wire `item.status`. See
  [`turn-lifecycle.md §Tool lifecycle`](../../../docs/architecture/turn-lifecycle.md#1-tool-lifecycle).
- **Turn lifecycle** — `turn/completed` carries `{threadId, turn}`.
  `turn.status` is the lifecycle authority; current Codex does not
  include a final assistant message id on this envelope.
- **Background terminals.** Codex has no `run_in_background` flag, but
  `exec_command` can yield back to the model while its PTY keeps
  running. Typed `item/started` / `item/completed` commandExecution events are
  the UI history source; raw `exec_command` tool output is model-facing text
  that may enrich live process metadata but must not create, delay, or reorder
  chat history. `TerminalInteractionNotification` is the source for waited and
  interacted marker rows (per
  [invariant 25](../../../docs/architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics)).
  This package surfaces item lifecycle fields via `enrichItemMeta` and emits
  an internal `EventCodexExecResult` from raw `exec_command` output only for
  live-state enrichment.
  Heuristic event-ordering classifiers are still forbidden as the
  authorization — the yield moment is just the observable trigger for
  an already wire-authorized commitment. Per-row stop is wired end to
  end: `thread/backgroundTerminals/terminate {threadId, processId}`
  (codex 0.140.0+) is wrapped in `session_background.go`, bound as
  `App.TerminateCodexBackgroundTerminal`, and drives the same tray Stop
  button Claude's `stop_task` drives. The join key is the `process_id`
  `enrichItemMeta` stamps on the item, which triage allowlists onto the
  transient tray row — see
  [`codex.md §Background terminals`](../../../docs/references/codex.md#background-terminals).
  Stopping a row is a user action on an already-authorized row and never
  a source of `is_background` authorization.

## Collab agent lifecycle (MultiAgentV1 and MultiAgentV2)

Codex's closest analog to Claude's backgrounded tools, but
structurally different — **a spawn creates a child thread**, not
a backgrounded tool. See
[`codex-wire.md §Collab agent lifecycle`](../../../docs/references/codex-wire.md#collab-agent-lifecycle-multiagentv1-and-multiagentv2)
for the wire sequence. Key points:

- V1 ownership arrives on `collabAgentToolCall spawnAgent`; V2 ownership
  arrives on completed `subAgentActivity kind:"started"`.
- V2 may start/stream the child before the ownership item. Every unknown
  non-root thread is quarantined fail-closed, including recursively spawned
  grandchildren; child lifecycle never becomes root lifecycle.

- `spawn_agent` tool_call completes **immediately** (status:
  completed) when the spawn request is accepted; child runs on a
  separate `thread_id`.
- Parent's `turn/completed` does NOT wait for spawned children.
- Child completion changes launch live state, but transcript presentation waits
  until Codex delivers the result into parent model context. MultiAgentV2 records
  that boundary as a parent-rollout `inter_agent_communication` containing a strict
  `Message Type: FINAL_ANSWER` envelope. Agent Overflow emits the flat completion
  row only from that record. Legacy flows use either explicit `wait` or a
  mailbox-delivered raw message carrying a `<subagent_notification>` XML tag.
  Codex exposes the legacy marker
  as contextual `role:"user"` input. Fresh `thread/start` sessions can
  expose it through `rawResponseItem/completed` when raw events are
  enabled; resumed sessions cannot opt into that raw stream, so
  Agent Overflow also tails the active rollout JSONL from EOF and parses
  appended `inter_agent_communication` records. Traces
  and older flows can expose the same marker inside a serialized
  `InterAgentCommunication` assistant/commentary message or a standalone
  user-message carrier.
- Wire enum for the wait tool is `"wait"` (not `"waitAgent"`).

## Responsibility boundary

- What BELONGS here:
  - JSON-RPC frame marshal / unmarshal and v2 ThreadItem shape handling.
  - Sandbox-method → `ApprovalRequest` translation.
  - Session request/response/notification correlation.
  - `options.go` binary probe and `Config.From(SessionOptions)`.
- What does NOT belong here:
  - Raw JSON-RPC shape inspection outside this package. Other callers
    should only see the normalized `provider.Event` types.
  - SQLite writes, `app.Event.Emit`, or cross-thread orchestration.

## Interactive Requests

Sandbox method requests (`file/write`, `command/execute`, ...) are
translated into the shared `ApprovalRequest` shape before leaving this
package. MCP elicitation comes in as its own method and currently maps
to `Kind: "mcp-elicitation"` — confirm the frontend has a branch for
this before relying on it.

`item/tool/requestUserInput` is a separate structured user-input flow, not
an approval kind. Validate that at least one question is present before
emitting, track it with the user-input resolve kind, and answer through
`RespondToUserInput`.

## Extension points

- To add a new provider notification: add it to the matching
  `protocol_*.go` classifier, update `session_notifications.go` only if
  it needs session state/routing, then unit test the normalized
  `provider.Event` output.
- To add a new server request: dispatch it in `server_requests.go`, add
  or reuse the metadata builder, and test the JSON-RPC response path.
- To add a new sandbox approval method: extend `approval.go`'s kind
  table and the shared `provider.ApprovalRequest`. Spike against
  `codex app-server` if the method shape isn't obvious from
  `codex-source`.

## Anti-patterns

- Do NOT inspect raw JSON-RPC shape from outside this adapter. Callers
  see normalized `provider.Event` values only.
- Do NOT invent `ThreadItem.status` values. Respect the v2 status enum
  from upstream — if a new status is needed, update `protocol.go` and
  the reconciler together.
- Do NOT fall back to a generic "unknown notification" path. A
  notification type is either dispatched or explicitly opted out.
  Enforced: a method no classifier claims and no session handler consumes
  reaches `warnUnclaimedNotification` (`notification_catalog.go`), which
  logs it once per method per session. Silence there previously let seven
  upstream notifications arrive unnoticed between the 2026-06 and 2026-07
  surveys. It is log-and-continue by design — a Codex release adding a
  notification must not break a live session — but it is never silent.
- Do NOT reach for the root Codex thread id as a field. It is
  `codexThreadID atomic.Pointer[string]`; read it with `rootThreadID()`
  and write it with `setRootThreadID()`. `NewSession` must start
  `readLoop` before it can receive the `thread/start` response that
  carries the id, so the constructor's write races every read-loop path
  that consults it — routing (`isUnmappedForeignProviderThread`),
  settings reconciliation, and collab ownership. It cannot move under
  `mu`: two of those readers hold `mu` already and would self-deadlock.
  There is exactly one write path (the resume seed, then the handshake
  response), both in `NewSession`.

## References

- **Codex source** — https://github.com/openai/codex
  Local: `/home/rmurphy/repos/codex`. Canonical wire format. If
  our parser disagrees with the source, the source wins.
- **CodexMonitor** — https://github.com/Dimillian/CodexMonitor
  Tauri, feature-complete client. Reference for client-side patterns:
  process lifecycle, reconnect, interruption, rollback, fork.
- Codex App Server docs: https://developers.openai.com/codex/sdk/#app-server
- `docs/references/codex.md` — workflow for reading these sources.
- `docs/references/spike-policy.md` — when both are silent.
