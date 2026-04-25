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

## Anti-patterns

- Do NOT import `internal/provider`. The allowed-reasoning-efforts
  map here is duplicated from `provider.AllReasoningEfforts`
  intentionally to avoid a dependency cycle — update both sides
  together.
- Do NOT sneak business logic into `Validate`. It enforces shape, not
  behavior.
- Do NOT write partial settings silently. If validation fails, the
  save is an error and the caller decides the user-facing message.

## References

- The frontend reads / writes settings through Wails bindings; the
  generator picks up `Settings` automatically.
