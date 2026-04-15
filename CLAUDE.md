# AGENTS.md

## Project: Agent Overflow

Go + Wails v2 + Svelte 5 desktop app for using coding agents (Codex, Claude Code).
See `ARCHITECTURE.md` for the full design rationale.

## Stack

- **Backend**: Go 1.25, Wails v2.12 bindings + event system, SQLite (WAL mode)
- **Frontend**: Svelte 5 (runes), Vite 8 (Rolldown), Tailwind CSS 4, TypeScript
- **IPC**: Wails bindings (auto-generated TS from Go structs) + `runtime.EventsEmit` for push
- **Providers**: Claude Code CLI (NDJSON over stdio) + Codex app-server (JSON-RPC 2.0 over stdio)

## Core Principles

- Go is triage + pipe. No orchestration engine, no event sourcing, no in-memory read models.
- Provider process is the source of truth during a turn. Don't duplicate its state.
- Persist per-item on completion, not per-turn. SQLite is a history cache, not an event store.
- Frontend memory bounded by visible thread. Heavy payloads (diffs, command output) stored in SQLite, loaded on demand.
- Errors are user-facing state, not log entries.

## Package Layout

- `main.go` / `app.go` — Wails entry point and bound struct
- `internal/provider/` — Provider process management, stdio protocols
- `internal/triage/` — Event classification, route to frontend vs SQLite
- `internal/store/` — SQLite: threads, items, payloads
- `frontend/src/` — Svelte 5 app
- `frontend/src/lib/` — Svelte components

## Commands

- `wails dev` — Run in dev mode (hot reload frontend + Go)
- `wails build` — Production build
- `cd frontend && npm run check` — Svelte/TypeScript checking
- `cd frontend && npx vite build` — Frontend-only build
- `go build ./...` — Go-only build check

## Conventions

- Go: `internal/` for all non-main packages. No `pkg/`.
- Svelte: Runes only (`$state`, `$derived`, `$effect`, `$props`). No legacy syntax.
- Tailwind: v4 CSS-native config via `@theme` in `app.css`. No `tailwind.config.js`.
- Events flow Go → frontend via `runtime.EventsEmit`. Frontend calls Go via bindings in `wailsjs/go/`.
- Provider-specific code stays in provider-specific packages. Don't force a unified abstraction.

## Origin

Informed by the `forge` repo (Node.js/React/Effect) which proved out the UX and provider handling. This is a ground-up rewrite optimizing for performance, memory efficiency, and minimal code.
