# internal/wsllauncher/

Detects WSL distros and orchestrates spawning the agent-overflow Linux
backend inside one.

Two callers:

- `cmd/agent-overflow-windows` (the Windows entry point) — uses the
  full surface (`ListDistros`, `Launch`, `InstallPayload`, etc).
- The WSL-side backend (root `main.go`) — calls `ListDistros` only,
  via the `app_wsl.go` bindings, so the Settings UI can enumerate
  installed distros and let the user switch which one the launcher
  resumes into. The WSL host has wsl.exe on `$PATH` via the standard
  Windows interop shim; on non-WSL Linux/macOS hosts the call is a
  no-op (returns `nil, nil`) so the Settings section self-hides.

## Layout

- `distro.go` — `wsl.exe -l -v` output parser. UTF-16 LE BOM-aware,
  whitespace-tolerant (column widths shift across Windows versions).
- `launcher.go` — public surface: `Distro`, `Bootstrap`, `Launcher`,
  `LaunchOptions`, plus the `readBootstrapLine` helper. Cross-platform
  in shape; Linux/macOS callers get errors from `Launch` /
  `InstallPayload` so the package compiles for unit tests on those hosts.
- `launcher_windows.go` — real implementation: `wsl.exe -l -v` exec,
  spawn with `CREATE_SUSPENDED` + Job Object adoption, payload install
  via `cp /mnt/c/<host-temp> ~/.local/bin/agent-overflow`.
- `launcher_other.go` — non-Windows implementation. `Launch` /
  `InstallPayload` return "not supported on Windows" (these only run
  from the host-side launcher binary). `ListDistros` is a real
  implementation: when running inside WSL (detected via
  `WSL_DISTRO_NAME` + `wsl.exe` on `$PATH` via Windows interop), it
  shells out and parses the same UTF-16 LE output as the Windows
  caller. On native Linux/macOS it returns `nil, nil` so the picker
  UI's empty-state branch is exercisable off-platform.
- `distro_test.go`, `launcher_test.go` — table-driven against fixture
  bytes (`testdata/`) and stubbed readers.

## Responsibility boundary

- What BELONGS here:
  - WSL discovery (`ListDistros`).
  - Spawning + lifetime management of the WSL-side backend
    (`Launch`, `Launcher.Wait`, `Launcher.Stop`).
  - Payload install via the `/mnt/c` automount (`InstallPayload`).
  - Bootstrap-line parsing (the `__AO_BOOTSTRAP__:` sentinel).
- What does NOT belong here:
  - The Wails app + WebView (lives in `cmd/agent-overflow-windows`).
  - The picker HTML or its template injection (lives there too).
  - Persisting picker config to `%APPDATA%\agent-overflow\wsl.json`
    (also in `cmd/agent-overflow-windows`).
  - The WSL-side backend itself — that's the root `main.go`'s headless
    mode (`--print-url-fd`).

## WSL2 localhost forwarding

WSL2 forwards `127.0.0.1:<port>` from inside the distro to the Windows
host's localhost via the vEthernet bridge.
`localhostForwarding=true` is the default in modern WSL2 — but a user
can disable it in `/etc/wsl.conf` or `%USERPROFILE%/.wslconfig`. When
disabled the Windows-side WebView2 cannot reach the WSL backend; the
Wails window would otherwise blank-screen.

`cmd/agent-overflow-windows/main.go::launchAndShow` runs a
deadline-bounded HTTP probe against `http://localhost:<port>/bootstrap.json`
after `Launch` returns. On probe failure, it routes the WebView to a
`/connectivity-error` page that names the actionable mitigation
explicitly:

```
[wsl2]
localhostForwarding=true
```

Set in `%USERPROFILE%/.wslconfig`, then `wsl --shutdown` from
PowerShell to apply. The launcher's Job Object hook still tears the
WSL child down on parent exit regardless of this failure mode.

## Job Object lifetime

Windows: `Launch` creates a `JOBOBJECT_EXTENDED_LIMIT_INFORMATION`
with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, assigns the wsl.exe child
to it, and resumes the suspended primary thread. When the parent
process (the Windows .exe) exits, all of its handles close, including
the Job Object handle — the kernel translates that into a kill signal
for the WSL-side process. This is the only reliable way on Windows
to ensure a wsl.exe child doesn't outlive the launcher, because
wsl.exe forks a vmcompute helper that's hard to track by PID.

The `CREATE_SUSPENDED` + adopt-then-resume sequence avoids a race:
without `CREATE_SUSPENDED`, a fast-failing child could exit before
`AssignProcessToJobObject` runs, leaving the Job Object empty and
no kill-on-close coverage.

## Anti-patterns

- Do NOT shell out to `wsl --shutdown` to kill the backend. That kills
  every process in the distro, not just ours, breaking other tools the
  user has running.
- Do NOT pipe the binary payload through wsl.exe stdin. Some Windows
  versions mangle binary content over pipes; the `/mnt/c` automount
  copy is the documented and tested path.
- Do NOT fail closed on UTF-16 BOM absence. Some `wsl.exe` builds skip
  the BOM (notably PowerShell-redirected output). The parser falls
  back to assuming UTF-16 LE if no BOM is present.

## References

- Microsoft docs on Job Objects:
  https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects
- WSL automount behaviour:
  https://learn.microsoft.com/en-us/windows/wsl/wsl-config#automount-settings
- `cmd/agent-overflow-windows/main.go` — primary consumer (full
  launcher surface).
- `app_wsl.go` — secondary consumer (calls `ListDistros` only, from
  the WSL-side backend, for the Settings UI distro picker).
