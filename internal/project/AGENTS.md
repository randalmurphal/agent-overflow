# internal/project/

Project-row lifecycle helpers that bridge git repository roots and the
`store.Project` schema.

## Layout

- `doc.go` — package purpose.
- `project.go` — `EnsureForWorkspace(store, workspacePath)`: looks up or
  creates the project row for a workspace, preferring the repository
  root so sibling checkouts and worktrees share one project. A workspace
  that is not in a repository falls back to the verbatim path.

  The root comes from `gitroot.MainRoot` — git's `--git-common-dir`
  semantics, **not** `--show-toplevel`'s. That difference is the point:
  `--show-toplevel` inside a LINKED WORKTREE answers with the worktree's
  own root, which minted one project per worktree, named after a branch.
  A project is the repository; a workspace is where the provider
  operates (root `AGENTS.md` core principle 7). Only the PROJECT is
  resolved this way — the caller's workspace path is what goes on the
  thread, so a worktree thread keeps working in its worktree.

  It takes no `*gitops.Core`: the resolution is pure filesystem reads,
  so it spawns nothing and is safe to run per row in a listing.

## Responsibility boundary

- What BELONGS here:
  - Project resolution that bridges a repository root (`internal/gitroot`)
    and a `store.Project` row.
  - Project-row creation policy (defaulting the name to
    `filepath.Base(candidatePath)`, choosing repository root over
    verbatim path).
- What does NOT belong here:
  - Bare project CRUD — those live in `internal/store` and surface
    through the `Projects*` bindings.
  - Workspace-change locks, thread CRUD, worktree management.
  - `app.Event.Emit` calls. Callers project the result onto whatever
    wire shape they need.

## Anti-patterns

- Do NOT call back into App from here. The package only knows about
  `store` and `gitroot`. If a caller needs richer behavior
  (e.g. emit on create), the caller composes after `EnsureForWorkspace`
  returns.
- Do NOT introduce package-level state. Stateless helpers only.
- Do NOT reintroduce a `git` subprocess for root resolution. One
  invocation per call is what made this unusable from a listing, and
  `--show-toplevel` answers the wrong question for a worktree.

## References

- `internal/gitroot/AGENTS.md` — how a path resolves to a main
  repository root, and why a deleted worktree needs the registration
  side instead.
- `app_projects.go` — currently a thin wrapper over `EnsureForWorkspace`
  plus the bound `ListProjects` / `CreateProject` / `RenameProject` /
  `DeleteProject` / `MoveProject` methods.
- `internal/store/projects.go` — underlying SQL CRUD.
