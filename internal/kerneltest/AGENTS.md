# Provider test isolation

This package supplies mandatory isolation for tests that can start provider
processes. Repository policy requires tests to avoid real provider binaries,
homes, credentials, and billed requests.

Any fixture that constructs a session-capable `App`, or adds a provider probe,
catalog, text-generation, or other spawn path, must install `IsolateSpawns` and
wire all returned seams into the subject:

- redirect `HOME` and `USERPROFILE` to an empty temporary home;
- point both provider settings at the failing test binary;
- install `DisabledCodexModelCatalog()`;
- install `StubTextGenerationExecutor()`; and
- retain the unexpected-start cleanup check.

A test that intentionally exercises a session replaces the failing binary with
a mock from `internal/testutil`. Do not weaken the unexpected-start check or
copy this isolation into package-local fixtures.

`internal/app` centralizes the caller-side wiring in
`isolateE2EProviderSpawns`, used by its application fixtures. Its
`providersmoke` build-tag twin is the sole exception: `make provider-smoke` is
an explicit manual gate against authenticated real CLIs and spends model
tokens. No ordinary test or additional build tag may bypass isolation.

This package imports `testing` and must never be imported by production code.
Run `go test ./internal/kerneltest` after changing the isolation contract.
