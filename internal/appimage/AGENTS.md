# AppImage child environment

This package removes AppImage runtime markers and mount-local search paths from
environments inherited by child processes.

Scrubbing is gated by nonempty APPIMAGE or APPDIR markers, operates on POSIX
path boundaries, preserves order, does not mutate input, and is idempotent.
Never infer an AppImage from a mount-shaped path. APPDIR=/ disables path
stripping. Remove an emptied search variable rather than setting it blank.

ScrubInherited returns nil when unchanged so exec.Cmd keeps normal inheritance.
Running is for operations that require a writable installation; child spawns use
Scrub. Route provider child environments through the provider environment
builder rather than assembling them independently.
