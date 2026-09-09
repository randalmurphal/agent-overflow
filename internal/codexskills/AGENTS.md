# Codex skill cache

This package defines caller-facing Codex skill shapes and coalesces
`skills/list` reads. The application supplies `Fetch`; raw wire parsing
remains in `internal/provider/codex`.

Keys include binary and working directory because bundled and repository skills
vary across them. The account is not a dimension while reads use canonical
`CODEX_HOME`; grow the key if that changes.

Cache and share failures for `DefaultErrorTTL`, return defensive clones, and
do not cache results started before the current generation. Existing waiters
still receive those results. A `skills/changed` notification has no scope, so
it invalidates the entire cache.
