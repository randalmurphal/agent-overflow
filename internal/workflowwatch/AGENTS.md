# Workflow transition watch buffer

This package provides the bounded process-local ring used by workflow long
polls. SQLite remains authoritative for run state and park causes.

- `Hub` must have a usable zero value.
- `Record` runs on the engine command loop and must not perform I/O.
- Sequence zero means no cursor and can never identify a recorded transition.
- Report a gap when a cursor falls before retained entries or after the current
  sequence.
- Keep wire projection and event emission in `internal/app`.
