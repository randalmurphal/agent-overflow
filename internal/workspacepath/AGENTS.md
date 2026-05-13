# internal/workspacepath/

Validator for user-supplied workspace-relative paths. The single
exported helper, `NormalizeRelative`, rejects empty / absolute /
parent-escaping inputs and returns the OS-cleaned relative form that
callers can safely join under a workspace root.

## Surface

- `NormalizeRelative(relativePath string) (string, error)` — returns
  the cleaned path on success; errors carry a `workspace path` prefix
  so the calling binding can surface them verbatim.

## Responsibility boundary

- What BELONGS here:
  - Path-string validation that doesn't need to read the filesystem.
  - Sanity checks shared by every binding that writes inside a
    workspace root.
- What does NOT belong here:
  - Filesystem ops (mkdir, write, stat). Those stay with the caller
    so the validator stays pure.
  - Cross-platform symlink resolution. We rely on `filepath.Clean`'s
    OS-native semantics; symlink resolution is the OS / `os.WriteFile`
    boundary's responsibility.
  - Normalising provider-supplied absolute paths back to workspace-
    relative form — that's `internal/triage/tool_paths.go`'s job and
    its semantics differ (it accepts an absolute path + workspace and
    converts; this package validates an already-relative input).

## Anti-patterns

- Do NOT relax the parent-escape rejection. A `..` segment that
  resolves to a path still inside the workspace root looks safe but
  invites bugs on case-insensitive filesystems and symlinked roots;
  reject the input rather than special-case it.
- Do NOT log the input on rejection. Some callers reject paths that
  carry partial user prompts; the validator must not log them.
