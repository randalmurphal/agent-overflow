# internal/kerneltest/

Shared provider test isolation. Helpers that do not need `*App` live here
so fixtures in every package can use the same checks.

## The rule

**Any fixture, in any package, that constructs a session-capable `App`,
or that adds a new spawn path (provider probes, catalogs, textgen-style
side effects), MUST install this package's isolation and register with
the same unexpected-start check.** Mocking is mandatory for every test.
Replace both provider binaries, redirect HOME/USERPROFILE, and install both
side-effect stubs. Omitting any of these leaves the fixture incomplete.

Why it is not negotiable: `make go-test` runs on machines whose
`~/.claude` / `~/.codex` hold live logins. A test that spawns the real
CLI and then kills it can consume a single-use refresh token without
persisting the rotation, destroy the developer's login, and burn billed
tokens. Root `AGENTS.md` §Permanent invariants carries the policy.

## Layout

- `isolate.go`: `IsolateSpawns` (temporary home, failing provider test double,
  and unexpected-start check), its two halves
  `DetachHome` / `PoisonProviderBinary`, the `ProviderBinarySettings`
  patch that configures both Claude and Codex, and the two
  side-effect stubs: `DisabledCodexModelCatalog` and
  `StubTextGenerationExecutor`.

Takes `testing.TB`, not `*testing.T`, so the failure check itself is testable
(`isolate_test.go` drives it through a recording TB and asserts the
failure fires with the recorded argv).

## The two layers

1. **Failing provider test doubles.** `PoisonProviderBinary` points both
   provider binary settings at a script that appends its argv to a sentinel
   and exits 127. A cleanup registered when installing the test double
   (before the caller's session teardown, so
   LIFO runs it after every session is closed) fails the test if the
   sentinel exists, naming the spawn. A test that needs a live session
   installs a mock (`testutil.WriteMockClaudeScript` /
   `WriteMockCodexSession`) in place of that failing script.
2. **Detached home.** `HOME`/`USERPROFILE` point at an empty temp dir, so
   anything that still reaches a real binary (or reads a provider home
   directly) finds no credentials and no session history.

A miss in one layer is caught by the other. Do not install only one.

## What stays with the caller

The seams live on the subject, so wiring is caller-side by necessity:

- Writing `ProviderBinarySettings(testBinary)` into whatever settings service
  the subject resolves binaries from.
- Installing `DisabledCodexModelCatalog()` and
  `StubTextGenerationExecutor()` on the subject's fields.

In package `internal/app`, `isolateE2EProviderSpawns`
(`internal/app/app_e2e_isolation_test.go`) is that glue, and it is what
`setupE2EApp` and `newTestAppWithStore` call.
Its `//go:build providersmoke` twin is a deliberate no-op: that gate exists
to exercise production's DEFAULT binary resolution against the real,
authenticated CLIs, is manual-only (`make provider-smoke`), and is
documented as spending real tokens. Nothing else may have a twin.

## Anti-patterns

- Do NOT weaken a check to make a test pass. Tests that exercise a session
  install a mock provider in place of the failing script.
- Do NOT copy these helpers into a package-local fixture. A copy drifts,
  and the drift is invisible until it costs a login.
- Do NOT add a build tag to this package. The one build-tagged no-op is
  the application-test wrapper, and it exists for the smoke gate only.
- Do NOT import this from production code. It imports `testing`.
