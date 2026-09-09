# `internal/commitmsg`

Pure prompt construction, structured-output decoding, and commit-message sanitization for `GenerateCommitMessage`. Workspace resolution, settings, provider selection, and CLI execution stay in `internal/app`.

- Keep `Timeout` as a per-provider-attempt budget.
- Apply prompt section limits through `textgen.LimitPromptSection`.
- Keep Codex and Claude schema constants separate; their CLI contracts may diverge.
- An unknown or empty style must still produce conventional-commit guidance.
- `SanitizeSubject` must return one quote-free line capped at 72 runes.

Tests validate both structured schemas and the final prompt and sanitizer behavior.
