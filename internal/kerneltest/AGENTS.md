# internal/kerneltest/

The importable half of the provider-spawn isolation guard. Anything that
does not need `*App` lives here so that a fixture in ANY package can
install the same guard; the root-package `_test.go` file it came from was
unimportable, which meant a test that moved packages silently lost the
net.

## The rule

**Any fixture, in any package, that constructs a session-capable `App` —
or that adds a new spawn path (provider probes, catalogs, textgen-style
side effects) — MUST install this package's isolation and register with
the same tripwire.** Mocking is mandatory-by-default, never opt-in per
test. Poisoning one provider but not the other, detaching HOME but
skipping the poison, or stubbing only the seam you happen to be testing
all count as not installing the guard.

Why it is not negotiable: `make go-test` runs on machines whose
`~/.claude` / `~/.codex` hold live logins. A test that spawns the real
CLI and then kills it (every fixture teardown does) can consume the
single-use refresh token without persisting the rotation, which destroys
the developer's login hours later — and every leaked session burns real,
billed tokens. Incident 2026-08-03: workflow wake delivery spawned 143
real Claude sessions over nine days and killed the active account's OAuth
grant. Root `AGENTS.md` §Permanent invariants carries the full history.

## Layout

- `isolate.go` — `IsolateSpawns` (detached HOME + poisoned binary +
  tripwire, the one call a new fixture wants), its two halves
  `DetachHome` / `PoisonProviderBinary`, the `ProviderBinarySettings`
  patch so no caller poisons Claude and forgets Codex, and the two
  side-effect stubs: `DisabledCodexModelCatalog` and
  `StubTextGenerationExecutor`.

Takes `testing.TB`, not `*testing.T`, so the tripwire itself is testable
(`isolate_test.go` drives it through a recording TB and asserts the
failure fires with the recorded argv).

## The two layers

1. **Poisoned binary.** Both provider binary settings point at a script
   that appends its argv to a sentinel and exits 127. A cleanup
   registered at poison time — before the caller's session teardown, so
   LIFO runs it after every session is closed — fails the test if the
   sentinel exists, naming the spawn. A test that needs a live session
   installs a mock (`testutil.WriteMockClaudeScript` /
   `WriteMockCodexSession`) over the poison.
2. **Detached home.** `HOME`/`USERPROFILE` point at an empty temp dir, so
   anything that still reaches a real binary — or reads a provider home
   directly — finds no credentials and no session history.

A miss in one layer is caught by the other. Do not install only one.

## What stays with the caller

The seams live on the subject, so wiring is caller-side by necessity:

- Writing `ProviderBinarySettings(poison)` into whatever settings service
  the subject resolves binaries from.
- Installing `DisabledCodexModelCatalog()` and
  `StubTextGenerationExecutor()` on the subject's fields.

In package `main`, `isolateE2EProviderSpawns` (`app_e2e_isolation_test.go`)
is that glue, and it is what `setupE2EApp` and `newTestAppWithStore` call.
Its `//go:build providersmoke` twin is a deliberate no-op: that gate exists
to exercise production's DEFAULT binary resolution against the real,
authenticated CLIs, is manual-only (`make provider-smoke`), and is
documented as spending real tokens. Nothing else may have a twin.

## Anti-patterns

- Do NOT weaken a check to make a test pass. Install a mock over the
  poison instead; that is the supported escape hatch.
- Do NOT copy these helpers into a package-local fixture. A copy drifts,
  and the drift is invisible until it costs a login.
- Do NOT add a build tag to this package. The one build-tagged no-op is
  the root-side wrapper, and it exists for the smoke gate only.
- Do NOT import this from production code. It imports `testing`.
