# Agent Overflow

Desktop app for using coding agents (Claude Code, Codex) with a shared UX.
Built on Go 1.25, Wails v3, and Svelte 5.

## Setup

Requires Go 1.25+ and Node 24+. On Linux, Wails v3 also needs
`libgtk-3-dev`, `libwebkit2gtk-4.1-dev`, `pkg-config`, and `gcc`
(install via your distro's package manager).

```sh
make install    # installs wails3 CLI (via go.mod tool directive) + npm deps
```

## Run

```sh
make dev        # dev mode with hot reload (wails3 dev)
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
