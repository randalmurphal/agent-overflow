# internal/settings/

User preferences persisted as a single JSON file with sparse
serialization (zero-value fields are omitted so defaults are
forward-compatible).

## Layout

- `settings.go` — `Settings` struct, `Load` / `Save`, the sparse
  JSON marshal/unmarshal, schema versioning (`CurrentSchemaVersion`).
- `validate.go` — enum allow-lists (theme, timestamp format, provider,
  reasoning effort, text-generation provider) plus the
  `ValidateRemoteEndpointURL` / `ValidateRemoteEndpointToken` helpers
  used by both the App-level remote-endpoint mutators and the
  `--connect` URL parser. Single `Validate` entry point for the
  Settings struct as a whole.
- `providerenv.go` — the `ProviderEnvVar` shape (user-defined
  environment for a provider's subprocesses), its name/value rules,
  the reserved-name deny-list, the load-time sanitizer, the
  `RedactProviderEnvVars` wire projection, and the
  `SetProviderEnvVar` / `DeleteProviderEnvVar` Service mutators.
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
- To change allowed values for an existing enum: update the map in
  `validate.go` and the migration note; old values are normalized on
  load, never at write time.

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
- Do NOT sneak business logic into `Validate`. It enforces shape, not
  behavior.
- Do NOT write partial settings silently. If validation fails, the
  save is an error and the caller decides the user-facing message.

## References

- The frontend reads / writes settings through Wails bindings; the
  generator picks up `Settings` automatically.
