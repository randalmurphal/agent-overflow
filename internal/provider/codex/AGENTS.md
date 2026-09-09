# Codex provider

This package runs one `codex app-server` process per active thread and maps
JSON-RPC into `provider.ProviderEvent`. Read
[codex-wire.md](../../../docs/references/codex-wire.md) before changing
requests, notifications, queue behavior, collaboration, or history handling.

## Session and concurrency

The read loop owns its private accounting state. Guard shared session state with
the documented mutex or atomic, and never perform callbacks, provider writes,
or event emission while holding state locks. `Close` must clear every
session-scoped state group and settle pending requests.

Lock order is `controlMu -> mu -> childLifecycleMu -> eventMu`.
`ApprovalRegistry` and `collabAsyncMu` are leaf locks. Only `eventMu` may
remain held across `onEvent`; `mu` never may. Keep read-loop coordination
values on their existing atomics.

`NewSession` completes initialize, version checks, and exactly one
`thread/start` or `thread/resume` before exposing the session. A resume sends
`excludeTurns:true`; AO owns rendered history and must not replay provider
turns. Metadata-only reads use `includeTurns:false`. Paginated history and
revert require their declared version gates; legacy sessions keep legacy
fallback behavior.

`Send` claims a turn before writing because `turn/started` can arrive before
the RPC response. Preserve `clientUserMessageId` on sends and steers. Interrupt,
revert, queued-turn, and replacement-session paths must reject responses whose
session or history generation has changed.

Use the connected app-server's initialize user agent for every version gate.
Treat empty or unparseable versions as unsupported. Mid-turn user messages use
`turn/steer`, never the provider queue.

AO reads and deletes provider-owned queue entries but never adds, starts,
updates, or reorders them. Partial listings are errors. Purge before rollback
or resume can dispatch queued work; abort when the purge cannot be made
reversible. An unclaimed `turn/started` is adopted as active with origin
`external-queue`.

## Routing and collaboration

Notification routing resolves identity and ownership before classification.
Hold unknown-child notifications and requests in the bounded pending queue
until ownership resolves, and reject them when its deadline expires. Child
token usage may be re-scoped to the root; child lifecycle and content must not
mutate root turn state.

Recover collaboration ownership from persisted launches and verified descendant
metadata. Bounded item-page reads may locate an original spawn when its event
was missed; never replay unrelated transcript items or use a send as a spawn.
Only an owned child's execution signals may settle current launch runtime.

Use typed wire fields for background and collaboration ownership. Requested
child model and effort values are not the effective profile; obtain it from the
child's metadata-only resume response. Stop a child through its owned
`turn/interrupt`.

Read `session_rollout_notifications.go` before changing resumed-session or
child-ownership setup. Fresh starts never tail the rollout. A resumed session
arms mailbox observation when reusable children are known or discovered.
Child readers use file notifications to wake idle recipients, share one worker,
and retain bounded partial records. Fresh roots keep their raw subscriptions.

## Native operations

Transfer discovery uses a threadless one-shot app-server after the source
writer closes. It may call initialize, queue inspection, and metadata-only
thread reads, but no execution verb. Queue method-not-found proves unsupported;
other queue errors leave ownership unknown.

Every native fork sends `excludeTurns:true`. An anchored fork validates its
surviving tail with a bounded metadata-only read. `thread/revert` uses an exclusive
`beforeTurnId`; `thread/fork` uses an inclusive `lastTurnId`. Keep those
anchors separate. A full fork omits `lastTurnId` and skips tail validation.

Typed command-execution items own terminal history. Raw `exec_command` output
may enrich runtime metadata but must not create or reorder timeline rows.
Background-terminal termination joins on the provider process ID.

Account, model, skill, MCP, approval, and dynamic-tool frames retain their
provider-native wire types inside this package. Unknown notification methods
must remain visible as drift or explicit errors rather than silently becoming
success.
