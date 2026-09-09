# `internal/settings`

Typed user settings, validation, defaults, residency, and persistence. The service spans three homes: host settings in sparse JSON, user settings in the reserved `user:default` UI-state scope, and device settings in the calling screen's bucket. Before `AttachTierStore`, all tiers remain file-backed for early boot readers.

Remote-access behavior and residency are specified in [remote access](../../docs/specs/remote-access.md).

## Storage and mutation

- `Service.mutate` is the only persisted-write path. It loads current state, applies one mutation, validates, routes changed keys by tier, writes, updates the cache, releases the lock, then notifies observers.
- A no-op mutation emits nothing. Observer callbacks run outside the lock.
- File and SQLite writes are not one transaction. Return partial-write errors and emit no success notification.
- Sparse JSON omission is part of forward-compatible defaults. Do not serialize resolved defaults into the host file.
- `tier.go` is a total routing table. Every new key needs a host, user, or device tier and completeness coverage.
- Device-class defaults apply only to device-tier keys in this order: global defaults, class defaults, bucket rows. Resolve class defaults at read time; never persist them.
- Moving a key between tiers is a storage migration. Add it to `retieredKeys` and preserve existing destination values.
- `BackendScreen` is only for settings applied by the backend machine's own desktop surface. Ordinary reads use the calling connection's bucket and class.

## Validation

Public writes use strict validation and return actionable errors. Load uses lenient sanitization so one stale field does not prevent startup, but every dropped or repaired authored value must be logged. Keep strict and lenient behavior paired in tests.

Empty values usually mean provider or platform default. Do not replace an unset external-tool option with an Agent Overflow default unless the setting contract explicitly requires one. Provider-specific settings must return zero values for providers that cannot honor the complete behavior.

Provider environment variables are secrets. Validate names and reserved variables at write time, preserve values only in storage and process launch, and redact every wire projection.

Bound individual and aggregate list/string payloads that cross the wire. Reject invalid values on write; load-time sanitizers may discard invalid entries only with a visible log.

## Defaults and generated mirrors

`DefaultSettings` defines compatibility for absent keys. Opt-in features stay absent or false; historically enabled behavior must be represented explicitly when omission would turn it off.

Generated frontend defaults and Go defaults are one contract. Change generator input, generated artifacts, package-manager-driven generation checks, and frontend consumers together. Do not edit generated outputs by hand.

Settings ids that mirror another package's vocabulary remain pinned by bidirectional tests. Avoid importing provider, spinner, or identity packages solely to share a small string enum when that would invert dependencies.

### Frontend defaults

The generated defaults are the only backend-to-frontend default mirror. Keep frontend-local defaults out of the Go settings shape.

### Retired fields

Remove a retired setting from validation, residency, generated defaults, bindings, and every consumer together. Preserve only the compatibility read or migration needed for existing persisted data.

## Feature ownership

- `providerenv.go`: provider subprocess environment and redacted projections.
- `promptoverrides.go`: bounded provider prompt overrides and disabled-tool selections.
- `claudesession.go`, `claudecrosssession.go`: Claude-only session axes and live reconciliation inputs.
- `network.go`: LAN, domain/TLS, tailnet, and configured preview ports. Validate independent halves independently.
- `spinner.go`: custom verbs, animation exclusions, and compaction selection.
- `classdefaults.go`, `residency.go`, `tier.go`: storage routing and per-screen resolution.
- `gendefaults.go`: Go-to-frontend default generation.

Frontend-only preferences stay in frontend storage unless the backend must enforce or consume them. App code owns transport authorization, live-session reconciliation, and user-facing events.
