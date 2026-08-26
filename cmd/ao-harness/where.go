package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// whereMatcher is a `--where dotted.path=value` filter over an event
// payload. Deliberately tiny: a wait filter is nearly always "this field
// equals this string", and a query language would be a second thing to
// learn for a case that does not arise. Anything richer is what
// `events tail | jq` is for.
type whereMatcher struct {
	path  []string
	value string
}

// parseWhere reads one `path=value` clause. The value may be empty
// ("--where error=") which matches an empty string, so the split is on
// the FIRST '=' only — a JSON value can contain one.
func parseWhere(clause string) (whereMatcher, error) {
	path, value, found := strings.Cut(clause, "=")
	if !found {
		return whereMatcher{}, usagef("--where %q is not path=value", clause)
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return whereMatcher{}, usagef("--where %q names no field", clause)
	}
	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if segment == "" {
			return whereMatcher{}, usagef("--where %q has an empty path segment", clause)
		}
	}
	return whereMatcher{path: segments, value: value}, nil
}

// match reports whether the payload's field at the dotted path equals
// the clause's value.
//
// Comparison is on the value's TEXT: `--where turn=2` matches the number
// 2 and `--where done=true` matches the boolean, because a shell has no
// types and requiring `--where turn='2'` to mean something different
// from `--where turn=2` would be a trap. A JSON string compares by its
// contents, without the quotes.
func (w whereMatcher) match(data json.RawMessage) bool {
	value, ok := lookupPath(data, w.path)
	if !ok {
		return false
	}
	text, ok := jsonScalarText(value)
	if !ok {
		return false
	}
	return text == w.value
}

// lookupPath walks object keys. Arrays are addressed by decimal index,
// so `--where units.0.state=done` works without a syntax of its own.
func lookupPath(data json.RawMessage, path []string) (json.RawMessage, bool) {
	current := data
	for _, segment := range path {
		if len(current) == 0 {
			return nil, false
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(current, &object); err == nil {
			next, ok := object[segment]
			if !ok {
				return nil, false
			}
			current = next
			continue
		}
		var array []json.RawMessage
		if err := json.Unmarshal(current, &array); err != nil {
			return nil, false
		}
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= len(array) {
			return nil, false
		}
		current = array[index]
	}
	return current, true
}

// jsonScalarText renders a JSON scalar the way a shell would spell it.
// Objects and arrays have no text form a `=` comparison could mean, so
// they never match.
func jsonScalarText(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "", false
	}
	switch trimmed[0] {
	case '{', '[':
		return "", false
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		return s, true
	default:
		// Numbers, booleans and null compare as written. A number keeps
		// the wire's own spelling rather than a float round-trip, so 2
		// stays "2" and never becomes "2e+00".
		return trimmed, true
	}
}
