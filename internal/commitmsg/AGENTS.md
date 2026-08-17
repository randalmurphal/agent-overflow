# internal/commitmsg/

Pure prompt builder, decoder, and sanitisers behind the
`GenerateCommitMessage` App method.

The App-coupled glue — workspace resolution, settings routing, CLI
spec construction — stays in `app_commit_message.go`. This package
only knows how to assemble the prompt, parse the structured output,
and trim the model's response into a well-formed subject + body.

## Surface

| Symbol | Purpose |
|---|---|
| `Timeout` | 180s budget for ONE CLI invocation. Per-attempt, not shared: `runTextGenWithFallback` hands each provider attempt a fresh one. Callers wrap their `context.WithTimeout` and pass the same value to `textgen.RunCodex` / `textgen.RunClaude`. |
| `PromptStagedSummaryLimit`, `PromptStagedPatchLimit`, `PromptCustomStyleLimit` | Prompt-layer caps (6k / 40k / 2k) applied to the staged summary, patch, and custom-style sections. Mirrors t3-code's Prompts.ts. |
| `StyleConventional` / `StyleCustom` / `StyleRepo`, `StyleGuidance`, `RepoStyleSubjectCount` | Writing-style guidance embedded in the prompt. Kinds mirror `settings.CommitMessageStyle`; an unrecognized kind or an empty payload (blank custom instructions, no repo history) falls back to the Conventional Commits rule so the prompt is never guidance-free. The repo style embeds at most `RepoStyleSubjectCount` (20) recent subjects, gathered by the caller via `git.Core.RecentCommitSubjects`. |
| `CodexSchemaJSON` | The `--output-schema` JSON the Codex CLI gets. Validated by `commitmsg_test.go` so a typo can't reach prod. |
| `ClaudeSchemaJSON` | The `--json-schema` JSON the Claude CLI gets. Distinct constant so the two can diverge if escaping rules require it. |
| `BuildPrompt(summary, patch, branch, style) string` | Assembles the natural-language instruction. Base rules and section shape match t3-code's `Prompts.ts`; the appended style rule mirrors t3-code's source-control writing-style config. Section budgeting goes through `textgen.LimitPromptSection`. |
| `DecodeClaude(stdout) (subject, body, err)` | Wraps `textgen.DecodeClaudeStructuredLastLine` with the commit envelope shape and the subject-required validation. |
| `SanitizeSubject(raw) string` | Single-line, quote-stripped, period-trimmed, 72-rune ellipsis-capped. Returns `""` when the model returns nothing usable. |
| `SanitizeBody(raw) string` | Trims whitespace, collapses 3+ newline runs to 2. No length cap. |

## Design notes

- Imports only `agent-overflow/internal/textgen` (for the generic
  structured-output helpers) and stdlib. No store, no provider, no
  App.
- `BuildPrompt`'s base rules are intentionally identical to t3-code's
  prompt text — keeping the two apps interchangeable from the user's
  perspective means a tester can compare outputs without controlling
  for prompt drift. The style rule is the one deliberate addition, and
  it mirrors t3-code's own writing-style configuration.
- The wire shape exposed to the frontend (`GeneratedCommitMessage`)
  stays in `app_commit_message.go` alongside the bound method so the
  Wails binding generator picks it up.
