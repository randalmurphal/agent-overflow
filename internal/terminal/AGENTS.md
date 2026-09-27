# PTY terminal sessions

This package owns ephemeral PTY processes, bounded replay, resize, input, output
fan-out, and session lifetime. It does not persist terminal state or emulate a
terminal; the frontend uses xterm.js.

Replay remains capped at 256 KiB per session and evicts oldest bytes. Strip
terminal query sequences only from replay snapshots so hydration cannot answer
old queries into shell input; live output stays unchanged. Output pumps must not
block on a slow subscriber.

Serialize Resize and Refresh through the session lock. Refresh uses a temporary
winsize change, so direct Process.Resize calls can race and lose the user's
current size. Normalize TERM and COLORTERM for xterm.js and route the remaining
child environment through internal/appimage.

Close PTY descriptors and reap children on every path. A session ends when its
shell exits, after delivering what the pty already holds, even while a
background job keeps the pty open. On Linux the master is pollable so that exit
can wake its read; never call `File.Fd` on it, which restores blocking mode.
Tests cover concurrent resize and refresh, replay wrapping and sanitization,
slow subscribers, repeated close, and child exit.
