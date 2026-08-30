# internal/dirbrowse/

Project-picker directory listing. Backs the `BrowseDirectory` binding;
treats "path missing" and "path is a file" as empty-listing UI states
rather than errors so the AddProject modal can browse on every
keystroke without flooding the server log.

## Layout

- `dirbrowse.go`: `Browse(path)` plus `Listing` / `Entry` wire shapes
  and the `EntryLimit` truncation cap. Symlinks are stat'd through to
  their target so a symlink-to-dir shows up as `IsDir=true`; `IsRepo`
  is populated for directories that contain a `.git` (dir OR file,
  the worktree pointer case).

## Responsibility boundary

- What BELONGS here: path normalisation (`""`/`"~"`/`"~/sub"`
  expansion + `filepath.Abs` + `filepath.Clean`), stat / read-dir +
  sort, the missing-path empty-listing fallback, the EntryLimit cap.
- What does NOT belong here: caching (every keystroke restats, since the
  modal's UX depends on always seeing the current FS), project-row
  bookkeeping (see `internal/project`), or any per-thread state.

## Anti-patterns

- Do NOT bubble `ErrNotExist` / "is a file" as an error. The modal
  calls Browse on every keystroke; a 500 per partial path makes
  typeahead unusable and floods the log.
- Do NOT lstat directly. Symlinks should report their target's type
  so the picker can descend into them. The deliberate
  best-effort fallback to lstat on broken symlinks (IsDir=false) is
  documented inline.
- Do NOT change the `Listing` / `Entry` JSON tags or field set without
  a coordinated frontend change. `frontend/src/lib/types/models.ts`
  carries a hand-maintained mirror.
