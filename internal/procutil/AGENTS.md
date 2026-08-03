# internal/procutil/

Two primitives shared by every supervised child process the app starts:
process-group kill configuration and a bounded output tail. Stdlib-only.

## What this package owns

- `ConfigureGroup(*exec.Cmd)` — puts the command in its own process group
  (`Setpgid`) and makes `Cmd.Cancel` deliver `SIGKILL` to the whole group,
  with a one-second `WaitDelay` bounding the reap. A setup hook or a check
  command routinely spawns children (`sh -c 'make … & wait'`); killing only
  the direct child leaves them holding the worktree open past the timeout
  that was supposed to end them. The Windows build is a `WaitDelay`-only
  stub — workflow commands execute in the Linux backend under WSL, so the
  Windows binary never reaches this path.
- `TailBuffer` — retains the last N bytes written. Command output is
  unbounded and its useful end is the tail. `Truncated()` reports whether
  anything was dropped, so a narrative can say so. Writes are mutex-guarded
  because one buffer is wired to both stdout and stderr, which os/exec pumps
  from two goroutines.

## Callers

- `internal/worktreesetup` — the per-command runner.
- `app_workflow_tool.go` — `driver: tool` phase attempts and tool fan-out
  units.

Both currently pass the tail buffer as the command's only output sink. A
streaming consumer wraps it rather than replacing it: the tail is what the
failure message quotes, and that must not depend on a subscriber existing.

## Anti-patterns

- Do NOT re-implement either primitive locally. A second process-group
  configuration is a second chance to forget the group kill.
- Do NOT confuse this with `internal/git`'s `newLimitedBuffer`, which
  retains the HEAD of a stream (git's diagnostics lead). This one retains
  the tail.
