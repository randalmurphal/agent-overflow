# internal/importir/

The neutral vocabulary the session importer speaks across the provider
boundary. Provider readers emit `[]Event`; `internal/sessionimport` turns
those into store rows.

## Surface

| Symbol | Purpose |
|---|---|
| `Event` | `provider.ProviderEvent` plus `SourceUUID` / `SourceOffset` — the coordinates of the session-file line the event was read from. |
| `Warning` | `{Code, Message}` — a non-fatal finding surfaced next to the session it concerns. |

`Event` embeds the live-session event shape on purpose: the import writer
builds store rows from the same struct triage does, so an imported thread
and a thread AO ran itself have the same row shapes. What the embedded
event cannot carry is provenance, which is the whole reason this type
exists — `items.meta` records the source uuid, and a refresh needs to know
where the previous import stopped.

The two coordinates answer different questions and are **not**
alternatives:

- **`SourceUUID` is provenance, and it is REQUIRED of both providers** on
  every item-producing event. Claude hands over the transcript row's own
  `uuid`. A Codex rollout record has no id of its own, so its reader mints
  `line:<byte offset of the line's first byte>` — stable for the life of an
  append-only file, unique within it, and prefixed so it can never be
  mistaken for a number to do arithmetic on. The writer refuses an event
  without one; `items.meta.import_source_uuid` is where it lands.
- **`SourceOffset` is an optional resume position** — the byte offset one
  past the line's terminating newline. Codex sets it because a tail read
  from an offset is the cheap refresh (and a file that SHRANK is the signal
  that the source diverged); Claude leaves it zero, because a transcript is
  a uuid DAG and a byte offset says nothing about position in the
  conversation.

A reader that set only `SourceOffset` would be refused. That is
deliberate: two spellings of the same coordinate would disagree the moment
one line produced several events.

## Responsibility boundary

- What BELONGS here: types both halves of the importer need to agree on.
- What does NOT belong here: anything that reads a file, writes a row, or
  knows a provider's wire format. Provider packages own their readers;
  `internal/sessionimport` owns the store side.

## Anti-patterns

- Do NOT let this package import `store`, `triage`, or a provider
  subpackage. Its import list is stdlib + `internal/provider`, and that is
  what lets `internal/provider/claude/...` depend on it without acquiring a
  path back to SQLite (see `internal/CLAUDE.md` — provider packages never
  import store or triage).
- Do NOT grow it into a second event model. A shape both providers already
  express as a `ProviderEvent` belongs on `provider.ProviderEvent`, not in
  a parallel type here.
