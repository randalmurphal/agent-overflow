# internal/settings/

User preferences persisted as a single JSON file with sparse
serialization (zero-value fields are omitted so defaults are
forward-compatible).

## Layout

- `settings.go` — `Settings` struct, `Load` / `Save`, the sparse
  JSON marshal/unmarshal, schema versioning (`CurrentSchemaVersion`).
- `validate.go` — enum allow-lists (timestamp format, provider,
  reasoning effort, text-generation provider) plus the
  `ValidateRemoteEndpointURL` / `ValidateRemoteEndpointToken` helpers
  used by both the App-level remote-endpoint mutators and the
  `--connect` URL parser. Single `Validate` entry point for the
  Settings struct as a whole. The generic list/string normalizers live
  here too — `dedupeTrimmed` and `truncateRuneSafe` — because each has
  callers in more than one feature file; `truncateRuneSafe` bounds a
  length without splitting a rune AT THE CUT and is deliberately not a
  UTF-8 repair pass, so an already-invalid stored value keeps its bytes
  instead of collapsing to empty.
- `providerenv.go` — the `ProviderEnvVar` shape (user-defined
  environment for a provider's subprocesses), its name/value rules,
  the reserved-name deny-list, the load-time sanitizer, the
  `RedactProviderEnvVars` wire projection, and the
  `SetProviderEnvVar` / `DeleteProviderEnvVar` Service mutators.
- `promptoverrides.go` — the `PromptOverride` shape (per-provider
  system-prompt replacement scoped to a model list) plus the disabled-tool
  lists, their bounds, the strict validators and the lenient load-time
  sanitizers, and the `PromptOverridesForProvider` /
  `DisabledToolsForProvider` selectors. Both selectors route `claude-tui`
  onto the Claude lists, exactly like `HiddenModelsForProvider`: it is the
  same binary, and the interactive TUI honors `--system-prompt-file` and
  `--disallowedTools` the same way headless does (spike-verified 2.1.234;
  `internal/provider/claudetui/launch.go` passes both).
  Neither tool list is enum-checked
  here: they speak two different vocabularies (Claude raw tool names,
  Codex curated toggle ids) and validating either against a table this
  package cannot see would make a settings file that outlives one AO
  version unloadable. Unknown Codex ids are skipped with a log line in
  `internal/provider/codex`; an unknown Claude name is inert to the CLI.
  The prompt lists carry TWO byte bounds, and the aggregate one is the
  load-bearing half: `GetSettings` ships both lists whole on every read,
  including to a LAN client, so `MaxPromptOverrideLen` alone would put
  50 × 64 KB per provider on the wire. `MaxPromptOverridesTotalLen` caps
  the sum — the strict path refuses an over-cap list, the lenient path
  keeps whole entries until the sum would exceed it and logs what it
  dropped. Every cap this file applies to a hand-authored list is
  audible: a tail that vanishes on load is otherwise indistinguishable
  from a save that never happened.
- `remote.go` — the `RemoteEndpoint` shape and its CRUD helpers
  (`Add` / `Update` / `Delete` / `Touch`). Backs the `--connect`
  target list the desktop binary's settings panel exposes.

## Responsibility boundary

- What BELONGS here:
  - Reading / writing the user's JSON settings file.
  - Validating enum fields against the allowed sets.
  - Default values (implicit via Go zero values).
- What does NOT belong here:
  - Settings UI — that's the frontend.
  - Provider-specific runtime config — that's derived from `Settings`
    plus `store.ThreadView` at session creation.

## Extension points

- To add a new setting: add the field + a default + (if enum) an
  allow-list + a `Validate` branch + a test that asserts round-trip.
  A field whose intended default is the Go zero value stays OUT of
  `DefaultSettings` — that is what makes an absent key read as the
  default for every settings file written before the field existed.
  `ClaudeTUIEnabled` is the deliberate example (opt-in claude-tui
  visibility, 2026-08-18): `ClaudeEnabled` / `CodexEnabled` beside it
  default true, so the inversion is documented at the field and pinned
  by `TestClaudeTUIEnabledDefaultsOffAndRoundTrips`. Do not "fix" it by
  adding it to `DefaultSettings` — `writeSparse` persists what differs
  from the defaults, so that would drop the user's `true` on write.
- To change allowed values for an existing enum: update the map in
  `validate.go` and the migration note; old values are normalized on
  load, never at write time.

## Retired fields

A setting that MOVED somewhere else — not one that was deleted — is
retired rather than kept: it comes out of the `Settings` struct and goes
into `retiredSettingsFieldNames()`. The name in that set does two things.
`captureUnknownFields` skips it, so the sparse writer does NOT round-trip
it, and `Validate` never sees it because the struct has no field for it.

The consequence is the part to get right: a retired value is **consumed
once and then gone**. Unmarshalling drops it, nothing republishes it, and
the next `Update` — any update, from anywhere — rewrites the file without
it. It is not "left on disk".

`Service.RetiredString(field)` is the one legitimate reader: a raw read of
the FILE (not the typed cache, which cannot hold a field the type no
longer has) that answers `""` for every failure. It exists for the
one-time migration that moves the old value to wherever it now lives.
Two rules follow, and both are load-bearing:

- The migration must run on the BOOT path, before any `Update` can reach
  the file. `app_startup.go`'s `initThemeDirectory` is the live example
  (`theme` → `<configDir>/themes/appearance.json`,
  `docs/specs/theme-system.md` §6.2), and `app_theme_test.go` pins the
  ordering in both directions.
- A migration that can FAIL must carry the value in process state, because
  the drop happens whether or not the migration succeeded.
  `theme.Service` keeps it in `bootPending` / `pendingLegacy` and retries
  from the next read.

## Secrets on the wire

Two fields hold material that must not cross the transport boundary in
bulk: `RemoteEndpoints[*].Token` and the values of custom environment
variables flagged `sensitive`. `GetSettings` is reachable from a
LAN-attached client, so `redactedSettings` (app_settings.go) clears both
on every read path.

That makes the generic patch path unsafe for those fields — a
`GetSettings -> mutate -> Update` round trip would write the redaction
back. `Service.Update` therefore REJECTS `remoteEndpoints`,
`claudeCustomEnv`, and `codexCustomEnv`; each has dedicated mutators
that read the persisted value before writing. Any future field that
gets redacted on read must follow the same pattern in the same commit.

## Anti-patterns

- Do NOT import `internal/provider`. Two tables here are duplicated
  from that package on purpose, to avoid a dependency cycle:
  the allowed-reasoning-efforts map (from
  `provider.AllReasoningEfforts`) and the custom-environment
  deny-list (from `provider.ReservedEnvNames`). Update both sides
  together; the deny-list has a root-package test
  (`TestReservedEnvNamesMatchTheProviderPins`) that fails on drift in
  either direction.
- Do NOT assume nothing under `internal/` imports this package. One does:
  `internal/promptoverride` imports it for the `PromptOverride` shape —
  the only inbound edge from another `internal/` package, and the reason
  this package's types have to stay dependency-free. It is the pure half
  of the same feature (matching an entry to a session model, rendering
  its placeholders) and lives there because it needs
  `internal/provider`'s slug normalizer, which is exactly the import
  banned above. Keep the direction one-way: nothing here may reach back
  into `promptoverride`, or the cycle returns through the front door.
- Do NOT sneak business logic into `Validate`. It enforces shape, not
  behavior.
- Do NOT write partial settings silently. If validation fails, the
  save is an error and the caller decides the user-facing message.

## References

- The frontend reads / writes settings through Wails bindings; the
  generator picks up `Settings` automatically.
