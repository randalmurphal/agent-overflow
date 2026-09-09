# Process supervision

This package selects and maintains the installed application generation that
should run for a data root. It coordinates process lifetime and bundle
publication; it does not own provider sessions or application state.

## Selection and startup

Selection is deterministic from validated candidates and persisted generation
state. Reject incomplete or incompatible candidates instead of guessing from
timestamps. Keep selection, state publication, and child spawn recoverable after
interruption.

One supervisor goroutine owns mutable state. Feed it explicit child-exit,
candidate, shutdown, and retry events; do not mutate selection through side
channels. Reap every child and close every pipe and watcher on failure.

Spawn with explicit argv, environment, working directory, and inherited handles.
Preserve original argv across generation switches, including updater helper and
frontend modes. Do not pass supervisor-only flags to the application. Readiness,
rather than process creation, establishes successful startup.

Data-root identity defines the supervision domain. Locks, state, candidates,
logs, and cleanup use the same resolved root. Never allow two supervisors to
publish or run different generations for one root.

## macOS bundles

Never rewrite a running macOS application bundle. Publish a complete bundle at a
distinct path, validate it, atomically select it, and retain the prior bundle
while any process may execute from it. Cleanup only generations proven inactive
and outside rollback retention.

Publication preserves signing and executable layout. Do not copy files into an
existing bundle, spawn through a mutable symlink, or delete the old generation
immediately after launch.

## Lifetime

Supervisor locks and other lifetime handles are close-on-exec. Provider,
browser, editor, and orphan-reaper descendants may outlive the application and
must not keep the next generation locked out.

Cancellation starts graceful shutdown, followed by the platform's bounded
escalation. Process groups or jobs target only application descendants. Tests
cover interruption, readiness failure, replacement races, repeated shutdown,
inherited handles, argv preservation, rollback, and running-bundle retention.
