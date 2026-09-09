# Provider session runtime

`Manager` owns process-local provider-session registration, start handoff,
scoped AO-token authority, removal, and Claude live-config cleanup under one
mutex.

Never call provider I/O, stores, application callbacks, event emitters, or
orphan-reaper operations while holding the manager lock. The application owns
session creation and close, persistence, routing, account policy, queueing, and
revert behavior.

Tests remain pure runtime tests. If a test gains a provider spawn path, install
`kerneltest.IsolateSpawns`.
