# `internal/workspacepath`

Pure lexical validation for user-supplied workspace-relative paths.
`NormalizeRelative` trims and cleans with platform-native `filepath`
semantics, then rejects empty, absolute, dot, and parent-escaping results.

The package does not read the filesystem, resolve symlinks, establish
existence, or check workspace ownership. Callers perform those operations after
joining the accepted relative path beneath their verified workspace root.
Provider-supplied absolute-to-relative normalization belongs to
`internal/triage/tool_paths.go` and has different semantics.

Keep errors suitable for direct user display and do not log rejected input.
