# internal/provideraccountapp/

This package is the application boundary for managed provider accounts.
`Manager` owns the selection mutex, both provider reconcile mutexes, credential
fingerprints, account metadata/credential stores, the audit path, native-login
and removal sagas, external-login reconciliation, organization enrichment, and
credential-committing usage refresh. Never split one of those locks from its
wards. `internal/app` keeps provider session runtime and implements `SessionGateway`; a
send holds a `SelectionLease` across the provider write so activation is
ordered before or after that write, never through it.

Usage refresh belongs to the Manager because it commits credential rotations.
Rate-limit storage/emission stays behind narrow injected ports owned by
`providerlifecycleapp`. Provider event
handling, session-account events, provider status, queue/revert/review, and the
session runtime remain in `internal/app`.
