# Shared test utilities

This package contains stateless fixture helpers shared by multiple test
packages. Test-specific setup stays beside the test that owns it.

- `app.go` writes mock Claude NDJSON and Codex JSON-RPC executables. Tests use
  them with `internal/kerneltest` isolation to exercise provider pipelines
  without real CLIs.
- `git.go` creates local repositories and local bare origins. No helper here
  reaches a network.
- `store.go` inserts the project row required by thread foreign keys.
- `logcapture.go` captures logs for assertions.

Keep behavior and assertions in the package under test. Helpers only stage
fixtures and return their handles or paths. Do not add `*App` construction here;
application fixtures live under `internal/app` where private seams are
available.

`CanonicalPath` intentionally duplicates `internal/git.CanonicalPath` because
`internal/git` tests import this package. Importing `internal/git` here creates
an import cycle.

Every helper accepts `testing.TB` or `*testing.T` and uses per-test temporary
state. Run `go test ./internal/testutil` after changing shared fixtures.
