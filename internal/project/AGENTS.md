# `internal/project`

Project-row lifecycle helpers bridging Git repository roots and `store.Project`,
plus the project configuration-directory path helper. A project is the Git
repository; a workspace is where an agent operates.

`EnsureForWorkspace` prefers `gitroot.MainRoot`, then the verbatim workspace
path, and creates a row only when neither exists. This makes linked worktrees
share their main repository's project while the thread still retains its own
workspace. Keep root resolution filesystem-only; subprocess-derived repository
identity is added by `internal/projectapp` after creation. Callers own project
events and richer application policy.
