# Codex thread lifecycle

This package owns ghost background-row reconciliation, bounded reopen probing
and resume, and cumulative provider thread-cost reads after session creation.

Keep cumulative cost separate from the append-only usage ledger. Persist it
only at turn settlement, reject reads whose `SessionRef` changed during
rollback, and overlay only the ungrouped lifetime query it can answer.

Session creation, event fan-out, rollback policy, queueing, review state,
account switching, and rate limits remain in `internal/app`. Tests use
injected sessions and temporary stores; never start a real provider or read a
real Codex home.
