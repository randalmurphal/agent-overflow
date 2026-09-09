# Provider accounts

This package stores account metadata and activates provider-native credentials.
`provider-accounts.json` never contains credentials, tokens, authorization
codes, or OAuth URLs.

Credential bytes are opaque. Read only bounded regular files without symlinks,
write through `atomicfile.Write` with mode 0600, and validate account IDs
before using them as path components. Saved slots contain only the provider's
native credential file. Normal provider processes always use the canonical
native home.

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

Credential write APIs reject signed-out husks. Preserve a newer canonical
rotation into the outgoing slot during activation failures. Rollback removes
new structure but never restores older credential bytes.

Never set `CLAUDE_CONFIG_DIR` for a canonical-home run, even to its default.
Reserve and clear `CLAUDE_SECURESTORAGE_CONFIG_DIR` on every corresponding
spawn. All macOS `security(1)` calls remain in `claude_keychain.go`.

Pruning requires a matching `providerHome` ownership stamp. Tests and harness
runs use temporary homes and the file-backed keychain seam; they never inspect
or modify real provider homes.
