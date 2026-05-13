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
| `Timeout` | 180s budget for a CLI invocation. Callers wrap their `context.WithTimeout` and pass the same value to `textgen.RunCodex` / `textgen.RunClaude`. |
| `PromptStagedSummaryLimit`, `PromptStagedPatchLimit` | Prompt-layer caps (6k / 40k) applied to the staged summary and patch sections. Mirrors t3-code's Prompts.ts. |
| `CodexSchemaJSON` | The `--output-schema` JSON the Codex CLI gets. Validated by `commitmsg_test.go` so a typo can't reach prod. |
| `ClaudeSchemaJSON` | The `--json-schema` JSON the Claude CLI gets. Distinct constant so the two can diverge if escaping rules require it. |
| `BuildPrompt(summary, patch, branch) string` | Assembles the natural-language instruction. Matches t3-code's `Prompts.ts` line-for-line so both apps produce the same output shape for identical input. Section budgeting goes through `textgen.LimitPromptSection`. |
| `DecodeClaude(stdout) (subject, body, err)` | Wraps `textgen.DecodeClaudeStructuredLastLine` with the commit envelope shape and the subject-required validation. |
| `SanitizeSubject(raw) string` | Single-line, quote-stripped, period-trimmed, 72-rune ellipsis-capped. Returns `""` when the model returns nothing usable. |
| `SanitizeBody(raw) string` | Trims whitespace, collapses 3+ newline runs to 2. No length cap. |

## Design notes

- Imports only `agent-overflow/internal/textgen` (for the generic
  structured-output helpers) and stdlib. No store, no provider, no
  App.
- `BuildPrompt` is intentionally identical to t3-code's prompt
  text — keeping the two apps interchangeable from the user's
  perspective means a tester can compare outputs without controlling
  for prompt drift.
- The wire shape exposed to the frontend (`GeneratedCommitMessage`)
  stays in `app_commit_message.go` alongside the bound method so the
  Wails binding generator picks it up.
