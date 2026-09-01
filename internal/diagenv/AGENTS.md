# internal/diagenv/

Names the opt-in diagnostic and isolated-boot environment variables, and
the passthrough list the WSL-boundary launchers forward via WSLENV.
Stdlib-only; exists so `cmd/agent-overflow-windows` and the dev
supervisor can reference the full set without importing the packages that
implement each one.

## Layout

- `diagenv.go`: the variable-name constants (`Pprof`, `RendererDiag`,
  `HarnessRealBrowser`) and `Passthrough()`, the list both launcher hops
  feed into `wsllauncher.AppendWSLENV`.

## Responsibility boundary

- What BELONGS here: the canonical name of any opt-in env var that must
  cross the Windows→WSL boundary, plus its one-paragraph effect doc.
- What does NOT belong here: the behaviors themselves. `Pprof` is
  implemented by `internal/observability/pprofserve`; `RendererDiag` by
  the transport server's `CrossOriginIsolate` config (wired in
  `main.go`); `HarnessRealBrowser` by `realBrowserEngineRequested` in
  `main_harness.go`, which is the only reader.

## Extension points

- Adding a variable: add the constant with an effect comment, add it to
  `Passthrough()`, and implement the behavior in the owning package. Both
  launcher hops (dev supervisor `childEnv`,
  `wsllauncher.LaunchOptions.PassthroughEnv`) consume `Passthrough()`, so
  no launcher edit is needed. The WSL-shell → Windows-launcher hop is a
  separate list: `DEV_WSL_FWD_VARS` in the root `Makefile`.

## Anti-patterns

- Do NOT put behavior here. This package is names only. A diagnostic
  that needs code lives with the subsystem it inspects.
- Do NOT add a variable to `Passthrough()` that configures the PRODUCT.
  Feature configuration goes through settings, not env vars. What belongs
  here is an opt-in that only ever applies to a diagnostic or an isolated
  (`--harness` / `--soak`) boot, which has no user to set a setting.
