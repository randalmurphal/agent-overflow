# Provider boundary

This package owns coding-agent process lifecycle, provider-neutral events,
session options, runtime modes, approval coordination, and child environment
construction. Provider-native frames stay in `claude`, `claudetui`, or
`codex`. Persistence, UI emission, and cross-thread policy stay above this
package.

## Process and probe rules

`BuildEnvironment`, `FilterEnvironment`, and version detection must apply
`appimage.Scrub`. Build additive `PATH` values from the scrubbed base.
`ReservedEnvNames` is the source of truth for values AO pins or clears; keep
the settings copy and its drift test synchronized.

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
`[1m]` marker for catalog lookup. Argv builders use
`ModelDeclaresNoReasoningEffort`; persisted effort columns cannot represent
the absence of an effort flag.

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
