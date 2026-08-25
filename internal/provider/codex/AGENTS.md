# internal/provider/codex/

Wraps `codex app-server`. One process per active thread, JSON-RPC 2.0
over stdio.

## Layout

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
  resumed app-server sessions do not expose as raw events.
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
  `app_codex_thread_cost.go`.
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

`Session` carries five mutexes. **Take them in this order or not at all:**

```
mu  →  childLifecycleMu  →  eventMu
```

The approvals registry's own lock (`approvals`, a shared
`provider.ApprovalRegistry`) and `collabAsyncMu` are **leaves**: no other
`Session` lock may be acquired while either is held. `ApprovalRegistry.Drain`
RETURNS the released requests instead of resolving them, which is what lets
`drainPendingApprovals` write and emit with that lock already gone, and
`startCollabAsync` only hands work to a goroutine, which enters the order on
its own stack.

Two edges are actually exercised today, both for the same reason — a child's
lifecycle emission must not be overtaken, and the external callback must not
run under the lifecycle lock:

| Site | Sequence |
|---|---|
| `Close` (`session.go`) | takes `childLifecycleMu` under `mu` to drop `childLifecycleRevision` |
| `observeAndEmitChildLifecycle`, `emitRecoveredChildStatus` (`collab_agents.go`) | reserve `eventMu` under `childLifecycleMu`, then release `childLifecycleMu` before delivering |

Three rules follow, and they are what keep the order shallow:

- **`mu` is never held across an emit.** Every emitter builds its events
  under `mu`, releases it, and only then calls `emitEvent` / `emitEvents`
  (`thread_settings.go` is the canonical shape — it says so in a comment).
  So the `mu → eventMu` edge exists in the order but is not taken anywhere:
  the only path from `mu` toward emission is the `Close` edge above.
- **`eventMu` is the only lock that may be held across `s.onEvent`.**
  `emitEventLocked` (`session.go`) is the sole caller of the callback, and
  it runs under `eventMu`. That is what gives the provider callback its
  serialized-delivery contract without pinning session state while the app
  layer runs — an app-layer callback re-entering the `Session` deadlocks
  only if a future emitter starts holding `mu` across an emit.
- **Atomics exist to stay out of this order.** `codexThreadID`,
  `appServerVersion`, `threadHistoryMode`, `pendingRevert`,
  `threadQueueNative`, `closing` and `nextID` are atomic precisely so
  read-loop paths that already hold `mu` can consult them without a second
  lock — see the anti-pattern on the root Codex thread id below.

## Methods we call

- `thread/start`, `thread/resume`, `thread/fork` (optionally cut at a
  `lastTurnId` anchor — the history cut every supported codex answers,
  and AO's fallback since it started preferring `thread/revert`;
  upstream deprecated `thread/rollback` and AO no longer calls it — see
  §"History truncation: three cuts, all turn-granular").
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
- `thread/revert` — `#[experimental("thread/revert")]` upstream (0.148),
  so it rides the `capabilities.experimentalApi` every AO handshake
  already sets. Truncates ONE thread's durable history in place at an
  EXCLUSIVE `beforeTurnId` (the first dropped turn) and keeps the thread
  id, its runtime and this connection's subscriptions — upstream
  explicitly reloads rather than tearing down so clients need no
  follow-up `thread/resume`. AO calls it for edit-and-resend
  (`app_codex_revert.go`) wherever the thread supports it, and falls
  back to `thread/fork` where it does not. Three things the wrapper in
  `session_revert.go` exists to enforce:
  - **Paginated threads only.** Upstream refuses a legacy-history thread
    outright, first thing, before touching anything. That refusal is the
    reason the fork fallback is safe rather than a guess.
  - **Never mid-turn.** Unlike every other guard in this package this
    one INVERTS upstream: `thread_revert_response` submits a shutdown
    and waits up to 10s, so a mid-turn revert silently destroys the
    running turn. AO refuses instead.
  - **The response's `turns` is always empty** (upstream points clients
    at `thread/turns/list`), so the only validation available is the
    thread-identity echo — and it is the load-bearing one, since the
    caller keeps its `SessionRef` pointed at that thread.
  - **An anchor upstream cannot resolve is a state, not a failure.**
    `ErrThreadRevertAnchorUnresolvable` covers the pre-mutation refusals
    `history_base_at_boundary` raises ("turn not found", no persisted
    rollout position, boundary outside the inherited history). The
    dominant cause is a cut that already landed on the provider side while
    its local half failed, so a hard error there would wedge that thread's
    edit-and-resend permanently; the caller falls back to the fork cut,
    whose anchor is the last KEPT turn and therefore survives either way.
- `thread/queue/list` / `delete` — the READ and DELETE half of the provider's
  own user-message queue, both `#[experimental]` (0.148) and so riding the
  `capabilities.experimentalApi` the handshake already sets. AO calls these to
  clear rows another producer left behind, never to put a message of its own
  anywhere: `thread/queue/add`, `start`, `update` and `reorder` are all
  uncalled, and a mid-turn user message goes to `turn/steer` on every
  supported codex. See §"The provider's queue".
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
  - **The composer-change caller is never mid-turn.** The app layer gates
    that call on the thread being idle
    (`app_session_config.go#threadTurnInFlight`). Skipping the push while busy
    loses nothing: the same values ride the next `turn/start`, and the RPC's
    value is the echo, not mutating a running turn.
  - **The runtime-mode axes are never routed through it.** `approvalPolicy`,
    `sandboxPolicy` and `approvalsReviewer` stay on `turn/start` (see
    RuntimeMode in the parent guide), which re-asserts all three every turn —
    and AO starts every turn on the thread, so there is no dispatch path that
    could run one without them. A second writer would only add a way for the
    two to disagree.
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
  Since 0.148 the params carry an optional `threadId`, which switches the
  response to `threadUsage` — Codex's own cumulative cost estimate for
  one thread. AO sends it once per settled top-level turn on the live
  session (`Session.ReadThreadUsage`), never on a timer and never while
  idle. Older builds typed the params as `Option<()>`, so the field is a
  hard error there rather than an ignored key: the call is gated on the
  handshake-reported version and is simply not made below 0.148.
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
  response's `reviewThreadId` is the routing authority for the thread. For
  inline delivery, review items carry the response turn id while the private
  `turn/started` id is the interrupt authority. Do not merge those roles.
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

### Child thread-wide suppression, and its one carve-out

A child thread's THREAD-WIDE notifications describe the child, not the
parent, so projecting them would let a subagent overwrite the parent's
context meter, title, or compact state (ADR-002).
`isChildSuppressedThreadNotification` drops them, and
`isUnsafeChildProjectionEvent` is the second gate on the event side.

`thread/tokenUsage/updated` is the one method deliberately NOT in the
suppression set. It is intercepted in `interceptChildNotification`
(`session_notifications.go`) BEFORE the child-state branch and converted, by
`emitChildTokenUsageProgress`, into an `EventSubagentProgress` scoped to
the spawn card:

- `ItemID` is the spawn's `parentToolUseID` — the launch row the tick
  renders on;
- `ParentToolUseID` is that spawn's OWN parent, resolved by trimming the
  child's canonical agent path (`/root/reviewer/deep` → `/root/reviewer`)
  and mapping it back to a launch. Depth-1 children and path-less builds
  resolve to `""`, which is correct: their spawn is top-level;
- `Meta.TotalTokens` is the child's TRUE CUMULATIVE spend, every token
  counted once, assembled by `childAgentTokenSpend` as
  `total.inputTokens - total.cachedInputTokens +
  total.cacheWriteInputTokens + total.outputTokens`. Every term is a
  provider cumulative that only grows, so the figure is MONOTONIC through
  a child's own compaction. It is deliberately neither of the two figures
  already on the wire: `total.totalTokens` re-counts the cached prompt
  every round (a real 42-minute child reported 4,570,684 there against
  209,724 of actual spend), and `normalizeThreadTokenUsage`'s
  `last.totalTokens` is context occupancy, which drops every earlier
  round's output.

  One card component renders both providers, and Claude cannot be given
  the same treatment: its `task_progress` envelope is
  `{total_tokens, tool_uses, duration_ms}` with no breakdown, and 2.1.237
  builds that total as LATEST input plus all output. The two quantities
  agree until a compaction — summing each round's FRESH input is how the
  current context got its size — and diverge by 1.2% on the 42-minute
  child above. Claude's is the one that can dip, which is why
  `triage.mergeSubagentProgress` takes the newest value for this field
  rather than the max.

It never reaches `usageAcct.observe` and is never emitted as
`EventTokenUsage`, so the parent's meter and per-turn accounting are
untouched. This is the only channel through which a child's usage
reaches the parent thread; anything else remains suppressed.

## Externally queued turns

A turn can start on a thread AO owns without AO ever sending
`turn/start`. Treat that as a normal condition, not a protocol violation.

**How it happens** (traced in codex-source at rust-v0.149.0 — the
mechanism has not been observed against a live binary here, since the
installed CLI is 0.147.0):

- Every app-server backed by a LOCAL thread store installs the queued-item
  extension unconditionally (`codex-rs/app-server/src/extensions.rs`).
  There is no initialize capability that opts out of it, so AO's
  app-server runs it too.
- `QueuedItemService::watch_external_messages`
  (`codex-rs/ext/queue/src/service.rs`) polls SQLite's cheap `data_version`
  on `state_5.sqlite` every **10 seconds**, asks the durable revision index
  which LOADED threads changed, emits `thread/queue/changed` for each, and
  spawns a dispatch task that calls `start_turn_if_idle` — retrying every
  10s while the thread stays busy.
- The producer is `codex queue --thread <uuid> --message <text>`
  (`codex-rs/cli/src/queue_cmd.rs`). It writes one SQLite row and exits: no
  running app-server needed, and it never takes the thread writer lock, so
  it works **while AO holds the thread**.

**What AO sees**, in order: `thread/queue/changed`, then — up to ~10s
later — a `turn/started` with no `turn/start` of ours, followed by a full
`item/*` stream including an `item/completed` `userMessage` AO never sent.

**What AO does** (`external_turns.go`, wired in
`session_notifications.go`):

- The `turn/started` is **adopted** as `activeTurnID` regardless of origin.
  An injected turn AO cannot Interrupt or Steer is worse than one it cannot
  attribute.
- Origin is tracked per turn id. A local claim is a COUNTER
  (`beginLocalTurnStart` before the RPC, `bindLocalTurnStart` on the
  response) because `turn/started` can beat the `turn/start` response onto
  the read loop; without the counter that race would classify AO's own turn
  as external. A request TIMEOUT deliberately does not release the claim —
  the turn may exist.
- The verdict is FINAL at `turn/started`. `adoptTurnStart` has two answers and
  no third: AO starts every turn it owns with a `turn/start` of its own, so an
  unclaimed one is somebody else's. (It could once DEFER, back when AO put
  messages in the provider's queue and the app-server dispatched them —
  see §"The provider's queue".) The `userMessage` echo only READS the recorded
  answer, which is what keeps the adoption log one line per turn and the
  turn-start marker from disagreeing with the rows underneath it.
- An adopted external turn stamps `Meta.origin = "external-queue"`
  (`ExternalTurnOriginQueue`) on the `EventTurnStart` and on the
  `EventUserText` echo, so the injected prompt is not persisted or rendered
  as something the person in front of Agent Overflow typed. It is a value,
  not a boolean: a second external producer must be distinguishable, not
  folded into "not ours".
- `thread/queue/changed` is `{threadId}` and nothing else at rust-v0.149.0 —
  no count, no item id, no text — so what it raises depends on whether the
  queue can be READ. On a pre-0.148 app-server it raises one
  `EventNotification` (`Meta.kind = "external_queue"`) that reports no depth,
  because there is no `thread/queue/list` to ask. On 0.148+ the notice is
  evidence-driven instead: see below.
- Classification fails toward "local": an unknown turn id reads as local,
  and at the origin-map cap new turns read as local. Mislabelling an AO
  turn as injected corrupts the user's own transcript; the reverse costs
  only the marker.

## Turn identity and the steer contract

Every outbound `turn/start` and `turn/steer` carries
`clientUserMessageId` (`SendOptions.ClientUserMessageID`), and the
`userMessage` ThreadItem it produces echoes it back as `clientId`
(`ThreadItem::UserMessage`, rust-v0.149.0
codex-rs/app-server-protocol/src/protocol/v2/item.rs:236), which
`protocol_item.go` puts on the event as `client_id` meta. That pairing is how
a caller matches an echo to the row that produced it without relying on
ordering. Upstream types the field `Option<String>` on both params structs and
has since 0.136 — below AO's 0.143 provider floor — so it is sent
unconditionally and there is no version gate. An EMPTY value is omitted rather
than sent: upstream mints its own uuid for a producer that supplies none, so an
explicit empty string would be a value no echo could ever match.

**A mid-turn message goes to `turn/steer`, on every supported codex.** The
provider's own queue is not a destination AO writes to — see below.

`turn/steer` takes no config fields, so an in-flight turn can never be
reconfigured, and it takes `expectedTurnId`, which upstream REQUIRES to be
non-empty (`turn_processor.rs` refuses an empty one before the request reaches
the session). AO fills it from the session's tracked `activeTurnID` and refuses
with `ErrNoActiveTurn` — without writing — when there is none.

**Three refusals, one JSON-RPC code.** All of them come back as -32600
`invalid_request`, so the code discriminates nothing and
`classifySteerRejection` (session_turn.go) reads the payload:

| upstream `SteerInputError` | how it is recognised | AO's answer |
|---|---|---|
| `NoActiveTurn` | message `no active turn to steer` | `ErrNoActiveTurn` |
| `ExpectedTurnMismatch` | message ``expected active turn id `X` but found `Y` `` | `ErrNoActiveTurn` |
| `ActiveTurnNotSteerable` | `error.data`'s `codexErrorInfo` is `{"activeTurnNotSteerable":{turnKind}}` | `ErrTurnNotSteerable` |

The first two are the same RACE — the turn ended, or a new one started,
between the frontend reading the active-turn registry and the steer arriving —
and the recovery is to open a turn of its own (`IsNoActiveTurnRace`, which the
app layer already falls back on). The mismatch message NAMES the turn id
upstream found, and AO deliberately does not parse it out for a retry: by the
time the answer is read that id can have rolled again, and a steer aimed at a
turn the message was not written for is worse than a fresh turn. A mismatch
just means requeue.

The third is a different STATE and must never be folded into the race: a turn
IS running — a review (`review/start`) or a compaction
(`thread/compact/start`) — and it simply cannot take input. Starting a second
turn there would interleave the user's message with the review, so it gets its
own sentinel and the app layer holds the message for the next turn boundary.
It is also the only one of the three upstream attaches structured data to,
which is why `RPCError` carries `Data` verbatim: without it, "not steerable"
is separable from the two races only by its English sentence.

## The provider's queue (codex >= 0.148)

**AO reads and deletes; it never adds.** `thread/queue/add` has no wrapper and
no caller, and neither do `start`, `update` or `reorder`. What remains in
`thread_queue.go` is the surface a client needs for a queue it does not
participate in: `QueueList`, `QueueDelete`, `PurgeQueue` over them, the
handshake-frozen `ThreadQueueNative` gate, and the `thread/queue/changed`
reconcile.

`start` is the one that would be actively dangerous: `QueuedItemService` is a
`ThreadLifecycleContributor` whose `on_thread_idle` → `dispatch_if_idle` →
`start_turn_if_idle` → `delete_locked` drains the head by itself, and `enqueue`
calls `wake_if_loaded`, so a client `start` races that drain and can run one
submission twice. `TestQueueStartIsNeverCalled` fails the build if a caller
appears. `update` and `reorder` are declined for a duller reason — nothing here
edits or re-orders a message another producer owns — and the mock refuses both
with `-32601` so a harness run that grows a caller fails loudly
(`cmd/ao-mockprovider/codex_queue.go`).

**`ThreadQueueNative` is a capability, not a dispatch decision.** It is frozen
once, at handshake time (`recordThreadQueueSupport`, straight after
`recordAppServerVersion`), and says only whether `list` / `delete` exist on
this app-server. What reads it is RECOVERY: a rollback has to purge rows a
foreign producer left in codex's SQLite, and where the family is missing there
is nothing to attempt — a state the app layer must be able to see rather than a
failure to swallow. Empty or unparseable `userAgent` fails closed.

**Ownership is injected, never derived from the id.** `Config.OwnsQueuedClientID`
is a `func(clientUserMessageID string) bool` the app layer supplies; nil means
every listed submission is foreign, which is the honest default for a package
that writes no rows. It cannot be answered here: AO's own row ids are
deterministic, so the grammar is not a credential — a second Agent Overflow
profile against the same Codex home mints the same ones, and anything speaking
`thread/queue/add` may simply supply one. Only the app layer holds the store
rows that could account for an id.

**The `thread/queue/changed` notice is evidence-driven where it can be.**
Where there is no `list` the classifier's own notice stands as written — it
reports that something was queued, with no depth and no authorship, because
nothing can be asked. Where the family exists the immediate event is dropped
and a bounded list decides, so a notice names a submission that was actually
there. The walk is SINGLE-FLIGHTED with a dirty re-run (`startQueueReconcile`):
every mutation and every automatic dispatch raises a change, so an N-message
queue produces ~2N notifications and only the state at the end of the burst is
worth reporting. Its context comes from the session's lifetime, so a teardown
mid-walk cancels it, and `reportedForeignSubmissions` makes the notice once per
submission id.

**A `thread/queue/list` can report a PREFIX.** Pagination stops on a repeated
cursor or at the page cap, and both used to return the rows so far with a nil
error — indistinguishable from a short queue. It returns
`ErrThreadQueueListIncomplete` alongside the prefix now, and the purge treats
that as a failure, because a listing that looked complete would let it report
success over rows it never saw.

**An element it cannot READ is the same failure.** Upstream's `QueuedSubmission`
is `{id, input, client_user_message_id}`, all three required and non-`Option`
(rust-v0.149.0 codex-rs/app-server-protocol/src/protocol/v2/thread.rs:869), so
an element that will not decode is a wire fault, not a short row. Returning an
EMPTY submission for it — which is what a swallowed `json.Unmarshal` did —
makes it indistinguishable from an ABSENT one, and absence is what the purge
acts on: it would skip the empty id and let the rollback truncate over a
submission still armed to run. `parseQueuedSubmission` reports instead,
`QueueList` stops the walk and returns the readable prefix with
`ErrThreadQueueListMalformed`, and an empty server-assigned `id` counts as
malformed because it is the only handle a delete takes. The one caller that
does NOT treat it as fatal is the foreign-submission notice walk, which only
ever adds notices for rows it can see.

**`thread/queue/delete`'s `deleted` is required.** `ThreadQueueDeleteResponse`
types it as a bare `bool` with no serde default, so upstream's own deserializer
refuses a body without the key. Decoding it into a Go `bool` turned any drift —
a rename, a nested envelope, an explicit `null` — into a benign-looking
`false`, which the purge reads as "already dispatched": the row counts as
accounted for and the rollback truncates history over a submission that may
still be armed. It decodes as a `*bool` and a missing or null one is an error.
A genuine `false` is a STATE ("matched nothing"), exactly like
`TerminateBackgroundTerminal`'s `terminated: false`.

**Rollback purges it, and a purge that cannot complete ABORTS the rollback.**
A row in codex's SQLite survives `stopSession`, and `on_thread_idle` dispatches
it on the next resume — re-running a message onto a thread the user just
truncated. `PurgeQueue` deletes every row over whichever connection the
rollback has: the LIVE session before the stop
(`app_conversation_rollback.go`), or, for a thread that had none, the throwaway
resume the history cut opens anyway
(`app_codex_revert.go#cutCodexThreadHistory` → `purgeCodexQueueBeforeCut`).
Nothing is spawned for the purge alone, and the two overlap harmlessly. It goes
FIRST on the cut connection because resuming a thread LOADS it, and a loaded
thread's idle hook is what dispatches the queue — which is also what
`Config.BeforeResume` exists for: it runs after the handshake froze
`ThreadQueueNative` and before the `thread/resume` that loads the thread.

A partial purge is not a degraded success: every row it failed to delete is a
message the user explicitly truncated away that the idle hook still dispatches
onto the shortened thread, silently, at the next resume. So the rollback
refuses before it stops the session and the cut refuses before it truncates — a
refusal is visible and retryable, a replay onto deleted history is neither.

**The refusal is retryable and mutation-free only because the purge reports
what it already deleted.** Deletes go one row at a time, so with A and B queued
the purge can remove A and then fail on B. History is untouched, which is what
the refusal promises — but A is out of codex's queue and no idle hook will
dispatch it. So `PurgeQueue` returns a `QueuePurge` naming the submissions it
removed, in order, rather than a count, and the abort path can put back
whichever of them the app layer's own store rows account for.

A FOREIGN submission the purge deleted cannot be put back. There is no
`thread/queue/add` caller here, and re-adding it would render another
producer's text as this user's own message. Those are counted (`QueuePurge.Foreign`,
a log figure and deliberately not an ownership verdict), named in the log, and
named in the refusal the user sees — never restored and never silently
forgotten.


## History truncation: three cuts, all turn-granular

AO uses TWO of the three. All three are TURN-granular, which is why the
parity note in
[`codex.md §Known upstream constraints`](../../../docs/references/codex.md#known-upstream-constraints)
still holds.

| Cut | Since | Shape | AO |
|---|---|---|---|
| `thread/fork { lastTurnId }` | 0.143.0 | forks THROUGH the turn, inclusive | **used** — the fallback cut, and the only one every supported codex answers |
| `thread/fork { beforeTurnId }` | 0.146.0, `#[experimental("thread/fork.beforeTurnId")]` | forks BEFORE the turn, exclusive; cannot be combined with `lastTurnId` | not used — a fork already has an anchor AO can state directly; the exclusive form would only move an off-by-one into the caller |
| `thread/revert { threadId, beforeTurnId }` | 0.148.0, `#[experimental("thread/revert")]` | replaces the thread's durable history IN PLACE, keeps the thread id and its subscriptions, and emits `thread/reverted` | **preferred** where available — see below |

The two AO uses describe the SAME boundary from opposite sides, and the
app layer resolves the two anchors separately (`resolveCodexForkAnchor`
walks down for the last kept turn, `resolveCodexRevertAnchor` walks up
for the first dropped one) because AO's turn rows can be missing either
neighbour.

Why the in-place cut is preferred now, having once been declined: it
does NOT destroy recoverable history. `revert_thread`
(`codex-rs/thread-store/src/local/revert_thread.rs`) writes a NEW
immutable rollout referencing the retained prefix and moves only the
SQLite rollout pointer — the pre-revert rollout survives on disk exactly
like a fork's source does. What it buys is thread identity: the thread
the user is editing stays the thread they keep, so provider-side thread
cost estimates, an external `codex resume`, and anything else keyed on
the Codex thread id survive an edit-and-resend.

Why the fork is still there: upstream refuses `thread/revert` on a
LEGACY-history thread, and a thread's history contract is fixed at
creation — `ThreadResumeParams` has no history-mode field, so no resume
can change it. AO therefore asks for it at birth:
`threadStartHistoryMode` (session_revert.go) puts `historyMode:
"paginated"` on `thread/start` whenever the connected app-server is
>= 0.148, matching what the codex TUI
(`codex-rs/tui/src/app_server_session.rs:1689`) and `codex exec`
(`codex-rs/exec/src/lib.rs:1187`) already do for every non-ephemeral
thread. The floor is the REVERT floor, not the field's own (paginated
history shipped in 0.147): a paginated thread on a server with no
`thread/revert` would carry the differences with none of the benefit.

Threads that predate the opt-in, and every thread created by a pre-0.148
binary, stay legacy for life and keep using the fork. So does any thread
whose server refuses paginated history — an app-server whose thread store
has no SQLite state database answers `thread/start` with "paginated
threads require thread/turns/list and thread/items/list support", raised
while destructuring the params and before any thread is created, so
`isHistoryPaginationUnsupported` retries once without the field rather
than failing the session.

What paginated changes, checked against rust-v0.149.0 for every AO
consumer of thread shape:

- `thread/resume` and `thread/read { includeTurns: true }` still return a
  fully populated `thread.turns`; the app-server materializes it through
  `paginated_thread_full_turns` instead of the rollout, so
  `collabHistoryOwnerships` and the fork's tail check are unaffected.
  Items are stored verbatim (`thread_items.item_json`), so
  `subAgentActivity` and `collabAgentToolCall` survive the projection.
- `thread/fork` works on a paginated source (`prepare_fork` with
  `ForkBoundary::ThroughTurn`) and still returns turns, so the fallback
  cut stays valid. Its only paginated-specific refusal needs
  `ephemeral: true`, which `ForkAt` never sends.
- The rollout file AO tails for subagent notifications
  (`session_rollout_notifications.go`) still carries what that reader
  wants: `RolloutItem::ResponseItem` and
  `RolloutItem::InterAgentCommunication` are persisted in BOTH modes
  (`codex-rs/rollout/src/policy.rs`). What paginated drops is the legacy
  `EventMsg` mirror (`user_message`, `agent_message`, `sub_agent_activity`,
  …), replaced by `item_completed` — which `rollout/` already reads
  (`convert.go` `paginated`, `items.go`).
- Live turn/item notifications are not history-mode dependent.
- Resume feeds the model `load_latest_model_context` rather than the full
  stored history. For a thread that never compacted these are the same
  items; after a compaction it is the compacted window, which is what the
  model had anyway.
- Two upstream refusals AO must not grow into: `thread/rollback`
  ("paginated threads do not support thread/rollback") and DETACHED review
  ("paginated threads do not support detached review",
  `turn_processor.rs:1308`). AO calls neither — `app_codex_review.go` uses
  `ReviewDeliveryInline` — but `ReviewDeliveryDetached` exists in
  `session_review.go` and must stay unused.
- Downgrade hazard: paginated rollouts are unreadable by a codex older
  than 0.147 (`reject_unknown_thread_history_mode`). AO's provider floor
  is 0.143, so a user who downgrades below 0.147 cannot open threads
  created after the opt-in. The same is already true of every thread their
  codex TUI has written.

## Upstream surface not consumed

Everything below exists on the wire at rust-v0.149.0 and is deliberately
ignored. Listed so a future sync can tell "we have not looked at this"
from "we looked and declined". Methods here are NOT in
`codexNotificationCatalog`'s consumed set; notification methods here are
opted out at initialize.

**0.147 → 0.149 additions AO declines:**

- `thread/queue/add` (0.148) — AO does not write to the provider's queue at
  all: a mid-turn message goes to `turn/steer`, correlated by
  `clientUserMessageId`. `thread/queue/start` is the member of the family AO
  must never call even in principle — dispatch is automatic
  (`QueuedItemService::on_thread_idle`), so a client `start` races the
  provider's own drain and can run one submission twice. `update` and
  `.../reorder` are declined for a duller reason: AO has no surface that edits
  or re-orders a message another producer owns. Only `list` / `delete` are
  adopted, and only to clear a foreign producer's rows; see §"The provider's
  queue".
- `project/create` / `delete` / `import` / `list` / `move` / `read` /
  `update` and the `project/changed` + `thread/project/updated`
  notifications (0.149) — AO owns its own project rows keyed on the git
  root (core principle 7). Adopting upstream's project identity would mean
  two authorities for the same concept.
- `Thread.projectId` (0.149) — the field rides on every `thread/start`,
  `thread/resume`, `thread/fork` and `thread/read` response AO already
  decodes. AO's structs are narrow and none uses `deny_unknown_fields`, so
  it is dropped silently; `TestThreadProjectIDIsIgnoredWithoutError` pins
  that.
- `server/diagnostics` (0.149) — a health surface with no AO consumer.
- `account/bedrock/discover` / `setup` (0.149) — AO does not offer a
  Bedrock login path.
- `McpServerStatus.pluginId` (0.149) — `mcpstatus.go` decodes only the
  fields the status projection needs; plugin provenance has no UI.

**Older, still declined:**

- `thread/rollback` — deprecated upstream, mutates in place, and its
  `num_turns` counts user-MESSAGE boundaries rather than wire turns.
- `close_agent` / `write_stdin` — model tools, not client-callable.

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
