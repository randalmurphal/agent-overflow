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
  switching atomically replaces only its credential. New logins and Codex
  inactive-account probes use short-lived 0700 temporary homes; retain only
  the resulting credential and delete all other temporary provider state.
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

### Swapping the canonical credential under live processes is SUPPORTED

Spike-verified 2026-08-18 against claude 2.1.234, three cases, throwaway
config homes and fabricated tokens against a local black-hole token endpoint
enforcing real single-use semantics (`/tmp/spike_claude_rot/multi.py`):

| Case | Setup | Result |
|---|---|---|
| swap to an EXPIRED different pair mid-session | process starts on valid A, disk swapped to expired B, then made to issue a request | it refreshed **B**, never touching A |
| swap to a VALID different pair mid-session | same, B valid | every later request carried **B's** bearer |
| two processes, one expired credential, started 1 ms apart | both want to refresh | **exactly one** token POST, one rotation, no `invalid_grant`, no husk |

So the CLI resolves its credential **from disk per request** — there is no
in-memory copy to go stale — and concurrent processes serialize their
rotation on the config-home lock. A user's terminal `claude`, Claude Code
itself, and AO's own sessions can all share this home while AO swaps the
file underneath them. That is the designed mode, not a tolerated race.

Two consequences worth keeping straight:

- **Do not "fix" this with per-account `CLAUDE_CONFIG_DIR` isolation.** It
  would buy nothing and cost the shared `~/.claude` settings, skills,
  plugins, and `projects/` transcripts every other subsystem reads.
- **A live session adopts the swap immediately**, so switching accounts
  re-bills every already-running session from its next request onward.
  `usage_ledger` has no account column, so that spend is not attributable
  after the fact. That is a product gap, not a correctness bug — but it is
  the real cost of switching mid-session, and it is the one thing here
  worth a decision.

None of this softens the teardown rule: the credential-loss window is the
CLI being KILLED between redeeming a refresh token and writing the
replacement, which is a single-process property and is what
`claude.rotationWatch` holds open for.

The corollary bites elsewhere: **after a retire, "the CLI reports no
identity" carries no information about the credential.** The record is gone
by construction and comes back only from an asynchronous profile fetch, so
the first probe following any switch answers with an empty `account` object
however healthy the login is. Code that reads that emptiness as a verdict on
the tokens will condemn working accounts — and if it also drops the probe's
credential bytes on that branch, it discards a single-use rotation that has
already landed, leaving the slot on a token the server retired (production
2026-08-18; `probeSelectedClaudeRateLimits` and
`providerstatus.ClaudeUnauthenticated` both carry the detail). Judge a
credential by its bytes or by asking the server, never by what the provider
said about itself.

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

Worse, a rotation is not even safe when nothing races it. Anthropic's token
endpoint commits the rotation the moment it processes the request — before
the client sees the response, with no grace window on the retired token — so
a dropped connection or a killed CLI mid-exchange ends the chain with no copy
anywhere (spike-verified 2026-08-16).

Two rules follow, and both are about who ASKED for the refresh:

- **The selected account refreshes in the canonical home, never a copy of it**
  (`probeSelectedClaudeRateLimits`). Its refresh is one the user's own work
  needs anyway, and the canonical home is where the CLI would do it.
- **A probe is never torn down mid-rotation.** The refresh is detached inside
  the CLI and the `initialize` response does not await it, so the probe's
  answer arrives while the rotation is still in flight — and closing stdin is
  what makes the CLI exit. Every Claude probe therefore carries a
  `ReadCredential` (wired once, in `claudeProbeConfig`, enforced by
  `TestClaudeProbeConfigIsTheOnlyProbeConstructor`) and holds teardown until
  an expected rotation lands. See `internal/provider/claude/rotation.go`.

  This covers the probe and nothing else, deliberately. The other short-lived
  Claude spawns (`claude mcp list`, text generation) run to their own natural
  completion rather than being cut off at a wire answer, and they never write
  credentials back — so they carry the same exposure as the user running
  `claude` by hand, which is the bar. The probe was special on both counts: AO
  kills it ~30ms after the answer, and it runs precisely when the token is
  known-expired.
- **Inactive accounts are never refreshed at all.** Usage for a non-selected
  Claude account is read-only: `probeInactiveClaudeRateLimits` reads the saved
  bytes over HTTP and, when they are expired or the server rejects them,
  reports stale usage rather than spending the account's one-shot chain on a
  background poll. Selecting the account is what signs it back in. Codex still
  probes from a temporary home — it has no single-use rotation to lose, and
  its app-server has to run to answer at all.

## Structural husk refusal

The provider's sign-out husk (blank tokens, other fields retained) is not a
credential, and no caller may persist one. The refusal lives in the write
layer rather than in caller discipline: `writeCredentialAt` and
`writeActiveCredential` return `ErrSignedOutCredential`, so every slot write,
canonical activation, and ephemeral seed refuses the same way and a forgotten
guard cannot cost a login. `CaptureAccountCredential` records a husked slot as
having no credential, so a rollback removes the husk instead of rewriting it,
and `CredentialUsable` is the "can this account be selected" question the UI
asks — present AND not signed out.

The husk shape is provider-specific, so it arrives inside the `Policy`
constructor argument every `NewCredentials` / `NewCredentialsWithFileKeychain`
call must supply; the app names the predicates once
(`providerCredentialPolicy`). Requiring them at construction is what makes a
store that silently accepts production-refused bytes impossible to build by
omission — the zero value is the choice made explicit, for focused tests that
exercise something other than these rules. The one deliberate bypass is
`WriteNativeCredentialForTest`, which impersonates the CLI — the actor that
legitimately writes a husk.

## Slots never move backwards — observed, not enforced

A saved slot holds one account for its whole life, so its credential has an
order: `Policy.ChainPosition` reads it (for Claude, the OAuth expiry — fixed
TTL, refreshed only near expiry, so every mint lands strictly further out).
`writeCredentialAt` LOGS a slot write whose bytes sit earlier in that chain
than what is already saved. Earlier is not a stale copy that self-heals; on a
single-use chain it is a token the server has already retired, so that write
is the last diagnosable moment before the account dies.

**It observes; it must not refuse.** Refusing was implemented and reverted.
Slot writes come from paths that have just read the live credential, so once
rollback stopped rewinding bytes (below) a genuine resurrection has no
remaining source — while a wrong verdict is unrecoverable in both directions.
It would drop a real rotation; and since a refusal cannot lower a slot's
position either, one bad value (a credential minted under a skewed clock, a
foreign shape with a different lifetime) would wedge that slot permanently.
Worse, the skip is invisible to callers that re-read the slot afterwards —
`Activate` installs what the slot HOLDS, not the bytes it was handed, so a
skipped write would make the next activation publish the stale credential and
then fail its own identity check, discarding a fresh login. Canonical writes
are not ordered at all: switching accounts legitimately installs an older
expiry.

`rollback.go` is where the class is actually closed. It captures only
STRUCTURE: `RestoreAccountCredential` removes a credential or a slot the
operation introduced and never rewrites bytes, because the operation being
rolled back is frequently what rotated the chain, so putting the captured
bytes back enshrines a retired token. Content has no state to return to.

Never point a canonical-home run at `CLAUDE_CONFIG_DIR`, not even at the
default path. Claude keys "is this the default home" off the variable being
*absent*, not off its value, and a non-default home hashes into a different
macOS Keychain service. Claude ≥2.1.220 additionally honors
`CLAUDE_SECURESTORAGE_CONFIG_DIR`, which overrides `CLAUDE_CONFIG_DIR` for
secure-storage naming alone — an inherited value would make a
temporary-home probe write its rotated single-use token into the canonical
account's Keychain item, so it is reserved and cleared everywhere
`CLAUDE_CONFIG_DIR` is (`provider.ReservedEnvNames`).

Because a rotation legitimately changes the credential bytes, the guard that
the canonical home still belongs to the selected account is the identity the
CLI reports, not a byte comparison.

Rotations that do happen must survive every failure path. The Codex
inactive-account probe reads its temporary home back on EVERY exit after the
CLI ran (a rotation lands on disk before the CLI answers), and the app
persists a non-selected rotation to its slot before any selection
re-validation can refuse. Activation re-reads canonical immediately before
the final overwrite and, if it moved since the caller's snapshot, preserves
the newer bytes into the outgoing slot — a mid-switch move is a rotation of a
single-use chain far more often than anything else, and a preserved
pre-rotation snapshot is a bricked login. The one thing never preserved
anywhere is the provider's sign-out husk (see above).

The metadata store carries a `providerHome` stamp (`ClaimProviderHome`,
first claim wins). `PruneOrphanedAccounts` is only run when the stamp
matches the home the credentials operate under: a store paired with a
foreign home (a scratch --data-dir against a real $HOME) must degrade to
"never prune", not "prune someone else's logins". Slot destruction is
announced through the app's `auditAccountEvent` — durable at
`<dataDir>/account-audit.log`, not just stderr.

## Layout

- `store.go` — thread-safe metadata and last-known quota persistence.
- `credentials.go` — credential-slot layout, the read/write primitives (with
  the sign-out refusal at the one write chokepoint), the required
  `SignedOutDetector` constructor argument, and the two "can this account be
  selected" queries: `CredentialPresent` (the file is there) and
  `CredentialUsable` (…and it is not a sign-out).
- `activation.go` — the mutations that move a credential between the
  canonical native store and a saved slot: `Activate` /
  `ActivateWithSnapshot`, `RemoveActive`, `RemoveAccount`, and
  `PruneOrphanedAccounts`.
- `identity.go` — retiring the provider-side identity record that lives
  outside the credential file.
- `rollback.go` — capture/undo of one account slot's STRUCTURE, used to
  unwind a failed login or adoption without deleting a slot it did not
  create and without rewinding a credential chain.
- `ephemeral_home.go` — short-lived homes for native login and Codex
  inactive-account probes. Claude no longer probes from one: an inactive
  Claude account is read over HTTP without any CLI run at all.
- `ephemeral_registry.go` — the crash net under ephemeral Claude homes.
  Every such home is recorded (one file per entry under
  `<claudeHome>/agent-overflow-ephemerals/`) BEFORE any credential can
  exist in it and unrecorded only by a fully successful cleanup; the
  boot sweep (`SweepEphemeralClaudeCredentials`, wired in
  `app_startup.go` after the orphan-slot prune) cleans up what a crash
  left behind. Adoption — restoring a stranded credential into its
  owning slot — requires an owner account ID on the entry, which only
  the retired inactive-usage-probe path ever recorded; since Claude
  stopped probing from temporary homes, the one remaining producer is
  the login flow, whose homes are ownerless (a crashed login has no
  slot to restore into) and are therefore discarded, not adopted. The
  adoption path stays because pre-existing entries from older builds
  can still surface at boot: it restores bytes only when the slot
  still exists and is dead (missing or husk); a healthy slot is never
  overwritten, a husk orphan is never adopted, entries younger than an
  hour are skipped (another live instance may own them), and the sweep
  refuses any recorded path that is not provably an ephemeral temp
  home.
- `claude_keychain.go` — the `claudeKeychain` seam: every `security(1)`
  invocation in the codebase lives here (pinned by
  `TestNoSecurityCallsOutsideTheKeychainSeam`). Holds the production
  backend matching Claude Code's native service naming and the
  file-backed stand-in used by test binaries and the agent harness.
  The production backend mirrors Claude Code's own
  fallbackStorage(keychain, plaintext), verified against the 2.1.220
  binary: the CLI migrates a login to `<configHome>/.credentials.json`
  and DELETES the Keychain item on any non-timeout Keychain-write
  failure (one locked keychain during an SSH-session refresh is
  enough), so read/present fall back to the file when the item is
  definitively absent (exit 44 — every other `security(1)` failure is
  an error, never absence), write deletes the file only when it
  re-created the item (CC's first-migration rule; an already-present
  item leaves the file for container sharing, CC issue #1414), and
  remove covers both stores. Naming mirrors CC exactly too: username
  sanitized to `claude-code-user` when it fails CC's
  `[a-zA-Z0-9._-]+` rule, service hash input NFC-normalized and not
  path-cleaned.
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

The security backend itself is tested through `installFakeSecurity`
(claude_keychain_test.go): PATH pinned to a directory holding only a
stub `security` script, so the real binary is unreachable. That is the
one sanctioned way to construct `securityClaudeKeychain` in test code —
and it is structurally enforced, not just convention: every method
routes through `securityCommand`, which inside a test binary refuses to
execute a `security` resolved from a system path
(`TestSecurityBackendRefusesSystemSecurityInTestBinaries`). A future
test that forgets the stub fails loudly instead of reaching the
developer's login keychain.
