# internal/textgen/

Short structured-output text-generation runs through a provider CLI. The
package owns the CLI shell-out, scratch-file scaffolding, output cap, and
JSON post-processing so callers only assemble the per-task argv, schema,
and decoder.

Backs (budgets are PER ATTEMPT — `runTextGenWithFallback` hands each
provider attempt its own):
- `app_commit_message.go` → `GenerateCommitMessage` (180s, structured
  `{subject, body}`).
- `app_thread_title.go` → `generatedThreadTitle` (3-min, structured
  `{title}` with optional image attachments) and `RegenerateThreadTitle`
  (same budget, no images).

The App-coupled config resolver (`app_text_generation.go`'s
`resolveTextGenerationConfig`) is the single boundary that depends on
`*App`; everything in this package is provider-agnostic plumbing.

## Layout

- `textgen.go` — `Config`, `CLISpec`, `CLIResult`, `CLIExecutor`, the
  default `ExecCLI` shell-out, `CreateScratchFiles`, `ReadOutputFile`,
  `TranslateCLINotFound`, `RedactError`, `FirstNonEmptyMessage`,
  `RunCodex`, `RunClaude`, and the post-processing helpers
  (`DecodeClaudeStructuredLastLine`, `NormalizeStructuredOutputLine`,
  `CapRunesWithEllipsis`, `LimitPromptSection`).

## Three rules worth knowing before you touch them

- **`TranslateCLINotFound`'s timeout keeps its sentinel.** It returns a
  typed error whose `Unwrap` is `context.DeadlineExceeded`, not a
  formatted string, because `runTextGenWithFallback` decides whether to
  try the alternate provider from `errors.Is` — a timeout must retry,
  only `context.Canceled` (shutdown) must not. Wrapping with `%w`
  instead would append "(context deadline exceeded)" to a message that
  already says which CLI timed out and after how long.
- **`RedactError` is the one redaction rule, and it lives here** because
  this package builds the `"codex CLI failed: <stderr>"` strings out of
  a subprocess's own output, which can carry the prompt, the workspace,
  or the environment. A CLI failure collapses to `provider CLI failed`;
  everything else — the timeout included — passes through, since its
  message repeats nothing the subprocess wrote.
- **`LimitPromptSection` cuts on a rune boundary**
  (`stringsx.ClipRunes`). The budget is bytes, but a torn UTF-8 sequence
  handed to a model is not a rounding error, and every prompt builder
  budgets its data sections through this function.

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
