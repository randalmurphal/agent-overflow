# internal/providerlifecycleapp

Owns provider quota lifecycle coordination: the account-scoped snapshot cache
and merge rules, durable 429 backoff, per-provider coalescing gates, turn-
activity polling cadence, separate Claude HTTP and Codex app-server refresh
paths, session-event quota attribution, and session-account projection.

`internal/app` keeps the provider event chokepoint and its exact ordering
through triage, observers, queue recovery, reconnect, and provider-specific
post-turn hooks.
Managed credential/adoption transactions remain in `provideraccountapp`, while
live session state remains in `sessionruntime`. Claude live-config and Codex
queue/revert/review transactions do not belong here.

Tests inject every provider probe and account/session port. Never spawn a real
provider binary or read a real provider home.
