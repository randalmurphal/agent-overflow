# Process helpers

This package owns three utilities:

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

Call `ConfigureGroup` before starting the command and pass the same command to
`KillConfiguredGroup`. These APIs do not verify process identity or establish
general descendant ownership; callers that act after PID reuse is possible need
their own stronger identity checks.

`TailBuffer` is safe for concurrent writers, accepts a non-positive limit as a
disabled buffer, and returns a snapshot string under its lock.
