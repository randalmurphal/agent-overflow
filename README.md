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

## Check

```sh
make check      # go build + frontend type check
make test       # go test + frontend unit tests
```

## Docs

Start at [`AGENTS.md`](AGENTS.md) for the project overview, stack,
principles, and repo map. Area-specific guides live alongside the code
as nested `AGENTS.md` files. Deep-dive design notes are under
[`docs/architecture/`](docs/architecture/); external reference repos
and the spike-test policy are under [`docs/references/`](docs/references/).
