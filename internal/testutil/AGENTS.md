# Shared test utilities

This package contains stateless fixture helpers shared by multiple test
packages. Test-specific setup stays beside the test that owns it.

- `app.go` writes mock Claude NDJSON and Codex JSON-RPC executables. Tests use
  them with `internal/kerneltest` isolation to exercise provider pipelines
  without real CLIs.
- `mockexec/` installs any test's mock executable as a link to one stable
  wrapper, so macOS assesses a single file instead of each written script. It
  imports only the standard library, so packages that `testutil` imports can
  use it too.
- `providerpath/` puts poisoned `claude` and `codex` first on PATH from
  TestMain, so a bare provider name never reaches an installed CLI, and fails
  the run naming the caller: the poison holds its caller while TestMain
  captures the goroutines waiting on a child process and the running tests.
  It also points SHELL at a stand-in
  that skips login startup files, so a shell sync cannot reorder PATH. Every
  test package that links a provider spawn package installs it;
  `TestEveryTestBinaryThatCanStartAProviderInstallsThePoison` fails one that
  does not. Standard library only, like `mockexec/`.
- `git.go` creates local repositories and local bare origins. No helper here
  reaches a network, and its git commands do not read the user's global
  config: a fixture that commits sets its own identity.
- `store.go` inserts the project row required by thread foreign keys.
- `logcapture.go` captures logs for assertions; `slowstep.go` parses and
  format-checks `logging.LogSlowStep` lines and lowers their threshold for
  one test, shared because four packages emit the line.

Keep behavior and assertions in the package under test. Helpers only stage
fixtures and return their handles or paths. Do not add `*App` construction here;
application fixtures live under `internal/app` where private seams are
available.

`CanonicalPath` intentionally duplicates `internal/git.CanonicalPath` because
`internal/git` tests import this package. Importing `internal/git` here creates
an import cycle.

Every helper accepts `testing.TB` or `*testing.T` and uses per-test temporary
state. Run `go test ./internal/testutil` after changing shared fixtures.
