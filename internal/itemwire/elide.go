package itemwire

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PathSeparator joins the segments of an elided leaf's path. Array
// indices are decimal segments, so `edits/0/new_string` reads the way
// the wire value nests.
const PathSeparator = "/"

// leafRef is one droppable value plus the container it hangs off, so a
// drop can remove a map key or blank an array slot without re-walking.
// Exactly one of parentMap / parentSlice is non-nil.
type leafRef struct {
	path        string
	size        int
	parentMap   map[string]any
	key         string
	parentSlice []any
	index       int
}

// clip shortens a string leaf to its first ClippedLeafBytes, cut on a
// rune boundary and closed with an ellipsis, and reports the bytes that
// took off the wire. The key and its position stay, so a reader that
// shows a bounded preview of the value (a header line, a prompt preview)
// still has the opening it shows; the ellipsis says the value goes on. A
// leaf that is not a string, which no real input reaches the floor with,
// is dropped instead (see drop).
func (l leafRef) clip() int {
	text, ok := l.value().(string)
	if !ok {
		freed := l.droppedBytes()
		l.drop()
		return freed
	}
	clipped := clipText(text, ClippedLeafBytes)
	l.set(clipped)
	return exactSize(text) - exactSize(clipped)
}

// drop removes the leaf from its container. An object member is
// deleted outright; an array element is replaced by null rather than
// removed, because `input.files[3]` is addressed by index and re-packing
// the slice would silently re-point every later reader. It is the
// fallback for a row still over budget once every candidate is clipped.
func (l leafRef) drop() {
	if l.parentMap != nil {
		delete(l.parentMap, l.key)
		return
	}
	l.parentSlice[l.index] = nil
}

// droppedBytes reports the bytes drop takes off the wire, measured from
// the value as it stands. An object member also frees its quoted key,
// the colon and the separating comma; an array element leaves a `null`
// behind.
func (l leafRef) droppedBytes() int {
	size := exactSize(l.value())
	if l.parentMap != nil {
		return size + exactSize(l.key) + 2
	}
	return size - 4
}

func (l leafRef) value() any {
	if l.parentMap != nil {
		return l.parentMap[l.key]
	}
	return l.parentSlice[l.index]
}

func (l leafRef) set(value any) {
	if l.parentMap != nil {
		l.parentMap[l.key] = value
		return
	}
	l.parentSlice[l.index] = value
}

// clipText is text's first max bytes, never splitting a UTF-8 sequence,
// followed by an ellipsis.
func clipText(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "\u2026"
}

// exactSize is a value's encoded length. Clip and drop measure exactly so
// the budget walk never believes it freed bytes it did not.
func exactSize(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// elideLargestLeaves clips leaf values in the decoded JSON value `raw`
// until `need` bytes have been freed, largest first, and returns the
// re-encoded value with the paths it elided (sorted, stable).
//
// Three rules make this safe to point at provider-shaped data nobody has
// an inventory of:
//
//   - Object keys, array indices and nesting are preserved, so a
//     consumer reading a sub-field it cares about finds the same value it
//     always found, or the value's opening closed with an ellipsis. A
//     projection that truncated the encoded JSON text instead would hand
//     the frontend a string that no longer parses, which is a defect, not
//     a smaller payload.
//   - Only leaves at or above `floor` are candidates. That is a SIZE
//     rule, not a field allowlist: every rendering consumer of a
//     `meta.input` leaf caps its own output well below the floor
//     (frontend `metaInputLeafRenderCaps.test.ts` pins the inventory),
//     so no leaf small enough to be rendered whole is ever a candidate.
//     A byte cap carries no field inventory to decay, which is exactly
//     why remote-access.md §14 prefers it to an allowlist projection.
//   - A candidate is clipped to ClippedLeafBytes, not removed, so a
//     capped reader (a header's first line, a prompt preview) renders
//     what it always rendered. A value is removed only when the row is
//     still over budget with every candidate clipped.
//
// `retain` names paths that stay whatever their size. It is empty for
// every caller but the one documented at retainedCommandPath.
//
// The budget SKIPS, it does not break: a candidate that is already
// elided or ineligible never stops the walk over later candidates, so
// one giant value cannot leave a row's other giant values on the wire.
func elideLargestLeaves(raw json.RawMessage, need, floor int, retain map[string]bool) (json.RawMessage, []string) {
	if need <= 0 || len(raw) == 0 {
		return raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Numbers stay literal: decoding 1755123456789 into a float64 and
	// re-encoding it would ship 1.755123456789e+12 to a consumer that
	// reads it as an id.
	dec.UseNumber()
	var tree any
	if dec.Decode(&tree) != nil {
		return raw, nil
	}

	var candidates []leafRef
	collectLeaves(tree, "", floor, retain, &candidates)
	if len(candidates) == 0 {
		return raw, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].size != candidates[j].size {
			return candidates[i].size > candidates[j].size
		}
		return candidates[i].path < candidates[j].path
	})

	// Clip first, largest first: most rows are over budget because of one
	// or two big values, and a clipped value keeps the opening its readers
	// show. Only a row still over budget once every candidate is clipped
	// (many large values, a MultiEdit's edit list) loses values outright,
	// so the ceiling holds however many candidates a row has.
	elided := make([]string, 0, 4)
	clipped := 0
	for _, leaf := range candidates {
		if need <= 0 {
			break
		}
		need -= leaf.clip()
		elided = append(elided, leaf.path)
		clipped++
	}
	for _, leaf := range candidates[:clipped] {
		if need <= 0 {
			break
		}
		if leaf.value() == nil {
			continue
		}
		need -= leaf.droppedBytes()
		leaf.drop()
	}
	if len(elided) == 0 {
		return raw, nil
	}
	out, err := json.Marshal(tree)
	if err != nil {
		// Re-encoding a value that just decoded cannot fail in
		// practice; if it ever does, the complete value is strictly
		// better than a half-projected one.
		return raw, nil
	}
	sort.Strings(elided)
	return out, elided
}

// collectLeaves walks the decoded tree appending every droppable leaf.
// Containers are never candidates: dropping an object or an array
// would take an unbounded number of sub-fields with it, and the
// largest-leaf walk reaches the bytes inside them anyway.
func collectLeaves(node any, path string, floor int, retain map[string]bool, out *[]leafRef) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			childPath := joinPath(path, key)
			// Retention covers the path AND everything under it. A
			// retained container is retained because a consumer reads
			// values inside it, so descending would drop exactly what
			// the retention exists to keep.
			if retain[childPath] {
				continue
			}
			if isContainer(child) {
				collectLeaves(child, childPath, floor, retain, out)
				continue
			}
			if size := encodedSize(child); size >= floor {
				*out = append(*out, leafRef{path: childPath, size: size, parentMap: value, key: key})
			}
		}
	case []any:
		for index, child := range value {
			childPath := joinPath(path, strconv.Itoa(index))
			if retain[childPath] {
				continue
			}
			if isContainer(child) {
				collectLeaves(child, childPath, floor, retain, out)
				continue
			}
			if size := encodedSize(child); size >= floor {
				*out = append(*out, leafRef{path: childPath, size: size, parentSlice: value, index: index})
			}
		}
	}
}

func isContainer(node any) bool {
	switch node.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

func joinPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	return prefix + PathSeparator + segment
}

// encodedSize is the wire cost of one leaf. Strings dominate and are
// measured without marshalling: the quoted form is the raw length plus
// two quotes plus whatever escaping adds, and undercounting escapes
// only makes the projection slightly conservative. Everything else is
// small and cheap to marshal exactly.
func encodedSize(node any) int {
	if text, ok := node.(string); ok {
		return len(text) + 2
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// hasKey reports whether a raw JSON object carries a non-empty value
// at `key`, without decoding the whole object.
func hasKey(raw string, key string) bool {
	if raw == "" || !strings.Contains(raw, `"`+key+`"`) {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return false
	}
	value, ok := obj[key]
	if !ok {
		return false
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	return len(value) > 0 && string(value) != "null"
}
