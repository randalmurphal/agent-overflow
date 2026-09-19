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

A saved catalog is bound to the binary identity that produced it. It is
metadata, not a promise: a consumer serves it only while the configured path
and the resolved file, size and mtime still match.

## Claude identity and rotation

Claude credentials and `oauthAccount` are separate. AO may retire
`oauthAccount` before replacing the canonical credential, but never writes a
new identity. The CLI repopulates it. An empty identity immediately after a
switch is unknown, not evidence of sign-out.

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
holds them after the wait.

`Account.RefreshTokenExpiresAt` is metadata, not a credential: the login
deadline the Claude CLI records at sign-in and never extends. Record it
wherever credential bytes are captured; `NoteRefreshTokenExpiry` is the only
writer. Past that deadline the account is signed out in effect, so activation,
probes and usage refreshes decline it and ask for a sign-in rather than
spawning a CLI whose refusal would blank the credential.

Credential write APIs reject signed-out husks. Preserve a newer canonical
rotation into the outgoing slot during activation failures. Rollback removes
new structure but never restores older credential bytes.

Never set `CLAUDE_CONFIG_DIR` for a canonical-home run, even to its default.
Reserve and clear `CLAUDE_SECURESTORAGE_CONFIG_DIR` on every corresponding
spawn. All macOS `security(1)` calls remain in `claude_keychain.go`.

Pruning requires a matching `providerHome` ownership stamp. Tests and harness
runs use temporary homes and the file-backed keychain seam; they never inspect
or modify real provider homes.
