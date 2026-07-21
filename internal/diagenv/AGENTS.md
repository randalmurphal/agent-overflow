# internal/diagenv/

Names the opt-in diagnostic environment variables and the passthrough
list the WSL-boundary launchers forward via WSLENV. Stdlib-only; exists
so `cmd/agent-overflow-windows` and the dev supervisor can reference the
full set without importing the packages that implement each diagnostic.

## Layout

- `diagenv.go` — the variable-name constants (`Pprof`, `RendererDiag`)
  and `Passthrough()`, the list both launcher hops feed into
  `wsllauncher.AppendWSLENV`.

## Responsibility boundary

- What BELONGS here: the canonical name of any diagnostic env var that
  must cross the Windows→WSL boundary, plus its one-paragraph effect
  doc.
- What does NOT belong here: the diagnostics themselves. `Pprof` is
  implemented by `internal/observability/pprofserve`; `RendererDiag` by
  the transport server's `CrossOriginIsolate` config (wired in
  `main.go`).

## Extension points

- Adding a diagnostic env var: add the constant with an effect comment,
  add it to `Passthrough()`, and implement the behavior in the owning
  package. Both launcher hops (dev supervisor `childEnv`,
  `wsllauncher.LaunchOptions.PassthroughEnv`) consume `Passthrough()`,
  so no launcher edit is needed.

## Anti-patterns

- Do NOT put behavior here. This package is names only — a diagnostic
  that needs code lives with the subsystem it inspects.
- Do NOT add a variable to `Passthrough()` that isn't a diagnostic
  opt-in. Feature configuration goes through settings, not env vars.
