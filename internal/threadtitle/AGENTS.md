# Thread title text processing

This package builds title prompts, formats bounded thread context, decodes
structured provider output, and sanitizes titles. `internal/threadtitleapp`
owns attachment selection, context reads, singleflight, and compare-and-swap
persistence; `internal/app` owns provider execution.

## Contracts

- Treat `Default` as both the fresh-title sentinel and the fallback for unusable
  model output. Persistence must compare and swap so generation cannot overwrite
  a user rename.
- Apply `Timeout` to each provider attempt independently, including fallback
  attempts.
- `Sanitize` produces one line, removes wrapping quotes, collapses whitespace,
  and enforces `MaxRunes` by rune count.
- Provider schemas remain separate. Enforce display length in `Sanitize`, not as
  a schema restriction that can discard an otherwise usable response.
- Prompt sections and regenerated context stay bounded through `textgen`
  helpers. Preserve role labels on retained fragments.
- Regeneration retains recent context, marks truncation caused either by the
  text budget or by rows omitted by the store, and retains the first user
  message when recent-context selection would otherwise lose it.
- Escape transcript boundary markers in user and assistant content before
  embedding them in prompts.
- Attachment metadata covers every attachment. Only the application-level list
  of files shown to a model is image-specific.
- Provider error redaction belongs to `internal/textgen`.
