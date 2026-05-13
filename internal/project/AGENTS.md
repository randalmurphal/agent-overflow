# internal/project/

Project-row lifecycle helpers that bridge git repository roots and the
`store.Project` schema.

## Layout

- `doc.go` — package purpose.
- `project.go` — `EnsureForWorkspace(store, core, workspacePath)`:
  looks up or creates the project row for a workspace, preferring the
  git repository root so sibling checkouts share one project. Nil core
  degrades to verbatim-path lookup/create.

## Responsibility boundary

- What BELONGS here:
  - Project resolution that requires both `*store.Store` and
    `*gitops.Core` together (the bridge).
  - Project-row creation policy (defaulting the name to
    `filepath.Base(candidatePath)`, choosing git root over verbatim
    path).
- What does NOT belong here:
  - Bare project CRUD — those live in `internal/store` and surface
    through the `Projects*` bindings.
  - Workspace-change locks, thread CRUD, worktree management.
  - `app.Event.Emit` calls. Callers project the result onto whatever
    wire shape they need.

## Anti-patterns

- Do NOT call back into App from here. The package only knows about
  `store` and `git` packages. If a caller needs richer behavior
  (e.g. emit on create), the caller composes after `EnsureForWorkspace`
  returns.
- Do NOT introduce package-level state. Stateless helpers only.

## References

- `app_projects.go` — currently a thin wrapper over `EnsureForWorkspace`
  plus the bound `ListProjects` / `CreateProject` / `RenameProject` /
  `DeleteProject` / `MoveProject` methods.
- `internal/store/projects.go` — underlying SQL CRUD.
