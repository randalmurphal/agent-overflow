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

## Layout

- `store.go` — thread-safe metadata and last-known quota persistence.
- `credentials.go` — credential-slot layout and atomic active-credential
  switching.
- `ephemeral_home.go` — short-lived homes for native login and inactive-account
  probes.
- `claude_keychain.go` — config-home-scoped macOS Keychain credential copying
  matching Claude Code's native service naming.

## Testing

Tests must inject a temporary home directory. Never inspect or mutate the
developer's real provider homes.
