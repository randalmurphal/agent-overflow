# internal/textgen/

Short structured-output text-generation runs through a provider CLI. The
package owns the CLI shell-out, scratch-file scaffolding, output cap, and
JSON post-processing so callers only assemble the per-task argv, schema,
and decoder.

Backs:
- `app_commit_message.go` → `GenerateCommitMessage` (180s budget, structured
  `{subject, body}`).
- `app_thread_title.go` → `generatedThreadTitle` (3-min budget, structured
  `{title}` with optional image attachments).

The App-coupled config resolver (`app_text_generation.go`'s
`resolveTextGenerationConfig`) is the single boundary that depends on
`*App`; everything in this package is provider-agnostic plumbing.

## Layout

- `textgen.go` — `Config`, `CLISpec`, `CLIResult`, `CLIExecutor`, the
  default `ExecCLI` shell-out, `CreateScratchFiles`, `ReadOutputFile`,
  `TranslateCLINotFound`, `FirstNonEmptyMessage`, `RunCodex`,
  `RunClaude`, and the post-processing helpers
  (`DecodeClaudeStructuredLastLine`, `NormalizeStructuredOutputLine`,
  `CapRunesWithEllipsis`, `LimitPromptSection`).

## Responsibility boundary

- What BELONGS here:
  - Pure provider-CLI invocation + output capture. `ExecCLI` builds the
    child environment through `provider.BuildEnvironment`
    unconditionally, overrides or not — that is the one env rule every
    provider subprocess gets, and it carries the `internal/appimage`
    scrub, so the override-free callers are not the only spawns whose
    CLI resolves its runtime against an AppImage mount.
  - JSON post-processing (last-line extraction, normalize, rune cap).
  - Test-injectable executor seam (`CLIExecutor`).
- What does NOT belong here:
  - App settings, provider binary discovery — those live in the App-
    coupled resolver (`app_text_generation.go`).
  - Prompt construction. Callers build the prompt; this package only
    knows how to run the CLI on whatever stdin they pass.
  - Output caps that conflate stdout/stderr with structured payload —
    `ProcessOutputLimit` bounds the human-readable capture,
    `JSONOutputLimit` bounds the structured-output file. They are
    separate constants and must stay separate.

## Anti-patterns

- Do NOT add per-task helpers (e.g. `RunCommitMessage`) here. Callers
  assemble argv + schema + decoder and call `RunCodex`/`RunClaude`. A
  task-specific helper would force this package to import wire shapes
  that belong elsewhere.
- Do NOT mix executor concerns into the post-processing helpers. The
  decoders are generic over `T any` for a reason — keep them free of
  task-specific schema literals.
- Do NOT swap the default Codex / Claude model defaults without
  coordinating with the test fixtures in `app_commit_message_test.go`
  and `app_thread_title_test.go`.
