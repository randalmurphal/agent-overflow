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
- `StagedFileProvider` is the `updater.Provider` adapter over one local file.
- `LinuxUpdaterBlocked` is the native-Linux preflight (`linuxgate.go`): an
  AppImage's read-only squashfs mount and a non-writable install directory
  both refuse the in-place swap, and both are decided BEFORE the updater is
  wired up so the feature reports unsupported instead of failing after a
  tens-of-MB download.

Pure and tag-free: no network, no `exec`, no Wails application package. That is
what lets the `nogui` WSL backend and the GUI Windows launcher both import it.

## What does NOT belong here

- **`internal/appupdate`** owns the backend lifecycle: GitHub release listing,
  by-tag targeting, checksum-sidecar verification, download/install state, WSL
  staging and directive emission. `internal/app` retains the stable `App` RPC
  facade plus desktop/WSL boot adapters and the Linux preflight call; root
  supplies executable-only boot inputs.
- **The launcher (`cmd/agent-overflow-windows`)** owns receiving the directive,
  deciding when to act on it, and the `app.Updater` lifecycle around
  `StagedFileProvider`.
- Anything requiring a running provider process, a store, or the transport.

## Trust notes

- `InstallDirective.Filename` is a **bare** name by construction. `Validate`
  rejects separators, `..`, and anything that is not a plain `.exe`. The
  launcher must resolve it under its own staging dir and never accept a path.
- `StageCopy` verifies the streamed bytes before the rename, so the destination
  never holds unverified content. The launcher still re-verifies via the
  Updater's streaming hash: the file crosses a filesystem the backend does not
  own between the two steps.
