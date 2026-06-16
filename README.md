# Agent Overflow

Desktop app for using coding agents (Claude Code, Codex) with a shared UX.
Built on Go 1.26, Wails v3, and Svelte 5.

## For friends helping test

This is a direct test release. There is no auto-update or code signing,
so Gatekeeper / SmartScreen will warn on first launch. Install from the
artifact bundle with `install.sh`; build from source only if you're
developing the app.

When something breaks, runtime logs and the SQLite DB live in your
platform's config directory (see *Files & locations* below). Zip the
`logs/` folder and send it with what you were doing.

## Files & locations

The app writes everything under your OS's user-config directory:

| Platform | Config root |
|---|---|
| macOS | `~/Library/Application Support/agent-overflow/` |
| Linux | `~/.config/agent-overflow/` (honors `$XDG_CONFIG_HOME`) |
| Windows via WSL | WSL distro Linux config root, e.g. `~/.config/agent-overflow/` |
| Windows launcher | `%APPDATA%\agent-overflow\` for launcher config/logs only |

Inside that root: `agent-overflow.db` (SQLite — every thread, item,
payload, attachment metadata), `logs/` (NDJSON provider stdio capture
when `AGENT_OVERFLOW_DEBUG=provider`, plus runtime logs), `attachments/`
(uploaded image / file bytes referenced by the DB), `design-workdirs/`
(per-thread design-mode scratch trees), and `settings.json`.

## Setup

Requires Go 1.26.2+, Node 24+, and pnpm 10+. On Linux, Wails v3 also needs
`libgtk-4-dev`, `libwebkitgtk-6.0-dev`, `pkg-config`, and `gcc`
(install via your distro's package manager; the GTK4 / WebKitGTK 6.0
stack ships on Ubuntu 23.04+ / Debian 13+).

```sh
make install    # installs wails3 CLI (via go.mod tool directive) + pnpm deps
```

## Direct install

The normal tester path is a GitHub release asset install. The script
auto-detects Linux, macOS, or WSL, downloads the matching artifact,
verifies `SHASUMS256`, and copies it into the platform install location.

```sh
curl -fsSL https://github.com/randalmurphal/agent-overflow/releases/latest/download/install.sh | sh
```

Pin a specific release when you do not want "latest":

```sh
curl -fsSL https://github.com/randalmurphal/agent-overflow/releases/download/v0.0.1/install.sh | sh -s -- --version 0.0.1
```

Before an official release exists, use the same installer against a local
release directory. `make release` writes artifacts to
`dist/release/<version>/`; the `--source` path makes the installer read
from that directory instead of GitHub. A Linux/WSL host can produce the
Linux and WSL artifacts; the macOS zip is produced on macOS unless a
working `wails-cross` Docker image is available.

```sh
make release
./scripts/install.sh --linux --download --source ./dist/release/0.0.1
./scripts/install.sh --macos --download --source ./dist/release/0.0.1
./scripts/install.sh --wsl --download --source ./dist/release/0.0.1
```

Linux installs to `~/.local/bin` and writes the desktop entry/icon under
`~/.local/share`. macOS installs to `~/Applications/Agent Overflow.app`.
The WSL installer must be run from inside WSL; it copies the Windows
launcher to `%LOCALAPPDATA%\Programs\Agent Overflow\agent-overflow.exe`
on the Windows filesystem and creates a Start Menu shortcut at
`%APPDATA%\Microsoft\Windows\Start Menu\Programs\Agent Overflow.lnk`.
That shortcut is what makes Agent Overflow show up in Windows app search
with its icon, and the local `%LOCALAPPDATA%` copy keeps Windows from
running it through `\\wsl.localhost`.

Use `--dry-run` to preview, `--system` for system locations where
supported, and `--uninstall` to remove app-owned install files. Passing a
local artifact path still works when you want to bypass download mode:

```sh
./scripts/install.sh --linux ./dist/release/0.0.1/agent-overflow-linux-amd64
./scripts/install.sh --macos ./dist/release/0.0.1/agent-overflow-darwin-arm64.zip
./scripts/install.sh --wsl ./dist/release/0.0.1/agent-overflow-wsl-amd64.exe
```

The installer download path has a local smoke test:

```sh
./scripts/test-install-download.sh
```

## Run

```sh
make dev        # dev mode with hot reload (local supervisor)
make build      # production build (wails3 build)
```

### Developing the Windows + WSL path

The native Windows backend isn't fully supported (terminals and
provider lifecycle are stubs); production Windows runs the WSL launcher
under `cmd/agent-overflow-windows/`. To dev the Windows path from
inside a WSL shell:

```sh
make dev-wsl    # cross-compile Linux ELF + Windows .exe, then launch
                # the .exe with --distro $WSL_DISTRO_NAME so the picker
                # is skipped (you're already shelled into your distro).
                # The .exe opens on the Windows desktop via WSL interop.
make build-wsl  # build only; hand the .exe off without launching.
```

`make dev-wsl` is non-persistent — it doesn't overwrite the saved
distro choice in `%APPDATA%\agent-overflow\wsl.json`, so a dev session
won't change which distro a production double-click of the .exe lands
in.

## Check

```sh
make check      # go build + frontend type check
make test       # go test + frontend unit tests
make verify     # full release gate
```

## Remote access

Agent Overflow's transport (the HTTP+WebSocket layer between the Svelte
SPA and the Go backend) defaults to loopback-only and binds to a fresh
ephemeral port at every launch. The launch URL — `http://127.0.0.1:<port>/?t=<token>` —
is what the embedded webview attaches to.

A handful of opt-in modes extend that:

- **`--listen <addr>`** binds the transport to a different interface
  (e.g. `0.0.0.0:54321` for LAN). The same launch URL works from any
  host that can reach the bound interface; the token gates access.
  Equivalent to flipping the "Allow remote access" toggle in Settings →
  Network, which persists the preference across launches.
- **`--connect ws://host:port/?token=<value>`** runs the desktop
  binary as a thin client against a remote backend. The local
  process boots a stub static-asset server, injects the bootstrap
  manifest, and points the webview at the remote backend over
  WebSocket. No local transport, store, or providers are started.
- **Windows + WSL silent mode** (`agent-overflow-windows.exe`) drops
  the Linux backend into a chosen WSL distro, runs it headless, and
  forwards `localhost:<port>` from inside the distro to the Windows
  host via WSL2's vEthernet bridge. The WebView2 attaches like any
  other local launch.

### Trust model

**Anyone holding the bootstrap token can RPC the host as the user that
launched the binary.** The transport's `LocalOnlyMethods` set refuses a
narrow surface (terminal spawn, git mutators, settings writes,
credential retrieval) for non-loopback peers — that's defense-in-depth
against a token leak on a shared LAN, not a security boundary you can
expose to the public internet.

For non-trusted networks, deploy behind a tunnel that handles
authentication and TLS:

- [Tailscale Serve](https://tailscale.com/kb/1242/tailscale-serve) —
  HTTPS on a `*.ts.net` hostname, ACL-gated.
- SSH local port forward (`ssh -L 54321:localhost:54321 host`).
- Reverse proxy (Caddy / Nginx / Cloudflare Tunnel) terminating TLS
  in front of the backend.

### Token hygiene

The token lives in the launch URL — which means it persists in browser
history, shell history, and any place that records URLs. Treat it as a
password. The token is regenerated on every launch, so closing and
reopening the app rotates it; if you've shared a launch URL, restart to
invalidate.

Tokens for saved `--connect` endpoints are stored plaintext in the
local settings file (the launcher needs to replay them when you click
Connect). The Settings UI exposes them only behind explicit "Show" /
"Copy" actions; bulk reads through `ListRemoteEndpoints` strip them.

## Docs

Start at [`AGENTS.md`](AGENTS.md) for the project overview, stack,
principles, and repo map. Area-specific guides live alongside the code
as nested `AGENTS.md` files. Deep-dive design notes are under
[`docs/architecture/`](docs/architecture/); external reference repos
and the spike-test policy are under [`docs/references/`](docs/references/).
