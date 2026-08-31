# internal/sessionruntime

Owns every process-local provider-session ward whose transitions must remain
atomic with live-session registration. `Manager` owns its mutex and never calls
provider I/O, stores, App callbacks, event emitters, or orphan-reaper operations
while holding it.

`Put`, removal, start handoff, scoped AO-token authority, and Claude live-config
cleanup are one lock domain. `internal/app` owns provider creation and close,
persistence, event routing, account selection, queue/revert policy, and Wails
façades.

Tests in this package must be pure runtime tests. If a future test can spawn a
provider, install `kerneltest.IsolateSpawns`; current tests use nil or fake-free
handles and must never invoke provider binaries.
