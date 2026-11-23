# internal/provideraccountapp/

`Manager` owns managed provider-account selection, credential stores and
fingerprints, audit, provider-specific reconcile locks, login and removal,
external-login reconciliation, organization enrichment, transfer validation,
and credential-committing usage refresh.

Keep each lock with all of its wards. `internal/app` owns live provider
sessions and implements `SessionGateway`; sends hold a `SelectionLease`
across provider writes, so account activation occurs wholly before or after
the write. A text-generation CLI is a credential use (`credentialuse.go`) for
its whole run. Every path that changes the selected account calls
`selectionChanging` under the reconcile lock once it has decided to change it
and before it reads the outgoing canonical credential or replaces it; it ends
every running use, and none is admitted until the lock is released. A refused
or declined change stops nothing. The store refuses a selection change while a use is
alive. Rate-limit persistence and emission enter through narrow
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
- Adoption lands a login by its identity and never switches: a saved account
  is renewed in its slot, a new one is appended. Only a login for the selected
  account, or the first login when none is selected, replaces the canonical
  credential. Reconciliation of an outside login does switch, because every
  provider process follows the canonical credential.
- Every route that changes the selected account (switch, removal, outside
  login, a probe that adopts one) calls `applySelection` with the account it
  left, after releasing `mu`. Auto-switch attributes refusals from it.
- Every adoption reports its outcome as a `SignIn` (added, renewed,
  switched). A credential change that is none of these, a token rotation or a
  boot re-adoption, publishes nothing. Renewal is a later Claude refresh
  deadline; Codex cannot tell a renewal from a rotation.
- Build login and probe environments from the same layers: configured user
  environment, boot-mode overrides, then the isolated provider-home pin.
  Endpoint configuration is part of account identity.
- The canonical record (`recorded.go`) is the one proof, for every managed
  provider, that canonical bytes are the selected account's login; only a
  credential identified or activated as that account's is recorded. Beside
  it is the verdict on a canonical login reconciliation identified and could
  not attribute to one saved account (`unresolvedLoginError`), keyed by the
  credential's digest and the saved identities: another credential, a
  changed identity or a removal forgets it. Until then no reconciliation
  identifies those bytes again, every reading and spend of the selected
  account answers with its sentence, a send is refused with it before
  anything is dispatched (`ReconcileBeforeSend`), and a switch, removal or
  sign-in goes through and preserves nothing from them.
- One resolver (`resolveObservedLoginLocked`) attributes an unrecorded
  canonical login of any managed provider, for reconciliation and for the
  selected Claude refresh. An account saved without an email takes an
  observed login, named or not, only while nothing the login carries
  contradicts it (`blankEmailAccountTakes`, also session attribution's
  rule). An email several saved accounts share, with no organization named,
  is nobody's for certain.
- A usage reading of any managed provider, or a Codex spend, of the selected
  account whose canonical credential is not the recorded one reconciles
  first (`reconciledUsageStart`), under the lock it already holds, and runs
  on exactly the credential reconciliation accepted: an account no longer
  selected is read from its slot, and a spend of it is refused unsent. An
  Antigravity reading releases the lock before its quota request. The commit
  lands a selected reading's slot write and record only for the recorded
  credential (`commitProviderUsageRefreshLocked`). A Codex rotation is
  placed only by the credential store's settle, live and at boot; the commit
  learns where it went.
- A saved Codex slot whose credential names another workspace than its
  account (`savedLoginOfAnotherWorkspace`; only a Codex credential carries
  one) is never activated: an inactive reading and a switch refuse it before
  anything is written, auto-switch reads that refusal as an account that
  needs a sign-in (`IsNeedsLogin`), and removal passes over it when it picks
  the replacement.
- The selected Claude refresh starts the CLI only while the canonical store
  still holds the credential the reading was admitted on. A sign-in landing
  between that check and the CLI's own refresh cannot be excluded: the
  CLI's lock is its own while it runs, so the refreshed credential is judged
  by the resolver. A saved Claude slot that already holds another account's
  login is not detected; its bytes name nobody without a profile request.
- `provider:account` carries the selected account only, which every client
  shows as the one in use. Any other card's change publishes the listing
  (`provider:account_listing`).
- Transfer validation uses a fresh provider-specific probe under the reconcile
  lock. Display caches never prove that the destination can accept ownership.
  Successful probes adopt credential rotation through the normal transaction.

Tests use `kerneltest.IsolateSpawns`, temporary provider homes, and explicit
mock executables for native login flows.
