# internal/terminal/

PTY-backed shell sessions for thread-scoped terminals. One `Manager`
per app, one `Session` per open terminal, a bounded ring buffer per
session for replay on reconnect.

## Layout

- `manager.go` defines the `Manager` type: owns the map of active
  sessions, the `OutputCallback` / `ExitCallback` fan-out, and the
  public API (`Open`, `Write`, `Resize`, `Refresh`, `Close`, `List`).
- `session.go` defines `Session`: a `Process` + ring buffer +
  subscriber fan-out. Owns the replay snapshot that re-hydrates an
  xterm on reconnect. `resizeMu` serializes `Resize`/`Refresh` so the
  latter's shrink→restore nudge can't be clobbered by a concurrent
  resize.
- `process.go` defines `Process`, wrapping the PTY master fd + child
  `*os.Process` + output pump goroutine. Pure spawn/read/signal; no
  policy. `Refresh` forces a TUI repaint via a one-row winsize nudge
  (shrink, pause, restore). See its doc comment for why a bare SIGWINCH
  is insufficient.
- `env.go` defines `normalizeTerminalEnv`: the child environment a PTY
  spawn gets. Owns one rule (replacing the inherited `TERM`/`COLORTERM`
  with what xterm.js actually renders) and delegates the AppImage scrub
  to `internal/appimage` (`Scrub`), which is the same marker-gated scrub
  every other process Agent Overflow spawns gets. Change the scrub there,
  not here; every other launch shape (dev, `.deb`, macOS) is passed
  through unchanged.
- `replay_sanitize.go` defines `stripReplayableQueries`, which drops
  terminal query sequences (DA, DSR, DECRQM, XTVERSION, kitty-keyboard,
  DECRQSS/XTGETTCAP, OSC color queries) from replay snapshots so a
  hydrating xterm doesn't re-answer them into the shell's input.
  Applied only on the `Replay` / `ReplaySnapshot` path. Live output
  stays raw because the attached xterm must answer queries for programs
  to work.
- `ring.go` is a byte-oriented circular buffer capped at 256 KiB per
  session.
- `shell.go` defines `resolveShell`: explicit > `$SHELL` > `/bin/sh`.
- `pty_file.go` is a tiny `pty.File` adapter for cross-platform sizing.

## Responsibility boundary

- What BELONGS here:
  - PTY spawn, read, resize, signal, close.
  - Bounded replay buffer and fan-out to subscribers.
  - Shell-resolution policy at session creation.
- What does NOT belong here:
  - Persistence. Sessions are ephemeral; no SQLite rows.
  - Business decisions about *which* shell to spawn for a given thread.
    The caller passes `SessionOptions`.
  - Rendering or terminal emulation. The frontend runs xterm.js.

## Extension points

- To add a per-session knob: extend `SessionOptions` and wire it
  through `Process.Start`. Keep the default path deterministic for
  tests.
- To change replay capacity: adjust `maxReplayBytes` in `session.go`.
  The cap is global; a per-session override would need Manager-level
  plumbing.

## Anti-patterns

- Do NOT let the replay buffer grow unbounded. 256 KiB is the ceiling;
  new writes evict oldest bytes. Keep this invariant.
- Do NOT fan output out synchronously from the read pump. The pump
  must stay ahead of any single subscriber; slow clients are the
  caller's problem.
- Do NOT leak file descriptors on exit. The close path in `Session` /
  `Process` is the regression-tested shape. Keep it.
- Do NOT mutate a PTY's winsize outside `Session.Resize` /
  `Session.Refresh`. Those serialize on `resizeMu` so Refresh's
  shrink→restore nudge can't be clobbered by a concurrent resize. A new
  path that calls `Process.Resize` (or `pty.resize`) directly bypasses
  that lock and reopens the lost-update race
  (`TestManagerRefreshSerializesWithConcurrentResize` guards it).

## References

- `docs/architecture/recovery.md` covers how replay ties into reconnect.
- `github.com/creack/pty` is the upstream PTY library.
