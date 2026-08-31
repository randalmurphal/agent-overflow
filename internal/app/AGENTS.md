# internal/app/

This package is the importable application shell. It owns `App`, all Wails-bound
facades and wire DTOs, service composition/lifecycle, explicit cross-owner
transactions, harness adapters, and application integration tests. Domain and
provider behavior still belongs in the narrower `internal/` packages mapped by
the parent guide.

## Wails compatibility

Root `service.go` declares a named `main.App` that embeds `*app.App`. Wails v3
and Go reflection include the promoted method set while retaining the wrapper's
`main.App.<Method>` identity. Do not replace the named wrapper with a type alias,
register `*app.App` directly, or change the explicit transport registration
labels: any of those changes every method ID.

`internal/transport/methodgen` scans this directory but hashes methods under
`main.App`. A bound method addition/removal must update LocalOnly classification
when applicable, regenerate Wails bindings with `-ts`, regenerate methodgen,
and verify all existing `$Call.ByID` values remain stable unless a wire migration
was explicitly approved.

App-owned generated models live in
`frontend/bindings/agent-overflow/internal/app/models.ts`. Never edit generated
bindings by hand.

## Bootstrap boundary

Executable-only inputs cross through `bootstrap.go`: build version, data-dir
override, harness/soak isolation, provider control environment, notification
adapters, updater configuration, backend identity, and window geometry. Keep
these as narrow package functions rather than exported `App` methods—an exported
method is a candidate wire RPC unless explicitly ignored.

The complete mocked-provider isolation set belongs in `ConfigureIsolation`.
Tests must never spawn real Claude/Codex binaries or touch real provider homes;
use the existing guarded fixtures and mock scripts.

## Tests

Application tests stay beside the shell. `main_test.go` changes their working
directory to the repository root because whole-repository AST contracts and
committed fixtures historically use root-relative paths. New tests should still
prefer `t.TempDir()` and explicit paths rather than adding more cwd dependence.

Use package-local tests for private transaction invariants. Put behavior tests
in the narrower owning package whenever the behavior does not require App-level
composition.
