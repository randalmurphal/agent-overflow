# internal/uitrace/

Dev-only JSONL render-trace appender used by the frontend's debug
console to capture UI state for after-the-fact inspection of visual
glitches.

## Layout

- `uitrace.go` — `Tracer` with `New(configDir)`, `Path()`, and
  `Append(lines)`. `Append` validates each line (per-line cap, JSON
  shape) and the batch (line count + byte cap), then writes under a
  process-local mutex. The file is rotated to `<path>.1` when the next
  append would push it past `MaxFileBytes`.

## Responsibility boundary

- What BELONGS here: validation, batching limits, rotation, and the
  atomic append. The on-disk shape (`<configDir>/ui-trace/ui-render.jsonl`)
  and the JSONL format are deliberately exported as constants so any
  tool that tails the file can resolve the path.
- What does NOT belong here: Wails binding wiring (`app_ui_trace.go`
  delegates), frontend serialisation, or any production-path tracing —
  this surface is dev-only by design and the bindings are exposed
  unconditionally because the caps make the cost negligible if the
  frontend ever stops batching.

## Anti-patterns

- Do NOT break the file layout (`DirName` / `FileName`) or the JSONL
  format — tools that tail the file depend on both.
- Do NOT silently truncate or drop oversized lines/batches. Returning
  an error keeps the misbehaviour visible to the caller instead of
  burying it in a partial write.
- Do NOT add a "graceful fallback" for an empty `configDir`. `New`
  errors loudly so callers can't accidentally start tracing into the
  process working directory.
