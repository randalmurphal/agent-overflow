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

## Wire Projection

**The stored row is complete; the copy a client receives is bounded.**
Every path that hands items to a client — the slice/cursor pagers,
`SyncThreadWindow`, live item upserts and patches — passes them through
`internal/itemwire` first. It drops values that are large and paint
nothing on arrival: oversized `meta.input` leaves, and inline diff
preview patches a client did not ask for. Whatever it removes, it names
in a typed marker on the row, and `GetThreadItemProjectionSource`
returns the stored value for a card that needs it back.

Two rules keep this from becoming a second storage shape:

- It is a **projection, not a truncation**. Object structure, keys, and
  array indices survive, so a consumer reading a sub-field finds either
  the value it always found or an absent key — never a JSON string that
  no longer parses. `internal/itemmeta` owns shaping on the persist
  path; this owns nothing there.
- The marker is a **render-time signal**. A fetched value is never
  merged back into the row, so a row cached in L1 or the IndexedDB
  replica can never masquerade as complete: a row is elided if and only
  if it says so.

## Background Tasks

A backgrounded command produces two timeline items:

- `background_started`: lightweight marker, rendered in the tray.
- `background_completed`: rich result card, appended at the completion
  position.

The tray is frontend state only. Background output accumulates in Go and
flushes to SQLite on completion as a payload. No real-time delta streaming
for background items.

## The Second Writer: Session Import

Triage is not the only thing that writes timeline rows. Session import
(`internal/sessionimport`) reads a provider's own session file off disk and
writes the same `items` / `payloads` / `turns` / `usage_ledger` rows for
history that already happened. It is the only writer that does not have a
live provider process behind it, which is what the differences follow from:

- **It bypasses `triage.Router` on purpose.** The Router has live-only side
  effects (session-ref updates, thread-activity bumps, `now()`-stamped usage,
  async settle goroutines). The importer reuses triage's *exported, Router-free
  shaping helpers* instead, so one definition of "what row does this event
  become" serves both. `internal/sessionimport/parity_test.go` drives one
  synthetic wire sequence per provider through both writers and asserts the
  rows match.
- **Nothing stamps `time.Now()`.** Every row carries the provider's own clock,
  end to end, including `turns.completed_at`.
- **It writes a whole session in one transaction** (`store.ApplyImportBatch`),
  not per item. The persist-per-item rule above exists to bound crash loss
  during a live turn; an import has nothing in flight to lose, and a 400-row
  session costs one fsync instead of 400. A failure part-way leaves no
  half-imported thread.
- **It does not bump `threads.updated_at` or thread activity.** Floating every
  imported thread to the top of the sidebar would contradict the timestamps it
  just wrote.

Where a live session's provider process is the source of truth for the turn,
an imported thread's source of truth is the session FILE, which keeps
growing after the import. `PlanUpdate` / `ApplyUpdate` re-read the tail from
the cursor in `thread_import_state`, and refuse when the thread has since been
resumed inside AO (the timeline and the file are then two different futures).
See `internal/sessionimport/AGENTS.md`.

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
- Command output → SQLite + meta to frontend, buffered per flush window (100ms / 64KB / lifecycle boundary).
- Thinking blocks → SQLite + preview to frontend.
- Turn metadata (cost, tokens) → inline to frontend + persist on thread.
- Errors → distinct event type; frontend renders as status/alert.
