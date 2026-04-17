# Data Flow

How provider output becomes visible state.

## Pipeline

```
Provider stdout
  │
  ├── small event (delta, notification, approval)
  │     └── app.Event.Emit → Frontend
  │
  └── heavy payload (diff, command output, thinking)
        ├── extract preview/stats → payload.meta
        ├── write meta + full content → SQLite payloads
        └── app.Event.Emit meta only → Frontend

  Item completes → INSERT into items (frozen)

Frontend "expand" click → Wails binding → SQLite payload read → render
```

## Item Lifecycle: Append-Only with Mutable Head

```
[frozen] [frozen] [frozen] ... [active item(s)]
└── in SQLite, immutable ─────┘ └── in frontend $state only
```

- Completed items are never modified once written to SQLite.
- The currently streaming item is frontend-only reactive state.
- On completion: write to SQLite → frontend replaces mutable state with the
  frozen version.

## Persistence Rule

**Persist per item, not per turn.** Each completed item writes immediately.
Crash loss is bounded to the currently active item (seconds of work, not
minutes). On recovery, resume the provider session and reconcile against
SQLite.

## Background Tasks

A backgrounded command produces two timeline items:

- `background_started` — lightweight marker, rendered in the tray.
- `background_completed` — rich result card, appended at the completion
  position.

The tray is frontend state only. Background output accumulates in Go and
flushes to SQLite on completion as a payload. No real-time delta streaming
for background items.

## Memory Model

- **Go**: flat. Only the event currently being triaged is in memory.
  No caching.
- **Frontend**: bounded by one thread's items + payload meta. ~1 MB typical.
  Thread switch is a full state replacement, not accumulation.
- **SQLite on disk**: grows indefinitely. Designed to handle hundreds of
  threads with thousands of items.

## Triage Classification

See `internal/triage/AGENTS.md` for the routing table. The short form:

- Text deltas, tool notifications, approvals → passthrough to frontend.
- Diffs → SQLite + meta to frontend.
- Command output → SQLite + meta to frontend.
- Thinking blocks → SQLite + preview to frontend.
- Turn metadata (cost, tokens) → inline to frontend + persist on thread.
- Errors → distinct event type; frontend renders as status/alert.
