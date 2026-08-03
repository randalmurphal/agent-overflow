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

## The retired `worktree_setup` block

The worktree setup recipe — files copied from the main checkout and argv
commands run at worktree creation — no longer lives in this package. It is
per-project app settings now (`internal/worktreesetup`, persisted on the
project row, edited in Settings → Projects), so chat-thread worktrees and
workflow worktrees run the same one.

The `worktree_setup` field survives on `Profile` for exactly one reason:
decoding is strict (`KnownFields(true)`), so deleting it would turn every
profile.yaml that still carries the block into an unexplained unknown-field
error. `Validate` reports its presence as the `worktree-setup.moved` finding
naming the new home; the contents are not inspected, because nothing executes
them. `Load` is fatal on findings, so an unmigrated profile refuses to load
rather than silently running no setup.

Authoring format, copy-safety rules, and the `AO_PROJECT_ROOT` /
`AO_WORKTREE_PATH` environment contract are documented in
[`internal/worktreesetup/AGENTS.md`](../../worktreesetup/AGENTS.md).

## Files

| File | Responsibility |
|---|---|
| `types.go` | Profile authoring, binding, and finding types. |
| `parse.go` | Size-bounded strict YAML parsing and absent-file defaults. |
| `validate.go` | Collected structural and semantic validation. |
| `secrets.go` | Explicit env/file resolution, child-process env rendering, and masking. |

Tests use `t.TempDir` and `t.Setenv`; they never inspect shared application
configuration.
