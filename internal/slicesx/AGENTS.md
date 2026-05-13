# internal/slicesx/

Tiny stdlib-only slice helpers that don't fit the stdlib's `slices`
package. Companion to `internal/stringsx` for non-string-only slice
operations.

## Surface

- `OrEmpty[T any](s []T) []T` — coalesces nil to an allocated empty
  slice. Used at JSON-encoding boundaries where the wire shape models
  the field as a non-null array — `json.Marshal(nil)` renders as
  `null`, forcing every downstream caller to add a defensive
  coalesce. Returning `[]T{}` lets the marshaller emit `[]`.

## Anti-patterns

- Do NOT add helpers that mutate the input slice. The semantics here
  are "thin marshaling adapter"; mutation belongs elsewhere.
- Do NOT add helpers that allocate when they could share storage.
  `OrEmpty` returns the input when it's non-nil; allocation only
  fires on the nil case.
