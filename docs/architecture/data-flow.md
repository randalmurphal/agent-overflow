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

  Item starts → INSERT streaming item and payload
  Deltas → live event plus bounded SQLite flushes
  Item completes → final flush and lifecycle settlement

Frontend "expand" click → Wails binding → SQLite payload read → render
```

## Item Lifecycle: Item-Granular with a Mutable Head

```
[settled] [settled] ... [streaming item(s)]
└──────────── all represented in SQLite ────────────┘
                         └── short delta flush windows in memory
```

- A streaming item and its payload are inserted when the block starts.
- Text and thinking deltas reach the frontend immediately and accumulate in a
  bounded persistence buffer. Command output is emitted when that buffer
  flushes.
- Time, byte, hydration, and lifecycle boundaries flush accumulated deltas to
  SQLite. Completion performs the final flush and settles the existing row.
- Later provider facts may still enrich a settled row through explicit update
  paths. Item-granular persistence does not mean immutable rows.

## Persistence Rule

**Persist per item, not per provider event or whole turn.** Canonical item and
payload rows are written at meaningful item boundaries. Streaming content is
flushed by bounded time and byte thresholds instead of writing every delta or
waiting for completion. On recovery, resume the provider session and reconcile
against the latest persisted item state.

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

Background behavior differs by provider and tool. Launch, process liveness,
task completion, and tray state are separate facts; some rows stay
`status='running'` as historical launch markers while liveness settles
elsewhere. Command output can flush while the command is still active. See
[`turn-lifecycle.md`](turn-lifecycle.md) for the authoritative tool, task, and
turn contracts.

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
  not per item. The item-granular rule above exists to bound crash loss
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

- **Go**: bounded per-thread correlation, live-state, and stream-flush buffers.
  Canonical history remains in SQLite.
- **Frontend**: bounded by one thread's items + payload meta. ~1 MB typical.
  Thread switch is a full state replacement, not accumulation.
- **SQLite on disk**: grows indefinitely. Designed to handle hundreds of
  threads with thousands of items.

## Triage Classification

See [`triage-routing.md`](triage-routing.md) for the routing table. The short
form:

- Text and thinking deltas → live frontend events plus bounded SQLite flushes.
- Explicitly live-only notifications and approvals → frontend event channels.
- Diffs → SQLite + meta to frontend.
- Command output → buffered SQLite + item upsert flushes (100ms / 64KB /
  lifecycle boundary).
- Turn metadata (cost, tokens) → live event plus the appropriate turn or usage
  rows.
- Errors → distinct event type; frontend renders as status/alert.
