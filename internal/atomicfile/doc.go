// Package atomicfile writes small JSON state files so a reader never observes
// a half-written or truncated file, even across a crash or a concurrent
// writer. It is the shared home for the temp-file + fsync + rename dance that
// the launcher's wsl.json and window.json (and any future per-user state file)
// rely on.
//
// Files land at 0600 and parent directories at 0700: these are per-user state
// files, and a multi-user host must not expose one account's state to another.
// It is intended for small config/state blobs, not large or hot-path data.
package atomicfile
