package adf

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// key and valueEqual compare attribute values the way their JSON
// encoding does, without encoding them.
func TestValuesCompareAsJSON(t *testing.T) {
	vals := []any{
		nil, "", "a", `a"b`, `a\b`, "�", "��", true, false, 0, 1, 2, -1, 0.0, math.Copysign(0, -1),
		1.0, 2.0, 2.5, 1e15, 1e16, 1e21, 1e-7, -3.0, math.NaN(),
		math.Inf(1), []any(nil), []any{}, []any{1.0, "x"}, []any{1, "x"},
		map[string]any(nil), map[string]any{},
		map[string]any{"a": 1, "b": "c"}, map[string]any{"b": "c", "a": 1.0},
		int64(2), json.Number("2"), []string{"x"},
		map[string]any{"a": []any{map[string]any{"z": nil}}},
	}
	for _, a := range vals {
		for _, b := range vals {
			ja, ea := json.Marshal(a)
			jb, eb := json.Marshal(b)
			want := ea == nil && eb == nil && string(ja) == string(jb)
			var ka, kb strings.Builder
			writeKeyValue(&ka, a)
			writeKeyValue(&kb, b)
			if (ka.String() == kb.String()) != want && ea == nil && eb == nil {
				t.Errorf("key(%#v) vs key(%#v): %q, %q", a, b, ka.String(),
					kb.String())
			}
			if valueEqual(a, b) != want {
				t.Errorf("valueEqual(%#v, %#v) = %v", a, b, !want)
			}
		}
	}
}
