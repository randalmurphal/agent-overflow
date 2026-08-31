# Provider-account Manager extraction

The Wails surface has five methods: `ListProviderAccounts`,
`LoginProviderAccount`, `SwitchProviderAccount`, `RemoveProviderAccount`, and
`RefreshProviderAccountUsage`. The migration keeps those exact root receivers,
DTOs, comments, RPC hashes, and `LocalOnlyMethods` entries.

## Ownership cut

`provideraccountapp.Manager` must take these fields in one change:

- the `provideraccounts.Store` and `provideraccounts.Credentials` pointers;
- `providerAccountMu` and every account/fingerprint read it guards;
- `providerCredentialFingerprints`;
- the Claude and Codex credential-reconcile mutexes;
- the account audit path and append operation.

Splitting any lock from those wards is forbidden. The Manager also owns the
login/switch/removal sagas, external-login reconciliation, stable canonical
credential probe, organization matching/backfill, and usage refresh because
those paths can commit a single-use credential rotation.

The root retains provider session processes, provider/session-account events,
provider status, queue/revert/review behavior, and periodic rate-limit policy.
The shared `usagebackoff.Ledger` remains a narrow dependency because the legacy
provider-wide Claude probe and account-scoped refresh entries share its durable
file.

## Narrow ports

- `SessionGateway.ApplySelection(provider, Selection)` projects a completed
  activation onto live sessions. It exposes no session map or `App` pointer.
- `SelectionLease` holds the Manager's selection read lock across one provider
  write. Root resolves the live session while holding the lease and releases it
  immediately after the write. Account activation takes the write side.
- `ProbeInvalidator` invalidates the existing Claude/Codex identity cache by
  typed `provider.ProbeCacheKey`; it does not run probes.
- `RateLimitSink` publishes or forgets snapshots. `UsageBackoff` exposes only
  `Remaining` and `Note`.
- Construction dependencies are explicit functions for lifecycle context,
  shutdown state, current settings, provider binary lookup, and browser open.
  Do not replace these ports with a `Host` interface mirroring `App` methods.

## Migration order

1. Land the contract-only package and root `SelectionLease` / `SessionGateway`
   adapters. Convert the send lock path to the lease while the original mutex
   still owns all state. This phase is behavior-neutral and independently
   race-testable.
2. Move pure provider policy and identity projection helpers with their unit
   tests: provider validation, signed-out/chain-position policy, account/DTO
   projection, identity matching, and org enrichment. Root DTO conversion stays
   explicit.
3. Add the Manager constructor and ports. Do not yet construct two authorities
   over the same stores or mutexes.
4. In one atomic state cut, move all fields listed under Ownership cut and
   switch startup initialization, the send lease,
   `app_provider_account_adapters.go`, and `app_provider_account_sessions.go`
   to Manager methods. There must be no compatibility mutex left on `App`.
5. Move account audit plus the env/identity/login/org/removal/accounts behavior.
   Replace the five root methods with thin projections. Move unit tests beside
   the Manager; retain native-login, real session adoption, and wire tests at
   root with the spawn-isolated fixtures.
6. Move account-scoped usage refresh and the selected/inactive Claude probe
   portions that can rotate or classify credential bytes. Inject the shared
   backoff and rate-limit sink. Leave periodic polling cadence and generic
   rate-limit bindings at root.
7. Regenerate Wails bindings and the transport registry. Require byte-identical
   App method hashes/models, focused tests, package and root race suites, and
   the no-real-provider spawn guard before deleting the old files.

The foundation in step 1 and pure-helper move in step 2 are landed. Steps 3–7
should be one coordinated wave; an eight-file mechanical move before step 4
creates two lock domains and is not safe.
