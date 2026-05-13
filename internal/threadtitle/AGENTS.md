# internal/threadtitle/

Pure prompt builder, decoder, and sanitisers behind the
auto-generated thread title flow.

The App-coupled glue — workspace resolution, settings routing, image
attachment plumbing, and the compare-and-swap into
`store.UpdateTitleIfCurrent` — stays in `app_thread_title.go`. This
package only knows how to assemble the prompt, parse the structured
output, and trim the model's response into a well-formed title.

## Surface

| Symbol | Purpose |
|---|---|
| `Default` | The sentinel a fresh thread starts with ("New Thread") and what `Sanitize` falls back to when the model returns nothing usable. Callers compare-and-swap with this value so a manually-renamed thread doesn't get clobbered. |
| `Timeout` | 3-minute budget for a CLI invocation. Matches t3-code; image-attached prompts can take longer than the bare-text path. |
| `MaxRunes` | 50-rune cap (with 3-rune ellipsis when truncated). Keeps the sidebar entry on one line on the narrowest supported window width. |
| `CodexSchemaJSON` | The `--output-schema` JSON the Codex CLI gets; requires the 50-char title. |
| `ClaudeSchemaJSON` | The `--json-schema` JSON the Claude CLI gets. Separate constant so the two can diverge if escaping rules require it. |
| `BuildPrompt(message, attachments) string` | Assembles the natural-language instruction. Mirrors t3-code's Prompts.ts. Section budgeting goes through `textgen.LimitPromptSection`. |
| `FormatAttachments(attachments) string` | Renders one bullet per attachment with filename, MIME type, and byte size, for the prompt's metadata section. |
| `DecodeClaude(stdout) (string, error)` | Wraps `textgen.DecodeClaudeStructuredLastLine` with the title envelope shape. |
| `Sanitize(raw) string` | Single-line, quote-stripped, internal-whitespace-collapsed, 50-rune ellipsis-capped. Returns `Default` when the model returns nothing usable so the compare-and-swap is a no-op. |
| `RedactError(err) error-string` | Replaces "CLI failed" subprocess errors with a stable opaque string for log emission. Non-CLI errors pass through. |

## Design notes

- Imports only `agent-overflow/internal/textgen` (for the generic
  structured-output and prompt-budgeting helpers) and
  `agent-overflow/internal/store` (for the `Attachment` shape). No
  provider, no App.
- `BuildPrompt` and `FormatAttachments` are intentionally identical
  to the t3-code prompt text — keeping both apps producing comparable
  output means a tester can verify titles across implementations
  without controlling for prompt drift.
