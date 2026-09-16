# Agent Overflow

Agent Overflow is a lightweight desktop app for working with coding agents.
It brings Claude Code and Codex into a shared interface for conversations,
repositories, and worktrees, while keeping execution in each provider's own
harness.

Built with Go, Svelte, and Wails, it uses the system webview and aims to keep
the app's own CPU and memory overhead low, leaving resources available for
the agents and tools doing the work.

Use it to work across projects and conversations, review changes, manage
worktrees, and run terminals alongside your agents. Optional remote access
lets you continue from another desktop, the Android app, or a browser.
An iOS app is also planned.
Planned integrations include Cursor, ACP-compatible agents, and local models.

[Install](#install) · [First conversation](#start-a-conversation) ·
[Remote access](#remote-access) · [Updates](#updates) ·
[Development](#build-from-source)

## Install

Published builds are available on the
[Releases page](https://github.com/randalmurphal/agent-overflow/releases).

| Platform | Release build | Requirements |
|---|---|---|
| macOS | Apple Silicon | Provider CLIs installed on the Mac |
| Linux | x86-64 | GTK 4 and WebKitGTK 6.0 runtime libraries |
| Windows | x86-64 launcher with a WSL backend | A working WSL distribution with interop enabled; install providers inside that distribution |
| Android | APK, Android 8.0 or later | A running Agent Overflow host; see [Android setup](#android-app) |

On macOS or Linux, run this in a terminal. On Windows, run it **inside WSL**:

```sh
curl -fsSL https://github.com/randalmurphal/agent-overflow/releases/latest/download/install.sh | sh
```

The installer detects the platform, downloads its release artifact, and
verifies it against the release's `SHASUMS256` manifest before installing.
You do not need Go, Node, or pnpm to use a release build.

- **macOS:** installs to `~/Applications/Agent Overflow.app`.
- **Linux:** installs to `~/.local/bin` and adds a desktop entry and icon.
- **Windows:** installs the launcher under
  `%LOCALAPPDATA%\Programs\Agent Overflow\` and adds a Start Menu shortcut.
  Launch it from Windows and choose the WSL distribution where your projects
  and providers live.

Desktop builds are not publisher-signed. macOS Gatekeeper or Windows
SmartScreen may require confirmation on first launch.

To choose a version or inspect the installer first, download `install.sh`
from that release and run `sh ./install.sh --help`. Use `--version VERSION`
to select a release, or `--dry-run` to preview installation. The installer
also accepts local artifacts and `--uninstall`; see its
[options](scripts/install.sh).

A separate `agent-overflow-headless-linux-amd64` artifact runs without GTK or
WebKit. For a server or a computer used without a desktop window, follow the
[headless setup guide](docs/architecture/serve-mode.md).

## Start a conversation

1. Install at least one provider on the computer that will run your agents:
   [Claude Code](https://code.claude.com/docs/en/quickstart) or
   [Codex CLI](https://developers.openai.com/codex/cli). Install Git if it is
   not already available. On Windows, do this inside the WSL distribution
   selected in Agent Overflow.
2. Sign in through the provider's CLI. Agent Overflow detects existing native
   logins. You can also sign in from the **Accounts** section under
   **Settings → Claude Code** or **Settings → Codex**.
3. Open Agent Overflow and check that provider's **Setup** section in settings.
   If its executable was not found, set **Binary path** to its installed path.
4. Choose **Add Project** in the sidebar and select a Git repository on that
   computer.
5. Start a new thread, choose a provider and model, and send a message. Use
   the repository root or a linked worktree for the work.

Provider accounts and usage remain with the provider. Agent Overflow uses
its installed harness, credentials, and native session files.

## Remote access

The **host** is the computer with your repositories and provider processes.
It must stay running and awake while you work from another device. Remote
access is optional and disabled by default. The connecting phone or browser
does not need provider CLIs installed.

Start on the host at **Settings → Remote access → Allow device access**.
Choose a network below, then follow the steps for your client.

### Choose a network

**On the same local network:** enable **Local network → Allow LAN connections**.
The Android app and another Agent Overflow desktop can pair over the host's
private HTTPS connection without installing a certificate on the device.
A browser needs its own certificate trust; use the Tailscale HTTPS route below
for straightforward browser setup.

On Windows, allow the normal Private network firewall prompt if shown.
The launcher exposes the connection through Windows even when its backend
runs inside WSL. The app does not change firewall rules for you.

**Across networks, or for browser access:** use the built-in Tailscale connection.

1. In your [Tailscale DNS settings](https://login.tailscale.com/admin/dns),
   enable MagicDNS and HTTPS certificates. See
   [Tailscale's HTTPS setup](https://tailscale.com/docs/how-to/set-up-https-certificates).
2. On the host, enable **Tailscale** under **Allow device access** and save.
   Leave the coordination-server field empty for ordinary Tailscale.
3. Open the sign-in link shown by the app and authorize its node on your
   tailnet. Wait for **Running** and an `https://…ts.net/` address.
4. On the connecting phone or browser's computer, connect Tailscale to the
   same tailnet.

Agent Overflow joins as its own Tailscale node. Signing in the host's separate
Tailscale app does not enroll Agent Overflow, and that separate app does not
need to stay connected for Agent Overflow's node to work.

Use a private LAN or tailnet. The backend is not intended for public internet
exposure; no router forwarding, public tunnel, or Tailscale Funnel is needed.
Network access and pairing are separate: reaching the host does not grant
access to your work.

### Android app

1. Download `agent-overflow-android.apk` from the same release as the host.
   Open it on the phone and allow installation from that browser or file
   manager when Android asks.
2. Set up a phone screen lock. The app uses Android's biometric or device
   credential prompt when it opens.
3. On the host, select **Allow a device to connect**. Choose **Local network**
   or **Tailscale**, then **My device** and **Phone or tablet**.
4. Open Agent Overflow on the phone. Use its in-app QR scanner to scan the
   invitation, or paste the pairing link.
5. Compare the verification numbers on both screens and approve the device
   on the host.

The phone can now open conversations, send messages, and answer approvals.
Pairing survives host restarts. Installed clients learn the host's available
LAN and Tailscale routes after connecting, so you do not need to pair separately
for each network. Tailscale must be connected on the phone to use a tailnet
route, including over cellular.

**Background push notifications need additional setup.** Core remote access
works without them. Push currently requires Firebase configuration in the APK
and a matching sender credential on the host. There is no shared push relay
for release users. See
[phone push setup](docs/architecture/remote-access-setup.md#notifications-and-troubleshooting)
if you want to configure it.

### Mobile or desktop browser

1. Set up the host's Tailscale HTTPS address above and connect the browser's
   device to the same tailnet.
2. On the host, select **Allow a device to connect → Tailscale → Phone or
   tablet** to generate a QR code and pairing link. This link also works in
   a browser. Choose **My device** for your own device or **View only** for
   read access.
3. Open the pairing link in the browser you intend to use. On a phone, scan
   with the phone's camera to open the browser; the Android app's scanner
   pairs the app instead.
4. Name the device, choose **Pair**, compare the verification numbers, and
   approve on the host.
5. Bookmark the host's address after pairing. The invitation is single-use;
   keep using the paired browser profile to return.

Clearing site data or using a different browser profile requires signing in
or pairing again. A registered passkey can be used for later sign-ins.
Optional **Browser lock** requires an existing passkey registered for the
host's configured HTTPS domain; its setup instructions appear under
**Allow device access → Security & passkeys** and **Browser lock**.

Browsers cannot use the native apps' certificate pinning. For browser access
over LAN without Tailscale, configure a hostname and a certificate that the
browser trusts in **Advanced network settings**. Plain HTTP lacks browser
features such as passkeys and does not encrypt traffic.

### Another desktop

1. On the host, select **Allow a device to connect**, choose the network and
   access level, then **Another computer**. Leave the pairing window open.
2. On the other desktop, open **Settings → Remote access → Connect to a
   computer** and select the discovered computer. If it is not listed, enter
   the address shown in the host's pairing window.
3. Compare the verification numbers and approve on the host.

For automatic Tailscale discovery, enable Agent Overflow's Tailscale node on
both desktops. A separate OS Tailscale connection can reach a typed address,
but does not populate the app's discovery list.

**My device** joins your personal devices so they can establish connections
to each other. **View only** grants limited access without joining that group.
Review or revoke devices under **Allow device access**. See
[personal devices](docs/architecture/remote-access-setup.md#personal-devices)
for how enrollment and removal work across computers.

For a client without a local execution host, headless services, or recovering
a changed host address, see the
[remote access guide](docs/architecture/remote-access-setup.md).

## Updates

On desktop, use **Settings → Updates**, or rerun the release installer and
restart the app. Installing an update preserves the app's data directory.

On Android, install the new release APK over the existing app to retain
pairing and local data. A debug APK uses a different signing key and cannot
be updated in place with a release APK; switching from debug to release
requires uninstalling and pairing again.

The Android app can also receive frontend updates from a connected host.
Native changes still require a new APK. Updating the APK starts with its
packaged frontend while preserving pairing; later frontend updates can apply
again.

## Data and troubleshooting

Agent Overflow stores its application data under these default directories:

| Platform | Data directory |
|---|---|
| macOS | `~/Library/Application Support/agent-overflow/` |
| Linux | `~/.config/agent-overflow/`, or `$XDG_CONFIG_HOME/agent-overflow/` |
| Windows backend | The Linux config directory inside the selected WSL distribution |
| Windows launcher | `%APPDATA%\agent-overflow\` for launcher settings and logs |

The application directory includes `settings.json`, `agent-overflow.db`,
`attachments/`, and `logs/`, along with connection and account state.
Provider-native session files own recoverable conversation history; SQLite
caches conversation items and stores app records. The database alone is not
a complete backup of your work or provider history.

| Problem | What to check |
|---|---|
| Provider is missing or cannot sign in | Check its settings page, binary path, and native login. On Windows, check inside the selected WSL distribution. |
| Another device cannot reach the host | Keep the host running and awake. Check LAN access or the app's Tailscale status and the connecting device's network. |
| Works on Wi-Fi but not cellular | Use the Tailscale route and keep Tailscale connected on the phone. A private LAN address is not reachable over cellular. |
| Browser reports a certificate error | Use the HTTPS hostname shown by the app after Tailscale setup, or configure a browser-trusted LAN certificate. |
| Invitation expired or was already used | Generate a fresh invitation on the host. |
| Every saved host address stopped working | Use **Connect to a computer → the offline computer → Change address → Verify & reconnect**. |
| Android refuses an APK update | Check that it is a release-to-release update. Debug and release signing keys differ. |
| Android connects but receives no background push | Check Android notification permission and the separate Firebase setup linked above. |

When reporting a problem, include the app version, operating system, what you
were doing, and the relevant runtime logs. Review logs before sharing them;
provider debug capture can contain conversation and tool content. Do not
attach the entire application data directory.

## Build from source

Use Go 1.26.6 or later, Node 24 or later, and the pnpm version pinned in
[`package.json`](package.json). Linux GUI builds also need `gcc`, `pkg-config`,
`libgtk-4-dev`, and `libwebkitgtk-6.0-dev` from the distribution's packages.

```sh
git clone https://github.com/randalmurphal/agent-overflow.git
cd agent-overflow
make install
make dev
```

`make install` installs the Go tools, frontend dependencies, and Chromium for
browser tests. `make build` creates a production desktop build. For the native
Windows window with a WSL backend, run `make dev-wsl` or `make build-wsl`
inside WSL.

For Android builds, `make apk` creates a debug APK and `make apk-release`
creates a signed release APK. These also require JDK 21 and an Android SDK;
release builds require your persistent signing key. See the
[Android build guide](mobile/AGENTS.md#build-and-release).

```sh
make check     # Go builds and frontend type checks
make test      # Go and frontend tests
make verify    # Automated build and test release gate
```

Browser end-to-end tests, Android emulator checks, and manual platform checks
have separate commands and prerequisites. `make verify` alone does not prove
all release paths. Follow [Development](docs/architecture/development.md) and
[release candidates](docs/architecture/release-candidates.md) for validation
and packaging.

For code changes, start with [AGENTS.md](AGENTS.md). The
[documentation index](docs/README.md) routes to architecture, product decisions,
and area-specific guides.
