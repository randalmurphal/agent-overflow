# internal/appimage/

The one scrub that removes an AppImage launch's artifacts from the
environment a child process inherits. Stdlib-only, pure, and applied at
every spawn site Agent Overflow owns.

## Why

The type-2 AppImage runtime mounts the app's squashfs (e.g.
`/tmp/.mount_agentXXXXXX`), exports `APPIMAGE` / `APPDIR` / `ARGV0` /
`OWD`, and — via the linuxdeploy AppRun hooks — prepends mount-local
segments to `PATH`, `LD_LIBRARY_PATH`, `XDG_DATA_DIRS`, and points
`GSETTINGS_SCHEMA_DIR` at the mount's schemas. That environment describes
how *this* process was launched. A child is not that process:

- the markers make tooling branch as if it were packaged inside the
  AppImage;
- the mount-local search paths shadow the user's real binaries,
  libraries, `.desktop` entries, and gsettings schemas;
- the mount is unmounted when Agent Overflow exits, so anything that
  outlives us (an editor, a browser) loses the paths it resolved against.

## Layout

- `scrub.go` — the markers, the search-path variable list, `Scrub`,
  `ScrubInherited`, and the `runtime` (marker detection + mount
  matching) they share.

## API

- `Scrub(env []string) []string` — env with the markers dropped and the
  mount's segments stripped out of the search paths. Marker-gated: with
  no markers present the input slice is returned as-is.
- `ScrubInherited() []string` — the scrubbed current process environment,
  or `nil` when nothing would change. The nil is the contract: `cmd.Env =
  appimage.ScrubInherited()` leaves a non-AppImage launch on `exec.Cmd`'s
  own inherit path instead of freezing a snapshot of `os.Environ()`.

Properties every caller may rely on: pure (the input is never mutated),
order-preserving, idempotent (a second pass finds no markers), and inert
for entries that are not `KEY=VALUE` pairs.

## Rules

- **Marker-gated, never heuristic.** Nothing is scrubbed unless
  `APPIMAGE` or `APPDIR` is present with a non-empty value. A path that
  merely *looks* mount-shaped in a dev, `.deb`, macOS, or WSL launch is
  the user's own path.
- **Mount matching is on path boundaries.** `/tmp/.mount_abc-backup/bin`
  and `/opt/tmp/.mount_abc/bin` are not under `/tmp/.mount_abc`.
  `APPDIR=/` disables path stripping entirely rather than emptying every
  search path.
- **An emptied search path is unset, not blanked.** An empty `PATH` has
  implementation-defined lookup behaviour and an empty `LD_LIBRARY_PATH`
  is a pointless extra loader search.
- **Paths are POSIX (`path`, not `filepath`).** The AppImage runtime is
  Linux-only; the package compiles into the Windows launcher binary,
  where it is inert, and must not pick up Windows separator rules there.

## Callers

Every process Agent Overflow spawns should route its child environment
through this package. Today:

| Site | How |
|---|---|
| `provider.BuildEnvironment` / `provider.FilterEnvironment` | `Scrub` on the inherited base — covers `provider.Spawn` (Claude + Codex sessions, probes), `claude.Login`, both MCP-status fetchers, `claudetui`'s full environment, and `textgen.ExecCLI` |
| `provider.runVersionCommand` | `ScrubInherited` |
| `terminal.normalizeTerminalEnv` | `Scrub`, before its own TERM/COLORTERM replacement |
| `editor.Open` | `ScrubInherited` |
| `externalurl.startCommand` | `ScrubInherited` |

Windows/WSL launcher paths (`cmd/agent-overflow-windows`,
`internal/wsllauncher`) are deliberately untouched — AppImage is Linux-only
and those spawn across the WSL boundary with their own env contract.

## Anti-patterns

- Do NOT add a variable to `searchPathVars` without an AppRun hook that
  actually prepends to it; the list is a claim about what the runtime
  pollutes, not a guess.
- Do NOT reach for `os.Getenv` inside a scrubbed assembly. Read the value
  back off the already-scrubbed slice — `BuildEnvironment`'s additive
  `PATH` merge is exactly the hole that reopens otherwise.
- Do NOT make the gate an app-level setting or a `runtime.GOOS` check.
  The environment itself is the evidence.
