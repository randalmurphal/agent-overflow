# Windows WSL launcher

This GUI entry point embeds the Linux backend, installs and starts it in WSL,
then hosts the frontend in WebView2. Keep WSL discovery, process lifetime,
payload installation, and launcher RPC transport in `internal/wsllauncher`.

## Startup and identity

- `updater.HandleHelperMode()` must remain the first operation in `main`. The
  updater helper is this executable and must dispatch before flags, logging,
  distro discovery, Wails, or single-instance setup.
- `parseLauncherFlags` owns the CLI shape. `--distro` is transient and must not
  replace the saved default. An invalid override returns to the picker instead
  of silently using saved configuration.
- `--profile` and `AGENT_OVERFLOW_PROFILE` feed one validated
  `appidentity.RuntimeMode`. Use that mode for every isolated resource:
  instance identity, data roots, browser profiles, logs, window state, CDP,
  backend arguments, payload location, and containment. Unknown profiles fail.
- Isolated profiles never read or write the production payload record.

`launchAndProbe` probes the authenticated `/bootstrap.json` endpoint. Retry
exactly once with `wsllauncher.ResetTransportPortFlag` only when Windows cannot
reach the listener at all. Stop the old backend before retrying. An HTTP
response proves that a new port will not address the failure. Error pages must
describe the observed class without raw errors, bodies, credentials, or URLs.

Trust a recorded payload path only when distro and embedded-byte digest match.
Invalidate the digest before replacement and record the new path and digest
only after a successful boot. If a matching path cannot launch, resolve and
reinstall once when the current WSL home produces a different path.

## Host integrations

The backend sends directives over the authenticated launcher connection. Keep
their validation at this process boundary.

- Notifications retain stable IDs. Retractions use
  `RemoveDeliveredNotification`; Windows may be unable to retract a delivered
  toast. Do not turn that platform limit into a user-facing failure.
- Update directives contain a validated bare filename. Create a fresh updater
  per attempt. Report `proceeding` before replacement and use
  `wsllauncher.ClassifyInstallAck`: proceed after an accepted or undelivered
  acknowledgement and stop after an explicit refusal. Keep the exit watchdog
  shorter than the helper's parent-exit timeout.
- Keep-awake directives go through `internal/power`; its locked OS thread owns
  `SetThreadExecutionState`. Reject unknown modes.
- Browser-host directives go through `internal/webview2host`. Create the host
  lazily, answer result-bearing `create` and `clear-data` operations even when
  construction fails, serialize reports off the UI thread, and close the host
  before its parent window and backend. Browser storage is per runtime mode and
  rejects symlink or reparse-point components.
- Scrub inherited WebView2 profile overrides before either WebView environment
  is created. A set-but-empty override is still an override.

The native LAN bridge binds Windows interfaces and relays TLS to the backend's
non-loopback WSL address. Do not inject a loopback `--listen` argument, expose a
WSL NAT address, proxy remote traffic through localhost, or forward launcher
credentials to remote clients. The backend owns restored network settings and
does not advertise until the launcher reports native state. See
[`internal/nativenetwork`](../../internal/nativenetwork/AGENTS.md).

## Lifetime, diagnostics, and build

Preserve the backend lifetime guarantees in
[`internal/wsllauncher`](../../internal/wsllauncher/AGENTS.md). Isolated
profiles install host and WSL memory containment before WebView2 starts.
Identity checks for WSL samples include PID, start time, and executable. Release
a governor lease only after both sides are confirmed stopped.

Minimized-window suspension rechecks state on the UI thread before suspending
and resumes on restore. The transport replay contract rebuilds missed state.

Windows GUI stderr is unavailable. Route launcher, Wails, and backend stderr to
the profile-specific launcher log. Chromium logging remains opt-in and rotates
the previous log before startup. Pin WebView2 user-data directories;
timestamped development executable names must not create disposable profiles.

The build task regenerates the frontend, bindings, Linux backend, and Windows
resources before compiling the launcher. A standalone package build may contain
stale embedded assets and is not release evidence.

See [agent harness](../../docs/architecture/agent-harness.md),
[soak rig](../../docs/architecture/soak-rig.md),
[embedded browser](../../docs/specs/embedded-browser.md), and
[remote access](../../docs/specs/remote-access.md).
