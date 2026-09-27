package claude

import (
	"encoding/json"
	"testing"
)

func TestReadIntAtAnyKey(t *testing.T) {
	for _, tc := range []struct {
		data string
		n    int
		ok   bool
	}{
		{`{"exitCode":2}`, 2, true},
		{" \t\n{\"exit_code\":3,\"exitCode\":4}", 3, true},
		{`[{"other":1},{"exitCode":5}]`, 5, true},
		{"\r\n[{\"exit_code\":6}]", 6, true},
		{`{"exitCode":"7"}`, 0, false},
		{`{"exitCode":1.5,"exit_code":8}`, 8, true},
		{`"exitCode: 9"`, 0, false},
		{`12`, 0, false},
		{`null`, 0, false},
		{`{"exitCode":`, 0, false},
		{``, 0, false},
		{`   `, 0, false},
	} {
		n, ok := readIntAtAnyKey(json.RawMessage(tc.data), "exit_code", "exitCode")
		if n != tc.n || ok != tc.ok {
			t.Errorf("readIntAtAnyKey(%q) = %d, %v; want %d, %v", tc.data, n, ok, tc.n, tc.ok)
		}
	}
}

// A value that cannot hold a key, such as a tool result's output text, is
// answered without decoding it.
func TestReadIntAtAnyKeyDoesNotDecodeANonContainer(t *testing.T) {
	for _, data := range []string{`"plain tool output text"`, `12`, `null`, ` `} {
		raw := json.RawMessage(data)
		allocs := testing.AllocsPerRun(100, func() { readIntAtAnyKey(raw, "exit_code", "exitCode") })
		if allocs != 0 {
			t.Errorf("readIntAtAnyKey(%q) allocs = %v, want 0", data, allocs)
		}
	}
}
