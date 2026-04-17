# Agent Overflow

Desktop app for using coding agents (Claude Code, Codex) with a shared UX.
Ground-up rewrite of [`forge`](/Users/randy/repos/forge) optimizing for
performance, memory efficiency, and minimal code.

## Stack

- **Backend**: Go 1.25, Wails v3 (system webview), SQLite via `modernc.org/sqlite`
  (pure Go, no CGO). WAL mode.
- **Frontend**: Svelte 5 (runes), Vite 8 (Rolldown), Tailwind CSS 4, TypeScript.
- **IPC**: Wails bindings (auto-generated TS from Go structs) for request/response;
  `app.Event.Emit` for server push.
- **Providers**: Claude Code CLI (NDJSON over stdio) and Codex app-server
  (JSON-RPC 2.0 over stdio).

## Commands

- `wails dev` — dev mode, hot reload
- `wails build` — production build
- `go build ./...` — Go-only sanity check
- `go test ./...` — Go tests
- `cd frontend && npm run check` — Svelte + TypeScript
- `cd frontend && npm test` — frontend unit tests

Every task must leave `go build`, `go test`, `npm run check`, and
`npm run build` passing.

## Core Principles

1. **Go is triage + pipe.** No event sourcing, no orchestration engine,
   no in-memory read models. The one deliberate exception — lightweight
   coordination when brokering between multiple provider processes and the
   frontend (deliberation turn tracking, design option flow) — is coordination,
   not orchestration, and is called out where it lives.
2. **Provider process is the source of truth during a turn.** Don't duplicate
   its state. Provider session files (`~/.claude/`, `~/.codex/`) are the
   authoritative history for crash recovery.
3. **SQLite is a history cache, not an event store.** Persist per-item on
   completion, not per-turn.
4. **Frontend memory is bounded by the visible thread.** Heavy payloads
   (diffs, command output, thinking) live in SQLite and load on demand.
5. **Errors are user-facing state, not log entries.**
6. **Provider-specific code stays in provider-specific packages.** Don't
   force a unified abstraction across Claude and Codex.
7. **Project ≠ workspace.** A project is the git repo. A workspace is where
   the provider operates (project root, or a separate worktree). Threads
   track both.

## Repo Map

```
/                             root guides (this file)
/main.go, /app.go, /app_*.go  Wails entry + bound methods
/internal/                    Go packages (see internal/AGENTS.md)
/frontend/                    Svelte 5 app (see frontend/AGENTS.md)
/docs/architecture/           deep-dive design docs
/docs/references/             external reference repos + spike policy
/docs/archive/                historical specs + ralph-loop artifacts
```

Area guides live alongside their code as `AGENTS.md` (with a `CLAUDE.md`
symlink). Start at the area closest to what you're touching — it will link
down if more depth is needed.

## Conventions

- Go: `internal/` for every non-main package. No `pkg/`.
- Svelte: runes only (`$state`, `$derived`, `$effect`, `$props`). No legacy
  stores or reactive `$:` syntax.
- Tailwind v4: CSS-native config via `@theme` in `app.css`. No
  `tailwind.config.js`.
- Wails bindings live in `frontend/bindings/` and are regenerated —
  never edit by hand.
- Events go Go → frontend via `app.Event.Emit`; frontend calls Go via
  the typed wrappers in `frontend/src/lib/stores/bindings.ts`.

## When Behavior Is Unclear

If you're uncertain how Claude Code, Codex, or an external tool behaves,
**do not guess from this repo**. Write a small isolated spike test outside
the project to confirm the behavior, then port the learning in. See
[docs/references/spike-policy.md](docs/references/spike-policy.md).

## Reference Repos

- **forge** (`/Users/randy/repos/forge`) — the Node/Effect project this one
  rewrites. UX and provider-handling reference. See
  [docs/references/forge.md](docs/references/forge.md).
- **Codex source** (`/Users/randy/repos/codex-source`, upstream
  https://github.com/openai/codex) — authoritative Codex CLI and
  app-server behavior.
- **CodexMonitor** (https://github.com/Dimillian/CodexMonitor) — Tauri,
  feature-complete reference implementation of a Codex app-server client.

See [docs/references/codex.md](docs/references/codex.md) for how to use
these when touching Codex code.

## Deferred (Not Currently in Scope)

These are intentional non-goals for the current phase — don't implement
them without a scope conversation first.

- **Workflow / phase / gate system.** Forge has one
  (`apps/server/src/workflow/`); the underlying idea ported from
  `/Users/randy/repos/orc` (see `docs/specs/TASK_TEMPLATES.md`,
  `docs/decisions/ADR-007-human-gates.md`). Not a core feature for v1.
- **Remote / web access.** Forge's `REMOTE.md` model (HTTP+WS server,
  auth token, Tailnet bind) is a planned future capability but is not
  being built yet. Until then: keep the transport boundary clean —
  Go → frontend goes through `app.Event.Emit` and Wails bindings only;
  don't let UI code reach into Go internals in ways that would lock out
  a future network transport.
- **Auto-updater wiring.** Wails v3 ships a built-in updater
  (https://v3alpha.wails.io/guides/distribution/auto-updates/); enable
  it when we're ready to distribute. No custom updater required.
