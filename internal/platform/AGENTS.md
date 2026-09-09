# WSL detection

This package answers whether the current Linux kernel reports Microsoft WSL.
`IsWSL` reads `WSLOSReleasePath`; `IsWSLFromOSRelease` is the injectable
form used by tests.

Detection is case-insensitive over the kernel release contents and returns
false on read failure. Keep policy, path conversion, process launching, and
other platform classification in their owning packages.
