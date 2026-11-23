# Provider boundary

This package owns coding-agent process lifecycle, provider-neutral events,
session options, runtime modes, approval coordination, and child environment
construction. Provider-native frames stay in `claude`, `claudetui`, or
`codex`. Persistence, UI emission, and cross-thread policy stay above this
package.

## Process and probe rules

`BuildEnvironment`, `FilterEnvironment`, and version detection must apply
`appimage.Scrub`. Build additive `PATH` values from the scrubbed base.
The names a user's custom environment may not set live in one list, the
settings deny-list (`deniedProviderEnvNames`, `internal/settings/providerenv.go`),
and settings enforces it by refusing a write that names one. A spawn path that
starts pinning or clearing a name adds it to that list in the same change.

`Spawn` takes a required `Lifetime`: `Watched` for a process that runs until
its owner closes it, `Bounded` for one that finishes within its context's
deadline. Both hand the group to the orphan reaper; the zero value, a nil
reaper and a bounded launch without a deadline start nothing. A launch config
that reaches `Spawn` carries a `Reaper`; tests pass `procutiltest.Reaper()`, or
`providertest.Lifetime()` to `Spawn` itself. Which launch has which lifetime is
in [process lifetime](../../docs/architecture/process-lifetime.md).

Probe working directories are required and absolute. Keep all dimensions of
`ProbeCacheKey`: binary, account, working directory, and the digest of the
configured environment. Never place credential values in cache keys.

## Runtime and model contracts

`AllRuntimeModes` defines legal modes. Unknown values must not silently widen,
restrict, or enable billed review. Provider mappings must cover every mode and
every transition. Claude restarts only for spawn-only changes; Codex applies
turn-scoped overrides. `claude-tui` does not enforce runtime mode, so callers
that require enforcement must reject it.

Keep model IDs separate from context tiers. Normalize Claude's trailing
`[1m]` marker for catalog lookup. `NormalizeModelSlug` folds spellings of
one ID and never resolves an alias. Model inputs pass `ValidateModelID`; a
Claude alias the CLI reports is read only through its reported model list
(`claude.ModelAliases`). Argv builders use
`ModelDeclaresNoReasoningEffort`; persisted effort columns cannot represent
the absence of an effort flag.

A model family (`claude-opus`, `gpt-sol`) is a model named without a version;
`modelfamily.go` owns it. An input that accepts a family is parsed once into a
`ModelRef` and resolved only by `ResolveModel`, which states the precedence.
Each provider states its version rule in `modelGrammarFor`, an exhaustive
switch: a provider with no versioned families says so and its inputs are
exact-only. Which inputs accept a family is in
[decisions](../../docs/decisions.md).

`SessionOptions` carries provider-independent thread settings. Provider
packages translate them into native `Config` values. Settings-owned disabled
tools remain provider-specific and spawn-only; see
[prompt-tool-overrides.md](../../docs/specs/prompt-tool-overrides.md).

## Interactive requests

`ApprovalRegistry` is the single-use ledger for outstanding provider
requests. It does not emit or write to a provider while holding its mutex.
Structured answer collection uses `UserInputRequest`, not an approval.
Any new request kind needs a frontend consumer in the same change.

Read [providers.md](../../docs/architecture/providers.md) and the appropriate
[Claude wire](../../docs/references/claude-wire.md) or
[Codex wire](../../docs/references/codex-wire.md) reference before changing
protocol behavior. When the references are silent, follow
[spike-policy.md](../../docs/references/spike-policy.md).
