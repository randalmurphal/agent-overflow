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

## Sign-in is a session, not a call

`loginsession.go` holds one live sign-in per provider behind a registry whose
lock is a LEAF: no other Manager lock is taken under it, and nothing is
published while it is held. Four bound methods drive it — `StartProviderLogin`,
`GetProviderLoginState`, `SubmitProviderLoginCode`, `CancelProviderLogin` — and
progress reaches every admitted client on the `provider:login` channel.

It replaced a single blocking RPC that opened a browser on the BACKEND'S
machine and waited for it. That shape has no remote answer: from a paired
phone, the link lands on a screen nobody is looking at and the call times out
before anyone could have finished. So the state is retained and pushed rather
than returned, and the client picks its own method — a page that cannot reach
`OpenExternalURL` asks for the REMOTE one without being told to.

Rules the drivers impose, and this layer obeys:

- **A burned Claude flow is restarted, never re-prompted.** One rejected
  callback kills the CLI's slot. The coordinator runs a fresh
  `claude_authenticate` and publishes the NEW link with a notice, because a
  user handed the same URL again will keep pasting the same dead code.
- **A Codex device flow finishes on another screen**, so the completion is a
  notification correlated by `loginId` and never a return value.
- **One provider process per attempt, closed however the attempt ends.** Both
  CLIs allow one login per process, and closing is what actually stops a
  cancelled device-code poll.
- **The spawn's environment is the probe's** (`providerLoginEnv`, `env.go`):
  the user's configured environment under the boot-mode layer under the
  isolated-home pin. The configured half is not optional — an
  `ANTHROPIC_BASE_URL` that changes which backend answers changes which account
  the person is signing in to, and adopting from a probe that ran elsewhere
  would file one login under another's identity.
