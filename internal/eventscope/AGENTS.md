# internal/eventscope/

Attribution extracted from an arbitrary event payload flowing through the
App's transport bus. `app_emit.go` derives it once per emit: the thread id
addresses a frame for the transport's watch filter and tags the thread's
replay log, and the scope root addresses a transcript frame to the
subagent scope it belongs to, without every emit site building an
envelope.

## Surface

| Symbol | Purpose |
|---|---|
| `ThreadIDFromEvent(data any) string` | Best-effort lookup. Tries `map[string]any` / `map[string]string` first, then reflection on a struct (or pointer-to-struct) for an exported `ThreadID` string field, then a JSON round-trip as a final fallback. Trims whitespace before returning. Returns `""` when no id is present. |
| `ScopeRootIDFromEvent(data any) string` | The `parentId` of the timeline row a payload is about: top-level `parentId` first, then `item.parentId`. Same lookup order as above; a struct declaring either field is answered by reflection, so root-scope frames never pay the JSON fallback. Returns `""` for a root-scope row or an unattributable payload. |

## Boundary

- What BELONGS here: payload-shape-tolerant attribution extraction. The
  helpers deliberately don't depend on any concrete event type.
- What does NOT belong here: emission, or deciding what an empty result
  means. The App layer owns when to derive and emit; the transport owns
  the fail-open reading of `""`.

## Constraints

- Do not introduce a list of "known event types" here. The whole
  point of the JSON-fallback branch is that anonymous struct literals
  declared next to bound methods get attributed without needing to
  thread a registration through this package.
- Do not import the provider package. The tests use local field-shape
  stand-ins (`providerLikeEvent`, `rowLike`) to keep this dep-free.
