# internal/workspacefiles/

Finds files inside a workspace for @-mention completion. Git-backed
workspaces use `git ls-files` so the user's `.gitignore` is honoured;
non-git workspaces fall back to a filesystem walk with a tight
`IgnoredDirs` whitelist. A short-TTL cache keeps the popover
responsive.

## Layout

- `search.go` — `Searcher` type with the TTL-cached `workspaceIndex`,
  the git / filesystem-walk strategies, and the scoring that produces
  the @-picker result list. `gitCommand` is overridable in tests so
  unit tests never shell out.

## Responsibility boundary

- What BELONGS here:
  - Listing / scoring files in a workspace for completion.
  - Per-workspace TTL cache invalidation.
  - Respecting `.gitignore` (via `git ls-files`) and the hard-coded
    `IgnoredDirs` whitelist outside git.
- What does NOT belong here:
  - File content search (grep) — that's a future feature with its own
    package.
  - Rendering the @-picker — the frontend owns presentation.

## Extension points

- To tune the @-picker defaults: adjust `DefaultTTL`,
  `DefaultMaxEntries`, `DefaultResultLimit` in `search.go`.
- To ignore additional directory names outside git: extend
  `IgnoredDirs`. Keep the list short — inside a repo we defer to
  `.gitignore`.
- To add a new result field: extend `WorkspaceFile`, update the
  frontend binding.

## Anti-patterns

- Do NOT traverse into `.git` or `node_modules` outside git workspaces.
  The whitelist is the gatekeeper; additions need a justification.
- Do NOT bypass the TTL cache. Callers hit `Searcher`, not raw walks.
- Do NOT scan content. This is a path-only index.

## References

- Forge parity target: the defaults match forge's @-picker closely
  enough to avoid surprising users who switch between the two.
