# internal/provideraccounts/

Owns Agent Overflow's multi-account metadata and the filesystem boundary used
to activate provider-native credentials.

## Security boundary

- `provider-accounts.json` contains metadata and last-known quota snapshots only. Never
  add access tokens, refresh tokens, API keys, OAuth URLs, or authorization
  codes.
- Saved account credentials live below the provider's own home (`~/.claude` or
  `~/.codex`) and retain the provider's native filename and JSON shape. The
  app writes no provider cache, history, database, or configuration state into
  the enclosing account directory and ignores unrecognized contents there. On
  macOS, Claude's config-home-scoped Keychain entries are the native store and
  are copied directly between Keychain services instead.
- Treat credential bytes as opaque. Reads reject symlinks and non-regular
  files; writes use `atomicfile.Write` (0600 + fsync + rename).
- Normal provider processes always run from the canonical native home. Account
  switching atomically replaces only its credential. New logins and inactive
  account probes use short-lived 0700 temporary homes; retain only the
  resulting credential and delete all other temporary provider state.
- Account IDs are path components. Validate them before joining.

## The account tuple

A login is not always one file. Claude Code splits it: `.credentials.json`
holds the tokens, and `~/.claude.json`'s `oauthAccount` holds the identity
the CLI reports, bills against, and caches entitlements for. That identity
is written from a profile fetch at login and is never re-derived from the
token on read, so **replacing the credential alone leaves the CLI
describing the previous account** — including when Agent Overflow's own
probe asks it who is logged in.

The rule: **Agent Overflow never writes a provider identity, only retires
one.** `retireProviderIdentity` deletes `oauthAccount` exactly as Claude
Code's own `/logout` does; the CLI's next start refetches the profile with
whichever token is installed and writes the identity back itself. The
provider stays the sole author of its own identity, so no copy exists here
to fall out of sync, and one account's email can never be paired with
another's tokens.

Ordering is load-bearing: retire the identity *before* the canonical
credential write. Every failure then converges (nothing moved, or the
provider re-derives the identity it already had). The reverse order has a
failure mode that does not self-heal.

Codex needs none of this — `auth.json` carries the account claims inside
the credential, so replacing the file replaces the identity.

Temporary homes follow the same rule from the other side: the copied
`.claude.json` is stripped of `oauthAccount`, or the CLI running there
would consider its identity already settled and never derive the one
belonging to the credential it was seeded with.

## Where a refresh is allowed to happen

Claude's refresh tokens are single-use and its rotation is serialized on a
lockfile scoped to the config home. Two homes holding the same token are two
processes taking different locks: whichever rotates first retires the other's
token, and the loser's home is left holding a credential the server will
never accept again. That is a login the user cannot recover without signing
in.

So **the selected account refreshes in the canonical home, never a copy of
it** (`probeSelectedClaudeRateLimits`). An inactive account has no canonical
copy — its slot is the only holder of its chain — so probing it from a
temporary home forks nothing and stays the right call.

Never point a canonical-home run at `CLAUDE_CONFIG_DIR`, not even at the
default path. Claude keys "is this the default home" off the variable being
*absent*, not off its value, and a non-default home hashes into a different
macOS Keychain service.

Because a rotation legitimately changes the credential bytes, the guard that
the canonical home still belongs to the selected account is the identity the
CLI reports, not a byte comparison.

Rotations must survive every failure path. The inactive-account probe reads
the temporary home back on EVERY exit after the CLI ran (a rotation lands on
disk before the CLI answers initialize), and the app persists a non-selected
rotation to its slot before any selection re-validation can refuse.
Activation re-reads canonical immediately before the final overwrite and,
if it moved since the caller's snapshot, preserves the newer bytes into the
outgoing slot — a mid-switch move is a rotation of a single-use chain far
more often than anything else, and a preserved pre-rotation snapshot is a
bricked login. The one thing never preserved anywhere is the provider's
sign-out husk, recognized through the `SetSignedOutDetector` seam (the app
wires `claude.CredentialsSignedOut`; this package stays provider-agnostic).

The metadata store carries a `providerHome` stamp (`ClaimProviderHome`,
first claim wins). `PruneOrphanedAccounts` is only run when the stamp
matches the home the credentials operate under: a store paired with a
foreign home (a scratch --data-dir against a real $HOME) must degrade to
"never prune", not "prune someone else's logins". Slot destruction is
announced through the app's `auditAccountEvent` — durable at
`<dataDir>/account-audit.log`, not just stderr.

## Layout

- `store.go` — thread-safe metadata and last-known quota persistence.
- `credentials.go` — credential-slot layout, atomic active-credential
  switching, and `CredentialPresent` (can this account be selected at all).
- `identity.go` — retiring the provider-side identity record that lives
  outside the credential file.
- `rollback.go` — capture/restore of one account slot, used to unwind a
  failed login or adoption without deleting a slot it did not create.
- `ephemeral_home.go` — short-lived homes for native login and inactive-account
  probes.
- `claude_keychain.go` — the `claudeKeychain` seam: every `security(1)`
  invocation in the codebase lives here (pinned by
  `TestNoSecurityCallsOutsideTheKeychainSeam`). Holds the production
  backend matching Claude Code's native service naming and the
  file-backed stand-in used by test binaries and the agent harness.
- `credentials_testhelpers.go` — `WriteNativeCredentialForTest`, the one
  blessed way for fixtures to seed the canonical native credential;
  inert outside test binaries.

## Testing

Tests must inject a temporary home directory. Never inspect or mutate the
developer's real provider homes.

A temporary home does NOT isolate the macOS Keychain: the active slot's
service name is fixed regardless of which userHome a `Credentials` was
built with. That is why the Keychain sits behind the `claudeKeychain`
seam (`claude_keychain.go`, which carries the full incident write-up):
`NewCredentials` installs a file-backed stand-in in test binaries
(`testing.Testing()`), the harness opts into the same stand-in via
`NewCredentialsWithFileKeychain`, and only ordinary runs get the
`security(1)` backend. Never add a `security(1)` call outside the seam
— `TestNoSecurityCallsOutsideTheKeychainSeam` enforces this.

The stand-in stores each credential at `<configHome>/.credentials.json`
— the exact non-darwin layout — so darwin tests need no special-casing:
mock provider binaries (subprocesses) that write the credential file are
visible through the seam, and fixtures seed the canonical store via
`WriteNativeCredentialForTest` to stay ignorant of the layout entirely.
