# Workflow memory

This package stores and renders durable notes that workflow agents explicitly
emit for later runs. This is product workflow memory, not a development log or
an instruction to update repository guides.

## Contracts

- Key storage by root run so descendants in one campaign share context while
  unrelated run trees remain isolated.
- Keep the note-kind vocabulary closed and ordered. Kind controls retention and
  rendering priority, so adding one requires updating validation, rendering,
  and tests together.
- Stamp provenance from trusted run context. CLI and envelope inputs must not be
  able to supply or override it.
- Enforce per-note, per-kind, and rendered-context bounds. Selection keeps all
  handoff notes first, then fills remaining space newest first; rendering groups
  by kind and keeps newest-first order within each group.
- Do not add implicit summarization or time-based expiry. Bounds and selection
  order are the aging policy.
- Treat the append-only JSONL file as recoverable data. `ReadNotes` skips and
  reports malformed lines; `Append` repairs a missing terminal newline before
  writing the next complete record.
- Keep run lookup, envelope extraction, CLI authorization, and prompt placement
  in their owning application and runner packages.
