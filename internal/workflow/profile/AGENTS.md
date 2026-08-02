# internal/workflow/profile/

Pure project-profile loading, validation, binding lookup, and explicit secret
resolution for the workflow system.

## Boundaries

- Keep engine, provider, process execution, persistence, transport, and keychain
  access out of this package.
- Loading never resolves secrets. Callers opt into `ResolveSecrets` immediately
  before use and register every returned mask before persisting untrusted text.
- Secret values must never enter errors, logs, or string renderings.
- `ResolvedSecrets` owns both directions of the process boundary: `Environ`
  renders `NAME=value` pairs for a child process (upper-cased, `-` → `_`, which
  is injective over the validated `[a-z0-9-]+` grammar, so two secrets can never
  collide on one variable), and `Mask` replaces every non-empty resolved value
  in text captured back from that process. Anything that hands secrets to a
  subprocess uses both — never one without the other.
- Worktree setup is authoring data only here; execution belongs to the
  app-owned runner before its first phase turn, never the engine goroutine.
- Binding names are `[a-z0-9-]+`. `capacities` is the one exception: it also
  accepts `provider:<name>`, the reserved namespace for the implicit
  per-provider resource every agent phase acquires. No other colon-bearing
  capacity name validates, so the namespace cannot be squatted.
- `max_fan_out_width` is a **ceiling, not a capacity** (D29): a capacity paces
  work that all still runs, this refuses a fan-out attempt outright. It is a
  `*int` so "absent" is distinguishable from an authored zero — absent resolves
  to `def.DefaultMaxFanOutWidth` through the single `def.EffectiveMaxFanOutWidth`,
  and an authored `0` or negative is a finding rather than an ignored line.
  There is deliberately **no value meaning unlimited**: a project that wants a
  wider bound writes the wider number. `Default()` leaves it unset for the same
  reason — one implementation of "unset means the default", not two that could
  drift.

## The `worktree_setup.run` environment

Each `run` argv executes with its working directory set to the new worktree and
the app's own environment (PATH and the user's toolchain intact) plus two
variables. They are appended last, so an inherited variable of either name
cannot shadow the real one. `workflowSetupEnv` (`app_workflow_setup.go`) is the
sole writer; the reader is the authored recipe, so this table is the contract.

| Variable | Value |
|---|---|
| `AO_PROJECT_ROOT` | absolute path of the project's main checkout — the tree `copy:` globs read from |
| `AO_WORKTREE_PATH` | absolute path of the worktree being set up — also the command's working directory |

They exist because a recipe can name neither checkout on its own: the worktree
path is generated per item, and the project root is not the working directory.
Without them the only expressible way to bring `.env` across is a `copy:` glob,
which snapshots the file and then silently diverges from the main checkout. A
`run` entry is an argv, not a shell line, so a recipe that wants expansion asks
for a shell explicitly:

```yaml
worktree_setup:
  run:
    - [sh, -c, 'ln -s "$AO_PROJECT_ROOT/.env" "$AO_WORKTREE_PATH/.env"']
```

`AO_PROJECT_ROOT` is deliberately not the same kind of value as the session
contract's `AO_PROJECT` (`internal/aocli/AGENTS.md`), which is a project
**slug**. A setup command runs no CLI and holds no session credential; these
two paths are the whole of its AO_* surface.

## Files

| File | Responsibility |
|---|---|
| `types.go` | Profile authoring, binding, and finding types. |
| `parse.go` | Size-bounded strict YAML parsing and absent-file defaults. |
| `validate.go` | Collected structural and semantic validation. |
| `secrets.go` | Explicit env/file resolution, child-process env rendering, and masking. |

Tests use `t.TempDir` and `t.Setenv`; they never inspect shared application
configuration.
