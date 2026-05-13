# internal/pathlinks/

Extracts file-path references from agent prose and validates them
against a workspace filesystem. Output feeds the chat surface's
auto-linkifier as an allowlist the frontend can trust — replacing the
old client-side regex that produced false positives for any
`prefix/word.word` shape.

## Layout

- `pathlinks.go` — `ExtractAndValidate(workspacePath, text) []PathRef`
  plus the regex / heuristic / stat pipeline.
- `pathlinks_test.go` — table tests covering the full TS-side test
  matrix (URLs, scoped npm, emails, version strings, parens, quotes,
  backticks, leading `./` and `../`) plus new fs-existence and dedup
  cases.

## Responsibility boundary

- What BELONGS here:
  - Regex extraction of path-shaped tokens from arbitrary text.
  - Heuristic rejection of obvious non-paths (URLs, scoped packages,
    emails, version strings, trailing-dot tokens, single-segment
    bare names).
  - `os.Stat` validation, with workspace-relative joining for
    non-absolute paths.
  - Per-occurrence `PathRef` output so the frontend can wrap every
    instance — with a single stat per unique path.
- What does NOT belong here:
  - DOM manipulation. The frontend wraps text nodes; this package
    only emits the allowlist.
  - Open-in-editor semantics. `internal/editor` owns spawn /
    workspace-boundary validation; this package is concerned with
    "does the file exist," not "can it be opened."
  - Markdown parsing. Triage routes raw text into this package
    unchanged; the regex tolerates arbitrary prose.

## Invariants

- One stat per unique path per call. A message that mentions
  `src/foo.ts` ten times produces ten `PathRef` entries but exactly
  one syscall. Regression coverage:
  `TestExtractAndValidate/repeated_mentions_*`.
- Boundary rule: the character immediately before a match must be a
  safe boundary (`[\s(\[{,;'"`<>=]`) or input-start. If the match
  begins with `@`, the boundary check applies to the char before the
  `@`. This is what rejects `email@host/path.ts` while accepting
  `@src/foo.ts` after whitespace.
- `@`-prefix is presentation only. `PathRef.Path` always carries the
  validated file path *without* the `@`. The frontend's find-and-wrap
  re-detects the `@` in surrounding text and widens the visual span;
  the click handler operates on the real path.
- Workspace-boundary check is the safety floor. Both `..`-traversal
  out of the workspace and absolute paths outside it are rejected at
  validation time — agent prose is untrusted, and without this guard
  `os.Stat` would expose an existence oracle for arbitrary host
  paths. The check mirrors `internal/editor.ResolvePath` so the
  click handler and the validator agree.
- Empty / non-canonical / non-absolute `workspacePath` drops every
  candidate. Without a usable root the boundary check can't run, so
  refusing is the only safe behavior.
- Candidate count is capped (`maxCandidates`) to bound worst-case
  syscalls under a hostile message body.

## Performance

A throwaway spike (validated outside the repo before the refactor)
measured:

- Typical 2–5 paths in 1.5KB message: ~60–100µs total.
- Heavy 25 paths in 8KB: ~500µs.
- Pathological 200 paths in 50KB: ~3ms.

Regex dominates; stat is microseconds warm. Cold-cache scenarios
(WSL2 + Windows mount, NFS) can multiply stat by 5–10×, but stays
sub-frame at message-complete boundary.

## Testing

- Table-driven `Test*` functions using `t.TempDir()` workspaces.
- `extractAndValidate(workspacePath, text, statFunc)` is the
  unexported test seam — pass a counting stat to assert call shape
  (one stat per unique path) or to enforce the candidate cap.

## References

- Frontend allowlist consumer: `frontend/src/lib/utils/markdownEnhance.ts`.
- Triage integration point:
  `internal/triage/stream_state.go` `settleStreamingText`.
- Click-time boundary check mirrored from
  `internal/editor.ResolvePath`.
