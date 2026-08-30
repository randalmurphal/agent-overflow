# internal/provideraccounts/

Multi-account metadata plus the filesystem boundary that activates
provider-native credentials. Claude's refresh tokens are single-use and
rotation is serialized on a lockfile scoped to the config home, so almost
every rule here exists because one wrong write ends a login the user
cannot recover without signing in again. Root `AGENTS.md` §Permanent
invariants carries the incident history and the test rules.

## Security boundary

- `provider-accounts.json` holds metadata and quota snapshots only. Never
  add tokens, API keys, OAuth URLs, or authorization codes.
- Saved credentials live below the provider's own home and keep its
  native filename and JSON shape (on macOS, the config-home-scoped
  Keychain entries are the native store). Write no other provider state
  into the account directory, and ignore unrecognized contents there.
- Credential bytes are opaque. Reads reject symlinks and non-regular
  files; writes go through `atomicfile.Write` (0600, fsync, rename).
- Normal provider processes always run from the canonical native home,
  and switching accounts replaces only its credential. New logins and
  Codex inactive-account probes use short-lived 0700 temporary homes and
  retain only the resulting credential.
- Account IDs are path components. Validate before joining.

## Identity: never written, only retired

A Claude login is two files. `.credentials.json` holds the tokens;
`~/.claude.json`'s `oauthAccount` holds the identity the CLI reports and
bills against, written from a login profile fetch and never re-derived
from the token, so replacing the credential alone leaves the CLI
describing the previous account. Codex needs none of this: `auth.json`
carries the account claims, so replacing the file replaces the identity.

- **AO never writes a provider identity, only retires one.**
  `retireProviderIdentity` deletes `oauthAccount` exactly as `/logout`
  does, and the CLI's next start refetches and writes it back itself.
- **Retire BEFORE the canonical credential write.** Every failure then
  converges; the reverse order has one that does not self-heal.
- The one narrow read: adoption may READ `oauthAccount` once, at account
  create or match, for the organization uuid the probe wire omits.
  Accepted only when its email matches the probe-reported one.
- Strip `oauthAccount` from a temporary home's copied `.claude.json`, or
  the CLI there considers its identity settled and never derives the one
  belonging to the credential it was seeded with.

**After a retire, "the CLI reports no identity" says nothing about the
credential.** The record is gone by construction and returns only from an
asynchronous profile fetch, so the first probe after ANY switch answers
with an empty `account` however healthy the login is. Judge a credential
by its bytes or by asking the server, and never drop the probe's
credential bytes on that branch: it discards a rotation that landed.

**Identity is (email, organization), matched in ONE place.**
`identity_match.go` owns the vocabulary: `Identity` (blank means UNKNOWN,
never "none"), `Contradicts` (only two KNOWN values can disagree, so a
legitimately-empty post-switch probe cannot condemn a working login),
`Confirms`, `FindByIdentity`, and the write-chokepoint rules (same-email
accounts need distinct non-blank org ids; a saved org enriches from blank
but never REBINDS). Call sites resolve through
`App.findAccountByObservedIdentity` and never compare emails directly.
Matching uses email plus org ID only: `OrgName` changes on an org rename.

## Credential writes and refreshes

- **The selected account refreshes in the canonical home, never a copy.**
  Two homes holding one token take different locks; the loser keeps a
  credential the server will never accept again.
- **A probe is never torn down mid-rotation.** Every Claude probe carries
  a `ReadCredential` (wired once in `claudeProbeConfig`, enforced by
  `TestClaudeProbeConfigIsTheOnlyProbeConstructor`) and holds teardown
  until an expected rotation lands (`claude/rotation.go`). Other
  short-lived Claude spawns need no cover: they run to natural completion
  and never write credentials back.
- **Inactive accounts are never refreshed at all.**
  `probeInactiveClaudeRateLimits` reads saved bytes over HTTP and reports
  stale usage rather than spending the account's one-shot chain on a
  background poll; selecting the account signs it back in. Codex still
  probes from a temporary home, having no single-use rotation to lose.
- **The sign-out husk is refused at the WRITE layer**, not by caller
  discipline: `writeCredentialAt` and `writeActiveCredential` return
  `ErrSignedOutCredential`, so a forgotten guard cannot cost a login. The
  provider-specific husk shape arrives through the `Policy` argument
  every `NewCredentials` call must supply, so a store that accepts
  production-refused bytes cannot be built by omission.
  `WriteNativeCredentialForTest` is the one deliberate bypass: it
  impersonates the CLI, the actor that legitimately writes a husk.
- **A backwards slot write is LOGGED, never refused.** `writeCredentialAt`
  compares `Policy.ChainPosition`. Refusing was implemented and reverted:
  it drops real rotations, one bad value wedges a slot permanently, and
  the skip is invisible to callers that re-read the slot. Canonical writes
  are not ordered at all, since switching accounts legitimately installs
  an older expiry.
- **`rollback.go` captures STRUCTURE only.** `RestoreAccountCredential`
  removes a credential or slot the operation introduced and never rewrites
  bytes, because the rolled-back operation is frequently what rotated the
  chain.
- **A rotation must survive every failure path.** Activation re-reads
  canonical immediately before the final overwrite and preserves newer
  bytes into the outgoing slot, since a mid-switch move is far more often
  a rotation than anything else. The husk is never preserved. And because
  a rotation legitimately changes the bytes, the guard that canonical
  still belongs to the selected account is the identity the CLI reports,
  never a byte comparison.

## Home and environment footguns

- **Never point a canonical-home run at `CLAUDE_CONFIG_DIR`, not even at
  the default path.** Claude keys "is this the default home" off the
  variable being ABSENT, not off its value, and a non-default home hashes
  into a different macOS Keychain service.
- **`CLAUDE_SECURESTORAGE_CONFIG_DIR` (Claude >= 2.1.220) is reserved and
  cleared everywhere `CLAUDE_CONFIG_DIR` is** (`provider.ReservedEnvNames`).
  It overrides `CLAUDE_CONFIG_DIR` for secure-storage naming alone, so an
  inherited value makes a temporary-home probe write its rotated
  single-use token into the canonical account's Keychain item.
- **Swapping the canonical credential under live processes is SUPPORTED**
  (spike-verified 2026-08-18, claude 2.1.234): the CLI resolves its
  credential from disk per request and concurrent processes serialize
  rotation on the config-home lock. Do not "fix" this with per-account
  `CLAUDE_CONFIG_DIR` isolation, which buys nothing and costs the shared
  `~/.claude` settings, skills, plugins, and transcripts every other
  subsystem reads. So applying a switch needs no session restart, and a
  live session re-bills to the new account from its next request
  (`usage_ledger` has no account column, so that spend is unattributable,
  a known product gap).
- **`PruneOrphanedAccounts` runs only when the `providerHome` stamp
  matches** the home the credentials operate under (`ClaimProviderHome`,
  first claim wins). A store paired with a foreign home, such as a scratch
  `--data-dir` against a real `$HOME`, degrades to "never prune" rather
  than pruning someone else's logins. Slot destruction is announced
  through `auditAccountEvent` at `<dataDir>/account-audit.log`.

## The Keychain seam and testing

Every `security(1)` invocation in the codebase lives in
`claude_keychain.go`, pinned by
`TestNoSecurityCallsOutsideTheKeychainSeam`. Never add one outside it.
That file carries the rationale, the incident, and how the production
backend mirrors Claude Code's keychain/plaintext fallback.

A temporary home does NOT isolate the Keychain: the active slot's service
name is fixed regardless of the injected home. `NewCredentials` therefore
installs the file-backed stand-in inside test binaries (harness:
`NewCredentialsWithFileKeychain`), using the exact non-darwin layout so
darwin tests need no special-casing, and `securityCommand` refuses a
system-path `security` in a test binary so a forgotten
`installFakeSecurity` fails loudly. Tests inject a temporary home and
seed canonical through `WriteNativeCredentialForTest`; never touch the
developer's real homes.
