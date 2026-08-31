# internal/uitrace/

JSONL diagnostic appenders for the frontend. Two channels share the
validation, caps, and rotation machinery:

- the dev-only render trace (`ui-render.jsonl`) behind the frontend's
  debug console, for after-the-fact inspection of visual glitches;
- the always-on frontend runtime-error log (`frontend-errors.jsonl`)
  fed by the global `error` / `unhandledrejection` handlers, so render
  exceptions are diagnosable without devtools open (a silent render
  throw also permanently leaks Svelte deriveds — see
  `ReportFrontendErrorBatch` in `app_observability.go`).

## Layout

- `uitrace.go` defines `Tracer` with `New(configDir)` (render trace),
  `NewErrors(configDir)` (error log), `Path()`, and `Append(lines)`.
  `Append` validates each line (per-line cap, JSON shape) and the
  batch (line count + byte cap), then writes under a process-local
  mutex. The file is rotated to `<path>.1` when the next append would
  push it past `MaxFileBytes`.

## Responsibility boundary

- What BELONGS here: validation, batching limits, rotation, and the
  atomic append. The on-disk shapes
  (`<configDir>/ui-trace/ui-render.jsonl`,
  `<configDir>/ui-trace/frontend-errors.jsonl`) and the JSONL format
  are deliberately exported as constants so any tool that tails the
  files can resolve the paths.
- What does NOT belong here: Wails binding wiring (`app_observability.go`
  delegates) and frontend serialisation. The render trace stays
  dev-only by design; the error log is the one always-on channel, and
  both bindings are exposed unconditionally because the caps make the
  cost negligible if the frontend ever stops batching.

## Anti-patterns

- Do NOT break the file layout (`DirName` / `FileName` /
  `ErrorFileName`) or the JSONL format. Tools that tail the files
  depend on both.
- Do NOT silently truncate or drop oversized lines/batches. Returning
  an error keeps the misbehaviour visible to the caller instead of
  burying it in a partial write.
- Do NOT add a "graceful fallback" for an empty `configDir`. `New`
  errors loudly so callers can't accidentally start tracing into the
  process working directory.
