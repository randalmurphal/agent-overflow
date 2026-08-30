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

## Non-obvious ownership

`ls` covers the rest. These are the files whose contents you would not guess
from the name.

| File | Holds |
|---|---|
| `session_start.go` | Spawn, `initialize`, the one `thread/start` / `thread/resume`, the `BeforeResume` window. Every failure here means "there is no usable thread". |
| `session_notifications.go` | `dispatchRoutableNotification`: five named steps in a fixed order (route, ownership claim, child interception, parent fold, classify and emit). |
| `child_routing.go` | The bounded, deadlined, fail-closed quarantine for not-yet-owned child threads. |
| `collab_profiles.go` | Effective MultiAgentV2 child model and effort resolution through metadata-only child resumes. |
| `live_update.go` | Mid-session model / effort / fast-mode / runtime-mode change with no restart. |
| `notification_catalog.go` | The pinned upstream notification list, the DERIVED opt-out complement, the per-session drift log. |
| `probe.go`, `identity_probe.go`, `login.go`, `mcp_auth_flow.go` | The credential and OAuth paths. Treat changes here as security-relevant. |
| `protocol_*.go`, `server_requests.go`, `approval.go` | Where a new notification classifier, a new server-request dispatch, and a new approval kind go. |
| `rollout/` | Read-only reader for Codex's own on-disk state. Has its own guide. |

## Session lock order

`Session` carries five mutexes. Take them in this order or not at all:

```
mu  ->  childLifecycleMu  ->  eventMu
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
  `appServerVersion`, `threadHistoryMode`, `pendingRevert`,
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
- **Refusals raised BEFORE upstream mutates anything** map to
  `ErrThreadRevertUnsupported` or `ErrThreadRevertAnchorUnresolvable`, so the
  caller can fall back to a fork on the same connection. Anything else is a
  hard failure: `thread/revert` shuts the thread runtime down partway through,
  and a fork built on a half-reverted thread would agree with neither. Mid-turn
  revert is refused here even though upstream ALLOWS it, because upstream
  submits a shutdown and drops the running turn on the floor.
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
