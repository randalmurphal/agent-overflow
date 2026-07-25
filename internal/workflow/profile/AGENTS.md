# internal/workflow/profile/

Pure project-profile loading, validation, binding lookup, and explicit secret
resolution for the workflow system.

## Boundaries

- Keep engine, provider, process execution, persistence, transport, and keychain
  access out of this package.
- Loading never resolves secrets. Callers opt into `ResolveSecrets` immediately
  before use and register every returned mask before persisting untrusted text.
- Secret values must never enter errors, logs, or string renderings.
- Worktree setup is authoring data only here; execution belongs to the
  app-owned runner before its first phase turn, never the engine goroutine.
- Binding names are `[a-z0-9-]+`. `capacities` is the one exception: it also
  accepts `provider:<name>`, the reserved namespace for the implicit
  per-provider resource every agent phase acquires. No other colon-bearing
  capacity name validates, so the namespace cannot be squatted.

## Files

| File | Responsibility |
|---|---|
| `types.go` | Profile authoring, binding, and finding types. |
| `parse.go` | Size-bounded strict YAML parsing and absent-file defaults. |
| `validate.go` | Collected structural and semantic validation. |
| `secrets.go` | Explicit env/file resolution and mask collection. |

Tests use `t.TempDir` and `t.Setenv`; they never inspect shared application
configuration.
