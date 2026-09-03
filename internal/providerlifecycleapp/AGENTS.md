# internal/providerlifecycleapp

Owns provider quota lifecycle coordination: the account-scoped snapshot cache
and merge rules, durable 429 backoff, per-provider coalescing gates, turn-
activity polling cadence, separate Claude HTTP and Codex app-server refresh
paths, session-event quota attribution, and session-account projection.

## Merging readings

Most quota readings are PARTIAL: a Claude `rate_limit_event` carries one
window, the header fallback carries two and can never see a model-scoped
bucket, and a Codex `account/rateLimits/updated` notification carries one
bucket. So `MergeSnapshot` merges per limit/window rather than replacing.

`RateLimitsSnapshot.Complete` is the exception, and only a source that reads
every bucket in one response sets it (Claude's `/api/oauth/usage`, Codex's
`rateLimitsByLimitId`). A complete reading also DROPS cached limits it omits,
because a provider removes a bucket from its answer once that bucket has no
usage — which is exactly what a mid-window reset produces. Without the drop,
the pre-reset percentage survived for the rest of the window (2026-09-01: a
Fable weekly row frozen at 90% while session and all-models correctly read
0%). Two rules follow from that:

- A parser that had to SKIP a limit must clear `Complete`. It no longer holds
  the whole answer, and pruning against it would delete a live quota.
- An entry the reading names but the boundary check rejects still counts as
  named. The server listed it; its cached value is the better answer.

The cache is a union of readings, never a reading, so the merged snapshot
always leaves `Complete` false — otherwise the persisted union would prune a
live reading on the next boot. `frontend/src/lib/stores/rateLimitsInfo.svelte.ts`
is this rule's twin and changes with it.

`internal/app` keeps the provider event chokepoint and its exact ordering
through triage, observers, queue recovery, reconnect, and provider-specific
post-turn hooks.
Managed credential/adoption transactions remain in `provideraccountapp`, while
live session state remains in `sessionruntime`. Claude live-config and Codex
queue/revert/review transactions do not belong here.

Tests inject every provider probe and account/session port. Never spawn a real
provider binary or read a real provider home.
