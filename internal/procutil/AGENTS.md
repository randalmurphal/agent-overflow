# Process helpers

This package owns two utilities:

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

Call `ConfigureGroup` before starting the command and pass the same command to
`KillConfiguredGroup`. These APIs do not verify process identity or establish
general descendant ownership; callers that act after PID reuse is possible need
their own stronger identity checks.

`TailBuffer` is safe for concurrent writers, accepts a non-positive limit as a
disabled buffer, and returns a snapshot string under its lock.
