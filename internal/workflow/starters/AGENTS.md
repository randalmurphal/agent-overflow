# Embedded workflow starters

This package embeds starter definitions and their sibling prompt or helper
files for `workflow new`.

- Keep each starter's leading binding inventory synchronized with every check,
  command, secret, capacity, and referenced file it requires.
- Validate the embedded starters as a complete set because starters may call
  one another.
- Renaming through `workflow new --id` changes only self-references. Calls to a
  different embedded starter keep that starter's canonical ID.
- Copy non-prompt sibling assets with the definition. Tests must exercise
  executable helpers, not only compare embedded bytes.
- Preserve the campaign-shaped patterns pinned by `patterns_test.go`; they are
  examples of supported workflow composition as well as templates.
