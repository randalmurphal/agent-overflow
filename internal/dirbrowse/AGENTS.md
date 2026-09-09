# `internal/dirbrowse`

Filesystem directory listing for the project picker. `Browse` accepts an
arbitrary user-selected path, including absolute, relative, `~`, and home
relative forms. It does not enforce a workspace containment boundary.

Missing paths and paths naming files return `Exists=false` without an error
because the picker calls this API while the user types. Permission, home
resolution, and other I/O failures remain errors. Follow a symlink when
classifying whether an entry is a directory, include hidden entries, sort
directories before files, and cap results with `EntryLimit`.

`Listing` and `Entry` are binding-visible shapes mirrored in
`frontend/src/lib/types/models.ts`. Coordinate field or JSON-tag changes with
that mirror.
