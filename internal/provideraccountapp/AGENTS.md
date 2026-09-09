# internal/provideraccountapp/

`Manager` owns managed provider-account selection, credential stores and
fingerprints, audit, provider-specific reconcile locks, login and removal,
external-login reconciliation, organization enrichment, transfer validation,
and credential-committing usage refresh.

Keep each lock with all of its wards. `internal/app` owns live provider
sessions and implements `SessionGateway`; sends hold a `SelectionLease`
across provider writes so account activation occurs wholly before or after the
write. Rate-limit persistence and emission enter through narrow
`providerlifecycleapp` ports.

## Credential operations

- Call `Deps.BeginWork` before any account or reconcile lock. Login transfers
  the lease to its session driver and releases it after process teardown and
  credential cleanup on every exit path. Switch, removal, probe, reconcile,
  transfer validation, and usage refresh retain admission through the complete
  transaction.
- `loginsession.go` owns one live login per provider. Its registry lock is a
  leaf: take no Manager lock and publish nothing while holding it.
- Provider login is asynchronous state projected on `provider:login`.
  `StartProviderLogin`, `GetProviderLoginState`,
  `SubmitProviderLoginCode`, and `CancelProviderLogin` address the same
  session. Submit and cancel need no second admission lease.
- A rejected Claude callback consumes that CLI attempt. Start a fresh
  `claude_authenticate` flow and publish its new URL.
- A Codex device flow may complete on another screen. Correlate completion by
  `loginId` through projected state rather than a blocking return.
- Each attempt owns one provider process and closes it on success, failure,
  cancellation, or replacement.
- Build login and probe environments from the same layers: configured user
  environment, boot-mode overrides, then the isolated provider-home pin.
  Endpoint configuration is part of account identity.
- Transfer validation uses a fresh provider-specific probe under the reconcile
  lock. Display caches never prove that the destination can accept ownership.
  Successful probes adopt credential rotation through the normal transaction.

Tests use `kerneltest.IsolateSpawns`, temporary provider homes, and explicit
mock executables for native login flows.
