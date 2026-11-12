# Process helpers

This package owns five utilities:

- `ConfigureGroup` and `KillConfiguredGroup` arrange and terminate an owned
  child process group using the platform implementation. On Unix,
  `SignalGroup` returns `os.ErrProcessDone` once no member of a group can
  still run, including the exited, unreaped group that macOS refuses with
  EPERM. Use it wherever a group signal's result is checked.
  `Exited` reports a process that has exited but is not yet reaped, which
  signal 0 still finds. On macOS, `RunningGroupMembers` and `RunningInGroup`
  read group membership without counting exited members.
- `TailBuffer` retains the last bounded bytes written to it and reports
  whether older bytes were discarded.
- `RunDrained` runs a command whose output must arrive whole. Use it instead
  of a `WaitDelay` that treats `exec.ErrWaitDelay` as success: that error is
  also what a slow writer with output still buffered produces. On Unix it
  drains each pipe after exit until EOF or until it stays empty for the
  given linger; on Windows a pipe still open after linger is an error.
  Pass the context the command was created with: exec stops watching it at
  exit, so on Unix RunDrained watches it while draining and returns its
  error.
- `StartGroup` starts argv as the leader of its own process group and
  returns a `Group`; `Group.Wait` supervises it to the end, and `RunGroup` is
  the two together. This is the one implementation of pipe ownership,
  cancellation, the leftover sweep and the drain: do not build another from
  `ConfigureGroup`. A done context starts nothing. The `Group` owns the
  output pipe, so output from grandchildren is captured, from the start and
  never after `Wait` returns. Cancellation sends SIGTERM, then SIGKILL after
  `TerminateGrace`, and sets `Stopped`: an exit status is then the command's
  answer to the signal, not its result. After the leader exits `Wait` stops
  whatever is still in the group and reports those processes as `Leftovers`;
  `KeepLeftovers` leaves them running for a command that may start a service
  on purpose. `OutputHeld` reports a pipe still open after the drain grace.
  `Wait`'s error is `*exec.ExitError` for a non-zero exit or a signal death,
  or `ErrGroupSurvived` when the leader ignored SIGKILL (it runs as another
  user) and was abandoned. `Output` should not return an error; one is
  reported as `PipeErr` and the rest of the output is discarded.
  `GroupOutcome.OutputErr` folds `PipeErr` and `OutputHeld` (as
  `ErrOutputHeld`) into one error for a caller that fails a command whose
  output was cut.
- `Terminal` (`OpenTerminal`, Unix only) is a pty that `GroupCommand.Terminal`
  makes the command's controlling terminal, for programs that prompt on
  `/dev/tty`. Stdin and `Output` are unchanged. The command starts through a
  `/bin/sh` shim that ignores SIGHUP and closes the inherited descriptor:
  the kernel hangs up the terminal's foreground group when the session
  leader exits, which would end leftovers before `Wait` counts them. The
  command's processes therefore start with SIGHUP ignored. An executable
  that is not found is still a start error; an exec failure after that is
  the shim's exit status. The caller closes the `Terminal` after `Wait`
  returns. `PollableMaster` makes a read of a pty master end on `Close` on
  Linux; elsewhere the read ends when no process holds the terminal.

Call `ConfigureGroup` before starting the command and pass the same command to
`KillConfiguredGroup`. These APIs do not verify process identity or establish
general descendant ownership; callers that act after PID reuse is possible need
their own stronger identity checks.

`TailBuffer` is safe for concurrent writers, accepts a non-positive limit as a
disabled buffer, and returns a snapshot string under its lock.
