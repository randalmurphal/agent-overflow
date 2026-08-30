# internal/threadtitle/

Pure prompt builders, thread-context formatter, decoder, and sanitisers
behind the thread title flow, both first-turn generation and the
user-triggered regeneration.

The App-coupled glue (workspace resolution, settings routing, image
attachment plumbing, the store read behind the regeneration context, and
the compare-and-swap into `store.UpdateTitleIfCurrent`) stays in
`app_thread_title.go`. This package only knows how to assemble the
prompt, parse the structured output, and trim the model's response into
a well-formed title.

## Surface

| Symbol | Purpose |
|---|---|
| `Default` | The sentinel a fresh thread starts with ("New Thread") and what `Sanitize` falls back to when the model returns nothing usable. Callers compare-and-swap with this value so a manually-renamed thread doesn't get clobbered. |
| `Timeout` | 3-minute budget for ONE CLI attempt. Per-attempt, not shared: a fallback to the alternate provider starts a fresh one, because a primary that burned the budget timing out would otherwise leave the fallback no time to answer. |
| `MaxRunes` | 50-rune cap (with 3-rune ellipsis when truncated). Keeps the sidebar entry on one line on the narrowest supported window width. |
| `CodexSchemaJSON` | The `--output-schema` JSON the Codex CLI gets. Deliberately carries no `maxLength`: length is enforced by the prompt and `Sanitize`, and a strict-schema rejection of a 51-character draft loses the title entirely. |
| `ClaudeSchemaJSON` | The `--json-schema` JSON the Claude CLI gets. Separate constant so the two can diverge if escaping rules require it. |
| `BuildPrompt(message, attachments) string` | The first-turn instruction: editorial rules, then the user's message, then attachment metadata when the send carried any. Both data sections budgeted through `textgen.LimitPromptSection` (8_000 / 4_000). |
| `BuildRegeneratePrompt(previousTitle, threadContents) string` | The re-title instruction: reading order (user messages first), editorial rules, worked examples, the previous title quoted via `strconv.Quote`, then the formatted thread contents. |
| `FormatAttachments(attachments) string` | Renders one bullet per attachment with filename, MIME type, and byte size, for the prompt's metadata section. |
| `Message` / `FormatThreadContext(messages, rowsDropped) string` | The regeneration context builder (`context.go`): oldest-first messages in, one budgeted `ROLE:\n…` transcript out. `rowsDropped` is the caller's report that the STORE's row window already excluded rows. |
| `DecodeClaude(stdout) (string, error)` | Wraps `textgen.DecodeClaudeStructuredLastLine` with the title envelope shape. |
| `Sanitize(raw) string` | Single-line, quote-stripped, internal-whitespace-collapsed, 50-rune ellipsis-capped. Returns `Default` when the model returns nothing usable so the compare-and-swap is a no-op. |

CLI-error redaction lives in `textgen.RedactError`. That package
authors the `"codex CLI failed: <stderr>"` strings the rule exists for.

## FormatThreadContext

Ported from t3-code's `ProviderCommandReactor.formatThreadTitleContext`,
with three deliberate divergences (marked below). The rules:

- **Newest-first retention** inside an 8_000-character budget. Where a
  thread ended up is what a re-title is about, so the oldest sections
  fall off first; the section that overruns the budget contributes its
  tail when any room is left.
- **The overrun section keeps its `ROLE:\n` header** (divergence). The
  regeneration prompt's step 1 is "Read the USER messages first", which
  an unlabeled tail blob defeats. Too little room for the header plus
  any text drops the section entirely rather than emitting a bare label.
- **Truncation has two sources** (divergence): the character budget, and
  the caller's `rowsDropped`, the STORE's newest-N row window having
  excluded rows. 201 short messages fit the budget whole and are still
  an incomplete thread, and the prompt tells the model to trust the
  truncation marker.
- **The first user message is pinned back on top** (capped at 2_000
  characters on its own) behind an `[Earlier content truncated]` marker,
  but ONLY when the newest-first walk did not already retain it
  (divergence). The original ask is what keeps a long thread's subject
  from drifting to its latest finding; pinning one the walk already kept
  would render the same ask twice. A thread with no user message, or one
  whose first user message survived in place, just gets the marker, and
  the retained text is re-collected inside a budget the marker fits in.
  A marker with nothing behind it is returned as `""` instead: callers
  read that as "no subject to name" and skip the run.

Budgets are byte counts (t3 counts UTF-16 units; exactness is not
load-bearing) but every cut lands on a rune boundary. A torn UTF-8
sequence in the prompt IS load-bearing. The rune-safe cuts themselves
are `stringsx.ClipRunes` / `stringsx.TailRunes`.

## Design notes

- Imports only `agent-overflow/internal/textgen` (for the generic
  structured-output and prompt-budgeting helpers),
  `agent-overflow/internal/store` (for the `Attachment` shape), and
  `agent-overflow/internal/stringsx` (rune-safe cuts). No provider, no
  App.
- The prompt text is adapted from t3-code's `TextGenerationPrompts.ts`,
  with ONE deliberate divergence: t3's tool-use line is replaced by an
  explicit no-tools rule. AO runs the Claude leg under `--safe-mode`
  without `--dangerously-skip-permissions`, so a tool call would be
  denied and only waste turns. t3's "use attached images" rule is also
  absent from the REGENERATION prompt. That path passes attachment
  names, never images.
- The prompt constants are pinned by snapshot tests. They are the
  feature: a silent edit changes every title the app generates.
