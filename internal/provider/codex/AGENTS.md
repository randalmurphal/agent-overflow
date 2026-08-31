# internal/provider/codex/

Wraps `codex app-server`. One process per active thread, JSON-RPC 2.0 over
stdio.

**The wire is documented elsewhere.**
[`docs/references/codex-wire.md`](../../../docs/references/codex-wire.md) is
authoritative for JSON-RPC shapes, notification semantics, the collab-agent
lifecycle, history modes, and the queue. Read it before changing handler
logic. This guide carries only what an agent must know before editing the
package: concurrency rules, version gates, and the AO-side decisions that look
arbitrary from the code. Provider floor is codex 0.143
(`provider.minimumCodexCLIVersion`), and the installed CLI at the last sweep
was 0.150.1 (2026-08-29). Run `codex --version` before trusting a version
claim here.

- `session.go` — the shared `Session` state struct, the `Config` it is built
  from, the accessors over both, dynamic-tool and MCP handler registration
  (plus the retained per-server startup states), and `Close`. Everything the
  read loop touches is either owned by the read-loop goroutine alone
  (`usageAcct`) or guarded — `mu` for the mutable session state, `eventMu` for
  emission, and an atomic for the root Codex thread id (see below).
  State is **grouped by concern into sub-structs** — `turn` (per-turn state),
  `origins` (who started each turn), `turnConfig` (what the next turn asks
  for) versus `settings` (what Codex reports it is running), `collab` (child
  identity), `childRouting` (the ownership quarantine), `collabHistory` (the
  resume traversal) and `rawCalls`. The locks, the atomics that exist to stay
  out of the lock order, the process, and the registered observers stay at the
  top level, as do the two fields guarded by a lock other than `mu`
  (`childLifecycleRevision`, `collabAsyncClosing`) — a group is assigned WHOLE
  under `mu`, so a field with a different guard cannot live in one.
  `Close` drops the session-scoped groups by zeroing each group rather than
  field by field, which is what keeps a field added later from being
  forgotten; `TestCloseReleasesSessionScopedState` fills every field of every
  such group reflectively and lists the state Close deliberately leaves.
- `session_start.go` — the sequence that turns a spawned process into a
  session with a thread. `NewSession` does the spawn, the `initialize`
  handshake, the version and queue-support records frozen off it, and the
  `BeforeResume` window a resume opens before the thread is loaded;
  `startOrResumeThread` runs the one `thread/start` / `thread/resume` RPC and
  everything that has to be true about its response before the session is
  handed out — the paginated-history downgrade retry, the thread-identity and
  `approvalsReviewer` echoes, the history-mode record, and a resume's collab
  rehydration. Every failure here means "there is no usable thread".
- `session_turn.go` — the turn verbs (`Send` / `Steer` / `Interrupt`), the
  per-turn output-schema binding that carries `SendOptions.OutputSchema` onto
  the turn id the wire hands back, the `turn/started` dedupe ledger, and
  `classifySteerRejection`. Send CLAIMS the turn before the write
  (`beginLocalTurnStart`) because `turn/started` can beat `turn/start`'s own
  response onto the read loop, and `clearTurnStart` releases what a timed-out
  claim left behind — see §"Externally queued turns" for what reads those
  claims. Both outbound verbs stamp `SendOptions.ClientUserMessageID` as
  `clientUserMessageId` — see §"Turn identity and the steer contract".
- `jsonrpc.go` — JSON-RPC request/response/notification writes,
  pending response correlation, read loop, and raw line dispatch.
- `account.go` — shared `account/read` decoding plus the cached app-server
  identity used to verify which saved login a live process is serving.
- `identity_probe.go` — fresh-process `account/read` probe used to detect
  native credential changes without a rate-limit request or model turn.
- `credential_identity.go` — `CredentialOrgID`: the ChatGPT workspace id
  parsed straight from `auth.json` bytes (id_token claim preferred over
  the refresh-stale top-level `account_id`), the org axis `account/read`
  structurally cannot carry.
- `launch.go` — shared app-server CLI arguments. Agent Overflow pins Codex to
  its native `auth.json` credential store so account switching is a
  deterministic atomic file replacement even where Codex `auto` mode would
  otherwise choose an OS keyring.
- `session_notifications.go` — notification pre-processing, child
  routing, classifier invocation, event enrichment, turn-state tracking,
  and final event emission. `dispatchRoutableNotification` is five steps in a
  fixed order, each a named function taking and returning its values
  explicitly: `resolveNotificationRoute` (which thread this frame belongs to,
  including the params rewrite that can change the answer, and the
  inline-review claim), `claimNotificationOwnership` (what the frame proves
  about child ownership, the spawn card it renders on, and the mailbox-carrier
  consumption that must happen before agent-path retention),
  `interceptChildNotification` (what a child may not project onto the parent,
  plus the token-usage carve-out that is re-scoped instead of dropped),
  `foldNotificationOntoParent` (plan delta, settings echo, usage accounting)
  and `classifyAndEmitNotification`.
- `child_routing.go` — bounded/deadlined fail-closed quarantine for
  notifications and server requests from not-yet-owned child threads.
- `collab_rehydrate.go` — bounded read-only descendant-history traversal and
  active-child subscription recovery on root resume.
- `session_rollout_notifications.go` — narrow tailing of the active
  Codex rollout file for detached-child mailbox notifications that
  resumed app-server sessions do not expose as raw events. The tail is
  ARMED, never unconditional: a resume records the rollout path and only
  starts polling when the thread can actually hit that gap — either
  `Config.ResumeHasUnresolvedSubagents` (the app layer's answer, from
  spawn launches with no completion row) or the first live
  `registerChildOwnership` on the wire. A fresh `thread/start` keeps raw
  events and never arms it.
- `server_requests.go` — server-initiated request dispatch:
  approvals, MCP elicitation, structured user input, and dynamic tools.
- `turn_input.go` — outbound `turn/start` / `turn/steer` user-input
  payload shaping.
- `interactive_requests.go` — the codex half of interactive-request
  bookkeeping: what a released request means on this wire. The ledger itself
  (track / claim / cancel / drain, with the Bug B9 dedupe) is
  `provider.ApprovalRegistry`, shared with claude; what stays here is the
  JSON-RPC id encoding, the `turnTransition` error write that unblocks the
  server request, and the interrupt-vs-close drain distinction.
- `plan_buffer.go` — proposed-plan delta buffering and fallback
  completion content.
- `raw_tool_calls.go` — raw `rawResponseItem` function-call tracking,
  `write_stdin`/`wait_agent`/`spawn_agent` enrichment, and event-log
  redaction for stdin payloads.
- `collab_agents.go` — spawned child-thread parent mapping, agent path
  mapping, receiver metadata enrichment, wait-target preservation, and
  child metadata refresh. Also owns the two child-projection gates:
  `isChildSuppressedThreadNotification` (thread-WIDE notifications from a
  child that must never be projected onto the parent) and
  `isUnsafeChildProjectionEvent` (event kinds a child may never emit onto
  the parent thread). See "Child thread-wide suppression" below.
- `session_helpers.go` — thread start/resume params, sandbox policy,
  approval/user-input metadata builders, and route-field parsing.
- `session_probe.go` — `thread/read` liveness probe used on reopen to
  reconcile stale running rows. `session_probe_testhelpers.go` hosts
  the fake probe reply harness.
- `session_fork.go` — the `thread/fork` RPC wrapper (`Fork` / `ForkAt`);
  a `lastTurnId` cut (Codex >= 0.143) is the history truncation that
  works on every supported codex, and the one AO falls back to. It
  replaced the deprecated `thread/rollback` and is not upstream's only
  cut — see §"History truncation: three cuts, all turn-granular".
- `session_revert.go` — the `thread/revert` RPC wrapper (`Revert`), the
  in-place cut AO PREFERS: same thread id, same rollout lineage, no
  repoint. Two gates decide whether it is available, both read off the
  handshake and both fail closed — codex >= 0.148
  (`threadRevertMinimumCodexVersion`) and a thread whose
  `thread.historyMode` is `paginated`, which is upstream's own
  precondition. The same file owns the opt-in that satisfies it:
  `threadStartHistoryMode` puts `historyMode: "paginated"` on
  `thread/start` at the same 0.148 floor, because upstream's default is
  legacy and a client that says nothing gets a thread that can never be
  reverted. `isHistoryPaginationUnsupported` is the downgrade retry for a
  server that refuses paginated history, mirroring upstream's own client
  fallback. Threads created before the opt-in stay legacy for life —
  `thread/resume` has no history-mode field — and keep using the fork
  cut. Refusals raised BEFORE upstream mutates anything map to
  `ErrThreadRevertUnsupported` so the caller can fall back to a fork on
  the same connection; everything else is a hard failure, because
  `thread/revert` shuts the thread runtime down partway through and a
  fork built on a half-reverted thread would agree with neither.
  Mid-turn is refused here even though upstream ALLOWS it — upstream
  shuts the runtime down and drops the running turn on the floor. The
  file also owns `thread/reverted`, the echo that releases the RPC's
  bounded wait; an UNSOLICITED one is logged loudly and never acted on,
  because the notification carries a thread id and no boundary.
- `thread_queue.go` — the provider's own user-message queue
  (`thread/queue/*`, codex >= 0.148), which **AO reads and deletes but never
  adds to**. `QueueList` / `QueueDelete` / `PurgeQueue` exist so a conversation
  rollback can clear rows a FOREIGN producer (`codex queue --thread …`) left in
  codex's SQLite, the handshake-frozen `ThreadQueueNative` gate says whether
  those methods exist at all, and the single-flighted `thread/queue/changed`
  reconcile raises the foreign-submission notice. Ownership is INJECTED
  (`Config.OwnsQueuedClientID`), because only the app layer holds the store rows
  that could claim an id; nil means every submission is foreign. Two refusals
  live here rather than being smoothed over: `QueueList` returns
  `ErrThreadQueueListIncomplete` (page cap or repeated cursor) or
  `ErrThreadQueueListMalformed` (an element this build cannot read) with the
  prefix it did read, because a listing that looked complete would let a purge
  report success over rows it never saw. `PurgeQueue` returns a `QueuePurge`
  naming the submissions it deleted rather than a count, so a caller that aborts
  on a partial purge can put its own messages back. `add`, `start`, `update` and
  `reorder` are all deliberately absent. See §"The provider's queue".
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
  every failure path degrades to exactly that. The runtime-mode axes
  (`approvalPolicy` / `sandboxPolicy` / `approvalsReviewer`) are NOT pushed
  through it: every turn AO opens is its own `turn/start`, which carries them
  as overrides, so a second authority for the same axes would only add a way
  for the two to disagree.
- `account_usage.go` — `account/usage/read`: the wire shape
  (`AccountUsage`, every summary field a pointer because absence is not
  zero), `Session.ReadAccountUsage` for a live connection, and
  `AccountUsageFetcher` for the ephemeral-process fallback.
  `classifyAccountUsageError` is the split between "this account/binary
  has nothing to report" (`ErrAccountUsageUnavailable`) and a real
  failure. Caching lives in `internal/codexusage`.
  The same file carries the THREAD-scoped read (codex >= 0.148):
  `ThreadUsage` and `Session.ReadThreadUsage`, which
  sends `{threadId}` and returns the provider's own cumulative cost
  estimate for this thread. It is version-gated on
  `threadUsageMinimumCodexVersion` because params were `Option<()>`
  through 0.147 — a `{threadId}` request there is a hard `invalid_params`
  error, not a graceful degradation — and the gate reads the version off
  the handshake (`app_server_version.go`), never a second probe. A
  thread-scoped response's account summary is all-`None` and its daily
  buckets `null`, so it must never feed `internal/codexusage`. Absence
  (`threadUsage: null`, an auth refusal, credits with no USD) is
  `ErrThreadUsageUnavailable` — a state, and the caller keeps AO's
  rate-table price; an echoed thread id that does not match is a wire
  fault, not an absence. The app-side policy (when it runs, where it
  persists, why it is not a ledger row) lives in
  `internal/codexthread`.
- `app_server_version.go` — the connected app-server's own build, parsed
  from `InitializeResponse.userAgent` (`codex_cli_rs/<semver> (...)`) and
  stored on the Session at handshake time. `appServerAtLeast` is what a
  per-method version floor asks. Empty or unparseable is treated as too
  old by every gate: a method floor must fail closed. This is strictly
  better than the boot-time `codex --version` probe for the purpose — it
  describes the process on the other end of THIS pipe.
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
  cumulative `thread/tokenUsage/updated` totals of the ROOT thread only
  (Codex has no per-turn usage signal and no USD cost on the wire). A
  spawned CHILD's tokenUsage never reaches it — it is intercepted and
  re-scoped as subagent progress, see "Child thread-wide suppression"
  below. Tracks
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
- `session_review.go` — `review/start` (`StartReview`,
  `StartReviewForTurn`) and
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
- `session_review_projection.go` — inline review projection. Codex exposes
  an outer review id on review items and the RPC response, plus a private
  execution id on `turn/started`. The outer id owns the visible turn. The
  private id stays in `activeTurnID` because it is the only id
  `turn/interrupt` accepts. Review activity is scoped under one
  `codex_review` launch and the formatted answer becomes one sourced
  `command_result` after the launch.
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

## Session lock order

`Session` carries six mutexes. Take them in this order or not at all:

```
controlMu  ->  mu  ->  childLifecycleMu  ->  eventMu
```

The approvals registry's lock (`approvals`, a shared
`provider.ApprovalRegistry`) and `collabAsyncMu` are LEAVES: no other
`Session` lock may be acquired while either is held. `ApprovalRegistry.Drain`
returns the released requests instead of resolving them, which is what lets
`drainPendingApprovals` write and emit with that lock already gone, and
`startCollabAsync` only hands work to a goroutine.

Two edges are exercised, both so a child's lifecycle emission cannot be
overtaken while the external callback stays off the lifecycle lock. `Close`
takes `childLifecycleMu` under `mu` to drop `childLifecycleRevision`.
`observeAndEmitChildLifecycle` / `emitRecoveredChildStatus` reserve `eventMu`
under `childLifecycleMu`, then release `childLifecycleMu` before delivering.

- **`mu` is never held across an emit.** Every emitter builds its events under
  `mu`, releases it, then calls `emitEvent` / `emitEvents`
  (`thread_settings.go` is the canonical shape).
- **`eventMu` is the only lock that may be held across `s.onEvent`.**
  `emitEventLocked` is the sole caller of the callback and runs under
  `eventMu`, which gives the callback its serialized-delivery contract without
  pinning session state while the app layer runs. A callback re-entering the
  `Session` deadlocks only if a future emitter holds `mu` across an emit.
- **Atomics exist to stay out of this order.** `codexThreadID`,
  `appServerVersion`, `threadHistoryMode`, `pendingRevert`, `revertEpoch`,
  `threadQueueNative`, `closing` and `nextID` are atomic precisely so
  read-loop paths already holding `mu` can consult them without a second lock.

`Close` zeroes each sub-struct WHOLE rather than field by field, so a field
added later cannot be forgotten, and a group is assigned whole under `mu`,
which is why the two fields guarded by another lock stay at the top level.
`TestCloseReleasesSessionScopedState` fills every such field reflectively and
lists what `Close` deliberately leaves.

## Version gates

Every gate reads the connected app-server's own build, parsed from
`InitializeResponse.userAgent` at handshake (`app_server_version.go`), never a
second `codex --version` probe: it describes the process on the other end of
THIS pipe. Empty or unparseable is too old to every gate, failing closed.

| Constant | Floor | Gates |
|---|---|---|
| `provider.minimumCodexCLIVersion` | 0.143.0 | the whole provider. A method at or below it needs no runtime probe. |
| `threadRevertMinimumCodexVersion` | 0.148.0 | `thread/revert` (`session_revert.go`) |
| `threadStartHistoryModeMinimumCodexVersion` | = the revert floor | asking for `historyMode: "paginated"` on `thread/start` |
| `threadQueueMinimumCodexVersion` | 0.148.0 | `thread/queue/list` / `delete`, frozen once into `ThreadQueueNative` |
| `threadUsageMinimumCodexVersion` | 0.148.0 | `account/usage/read {threadId}`. Below it the params were `Option<()>`, so the field is a hard `invalid_params` rather than a degradation. |
| `approvalPolicyForCodexVersion` | 0.149.0 | remaps `untrusted` to `on-request` (`options.go`) |

## Rules that are not negotiable here

- **Wire-typed signals, never heuristics.** Background execution, collab
  ownership and delivery boundaries are authorized by typed wire fields only.
  An event-ordering classifier is forbidden as the authorization even where it
  would be right, per
  [invariant 25](../../../docs/architecture/invariants.md#25-codex-backgrounding-uses-wire-typed-signals-never-heuristics).
- **A mid-turn user message goes to `turn/steer`.** Never `thread/queue/add`.
  A standing product decision, not a version workaround: it holds on every
  supported codex, and the queue methods AO does call are recovery only.
  `TestQueueStartIsNeverCalled` fails the build on a `thread/queue/start`
  caller.
- **The root Codex thread id is not a field.** It is
  `codexThreadID atomic.Pointer[string]`. Read `rootThreadID()`, write
  `setRootThreadID()`. `NewSession` starts `readLoop` before it can receive the
  `thread/start` response carrying the id, so the constructor's write races
  every read-loop path that consults it, and it cannot move under `mu` because
  two of those readers already hold `mu`.
- **No generic "unknown notification" path.** A method is either dispatched or
  explicitly opted out. One that no classifier claims reaches
  `warnUnclaimedNotification`, which logs it once per method per session, and
  silence there once let seven upstream notifications arrive unnoticed between
  the 2026-06 and 2026-07 surveys. It stays log-and-continue by design, since
  a Codex release adding a notification must not break a live session.

## Notifications

`initialize` carries `capabilities.optOutNotificationMethods`, computed as the
COMPLEMENT of what this package consumes (`notification_catalog.go`), so
adding a classifier case or a side-channel handler removes a method from the
opt-out automatically. Short-lived clients (login, probes, model list, MCP
status) opt out of everything except what they name. A method the catalogue
does not know is never opted out: known-and-ignored is silent because it never
arrives, and unknown is loud in the drift log.

`skills/changed` is a side channel rather than transcript content, claimed in
`sessionSideChannelNotifications`, which is what keeps it out of the opt-out
list. Upstream types it as an EMPTY struct, so the App layer drops the whole
`internal/codexskills` cache rather than pretending to narrow the scope.

## Turns AO did not start

A turn can start on a thread AO owns without AO sending `turn/start`, because
any `codex queue --thread` writer can inject one. Treat it as normal, per
[`codex-wire.md` §Externally queued turns](../../../docs/references/codex-wire.md#externally-queued-turns).
AO's rules live in `external_turns.go`, wired in `session_notifications.go`.

- **Adopt the `turn/started` as `activeTurnID` regardless of origin.** An
  injected turn AO cannot Interrupt or Steer is worse than one it cannot
  attribute.
- **A local claim is a COUNTER**, not a flag: `beginLocalTurnStart` before the
  RPC, `bindLocalTurnStart` on the response, because `turn/started` can beat
  the response onto the read loop. A request TIMEOUT deliberately does not
  release the claim, since the turn may exist.
- **The verdict is FINAL at `turn/started`** and has two answers, never three.
  AO starts every turn it owns with a `turn/start`, so an unclaimed one is
  somebody else's, and the `userMessage` echo only READS the answer.
- **An adopted external turn stamps `Meta.origin = "external-queue"`** on
  `EventTurnStart` and the `EventUserText` echo, so an injected prompt is
  never rendered as something the user typed. A value, not a boolean: a second
  producer must stay distinguishable. Classification otherwise fails toward
  "local", because mislabelling an AO turn as injected corrupts the user's own
  transcript.

## The provider-owned queue

**AO reads and deletes; it never adds.** `add`, `start`, `update` and
`reorder` have no wrapper and no caller. `thread_queue.go` keeps only what a
client needs for a queue it does not participate in: `QueueList`,
`QueueDelete`, `PurgeQueue`, the handshake-frozen `ThreadQueueNative`
capability, and the single-flighted `thread/queue/changed` reconcile.

- **Ownership is injected, never derived from the id.**
  `Config.OwnsQueuedClientID` is the app layer's answer, and nil means every
  submission is foreign. AO's row ids are deterministic, so the grammar is not
  a credential: another AO profile on the same Codex home mints the same ones.
- **A partial listing is an error, not a short queue.** `QueueList` returns
  `ErrThreadQueueListIncomplete` (page cap or repeated cursor) or
  `ErrThreadQueueListMalformed` (an unreadable element) alongside the prefix
  it did read, because a listing that looked complete would let a purge report
  success over rows it never saw.
- **Rollback purges the queue, and a purge that cannot complete ABORTS the
  rollback.** A queued row survives `stopSession`, and the idle hook dispatches
  it on the next resume, replaying a message onto history the user just
  truncated. `PurgeQueue` runs over whichever connection the rollback has
  (`app_conversation_rollback.go`, or `purgeCodexQueueBeforeCut`) and goes
  FIRST on it, because a loaded thread's idle hook dispatches the queue. That
  is what `Config.BeforeResume` is for.
- **The abort stays retryable because `QueuePurge` names the submissions it
  removed** in order rather than counting them, so a caller can put back
  whichever its own store rows account for. A FOREIGN one cannot be put back
  and is counted in `QueuePurge.Foreign`, named in the log and in the refusal.

## History truncation

Three turn-granular cuts exist upstream and AO uses two. Shapes, floors and
the paginated consequences:
[`codex-wire.md` §History truncation](../../../docs/references/codex-wire.md#history-truncation-threadrevert-and-historymode).

- `thread/revert` (`session_revert.go`) is PREFERRED: same thread id, same
  rollout lineage, no repoint, so provider-side cost estimates and an external
  `codex resume` survive an edit-and-resend. Two gates, both off the handshake
  and both failing closed: codex >= 0.148, and `historyMode: paginated`.
- `thread/fork { lastTurnId }` (`session_fork.go`) is the FALLBACK and the only
  cut every supported codex answers. It names the same boundary as revert from
  the opposite side (last KEPT turn versus first DROPPED one), which is why the
  app layer resolves the two anchors separately.
- `threadStartHistoryMode` asks for `paginated` at birth, because upstream's
  default is legacy and `thread/resume` has no history-mode field, so a thread
  that starts legacy can never be reverted. `isHistoryPaginationUnsupported`
  is the one-shot downgrade retry.
- **History-preserving refusals** map to `ErrThreadRevertUnsupported` or
  `ErrThreadRevertAnchorUnresolvable`, so the caller can fall back to a fork.
  The history-mode gate runs before shutdown. Anchor resolution runs after
  shutdown, but the handler reloads the unchanged thread before answering.
  Anything else is a hard failure because it can land between shutdown,
  pointer replacement, and reload. Mid-turn revert is the Esc un-send
  primitive: upstream owns active-turn shutdown,
  persistence, the cut and runtime reload on the existing connection.
  `controlMu` serializes it against root `turn/interrupt`; `revertEpoch` drops
  a stale root or child interrupt that waited across the cut.
- `thread/reverted` releases the RPC's bounded wait. An UNSOLICITED one is
  logged loudly and never acted on: it carries a thread id and no boundary.

## Child threads

A Codex spawn creates a CHILD THREAD rather than a backgrounded tool. Wire
sequence, V1 and V2 shapes, quarantine contract and delivery envelope:
[`codex-wire.md` §Collab agent lifecycle](../../../docs/references/codex-wire.md#collab-agent-lifecycle-multiagentv1-and-multiagentv2).

- **Every unmapped non-root thread is quarantined fail-closed**, recursively,
  including grandchildren (`child_routing.go`). Child lifecycle never becomes
  root lifecycle, and a parent's `turn/completed` does not wait for children.
- **Thread-WIDE child notifications are suppressed**
  (`isChildSuppressedThreadNotification`, with `isUnsafeChildProjectionEvent`
  as the second gate on the event side), so a subagent cannot overwrite the
  parent's context meter, title or compact state (ADR-002).
- **`thread/tokenUsage/updated` is the one carve-out.** It is intercepted
  before the child-state branch and re-scoped into an `EventSubagentProgress`
  on the spawn's launch (`childProgressEvent`), never reaching
  `usageAcct.observe` and never emitted as `EventTokenUsage`. The figure is
  the child's cumulative spend from `childAgentTokenSpend`, not
  `total.totalTokens`, which re-counts the cached prompt every round.
- **MultiAgentV2 spawn activity does not carry the child profile.** Raw
  `spawn_agent` model and effort fields are requests, not the effective result
  after Codex applies role and default configuration. Resolve the profile from
  the child's metadata-only `thread/resume {excludeTurns:true}` response and
  leave the badge blank until that response arrives. Never inherit the parent
  profile or copy the raw request into persisted launch metadata.
- **Transcript presentation waits for delivery into parent model context**,
  not for child completion, which changes only live launch state. Resumed
  sessions cannot opt into raw events, so `session_rollout_notifications.go`
  tails the active rollout from EOF for the delivery record. That tail is
  ARMED: only `Config.ResumeHasUnresolvedSubagents` or the first live
  `registerChildOwnership` starts it. Root resume itself rehydrates children
  from AO's own compact spawn metadata, never from provider history.
- **Client stop uses child `turn/interrupt`, not `close_agent`.** Codex 0.150.1
  exposes no client `close_agent` or `interrupt_agent` RPC. Resolve the launch
  item through `childParentByThread`, send `turn/interrupt` with the owned child
  thread and active child turn, and use an empty turn id only for the upstream
  startup-interrupt case. Scope approval drains to that provider thread so a
  child stop cannot dismiss root or sibling prompts. App-server shutdown ends
  live child work but does not erase the resumable child thread identity.

## Background terminals

Codex has no `run_in_background` flag, but `exec_command` can yield back to
the model while its PTY keeps running.

- Typed `item/started` and `item/completed` commandExecution events are the UI
  history source. Raw `exec_command` output is model-facing text that may
  enrich live process metadata but must not create, delay or reorder chat
  history. It produces the internal `EventCodexExecResult` only, and
  `TerminalInteractionNotification` feeds the waited and interacted markers.
- Per-row stop is wired end to end:
  `thread/backgroundTerminals/terminate {threadId, processId}` wrapped in
  `session_background.go`, bound as `App.TerminateCodexBackgroundTerminal`,
  joining on the `process_id` that `enrichItemMeta` stamps on the item.
  Stopping a row is a user action on an already-authorized row and is never
  itself a source of `is_background` authorization.
- All three background-terminal RPCs need `experimentalApi` and sit above the
  0.143 floor, so there is no runtime capability probe. `terminated: false`
  means "matched nothing", not an error. Wire detail:
  [`codex.md` §Background terminals](../../../docs/references/codex.md#background-terminals).

## Methods we call

Params and response shapes are in `codex-wire.md`. These are the AO-side rules.

| Method | AO's rule |
|---|---|
| `thread/start`, `thread/resume` | `buildThreadParams` always names `approvalsReviewer`, and `verifyApprovalsReviewerEcho` reads it back before the session is handed out. Resume always sends `excludeTurns: true`, because AO's SQLite projection is the transcript authority. |
| mid-life reconcile resumes | `session_probe.go`, `collab_profiles.go`, and `collab_rehydrate.go` send NO overrides. Codex ignores overrides when resuming a LOADED thread, and a divergent one arms its shutdown-and-cold-resume branch. A child resume response is also the authority for that child's effective model and effort. |
| `thread/fork` | Names none of the config axes, safe only because nothing executes until a `turn/start` re-asserts every axis. Do not special-case the reviewer without doing the same for the sandbox. |
| `turn/start` | Carries the per-turn config overrides, which is how a live model, effort, fast-mode or runtime-mode change lands with no restart. `SendOptions.OutputSchema` rides it and is never sticky. |
| `turn/steer` | Takes no config fields. Refuse with `ErrNoActiveTurn`, without writing, when there is no tracked `activeTurnID`. |
| `thread/settings/update` | Strictly ADDITIVE over the `turn/start` overrides (model, effort, `serviceTier`), so every failure path degrades to the previous behavior. The runtime-mode axes never route through it, and it is called only while the thread is idle. |
| `account/usage/read {threadId}` | The provider's own cumulative estimate for ONE thread, sent once per settled top-level turn and never on a timer. It must never feed `internal/codexusage`, which is account-scoped. |
| `skills/list` | Needs every cwd absolute and the list non-empty: upstream resolves a relative path against the ANSWERING process's cwd, so a live session and a throwaway fetcher would disagree. |
| `review/start` | The returned `reviewThreadId` is the routing authority. For inline delivery, review items carry the response turn id while the private `turn/started` id is the interrupt authority. Do not merge those roles. `ReviewDeliveryDetached` exists and must stay unused. |
| sandbox and approval family | `file/write`, `command/execute` and siblings arrive as server REQUESTS that must be answered. MCP elicitation maps to an approval, as `Kind: "mcp-elicitation"`. `item/tool/requestUserInput` does NOT: it is a structured user-input flow answered through `RespondToUserInput`, and must carry at least one question before emitting. |

## Anti-patterns

- Do NOT leak JSON-RPC frames or Codex-native types out of this package.
  Other callers see normalized `provider.Event` values only.
- Do NOT invent `ThreadItem.status` values. Respect upstream's v2 enum. A new
  status means updating the classifier and the reconciler together.
- Do NOT pre-nest Codex `config` override keys. They stay DOTTED and flat,
  because codex expands them into TOML itself (`config/src/overrides.rs`).
- Do NOT treat an unknown disabled-tool id as fatal. The toggle list is
  settings data that outlives any one AO version, so an unknown id is skipped
  with a log line, and `TestDisabledToolTogglesMatchTheFrontendMirror` catches
  a rename that leaves the frontend mirror behind.
- Do NOT add a third fast-mode anchor without confirming it in codex-source.
  `codexFastModeTier` matches upstream's own two (`id == "priority"`,
  `name == "fast"`), and `serviceTiers` is the model's whole tier menu, so a
  `flex` entry means the model declares a tier, not that it is fast-capable.

## References

- Codex source, `/home/rmurphy/repos/codex`, wins over anything here, but the
  checkout can lag the installed CLI. CodexMonitor
  (https://github.com/Dimillian/CodexMonitor) is a feature-complete client.
- [`docs/references/codex.md`](../../../docs/references/codex.md) for reading
  those sources, [`spike-policy.md`](../../../docs/references/spike-policy.md)
  for when both are silent.
