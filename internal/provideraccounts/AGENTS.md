# Provider accounts

This package stores account metadata and activates provider-native credentials.
`provider-accounts.json` never contains credentials, tokens, authorization
codes, or OAuth URLs.

Credential bytes are opaque. Read only bounded regular files without symlinks,
write through `atomicfile.Write` with mode 0600, and validate account IDs
before using them as path components. Saved slots contain only the provider's
native credential file. Normal provider processes always use the canonical
native home.

`Account` is the frontend-facing card. Backend-only per-account state belongs
beside the rows on `ProviderState`, not on `Account`, or every client gets a
copy of it with every card. `ProviderState.ClaudeCatalogs` holds the last Claude
probe's model answer per account id that way, through `RememberClaudeCatalog`
and `ClaudeCatalog`.

`Remove` is the only path that shrinks `ProviderState.Accounts`, so it is the
one place a keyed sibling map is forgotten. A new one deletes its entry there
too.

`Account.UsageResets` is the reset list the card offers, written only by
`SetUsageResets`, `BeginResetSpend` and `SettleResetSpend`; every other row
write carries it over, and an inserted row starts without one. An account's
unsettled spend, with its key, lives beside the rows in
`ProviderState.PendingResets`; `PendingReset` states what ends one.

A saved catalog is bound to the binary identity that produced it. It is
metadata, not a promise: a consumer serves it only while the configured path
and the resolved file, size and mtime still match.

## Claude identity and rotation

Claude credentials and `oauthAccount` are separate. AO may retire
`oauthAccount` before replacing the canonical credential, but never writes a
new identity. The CLI repopulates it asynchronously, so the record can
describe a different login than the credential beside it, and AO never
matches a credential on it or on the probe answer read from it. A Claude
credential's identity is the OAuth profile answer for its own access token
(`claude.ProbeProfile`); without that answer it is not attributed. An empty
identity immediately after a switch is unknown, not evidence of sign-out.

Identity matching is `(email, organization)` and belongs in
`identity_match.go`. Organization names are descriptive only. Same-email
accounts require distinct known organization IDs; saved identity may enrich
from unknown but never rebind.

Claude refresh tokens are single-use. The selected account refreshes only in
the canonical home, where all processes share the provider's lock. Probes near
rotation wait for the credential change before teardown. Inactive Claude
accounts use the read-only HTTP usage probe and are never refreshed in copies.

Every write to the canonical Claude credential, removal included, holds all
three of the CLI's locks in its order (`claude_refresh_lock.go`): the two
refresh locks, then `.storage-write.lock`, which the CLI takes innermost for
every secureStorage mutation, including a sign-in or a sign-out that never
refreshes. Each lock keeps the CLI's own stale threshold. A write that lands
inside a CLI refresh makes that CLI adopt the disk value and drop the rotation
it just performed. `ErrCredentialLockBusy` is the answer when the CLI still
holds them after the wait. A write that replaces the canonical credential
only while it holds a given token (`replaceActiveCredentialHolding`) checks
and writes under those locks in one operation. Codex has no such lock: a
`codex login` or a running Codex can write its canonical credential between
any check and write, and nothing here excludes it.

`Account.RefreshTokenExpiresAt` is metadata, not a credential: when the
login stops working (`claude.LoginExpiresAt`), the later of the refresh
deadline the CLI records at sign-in and the renewal of the last access token
issued before it. Record it wherever credential bytes are captured;
`NoteRefreshTokenExpiry` is the only writer. Past it the account is signed out
in effect, so activation, probes and usage refreshes decline it and ask for a
sign-in rather than spawning a CLI whose refusal would blank the credential.

`List` returns accounts in the order the user keeps them. `Add` appends without
selecting and `Reorder` is the only call that moves a row; selection never
reorders.

Credential write APIs reject signed-out husks. Preserve a newer canonical
rotation into the outgoing slot during activation failures. Rollback removes
new structure but never restores older credential bytes.

Never set `CLAUDE_CONFIG_DIR` for a canonical-home run, even to its default.
Reserve and clear `CLAUDE_SECURESTORAGE_CONFIG_DIR` on every corresponding
spawn. All macOS `security(1)` calls remain in `claude_keychain.go`.

A Keychain write too long for `security -i`'s 4096-byte line goes through
argv, as Claude Code's own write does. The item's ACL trusts only
`/usr/bin/security`, so a Security-framework write from AO would prompt on
every switch. Moving the login to the plaintext file instead would expose it
longer to the same readers.

Every ephemeral home, Claude or Codex, is registered with a lease the
owning process holds before a credential can exist in it
(`ephemeral_registry.go`). The boot sweep acts only on homes whose lease it
can take.

Each registry entry records the fingerprint of the refresh token its home
was seeded with (`TokenFingerprint`, provider-owned: the provider rewrites
its credential file for other reasons and keeps the token), and of the one
the owner's slot held then (`slotToken`), selected or not. `settleRotation` (`rotation.go`)
is the one writer of a Codex rotation, for a live usage probe as it
finishes and for the boot sweep after a crash. It places a rotation by
descent, never by whose login anything says it is: every store that holds
the seed's token takes it (the canonical credential only from the store
that selected the owner, and every saved slot), and the owner's slot takes
it while it still holds what it held at seeding. Each slot write first
records the seed's token as consumed beside the slot's lock
(`consumed.go`), and no slot write ever lands a credential holding it
again: a switch's preserve, an adoption, a delayed commit. A recovery
record that cannot be read stops every write to its slot.

A rotation nothing took is deleted only on evidence that nothing needs it:
a store already holds it, or the owner's slot holds neither recorded token.
Otherwise the home and its entry are kept (`ErrRotationKept`), reported
once, and settled again at the next boot. A missing or unreadable slot is
no evidence. The canonical credential and the owner's slot are settled
first; another slot that cannot be inspected stops neither, and keeps the
home and its entry, reported at every settle until it can be judged,
whether or not a store took the rotation (`RotationLanding.Unchecked`). Entries v0.0.19 wrote carry no `slotToken`, so one whose
rotation cannot be placed stays until its account is removed
(`ForgetAccountEphemerals`, called by account removal).

While the canonical store still holds the consumed token, the owner
promotes the slot (`PromoteSlotOverCanonical`): at boot, before
reconciliation, before an account probe, before a selected usage probe and
before a dictation token refresh. Coherence of the shared canonical
credential across stores otherwise belongs to external-login
reconciliation.

Every slot write and removal holds the slot's lock (`lockSlot`); a writer
that decides from the slot's contents holds it from the read to the write.
A writer holding bytes it read earlier replaces only what it read, and
never moves the slot backward on its chain (`ReplaceAccountCredential`).
Rollback removes only what it wrote.

Credential bytes travel as `CredentialSnapshot` or `secret.Bytes`, which
print as `<redacted>` from any field. A raw `[]byte` credential is a
function argument, never a struct field.

Every data root on a machine shares the provider home's slots, so a slot
that a root's account list does not name is usually another root's live
login. A root deletes a slot without a user's removal only when it reserved
that id and does not list it (`reserved_slots.go`): `ReserveAccountID` is the
only source of a new account id and persists it, with the data root and the
provider home of its slot, before any slot byte exists; inserting the row
clears it. Only that data root settles a reservation, only under that home,
and a slot removal is synced before its record is cleared. The
`providerHome` stamp is read only by earlier releases. Two data roots
listing one account id (a copied `provider-accounts.json`) are unsupported:
removal in either deletes the slot. Tests and harness runs use temporary
homes and the file-backed keychain seam; they never inspect or modify real
provider homes.
