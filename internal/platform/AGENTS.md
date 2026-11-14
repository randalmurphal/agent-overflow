# WSL detection

This package answers whether the current Linux kernel reports Microsoft WSL.
`IsWSL` reads `WSLOSReleasePath`; `IsWSLFromOSRelease` is the injectable
form used by tests.

Detection is case-insensitive over the kernel release contents and returns
false on read failure.

`AppendWSLENV` lists variables in `WSLENV`, the only way a variable crosses
between a WSL process and a Windows one in either direction.

Keep policy, path conversion, process launching, and other platform
classification in their owning packages.
