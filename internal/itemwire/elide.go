package itemwire

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
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

// drop removes the leaf from its container. An object member is
// deleted outright — an absent key is unambiguous, and every consumer
// in the frontend already type-checks the value it reads, so it falls
// through to its own fallback. An array element is replaced by null
// rather than removed, because `input.files[3]` is addressed by index
// and re-packing the slice would silently re-point every later reader.
func (l leafRef) drop() {
	if l.parentMap != nil {
		delete(l.parentMap, l.key)
		return
	}
	l.parentSlice[l.index] = nil
}

// freed reports the bytes this drop takes off the wire. An object
// member also frees its quoted key, the colon and the separating
// comma; an array element leaves a `null` behind.
func (l leafRef) freed() int {
	if l.parentMap != nil {
		return l.size + len(l.key) + 4
	}
	return l.size - 4
}

// elideLargestLeaves drops leaf values from the decoded JSON value
// `raw` until `need` bytes have been freed, largest first, and returns
// the re-encoded value with the paths it dropped (sorted, stable).
//
// Two rules make this safe to point at provider-shaped data nobody has
// an inventory of:
//
//   - Object keys, array indices and nesting are preserved, so a
//     consumer reading a sub-field it cares about either finds the same
//     value it always found or finds the key absent. A projection that
//     truncated the encoded JSON text instead would hand the frontend a
//     string that no longer parses, which is a defect, not a smaller
//     payload.
//   - Only leaves at or above `floor` are candidates. That is a SIZE
//     rule, not a field allowlist: every rendering consumer of a
//     `meta.input` leaf caps its own output well below the floor
//     (frontend `metaInputLeafRenderCaps.test.ts` pins the inventory),
//     so no leaf small enough to be rendered whole is ever a candidate.
//     A byte cap carries no field inventory to decay, which is exactly
//     why remote-access.md §14 prefers it to an allowlist projection.
//
// `retain` names paths that stay whatever their size. It is empty for
// every caller but the one documented at retainedCommandPath.
//
// The budget SKIPS, it does not break: a candidate that is already
// dropped or ineligible never stops the walk over later candidates, so
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

	dropped := make([]string, 0, 4)
	for _, leaf := range candidates {
		if need <= 0 {
			break
		}
		leaf.drop()
		dropped = append(dropped, leaf.path)
		need -= leaf.freed()
	}
	if len(dropped) == 0 {
		return raw, nil
	}
	out, err := json.Marshal(tree)
	if err != nil {
		// Re-encoding a value that just decoded cannot fail in
		// practice; if it ever does, the complete value is strictly
		// better than a half-projected one.
		return raw, nil
	}
	sort.Strings(dropped)
	return out, dropped
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
