# internal/providerlifecycleapp

Owns provider quota lifecycle coordination: the account-scoped snapshot cache
and merge rules, durable 429 backoff, per-provider coalescing gates, turn-
activity polling cadence, separate Claude HTTP and Codex app-server refresh
paths, session-event quota attribution, and session-account projection.

## Merging readings

Claude quota comes only from `/api/oauth/usage`. The stdio `rate_limit_event`
cannot say which account it reports (a response to a request sent before an
account switch lands after it), so the Claude parser drops it.

Some readings are PARTIAL: Claude's header fallback carries two windows and
can never see a model-scoped bucket, and a Codex `account/rateLimits/updated`
notification can carry one bucket. `MergeSnapshot` therefore merges per
limit/window rather than replacing, and drops a partial entry whose reset
boundary is older than the cached one.

`RateLimitsSnapshot.Complete` is the exception, and only a source that reads
every bucket in one response sets it (Claude's `/api/oauth/usage`, Codex's
`rateLimitsByLimitId`). A complete reading also drops cached limits it omits,
because a provider removes a bucket from its answer once that bucket has no
usage, which is exactly what a mid-window reset produces. Without the drop,
a pre-reset percentage would survive for the rest of the window. Two rules
follow:

- A parser that skipped a limit must clear `Complete`. It no longer holds
  the whole answer, and pruning against it would delete a live quota.
- A complete reading replaces every entry it names, whatever the boundaries
  say. A cached boundary can be wrong, and a wrong later boundary would
  otherwise reject every true reading until it passed.

The cache is a union of readings, never a reading, so the merged snapshot
always leaves `Complete` false; otherwise the persisted union would prune a
live reading on the next boot. `frontend/src/lib/stores/rateLimitsInfo.svelte.ts`
is this rule's twin and changes with it.

`internal/app` keeps the provider event boundary and its exact ordering
through triage, observers, queue recovery, reconnect, and provider-specific
post-turn hooks.
Managed credential/adoption transactions remain in `provideraccountapp`, while
live session state remains in `sessionruntime`. Claude live-config and Codex
queue/revert/review transactions do not belong here.

Tests inject every provider probe and account/session port. Never spawn a real
provider binary or read a real provider home.
