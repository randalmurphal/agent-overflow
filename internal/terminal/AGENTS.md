# internal/terminal/

PTY-backed shell sessions for thread-scoped terminals. One `Manager`
per app, one `Session` per open terminal, a bounded ring buffer per
session for replay on reconnect.

## Layout

- `manager.go` — `Manager` type: owns the map of active sessions, the
  `OutputCallback` / `ExitCallback` fan-out, and the public API
  (`Open`, `Write`, `Resize`, `Refresh`, `Close`, `List`).
- `session.go` — `Session`: a `Process` + ring buffer + subscriber
  fan-out. Owns the replay snapshot that re-hydrates an xterm on
  reconnect. `resizeMu` serializes `Resize`/`Refresh` so the latter's
  shrink→restore nudge can't be clobbered by a concurrent resize.
- `process.go` — `Process`: wraps the PTY master fd + child `*os.Process`
  + output pump goroutine. Pure spawn/read/signal; no policy. `Refresh`
  forces a TUI repaint via a one-row winsize nudge (shrink, pause,
  restore) — see its doc comment for why a bare SIGWINCH is insufficient.
- `ring.go` — byte-oriented circular buffer capped at 256 KiB per
  session.
- `shell.go` — `resolveShell`: explicit > `$SHELL` > `/bin/sh`.
- `pty_file.go` — tiny `pty.File` adapter for cross-platform sizing.

## Responsibility boundary

- What BELONGS here:
  - PTY spawn, read, resize, signal, close.
  - Bounded replay buffer and fan-out to subscribers.
  - Shell-resolution policy at session creation.
- What does NOT belong here:
  - Persistence — sessions are ephemeral; no SQLite rows.
  - Business decisions about *which* shell to spawn for a given thread
    — the caller passes `SessionOptions`.
  - Rendering or terminal emulation — the frontend runs xterm.js.

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
  `Process` is the regression-tested shape — keep it.
- Do NOT mutate a PTY's winsize outside `Session.Resize` /
  `Session.Refresh`. Those serialize on `resizeMu` so Refresh's
  shrink→restore nudge can't be clobbered by a concurrent resize. A new
  path that calls `Process.Resize` (or `pty.resize`) directly bypasses
  that lock and reopens the lost-update race
  (`TestManagerRefreshSerializesWithConcurrentResize` guards it).

## References

- `docs/architecture/recovery.md` — how replay ties into reconnect.
- `github.com/creack/pty` — upstream PTY library.
