# internal/provideraccounts/

Owns Agent Overflow's multi-account metadata and the filesystem boundary used
to activate provider-native credentials.

## Security boundary

- `provider-accounts.json` contains metadata and last-known quota snapshots only. Never
  add access tokens, refresh tokens, API keys, OAuth URLs, or authorization
  codes.
- Credential profile files live below the provider's own home (`~/.claude` or
  `~/.codex`) and retain the provider's native filename and JSON shape. On
  macOS, Claude's config-home-scoped Keychain entries are the native store and
  are copied directly between Keychain services instead.
- Treat credential bytes as opaque. Reads reject symlinks and non-regular
  files; writes use `atomicfile.Write` (0600 + fsync + rename).
- Shadow login homes symlink shared, non-secret provider state. Credential
  files are the only private entries and must never be symlinks.
- Account IDs are path components. Validate them before joining.

## Layout

- `store.go` — thread-safe metadata and last-known quota persistence.
- `credentials.go` — provider-home layout, isolated login homes, and atomic
  active-credential switching.
- `claude_keychain.go` — config-home-scoped macOS Keychain credential copying
  matching Claude Code's native service naming.

## Testing

Tests must inject a temporary home directory. Never inspect or mutate the
developer's real provider homes.
