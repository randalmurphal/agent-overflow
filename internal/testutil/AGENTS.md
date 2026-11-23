# Shared test utilities

This package contains stateless fixture helpers shared by multiple test
packages. Test-specific setup stays beside the test that owns it.

- `app.go` writes mock Claude NDJSON and Codex JSON-RPC executables. Tests use
  them with `internal/kerneltest` isolation to exercise provider pipelines
  without real CLIs.
- `mockexec/` installs any test's mock executable as a link to one stable
  wrapper, so macOS assesses a single file instead of each written script,
  and writes an executable whose own bytes a test needs with process starts
  held off ([conventions](../../docs/architecture/conventions.md#tests)). It
  imports only the standard library, `procutil` and `reporoot/`, so every
  test package but procutil's can use it, including the packages `testutil`
  imports.
- `reporoot/` returns the checkout's root and paths under it. Use it to read
  a repository file from a test; a path derived from `runtime.Caller` is
  module-relative under the Make targets' `-trimpath`. A test that inspects
  the tree through a child process such as `go list` calls
  `ObserveGoSources` first, and one whose child reads named files, as make
  reads the Makefile, calls `Observe` with them; the test cache cannot see
  what a child reads and would reuse a pass recorded before the tree
  changed. A test that reads the source tree walks it with `Walk`, which
  leaves out what `.gitignore` ignores, so build output does not rerun it.
  Standard library only.
- `providerpath/` puts poisoned `claude` and `codex` first on PATH from
  TestMain, so a bare provider name never reaches an installed CLI, and fails
  the run naming the caller: the poison holds its caller while TestMain
  captures the goroutines waiting on a child process and the running tests.
  It also points SHELL at a stand-in
  that skips login startup files, so a shell sync cannot reorder PATH. On
  Linux its Guard also fails the run when a process a test started is still
  running five seconds after the tests, found by a variable in its
  environment and named with its pid, working directory and command line;
  a process started with a fresh environment, such as the orphan reaper's
  sidecar, is not seen, and macOS checks nothing. Every
  test package that links a provider spawn package installs it;
  `TestEveryTestBinaryThatCanStartAProviderInstallsThePoison` fails one that
  does not. It imports only the standard library and low-level internal
  packages (`ownedtemp`, `stringsx`, `procutil`) that import no test fixture.
- A test reads a file outside its package through `reporoot/`: `Path`,
  `Files` for one directory, `GoFiles` for the whole Go source tree, and
  `ParseGoFile`, each naming files by their slash path from the root.
  `GoFiles` reads the package roots from the Makefile's `GO_PACKAGE_ROOTS`,
  their one declaration; a test does not list them itself. No
  TestMain changes directory. The test cache keys a pass on the files a test
  opens, resolved against the package directory, so a file opened through a
  path relative to another directory is left out of the key and a cached
  pass outlives a change to it.
- A per-process directory a shared fixture makes in TestMain (the poison,
  the detached home) is an `internal/ownedtemp` directory, swept again
  after the run, so one left by a killed test binary or a helper process
  that ended in `os.Exit` does not outlive the next run.
- A fixture built once and shared across a package's tests (a store
  template, a repository snapshot, a seeded database) is a
  `fixturedir.Lazy`, built on first use in the one directory
  `fixturedir.Guard` makes before the tests run (`storetest.Run` runs it).
  Never make such a directory on first use: its place would follow the
  TMPDIR of whichever test came first, and that test's cleanup would
  remove it.
- `helperproc/` ties a helper process to the test binary that started it
  (`Tie`, `Hold`), so a helper that stands in for a backend ends when that
  binary is killed or times out, and its temporary files land in its test's
  directory. A helper never blocks on `select {}`, and a test sets no
  temporary directory for one itself. Standard library only.
- `deadline.go` is a context whose deadline the test ends at a chosen
  moment, for code that acts on a deadline without racing a real clock.
- `waitbudget.go` is the one budget for a test's wait on work the code under
  test does not bound itself: two minutes, cut short to end 10 s before go
  test's timeout, and a failure once that is gone. `WaitBudget` is for a
  wait the test goroutine runs; `StepBudget` is asked when a step of the
  code under test starts, from any goroutine, for the test's pairing steps.
  A new wait of that kind uses one of them in place of a duration of its own.
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
state. After changing a shared fixture, run
`make go-test GO_TEST_PKGS='./internal/testutil/... <packages that use it>'`.
