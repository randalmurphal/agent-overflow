# Architecture

## Overview

Agent Overflow is a desktop app (Go + Wails + Svelte 5) for using coding agents
(Claude Code, Codex). Both providers run as subprocesses communicating over stdio.
Go is a thin triage/pipe layer. Svelte owns rendering. SQLite is the history cache.

## Provider Model

Both providers are subprocesses:
- **Claude Code CLI**: one process per active thread. Spawned with
  `--output-format stream-json --input-format stream-json --verbose`.
  NDJSON over stdin/stdout. User authenticates via their Claude subscription
  (CLI handles OAuth).
- **Codex app-server**: one process per active thread (matching forge's
  proven model). JSON-RPC 2.0 over stdin/stdout.

The provider process is the source of truth during a turn. We don't duplicate
its state. Provider session files (`~/.claude/`, `~/.codex/`) are the
authoritative history for crash recovery.

## Data Flow

```
Provider stdout
  │
  ├── small event (delta, notification, approval) → EventsEmit → Frontend
  │
  └── heavy payload (diff, command output, thinking)
        ├── extract preview/stats → meta
        ├── store meta + full content → SQLite payloads table
        └── EventsEmit meta only → Frontend
        
  Item completes → write to SQLite items table

Frontend click "expand" → Wails binding → SQLite payload read → render
```

## Persistence Model

**Persist per-item as items complete, not per-turn.**

During a turn, the provider emits item lifecycle events (started → deltas →
completed). Each completed item is immediately written to SQLite. This limits
crash-loss to the currently active item (seconds of work, not minutes).

On crash recovery: resume the provider session (both support this), reconcile
any missed items by comparing provider history against SQLite.

## Item Model: Append-Only with Mutable Head

```
[frozen] [frozen] [frozen] ... [active item(s)]
└── in SQLite, immutable ─────┘ └── in frontend $state only
```

- Completed items are never modified once written to SQLite.
- The currently streaming item is frontend-only reactive state.
- On completion: write to SQLite → replace mutable state with frozen version.

## Background Tasks

A backgrounded command produces two timeline items:
- `background_started`: lightweight marker, rendered in the background tray
- `background_completed`: rich result card, appended at completion position

The tray is frontend state only (list of running backgrounds). Background
output accumulates in Go and flushes to SQLite on completion as a payload.
No real-time delta streaming for background items — just progress in the tray.

## SQLite Schema (3 tables)

```sql
threads   — id, title, provider, session_ref, workspace_path, timestamps, archived
items     — id, thread_id, turn_index, item_index, kind, role, summary, payload_id, timestamps
payloads  — id, kind, meta (JSON preview/stats), data (BLOB full content), timestamps
```

- `threads`: loaded for sidebar. Lightweight.
- `items`: loaded per-thread. `summary` column has always-loaded content.
- `payloads`: on-demand only. `meta` is loaded with items, `data` on user request.

## Memory Model

- **Go**: flat. Only the current event being triaged is in memory. No caching.
- **Frontend**: bounded by one thread's items + payload meta. ~1MB typical.
  Thread switch = full state replacement, not accumulation.
- **SQLite on disk**: grows indefinitely. Handles hundreds of threads with
  thousands of items without issue.

## Process Management

- Codex: one `codex app-server` process per active thread.
- Claude: one `claude` CLI process per active thread. Inactive threads have
  no process; spawn on resume.
- Approval requests are bidirectional: provider → Go → frontend → user →
  Go → provider stdin. Tracked by request ID.

## Triage Layer (Go)

Events are classified by type, not size:
- Text deltas, tool notifications, approvals → passthrough to frontend
- Diffs → always to SQLite + meta to frontend
- Command output → always to SQLite + meta to frontend
- Thinking blocks → always to SQLite + preview to frontend
- Turn metadata (cost, tokens) → inline to frontend + persist
- Errors → emit as distinct event type, frontend renders as status/alert

## Guiding Principles

1. Provider process is the source of truth during a turn.
2. Go is triage + pipe. No event sourcing, no derived state projection.
   Exception: lightweight coordination (deliberation turn tracking, design
   option flow) lives in Go when it brokers between multiple provider
   processes and the frontend. This is coordination, not orchestration.
3. One mutable head, everything else frozen.
4. SQLite is a history cache, not an event store. Provider sessions are authoritative.
5. Frontend memory bounded by visible thread. Heavy payloads always on-demand.
6. Errors are user-facing state, not log entries.
7. Process model matches the provider — don't abstract real differences.
8. Persist per-item on completion, not per-turn.
9. Project ≠ workspace. A project is the git repo / root directory. A workspace
   is where the provider operates — either the project root (local mode) or a
   separate git worktree. Threads track both.

## Stack

- Go 1.25, Wails v2.12 (system webview, no Chromium)
- Svelte 5 (runes), Vite 8 (Rolldown), Tailwind CSS 4, TypeScript
- SQLite (WAL mode) for persistence
- IPC: Wails bindings (Go→TS auto-generated) + EventsEmit (push)
