# internal/wsllauncher/

Detects WSL distros and orchestrates spawning the agent-overflow Linux
backend inside one.

Two callers:

- `cmd/agent-overflow-windows` (the Windows entry point) uses the
  full surface (`ListDistros`, `Launch`, `InstallPayload`, etc).
- The WSL-side backend (root `main.go`) calls `ListDistros` only,
  via the `app_wsl.go` bindings, so the Settings UI can enumerate
  installed distros and let the user switch which one the launcher
  resumes into. The WSL host has wsl.exe on `$PATH` via the standard
  Windows interop shim; on non-WSL Linux/macOS hosts the call is a
  no-op (returns `nil, nil`) so the Settings section self-hides.

## Layout

- `distro.go` is the `wsl.exe -l -v` output parser. UTF-16 LE BOM-aware,
  whitespace-tolerant (column widths shift across Windows versions).
- `launcher.go` is the public surface: `Distro`, `Bootstrap`, `Launcher`,
  `LaunchOptions`, the two-binary contract constants
  (`ResetTransportPortFlag`, `PageURLPath`), plus the `readBootstrapLine`
  helper. Cross-platform
  in shape; Linux/macOS callers get errors from `Launch` /
  `InstallPayload` so the package compiles for unit tests on those hosts.
  Also owns `buildLaunchArgs` (the wsl.exe argv builder) so the argument
  order (including the `--cd "~"` working-directory pin) is unit-tested
  off-Windows, plus the shared stream drain (`newStreamScanner` /
  `drainStream`). Both child streams are drained for the child's whole
  lifetime: stdout continues on the same scanner once the bootstrap
  sentinel is consumed, because an unread pipe wedges the backend inside
  `write(2)` as soon as the OS buffer fills.
- `launcher_windows.go` is the real implementation: `wsl.exe -l -v` exec,
  spawn with `CREATE_SUSPENDED` + Job Object adoption, payload install
  via `cp /mnt/c/<host-temp> ~/.local/bin/agent-overflow`.
- `launcher_other.go` is the non-Windows implementation. `Launch` /
  `InstallPayload` return "not supported on Windows" (these only run
  from the host-side launcher binary). `ListDistros` is a real
  implementation: when running inside WSL (detected via
  `WSL_DISTRO_NAME` + `wsl.exe` on `$PATH` via Windows interop), it
  shells out and parses the same UTF-16 LE output as the Windows
  caller. On native Linux/macOS it returns `nil, nil` so the picker
  UI's empty-state branch is exercisable off-platform.
- `notification_client.go` is the cross-platform transport client used by
  the Windows launcher: replay-aware `notification:send` consumption,
  bounded reconnect, and RPC posting over the same connection. Kept
  cross-platform so its wire behavior is exercised by Linux unit tests.
  The notification wire contract itself (`Target`, `Send`, validation,
  content limits) lives in `internal/notify`, shared with the backend.
  Setting `HandleUpdateInstall` additionally subscribes the connection to
  `selfupdate.ChannelInstall` and dispatches every directive that passes
  `InstallDirective.Validate`. Invalid ones are logged and dropped, never
  handed on, since a directive names a file the launcher resolves on disk.
  That channel is ephemeral on the server, so it deliberately carries no
  replay cursor and no sequence tracking; only `notification:send` does.
  Leaving the callback nil keeps the wire exactly as it was.
- `notification_rpc.go` is the call layer both of the launcher's RPCs share
  (`NotificationActivated` and `ReportUpdateInstallStatus`, the latter pinned by
  `selfupdate.RPCReportStatus` and posted as `stage, version, message`): one
  pending map, one 5s timeout, one disconnect story. The two differ in what
  the timeout covers, and the difference is deliberate: `Activate`'s
  connection wait rides the caller's context so a cold-boot toast click
  survives the bridge still connecting, while `ReportUpdateInstallStatus` is
  bounded end to end (connection wait included) because a directive only
  arrives over a live connection, the backend's ACK deadline is already
  counting, and a report blocked on a reconnect would hold the launcher's
  install guard indefinitely and could land late enough to be refused. A call
  the backend *answered and rejected* returns `*RPCRefusedError`; every other
  failure (no connection, write error, timeout) returns a plain error. That
  split is load-bearing, not cosmetic. See below.
- In `install_ack.go`, `ClassifyInstallAck` turns the result of a
  `proceeding` report into the launcher's decision: **accepted** → swap;
  **refused** → abandon, because a server answer proves the report did not take
  effect (the backend's ACK deadline already unwound the install and told the
  user, or the directive is stale), so swapping would contradict the error on
  screen; **undelivered** → swap anyway, because an unanswered report is
  ambiguous (it may have landed with only its response lost), and abandoning
  then would strand a backend holding its fence for a swap that never comes.
  Kept here so all three branches are covered by Linux unit tests rather than
  living in the launcher's Windows-only driver.
- `notification_activation_queue.go` holds the bounded cold-click FIFO and
  serialized drain state shared with the launcher, also cross-platform for
  unit tests.
- `distro_test.go` and `launcher_test.go` are table-driven against fixture
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
  - The WSL-side backend itself. That's the root `main.go`'s headless
    mode (`--print-url-fd`).

## The page URL comes from the backend, once per navigation

`Bootstrap.PageURL` is the fully assembled URL the WebView2 navigates to.
The launcher never builds one: the URL carries a **one-time page ticket**
the browser exchanges for its session cookie, and only the backend can
mint one. A bootstrap line without `pageUrl` is refused at the parse
boundary rather than opening an empty window.

The ticket is spent by the load it was minted for, so the reload
keybinding cannot reuse the boot URL. It asks the backend's `PageURLPath`
(`/pageurl`) route for a fresh one, presenting `Bootstrap.Token` as a
bearer header. That path is restated here rather than imported so this
package stays linkable without the transport server; a drift-guard test
compares it to `transport.PageURLPath`. If the request fails the launcher
logs and reuses the launch URL — a spent ticket at worst, never a wedge.

`Bootstrap.Token` is the session credential for the launcher's OWN
requests (the connectivity probe, the notification socket). It is never
put on a page URL.

## WSL2 localhost forwarding

WSL2 forwards `127.0.0.1:<port>` from inside the distro to the Windows
host's localhost via the vEthernet bridge.
`localhostForwarding=true` is the default in modern WSL2, but a user
can disable it in `/etc/wsl.conf` or `%USERPROFILE%/.wslconfig`. When
disabled the Windows-side WebView2 cannot reach the WSL backend; the
Wails window would otherwise blank-screen.

A port Windows has RESERVED breaks the same hop: Hyper-V / WSL2
excluded port ranges (re-seeded on every Windows reboot, routinely
covering 49152+) make an otherwise healthy WSL listener unreachable
from the host. That one is recoverable, and
`ResetTransportPortFlag` is how: the launcher relaunches the backend
once with `--reset-transport-port`, which drops the backend's pinned
listen port so it adopts a reachable one. The flag name lives in this
package because both binaries need the same spelling.

`cmd/agent-overflow-windows/main.go::launchAndProbe` runs a
deadline-bounded HTTP probe against `http://localhost:<port>/bootstrap.json`
after `Launch` returns, presenting `Bootstrap.Token` as an
`Authorization: Bearer` header, and drives that single retry. If the retry also
fails, it routes the WebView to a `/connectivity-error` page that names
the actionable mitigation explicitly:

```
[wsl2]
localhostForwarding=true
```

Set in `%USERPROFILE%/.wslconfig`, then `wsl --shutdown` from
PowerShell to apply. The launcher's Job Object hook still tears the
WSL child down on parent exit regardless of this failure mode.

## Job Object lifetime

Windows: `Launch` creates a `JOBOBJECT_EXTENDED_LIMIT_INFORMATION`
with `KILL_ON_JOB_CLOSE | SILENT_BREAKAWAY_OK`, assigns the wsl.exe
child to it, and resumes the suspended primary thread. When the parent
process (the Windows .exe) exits, all of its handles close, including
the Job Object handle. The kernel translates that into a kill signal
for the explicitly-assigned `wsl.exe` process, which terminates the
WSL session and cascades to all Linux processes in the distro.

`SILENT_BREAKAWAY_OK` ensures that child processes of job members
automatically do NOT inherit the job. Without this, Windows-side
processes spawned through WSL interop (browsers via rundll32, VS Code,
editors) inherit the job from `wsl.exe` and get killed when the
launcher closes. The breakaway is safe because the WSL2 VM lifecycle
is managed by the Host Compute Service (HCS), not by our job. Killing
`wsl.exe` signals HCS to tear down the session regardless of whether
helper processes broke away.

The `CREATE_SUSPENDED` + adopt-then-resume sequence avoids a race:
without `CREATE_SUSPENDED`, a fast-failing child could exit before
`AssignProcessToJobObject` runs, leaving the Job Object empty and
no kill-on-close coverage.

## Anti-patterns

- Do NOT pass a Linux command through wsl.exe's implicit-shell mode
  (`wsl.exe -- <cmd> ...`); always `--exec`. The `--` form joins the
  argv with spaces and re-parses the string through the user's LOGIN
  shell, so correctness depends on which shell the user runs: quoting
  is destroyed and `$` references are pre-expanded in the outer shell
  (a zsh login shell turned the memory-limit wrapper's `exec "$@"`
  into `exec ""`, killing every harness-wsl boot — incident
  2026-08-30). `--exec` passes argv verbatim with no shell; when shell
  semantics are wanted, spell out `/bin/sh -c <script>` explicitly.
  This applies to EVERY wsl.exe call site, including the ones in
  `cmd/agent-overflow-windows` (payload, memory watchdog, containment
  evidence).
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
- `cmd/agent-overflow-windows/main.go` is the primary consumer (full
  launcher surface).
- `app_wsl.go` is the secondary consumer (calls `ListDistros` only, from
  the WSL-side backend, for the Settings UI distro picker).
