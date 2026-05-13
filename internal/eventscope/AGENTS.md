# internal/eventscope/

`ThreadIDFromEvent(payload)` peels the per-thread scope out of an
arbitrary event payload flowing through the App's transport bus. Used
by the observability fan-out in `app_emit.go` so emitted events can
be tagged onto the originating thread's replay log without every
event-emitting site building a separate scope envelope.

## Surface

| Symbol | Purpose |
|---|---|
| `ThreadIDFromEvent(data any) string` | Best-effort lookup. Tries `map[string]any` / `map[string]string` first, then reflection on a struct (or pointer-to-struct) for an exported `ThreadID` string field, then a JSON round-trip as a final fallback. Trims whitespace before returning. Returns `""` when no id is present. |

## Responsibility boundary

- What BELONGS here: payload-shape-tolerant scope extraction. The
  helper deliberately doesn't depend on any concrete event type.
- What does NOT belong here: emission. The App layer owns when and
  where to emit; this package only extracts attribution from payloads
  it didn't author.

## Anti-patterns

- Do NOT introduce a list of "known event types" here. The whole
  point of the JSON-fallback branch is that anonymous struct literals
  declared next to bound methods get attributed without needing to
  thread a registration through this package.
- Do NOT import the provider package. The test uses a local
  field-shape stand-in (`providerLikeEvent`) to keep this dep-free.
