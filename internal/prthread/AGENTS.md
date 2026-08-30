# internal/prthread/

Pure formatting helpers behind `CreateThreadFromPR`: rendering the
sidebar title, building the first user message, picking a code fence
that won't be closed by inner backtick runs, and truncating oversized
titles / diffs on rune boundaries.

The App-coupled glue stays in `app_thread_from_pr.go`: forge CLI
invocation (`gh view`, `glab mr view`), local-clone resolution,
project-row creation, and store reads/writes.

## Surface

| Symbol | Purpose |
|---|---|
| `MaxInlinedDiffBytes` | 256 KB cap on the inlined patch. Beyond this, `TruncateDiff` appends a marker recording how many bytes were dropped. |
| `MaxTitleRunes` | 120-rune cap on the thread title (sidebar truncates anything wider). |
| `ForgeNoun(forgeID) string` | Forge-aware short noun ("PR" / "MR"). Used by error strings and toast copy so GitLab users see "MR" instead of "PR". Unknown ids fall back to "PR". |
| `ForgeNounLong(forgeID) string` | Long-form counterpart ("pull request" / "merge request"). Same fallback rule. |
| `FormatTitle(forgeID, number, prTitle) string` | "PR #N: <title>" for GitHub, "MR !N: <title>" for GitLab. |
| `BuildUserMessage(ref, meta, diff) string` | Composes the first user message: title, link, author, branches, file count, body, and a fenced patch block. Uses `FenceForContent` so inner triple-backtick runs don't close the fence prematurely. |
| `FenceForContent(content) string` | Picks a backtick fence strictly longer than the longest backtick run found in content (minimum 3). |
| `TruncateDiff(diff) string` | Clips at `MaxInlinedDiffBytes` and appends a marker; shorter inputs pass through unchanged. |
| `TruncateTitle(title) string` | Rune-boundary truncation with `...` suffix so multibyte codepoints (CJK, combining marks, emoji) survive intact. Bug C6 regression guard. |

## Design notes

- Imports only `agent-overflow/internal/git` (for `PRReference` and
  `PRMetadata`) and stdlib. No store, no provider, no App.
- The fence-picking rule (strictly longer than the longest run,
  minimum 3) is captured here so the contract is explicit and tested
  directly. A future "PR diff in markdown" renderer should call
  this same helper rather than re-deriving the rule.
- `TruncateTitle` guards Bug C6: byte-based slicing at 117 used to
  split a multibyte rune into an invalid UTF-8 sequence. The
  rune-boundary cut is preserved here with its own regression tests.
