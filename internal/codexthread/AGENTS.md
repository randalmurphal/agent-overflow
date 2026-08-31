# internal/codexthread/

Owns the application-level lifecycle of a Codex provider thread after the
application shell has created it: ghost background-row reconciliation,
bounded reopen probing/resume, and cumulative provider thread-cost reads.

The cost reader is deliberately separate from the append-only usage ledger.
It persists Codex's whole-thread estimate only at turn settlement, fences
in-flight reads when rollback repoints `SessionRef`, and overlays only the
ungrouped lifetime query that a cumulative total can honestly answer.

`internal/app.App` retains session creation, provider event fan-out, rollback
policy, the ignored `ReconcileCodexOnReopen` compatibility method and DTO, and
the `GetUsageStats` binding. Queueing, revert mechanics, review state,
account switching, and rate limits do not belong here.

Tests use injected probe-only Codex sessions and path-scoped `storetest`
databases. Never spawn a real provider binary or read a real Codex home.
