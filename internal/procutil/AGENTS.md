# Process helpers

This package owns two utilities:

- `ConfigureGroup` and `KillConfiguredGroup` arrange and terminate an owned
  child process group using the platform implementation.
- `TailBuffer` retains the last bounded bytes written to it and reports
  whether older bytes were discarded.

Call `ConfigureGroup` before starting the command and pass the same command to
`KillConfiguredGroup`. These APIs do not verify process identity or establish
general descendant ownership; callers that act after PID reuse is possible need
their own stronger identity checks.

`TailBuffer` is safe for concurrent writers, accepts a non-positive limit as a
disabled buffer, and returns a snapshot string under its lock.
