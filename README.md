# Agent Overflow

Desktop app for using coding agents (Claude Code, Codex) with a shared UX.
Built on Go 1.25, Wails v3, and Svelte 5.

## Run

```sh
wails dev       # dev mode with hot reload
wails build     # production build
```

## Check

```sh
go build ./...                 # Go
go test ./...                  # Go tests
cd frontend && npm run check   # Svelte + TypeScript
cd frontend && npm test        # frontend tests
```

## Docs

Start at [`AGENTS.md`](AGENTS.md) for the project overview, stack,
principles, and repo map. Area-specific guides live alongside the code
as nested `AGENTS.md` files. Deep-dive design notes are under
[`docs/architecture/`](docs/architecture/); external reference repos
and the spike-test policy are under [`docs/references/`](docs/references/).
