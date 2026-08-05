# internal/editor/

Detection and spawn for the open-in-editor binding. Owns the WSL
bridge logic that maps a Linux-side path to the Windows-installed
editor reachable via the vendor's WSL Remote integration.

## Layout

- `detect.go` — catalog of supported editors (VS Code family, Cursor,
  Windsurf, Sublime, Zed) plus the `$EDITOR` / `$VISUAL` synthetic
  fallback. `DetectEditors(ctx)` walks PATH first and falls back to
  /mnt/c discovery on WSL.
- `wsl.go` — WSL detection (cached read of
  `/proc/sys/kernel/osrelease`), the well-known /mnt/c install paths
  per editor, the shim-script content sniff that decides whether a
  PATH-resolved binary actually targets a Windows install, and the
  Linux-path → `\\wsl.localhost\<distro>\...` UNC translator used by
  `explorer.exe` integrations.
- `preference.go` — `Resolve(detected, preferredID)` maps the user's
  settings preference onto the detection result, falling back through
  the catalog priority order and ultimately the env fallback.
- `spawn.go` + `spawn_unix.go` / `spawn_windows.go` — argv assembly
  per launch style, OS-specific `SysProcAttr` so the child outlives
  the parent, the child environment (inherited, minus the AppImage
  launch artifacts — `appimage.ScrubInherited`, which returns nil on
  every other launch shape so `exec.Cmd` inherits directly; an editor
  outlives us, so a mount-local `PATH`/`LD_LIBRARY_PATH` would break
  it the moment Agent Overflow exits), and `ResolvePath` — the
  path-shape contract `Open`
  enforces (absolute-canonical pass-through when no workspace is
  supplied; absolute-with-workspace must be inside the workspace;
  relative-against-workspace joining with traversal-escape and
  symlink-escape rejection).

## Responsibility boundary

- What BELONGS here:
  - Editor discovery (PATH walks, /mnt/c probes, env-var lookups).
  - The WSL bridge rule: a Linux-native install does NOT count as an
    available editor when running inside WSL, even if it is on PATH.
  - argv assembly for each launch style and the spawn primitives.
  - The path-shape contract enforced before spawn: `ResolvePath` is
    the LAN-bind safety floor — relative-input resolution against an
    absolute, canonical `workspacePath` plus the traversal-escape
    guard. Frontend callers supply the workspace; this package owns
    the validation.
- What does NOT belong here:
  - Settings persistence — `internal/settings` owns that.
  - Frontend toasts / error rendering — `app_editor.go` returns the
    error and the frontend decides how to surface it.
  - File-content opening business logic — the package doesn't read
    or transform the file contents, it just hands the path off.

## Detection contract

`DetectEditors` returns the full catalog with `Available` populated.
On WSL, an entry is `Available = true` only when one of these is true:

1. PATH-resolved shim that ultimately exec's a `/mnt/c/...` Windows
   binary (Microsoft's default `code` script is the canonical case);
2. A direct hit under `/mnt/c/Users/<user>/AppData/Local/Programs/...`;
3. A direct hit under the system-wide `/mnt/c/Program Files/...` path.

A PATH-resolved Linux-native install (apt-installed `code-oss`, the
flatpak `cursor`, etc.) is deliberately NOT marked available on WSL.
Per the WSL editor-bridge feedback memory, those would render via
WSLg and miss the user's actual editor environment; falling back to
them silently is worse than reporting "no editor available" so the
user sees the Remote-WSL setup hint.

### Shim validation

Every Microsoft-family `code` shim (VS Code, Code Insiders, Cursor,
Windsurf, VSCodium) hardcodes `VERSIONFOLDER="..."` and dispatches
through `<install>/<VERSIONFOLDER>/resources/app/out/cli.js`. An
incomplete or stale uninstall can leave the `bin/code` script in
place while removing the cli.js — the shim then exits non-zero on
`Cannot find module .../cli.js`, but the shim's own
`--locate-extension` invocation suppresses stderr to `/dev/null`, so
the spawn looks successful while no editor window appears.

`validateWindowsCodeShim` (in `wsl.go`) reads each candidate shim and
stats the cli.js it points at. Broken candidates are skipped:

- `detectOne`: a PATH-resolved /mnt/c shim that fails validation falls
  through to `findWindowsInstall` instead of being accepted.
- `findWindowsInstall`: each user/system candidate is validated before
  being returned. The walk continues past broken installs.

Shims without a `VERSIONFOLDER="..."` line (Sublime, Zed, custom
`$EDITOR` targets) are passed through unchanged — there's no cheap
content-based check for those, and the spawn step is the right place
to learn whether they work. The fast-exit observer below catches their
runtime failures.

### Fast-exit observer

After `cmd.Start()` succeeds, `Open` waits up to `fastExitWindow`
(750ms) for the child to exit. Three branches:

- Exit 0 inside the window → success (e.g. VS Code's CLI handing off
  to a running window).
- Non-zero exit inside the window → error returned with the editor
  name and exit code. This is what surfaces `Cannot find module` and
  similar shim failures that slipped past validation.
- Still running at the timeout → success. The watcher goroutine
  continues, reaping the eventual exit cleanly.

The observer is the indirection seam `observeFastExit`; tests that
fake `startCmd` also fake the observer to avoid waiting on a child
process that was never actually started.

## Extension points

- To add a new editor: append it to `editorCatalog` in `detect.go`,
  add its WSL install paths to `wslInstallTable` in `wsl.go` (only
  needed if it ships a Windows install we need to find), and add a
  preference test exercising the new ID.
- To add a new launch style: add a `LaunchStyle*` constant, a
  `buildArgs` branch, and a `spawn_test.go` case.

## Testing

Tests use injectable `lookPath` / `readFile` / `readDir` / `stat` /
`envValue` hooks so the suite runs identically on macOS, Linux, and
Windows. Real spawning is mocked through `startCmd`. The WSL branch
is fully covered by fixtures — no real WSL host required to verify
the bridge logic, the shim-content sniff, or the install-path walk.

## Global state (intentional)

`internal/AGENTS.md` forbids global mutable state by default; this
package keeps three globals deliberately. Each is documented here so
the carve-out is traceable.

- `detectionCache` (`detect.go`) — bounded by `detectionCacheTTL`
  (60s). Backs `DetectEditors` so the App-level methods that call
  it (`OpenInEditor`, `ListAvailableEditors`) don't re-walk PATH +
  `/mnt/c` per click. On WSL each detection run crosses 9P at
  ~10-30ms per probe; clicking through 20 path links would otherwise
  re-walk synchronously per click. Mutated via `storeDetectionCache`
  / `RefreshEditors` under `sync.Mutex`. Test seam: `PeekDetectionCache`.
- WSL detection lives in `internal/platform` and is exposed here via
  `IsWSL`. The test-friendly `isWSLEnv(env)` still bypasses the live
  cache when fed an injected env so each test can use its own `/proc`
  fixture without poisoning process state.
- `lookPath` / `startCmd` (`spawn.go`) — exec.LookPath / Cmd.Start
  indirection seams. Tests substitute fakes to record invocations
  without spawning real processes. Production never overrides
  these; they are package-level vars so test code can rebind them
  for a single test under `t.Cleanup`.

If you add a new editor, do not add additional globals. The three
above are the package's full carve-out — extend the catalog and the
WSL install table instead.

## Anti-patterns

- Do NOT call `os.LookPath` or `os.Stat` directly from package code
  — go through the `detectEnv` indirection so tests can swap in
  fixtures. Production assembles the live env via `liveDetectEnv`.
- Do NOT hard-code file paths to user binaries inside the package
  body. The /mnt/c table in `wsl.go` is the canonical list; new
  editors get added there or skipped on WSL.
- Do NOT shell out via `cmd.exe` / `bash -c`. The spawn path passes
  the binary path and arguments directly so quoting around spaces in
  paths can't bite us.
