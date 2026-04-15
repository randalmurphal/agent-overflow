# AGENTS.md

## Project: Agent Overflow

Go + Wails v2 + Svelte 5 desktop app for using coding agents (Codex, Claude).

## Stack

- **Backend**: Go 1.25, Wails v2.12 bindings + event system
- **Frontend**: Svelte 5 (runes), Vite 8 (Rolldown), Tailwind CSS 4, TypeScript
- **IPC**: Wails binding system (auto-generated TS from Go structs), `runtime.EventsEmit` for push
- **Architecture**: Event sourcing — commands validated by decider, events applied by projector, append-only event log

## Package Layout

- `main.go` / `app.go` — Wails entry point and bound struct
- `internal/domain/` — Domain types: events, commands, thread/message models
- `internal/orchestration/` — Engine (command dispatch + event store), decider (command validation), projector (read model)
- `internal/provider/` — Provider adapter interface and registry
- `internal/session/` — Session manager: tracks active provider sessions per thread
- `frontend/src/` — Svelte 5 app
- `frontend/src/lib/` — Svelte components

## Commands

- `wails dev` — Run in dev mode (hot reload frontend + Go)
- `wails build` — Production build
- `cd frontend && npm run check` — Svelte/TypeScript checking
- `cd frontend && npx vite build` — Frontend-only build
- `go build ./...` — Go-only build check

## Conventions

- Go: `internal/` for all non-main packages. No `pkg/` — nothing is intended for external import.
- Svelte: Runes only (`$state`, `$derived`, `$effect`, `$props`). No legacy `export let` or `on:event` syntax.
- Tailwind: v4 CSS-native config via `@theme` in `app.css`. No `tailwind.config.js`.
- Events flow Go → frontend via `runtime.EventsEmit`. Frontend calls Go via auto-generated bindings in `wailsjs/go/`.
- Domain events are the source of truth. Read models are derived projections.

## Origin

Ported from the `forge` repo (Node.js/React/Effect). The domain model, event semantics, and session lifecycle translate directly. The Go rewrite drops the JS/TS server, Effect runtime, and React rendering overhead in favor of native Go + compiled Svelte.
