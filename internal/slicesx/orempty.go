package slicesx

// OrEmpty returns s when it is non-nil, otherwise an allocated empty
// slice. Used at JSON-encoding boundaries where the wire shape models
// the field as a non-null array — `json.Marshal(nil)` renders as
// `null`, forcing every downstream caller to add a defensive
// coalesce. Returning `[]T{}` lets the marshaller emit `[]`.
//
// The returned slice shares storage with the input when non-nil; the
// nil-case allocation is unavoidable.
func OrEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
