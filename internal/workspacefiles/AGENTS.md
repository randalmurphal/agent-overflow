# `internal/workspacefiles`

Bounded workspace file discovery for the @-mention picker.

Git workspaces use `git ls-files --cached --others --exclude-standard`; other
directories use a filesystem walk. Both remain beneath the resolved root and
apply the small `IgnoredDirs` exclusion set. The fallback does not treat a
repository's `.gitignore` as policy, and hidden files are otherwise eligible.

Use `Searcher` so callers share the per-workspace TTL cache, deterministic
scoring, result cap, and explicit invalidation. Do not scan file contents or
follow directory symlinks to extend coverage. Surface root and enumeration
errors that invalidate the index.

Transport and frontend filtering belong to callers.
