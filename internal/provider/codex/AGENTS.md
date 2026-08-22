# internal/provider/codex/

Wraps `codex app-server`. One process per active thread, JSON-RPC 2.0
over stdio.

## Layout

- `session.go` — the shared `Session` state struct, the `Config` it is built
  from, the accessors over both, dynamic-tool and MCP handler registration
  (plus the retained per-server startup states), and `Close`, which drops the
  session-scoped maps field by field. Everything the read loop touches is
  either owned by the read-loop goroutine alone (`usageAcct`) or guarded —
  `mu` for the mutable session state, `eventMu` for emission, and an atomic
  for the root Codex thread id (see below).
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
  the turn id the wire hands back, and the `turn/started` dedupe ledger. Send
  CLAIMS the turn before the write (`beginLocalTurnStart`) because
  `turn/started` can beat `turn/start`'s own response onto the read loop, and
  `clearTurnStart` releases what a timed-out claim left behind — see
  §"Externally queued turns" for what reads those claims.
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
- `thread_queue.go` — the provider-owned user-message queue
  (`thread/queue/*`, codex >= 0.148): the three RPC wrappers AO calls
  (`add` / `list` / `delete`) plus `PurgeQueue` over them, the
  handshake-frozen `ThreadQueueNative` gate, the client-id-keyed self-queued
  claim ledger that keeps an automatically dispatched turn attributed to this
  app (and `RearmSelfQueuedClaims`, which rebuilds it after a session death),
  and the single-flighted `thread/queue/changed` reconciliation. Two refusals
  live here rather than being smoothed over: `QueueAdd` fails when the claim
  ledger is full, because a dropped claim would announce the user's own next
  message as somebody else's, and `QueueList` returns
  `ErrThreadQueueListIncomplete` (page cap or repeated cursor) or
  `ErrThreadQueueListMalformed` (an element this build cannot read) with the
  prefix it did read, because a listing that looked complete would let a purge
  report success over rows it never saw. `PurgeQueue` returns a `QueuePurge`
  naming the submissions it deleted rather than a count, so a caller that
  aborts on a partial purge can put its own messages back. `start`, `update`
  and `reorder` are deliberately absent. See §"The provider-owned queue".
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
  (`approvalPolicy` / `sandboxPolicy` / `approvalsReviewer`) ride it ONLY on
  a queue-native session, where the next turn may be one the app-server
  starts out of its own queue with no overrides at all — see the file's own
  doc block.
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
- `thread/queue/add` / `list` / `delete` — the provider's own user-message
  queue, all `#[experimental]` (0.148) and so riding the
  `capabilities.experimentalApi` the handshake already sets. On a 0.148+
  app-server these REPLACE `turn/steer` as the destination for a message the
  user sends mid-turn. `add` is always preceded by a full
  `thread/settings/update` assertion — a queued turn carries no overrides of
  its own. `thread/queue/start`, `update` and `reorder` are not called — see
  §"The provider-owned queue".
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
    (`app_session_config.go#threadTurnInFlight`) UNLESS the session is
    queue-native, because there a mid-turn change has to reach the thread
    before the next dispatch. `QueueAdd` is the other mid-turn caller, by
    design. Both are safe on upstream's own terms: every axis is documented
    "for subsequent turns" and the op updates the session configuration a
    later turn is built from, never the running turn's TurnContext.
  - **The runtime-mode axes are routed through it only on a queue-native
    session.** `approvalPolicy`, `sandboxPolicy` and `approvalsReviewer`
    otherwise stay on `turn/start` (see RuntimeMode in the parent guide),
    which re-asserts all three every turn. A queued turn re-asserts nothing
    — `ThreadQueueAddParams` carries no overrides and the drain builds a
    `ThreadSettingsOverrides::default()` — so on those sessions the thread's
    stored policy IS the turn's policy and the push is what makes a
    tightening reach the turn that runs the queued row.
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

### Child thread-wide suppression, and its one carve-out

A child thread's THREAD-WIDE notifications describe the child, not the
parent, so projecting them would let a subagent overwrite the parent's
context meter, title, or compact state (ADR-002).
`isChildSuppressedThreadNotification` drops them, and
`isUnsafeChildProjectionEvent` is the second gate on the event side.

`thread/tokenUsage/updated` is the one method deliberately NOT in the
suppression set. It is intercepted in `dispatchRoutableNotification`
BEFORE the child-state branch and converted, by
`emitChildTokenUsageProgress`, into an `EventSubagentProgress` scoped to
the spawn card:

- `ItemID` is the spawn's `parentToolUseID` — the launch row the tick
  renders on;
- `ParentToolUseID` is that spawn's OWN parent, resolved by trimming the
  child's canonical agent path (`/root/reviewer/deep` → `/root/reviewer`)
  and mapping it back to a launch. Depth-1 children and path-less builds
  resolve to `""`, which is correct: their spawn is top-level;
- `Meta.TotalTokens` is the child's CUMULATIVE total
  (`tokenUsage.total.totalTokens` via `childCumulativeTokenTotal`), not
  `normalizeThreadTokenUsage`'s `last.totalTokens` — that one is context
  occupancy, and `SubagentProgressMeta.TotalTokens` means spend.

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
- A `turn/started` with only SELF-QUEUED claims outstanding resolves to
  neither answer: `adoptTurnStart` returns `turnAdoptionUndecided` and stamps
  nothing, because the provider drains one FIFO that can also hold a foreign
  producer's rows. The `userMessage` echo's `clientId` is what decides it —
  see §"The provider-owned queue".
- An adopted external turn stamps `Meta.origin = "external-queue"`
  (`ExternalTurnOriginQueue`) on the `EventTurnStart` and on the
  `EventUserText` echo, so the injected prompt is not persisted or rendered
  as something the person in front of Agent Overflow typed. It is a value,
  not a boolean: a second external producer must be distinguishable, not
  folded into "not ours".
- `thread/queue/changed` is `{threadId}` and nothing else at rust-v0.149.0 —
  no count, no item id, no text — so what it raises depends on whether AO is
  itself in the queue. On a pre-0.148 app-server it raises one
  `EventNotification` (`Meta.kind = "external_queue"`) that reports no depth,
  because there is no `thread/queue/list` to ask. On 0.148+ the notice is
  evidence-driven instead: see below.
- Classification fails toward "local": an unknown turn id reads as local,
  and at the origin-map cap new turns read as local. Mislabelling an AO
  turn as injected corrupts the user's own transcript; the reverse costs
  only the marker.

## The provider-owned queue (codex >= 0.148)

From 0.148 the queue is not just something that happens TO AO — it is where
AO puts a message the user sends while a turn is running. `thread_queue.go`
owns the three methods AO calls (`add` / `list` / `delete`) and the
reconciliation of `thread/queue/changed` once AO is a participant.

`update` and `reorder` exist upstream and are deliberately NOT wrapped: the
composer cannot edit or re-order a message once the provider owns it, so a
wrapper for either would be dead code whose wire shape nothing verifies. The
mock refuses both with `-32601` so a harness run that grows a caller fails
loudly (`cmd/ao-mockprovider/codex_queue.go`).

**One decision, taken once, at handshake time.** `recordThreadQueueSupport`
runs immediately after `recordAppServerVersion` and freezes
`Session.ThreadQueueNative()` for the session's whole life. Everything else
reads that frozen flag. It cannot be re-derived per call: the two queues must
be MUTUALLY EXCLUSIVE per session, and a flag that could flip mid-session
would let one message take AO's path and the next take the provider's, with
no way to reconcile the ordering between them. Empty or unparseable
`userAgent` fails closed to the old behaviour.

**Which queue owns a mid-turn message** (`app_flush_queue.go`):

| | codex < 0.148 | codex >= 0.148 |
|---|---|---|
| dispatch verb | `turn/steer` | `thread/queue/add` |
| when it reaches the model | immediately, into the RUNNING turn | when the thread next goes idle |
| row's turn index | the active turn | the NEXT turn |
| who starts the turn | nobody (it joins one) | the app-server's idle hook |

The turn-index difference is not cosmetic: a steered message is context for a
turn already underway, a queued one opens a turn of its own, and placing the
row in the wrong one puts the prompt below its own answer.

**AO must never call `thread/queue/start`.** `QueuedItemService` is a
`ThreadLifecycleContributor`; `on_thread_idle` → `dispatch_if_idle` →
`start_turn_if_idle` → `delete_locked`, and `enqueue` itself calls
`wake_if_loaded`, so an idle thread dispatches inside the `add` request.
Calling `start` on top of that races the drain. There is no wrapper for it
and `TestQueueStartIsNeverCalled` fails the build if one appears.

**`clientUserMessageId` is AO's optimistic row id.** Upstream requires the
field and mints a uuid when a producer omits it, so an empty value would
silently give up correlation rather than fail. AO passes the deferred row id
the flush dispatcher just allocated (`user:<turn>:flush:<n>`), which is what
lets a `thread/queue/list` say which entries are this app's. The echoed
`userMessage` carries it back as `clientId` (`ThreadItem::UserMessage`,
rust-v0.149.0 codex-rs/app-server-protocol/src/protocol/v2/item.rs:236), and
that echo is the AUTHORITY on which queued row the app-server just started —
see the claim ledger below. Triage still consumes the echo itself by FIFO;
the `clientId` decides ORIGIN, not row identity.

**Two claim ledgers, for two different lies:**

- A queue-dispatched turn starts with no `turn/start` of ours, which is
  exactly the shape of an injected turn. `noteSelfQueuedSubmission` claims
  before the write (an idle thread can dispatch before the response lands),
  and the claim is consumed BY CLIENT ID, never by position: the provider
  drains one FIFO that can hold a foreign producer's rows interleaved with
  AO's, so popping the oldest claim would hand it to whichever row happened
  to be at the head and render somebody else's message as the user's own.
  `turn/started` therefore DEFERS (`turnAdoptionUndecided`) while any
  self-queued claim is outstanding and stamps nothing; the `userMessage`
  echo's `clientId` decides it (`resolveUserEchoOrigin`), which is also the
  row that actually needs protecting. The claim survives a request TIMEOUT,
  same asymmetry as `beginLocalTurnStart` — see `IsAmbiguousQueueAddTimeout`.
  The ledger is in-memory, so a session that comes back to a non-empty queue
  rebuilds it from `RearmSelfQueuedClaims`; the app layer decides which listed
  rows are AO's (`app_flush_queue_provider.go#rearmCodexProviderQueueClaims`)
  and hands them over. It is a map keyed by client id and it REFUSES at its
  cap rather than evicting: a dropped claim is a live message that would be
  announced as somebody else's, so `QueueAdd` fails the add instead (see
  §"Two states, not one" below). AO's own queue frees an entry as codex
  accepts it, so more than its cap can accumulate provider-side across a long
  turn — the cap here is sized for that, not for AO's queue length.
- AO's own `add` and `delete` raise `thread/queue/changed` too, and so does
  every automatic dispatch (the drain deletes the row it started), so the
  unconditional notice would announce the user's own message as coming from
  outside. In native mode the classifier's event is dropped and replaced by
  an async `thread/queue/list` diffed against AO's own client ids; only a
  submission AO never added raises the notice, and
  `reportedForeignSubmissions` makes it once per submission. The walk is
  SINGLE-FLIGHTED with a dirty re-run (`startQueueReconcile`) — an N-message
  queue produces ~2N notifications and the state at the end of the burst is
  the only one worth reporting — and its context comes from the session's
  lifetime, so a teardown mid-walk cancels it.

**Ownership is the persisted row, never the id grammar.** AO's queued ids
(`user:<turn>:flush:<n>`) are deterministic, so they are not a credential: a
second Agent Overflow profile against the same Codex home mints the same ones,
and anything speaking `thread/queue/add` may simply supply one. Recognising the
grammar would re-arm a foreign submission as AO's and render its author's
message as the user's own. Every provider-queue add eager-persists and MARKS
its row before the write, keyed by the id that goes on the wire, so the row's
existence in this app's store is the token
(`app_flush_queue_provider.go#providerQueuedRowsForThread`). Nothing else needs
to be persisted for it, and nothing on the wire can forge it.

**Two states, not one: `providerQueued` and `providerQueueHandoff`.** The
marker has to go on BEFORE the add — an add that lands and is never acked is
exactly the case where this process may not come back to stamp anything — so on
its own it cannot tell "the provider has this message" from "AO was about to
ask it to", and those need opposite recoveries. `internal/itemmeta` carries
both: PROVEN (`MarkProviderQueued`, or `ConfirmProviderQueueHandoff` once the
ack or a `thread/queue/list` read-back proves it) means absence from the queue
is a dispatch, so the row is history; UNPROVEN (`MarkProviderQueueHandoff`)
means absence overwhelmingly means the add never landed, so the message has no
owner at all and goes back to the composer. Without the split the second case
is stranded forever, because the marker makes every recovery path step around
a row the provider never took.

**Session start reconciles, and a failed list does not end it.**
`reconcileCodexProviderQueueOnResume` is `Config.BeforeResume`, so it runs in
the one window after the handshake froze `ThreadQueueNative` and before the
`thread/resume` that loads the thread and lets its idle hook dispatch. It
splits the store's marked rows against `thread/queue/list`: present means the
provider holds it (re-arm the claim and the pending send, and promote a still
unproven hand-off), absent and unproven means it was never taken (return it to
the composer), absent and proven means it ran. The list is retried ONCE and, if
it still fails, ownership is answered from the store alone — but only for the
PROVEN rows. For those the store is a complete answer: the provider acked the
add, so either the row is still queued (the claim and the pending send are
exactly what its dispatch needs) or it already ran (both are inert, because
both are consumed by client id and no echo can arrive for a row that is gone),
and re-arming is what keeps the provider from dispatching AO's own message as
an injected turn.

An UNPROVEN row gets NEITHER, and that asymmetry is the point. Its add was
written and never acked, so the provider may hold it or may never have seen it,
and the two answers want opposite recoveries. Restoring is wrong if the add
landed — the user gets a draft of a message that is also scheduled to run, and
sending it is a duplicate. Re-arming is wrong for the commoner case: with no
add on the other end no echo can ever consume the claim or the pending send, so
the pending send sits in the FIFO forever, where `HasPendingSendForThread`
reads it and refuses every revert-and-resend on the thread, while the message
itself is stranded outside both the provider and the composer. So unproven rows
are left exactly as they are, the next session start that CAN read the queue
resolves them, and the notice names them separately
(`codex_queue_unreconciled` reports the proven count and the unproven count as
two different states), the same posture as the pre-0.148
`codex_queue_unsupported` notice.

**A `thread/queue/list` can report a PREFIX.** Pagination stops on a repeated
cursor or at the page cap, and both used to return the rows so far with a nil
error — indistinguishable from a short queue. It returns
`ErrThreadQueueListIncomplete` alongside the prefix now, and every caller that
must not mistake a partial answer for an empty tail (the purge, the resume
reconcile) treats it as a failure.

**An element it cannot READ is the same failure.** Upstream's `QueuedSubmission`
is `{id, input, client_user_message_id}`, all three required and non-`Option`,
so an element that will not decode is a wire fault, not a short row. Returning
an EMPTY submission for it — which is what a swallowed `json.Unmarshal` did —
makes it indistinguishable from an ABSENT one, and absence is exactly what the
two recovery callers act on: the resume reconcile would hand an unproven AO row
back to the composer while codex still held it, and the purge would skip the
empty id and let the rollback truncate over a submission still armed to run.
`parseQueuedSubmission` reports instead, `QueueList` stops the walk and returns
the readable prefix with `ErrThreadQueueListMalformed`, and an empty
server-assigned `id` counts as malformed because it is the only handle a delete
takes. The one caller that does NOT treat it as fatal is the foreign-submission
notice walk, which only ever adds notices for rows it can see.

**`thread/queue/delete`'s `deleted` is required.** `ThreadQueueDeleteResponse`
types it as a bare `bool` with no serde default, so upstream's own deserializer
refuses a body without the key. Decoding it into a Go `bool` turned any drift —
a rename, a nested envelope, an explicit `null` — into a benign-looking
`false`, which both readers treat as "already dispatched": the claim ledger
holds its claim and the purge counts the row as accounted for, so the rollback
truncates history over a submission that may still be armed. It decodes as a
`*bool` and a missing or null one is an error.

**Rollback purges it, and a purge that cannot complete ABORTS the rollback.**
AO's own flushqueue is cleared in process, but a row already accepted by
`thread/queue/add` lives in codex's SQLite: it survives `stopSession` and
`on_thread_idle` dispatches it on the next resume, re-running a message the
user just rolled back onto a thread that no longer holds it. `PurgeQueue`
deletes every row — foreign ones too, named in the log, because a foreign row
carries the same hazard — over whichever connection the rollback has: the LIVE
session before the stop (`app_conversation_rollback.go`), or, for a thread that
had none, the throwaway resume the history cut opens anyway
(`app_codex_revert.go#cutCodexThreadHistory` → `purgeCodexQueueBeforeCut`).
Nothing is spawned for the purge alone, and the two overlap harmlessly: after a
successful live purge the second one lists an empty queue. It goes FIRST on the
cut connection because resuming a thread LOADS it, and a loaded thread's idle
hook is what dispatches the queue.

A partial purge is not a degraded success: every row it failed to delete is a
message the user explicitly truncated away that the idle hook still dispatches
onto the shortened thread, silently, at the next resume. So the rollback
refuses before it stops the session and the cut refuses before it truncates —
a refusal is visible and retryable, a replay onto deleted history is neither.
A LIVE session on a pre-0.148 app-server has no purge to attempt, and that is
not automatically safe either: if the store says this thread has rows a newer
Codex accepted, the rollback is refused there too.

**The refusal is retryable and mutation-free only because the purge reports
what it already deleted.** Deletes go one row at a time, so with A and B queued
the purge can remove A and then fail on B. History is untouched, which is what
the refusal promises — but A is out of codex's queue, no idle hook will
dispatch it, and every recovery path steps around a row marked provider-queued,
so an abandoned rollback would silently eat a message the user queued. So
`PurgeQueue` returns a `QueuePurge` naming the submissions it removed, in
order, and the abort path
(`app_flush_queue_provider.go#restorePurgedProviderQueueRows`) puts AO's own
back through the same restore-to-composer route the never-queued case uses,
under the `provider_queue_purge_aborted` reason. Ownership there is the
persisted row, never the id grammar: a deleted submission is restored only when
this app's store holds an unclaimed provider-queued row under that
`clientUserMessageId`.

A FOREIGN submission the purge deleted cannot be put back. There is no
`thread/queue/add` that preserves another producer's authorship, and re-adding
it would render that author's text as this user's own message. Those are
counted, named in the log, and named in the refusal the user sees — never
restored and never silently forgotten.

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

- `thread/queue/start` (0.148) — the member of the queue family AO must never
  call: dispatch is automatic (`QueuedItemService::on_thread_idle`), so a
  client `start` races the provider's own drain and can run one submission
  twice. `thread/queue/update` and `.../reorder` (0.148) are declined for a
  duller reason — AO has no surface that edits or re-orders a message the
  provider already owns. `add` / `list` / `delete` are adopted; see §"The
  provider-owned queue".
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
