# internal/untrustedtext/

The one quoting rule for model- and user-produced text embedded in a
prompt another agent will read. `Field` / `Quote` render a value as a
single ASCII-escaped quoted token (rune-bounded, `<` `>` `&` hidden as
`\u` escapes); `Truncate` is the shared rune-boundary cap. Stdlib-only
and pure.

## Invariants

- **One definition of "quoted as data".** The workflow triage seed and
  the wake composer both compose model-authored fields into prompts; if
  they quoted differently, an injection that survives one surface would
  be invisible to tests of the other. New prompt-composing surfaces use
  this package rather than growing a local escaper.
- **Escaping never changes the value.** The output is a valid Go/JSON
  string literal that unquotes to the original text. Markup bytes are
  hidden from scanning surfaces, not stripped.
- **Truncation is visible.** A cut value ends in `TruncationSuffix`,
  appended outside the quoting so a reader can tell truncation from
  content. A non-positive budget means "no budget", never "empty".
- Invalid UTF-8 is replaced (`�`), not dropped. Bytes that are not
  text still show up as something.

## Anti-patterns

- Do NOT compose a model-written field into a prompt raw because it
  "looks safe" (an id, a status). The rule is one rule precisely so
  callers never classify.
- Do NOT add surface-specific variants (markdown-flavoured, HTML-only).
  One rendering that is safe everywhere beats three that each assume a
  surface.

## References

- `internal/workflow/wake/` is the wake composer (primary consumer).
- `app_workflow_triage.go` is the triage seed composer.
