# assetwatch/

Live reload for flat appearance directories and installation-name metadata.

## Boundary

- `watcher.go` owns the shared fsnotify loop, trailing-edge debounce,
  directory re-arm, and self-write suppression ledger.
- `theme.go`, `spinner.go`, and `devicename.go` own the concept-specific filename policies and
  expose distinct watcher types. The shared core remains private.
- Event-channel selection, logging a degraded startup, and App lifecycle
  wiring stay in `internal/app`.

Do not export the generic watcher, mutexes, clock, suppression ledger,
relevance predicates, debounce durations, or fsnotify handles. Tests that pin
those invariants belong in this package so production callers get a narrow
API.

Theme suppression is intentionally a one-second non-destructive window. It
may swallow an external edit racing the app's own write; shortening it makes
routine atomic-write event bursts echo back to the frontend. Preserve that
tradeoff unless behavior is being changed deliberately.

## Verification

Run `go test ./internal/assetwatch -count=1` after changes. Directory-removal
re-arm is platform-sensitive and must remain covered by the live fsnotify
tests.

`DeviceNameWatcher` watches only the installation identity file through the same
directory core, so atomic replacements by a simultaneously running frontend-only
process reach host clients too. Its owner closes it before its event bus. It does
not suppress own writes: that single debounced event also drives peer propagation.
