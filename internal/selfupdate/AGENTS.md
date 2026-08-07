# internal/selfupdate/

The cross-process contract for the Windows/WSL self-update split, imported by
**both** sides: the headless WSL backend (`-tags nogui`, repo root `main`) and
the Windows launcher (`cmd/agent-overflow-windows`).

The split exists because the process that can download is not the process that
can swap. The WSL backend runs the full Wails updater state machine
(check / download / verify against GitHub releases) and stages
`agent-overflow-wsl-amd64.exe` into the launcher's
`%APPDATA%\agent-overflow\update` through `/mnt/c`. It cannot replace the
running launcher exe, so it emits an `InstallDirective` on `ChannelInstall`; the
launcher re-verifies the staged file's digest and drives its own `app.Updater`
with a `StagedFileProvider`, reusing the stock verify / swap-helper / relaunch
machinery instead of reimplementing it.

## What belongs here

- The wire shape and its validation (`InstallDirective`, `Validate`, `Digest`).
- The staging-directory primitives both sides use: `StageCopy` (verified,
  atomic), `SweepStagingDir`, `StagingDirName`.
- The `Marker` the backend writes before the install and reads on the next boot
  to detect a swap that never applied.
- `StagedFileProvider` — the `updater.Provider` adapter over one local file.

Pure and tag-free: no network, no `exec`, no Wails application package. The one
non-stdlib import is `wails/v3/pkg/updater` (stdlib-only itself) for the
`Provider` / `Release` types, plus `internal/atomicfile` for the marker.

## What does NOT belong here

- **`app_updater*.go` (repo root)** owns the GitHub-facing side: release
  listing, by-tag targeting, the checksum-sidecar lookup, the `verifiedProvider`
  fail-closed wrapper, the RPC surface, and emitting the directive.
- **The launcher (`cmd/agent-overflow-windows`)** owns receiving the directive,
  deciding when to act on it, and the `app.Updater` lifecycle around
  `StagedFileProvider`.
- Anything requiring a running provider process, a store, or the transport.

## Trust notes

- `InstallDirective.Filename` is a **bare** name by construction — `Validate`
  rejects separators, `..`, and anything that is not a plain `.exe`. The
  launcher must resolve it under its own staging dir and never accept a path.
- `StageCopy` verifies the streamed bytes before the rename, so the destination
  never holds unverified content. The launcher still re-verifies via the
  Updater's streaming hash: the file crosses a filesystem the backend does not
  own between the two steps.
