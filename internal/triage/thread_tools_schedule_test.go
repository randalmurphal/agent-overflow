package triage

import (
	"encoding/json"
	"testing"
)

func TestStampScheduledNextFireMarksOnlyThreadToolSchedules(t *testing.T) {
	t.Parallel()
	const reply = `{"kind":"loop","label":"board","next_fire":"2026-09-19T12:10:00Z"}`
	const loop = `{"mcp":{"server":"ao-thread-tools","tool":"thread_loop"},"input":{"label":"board"}}`
	var got map[string]any
	if err := json.Unmarshal([]byte(stampScheduledNextFire(loop, reply, false, 1789812000000)), &got); err != nil {
		t.Fatal(err)
	}
	if got["next_fire_at"] != float64(1789819800000) || got["next_fire_from"] != float64(1789812000000) {
		t.Fatalf("stamped meta = %v", got)
	}
	if _, ok := got["input"]; !ok {
		t.Fatalf("the stamp dropped the row's other keys: %v", got)
	}
	for _, tc := range []struct {
		name, meta, reply string
		failed            bool
	}{
		{"a failed call", loop, reply, true},
		{"another server's tool", `{"mcp":{"server":"elsewhere","tool":"thread_loop"}}`, reply, false},
		{"a native tool", `{"input":{"command":"ls"}}`, reply, false},
		{"a stop reply", loop, `{"label":"board","state":"cancelled"}`, false},
	} {
		if out := stampScheduledNextFire(tc.meta, tc.reply, tc.failed, 1789812000000); out != tc.meta {
			t.Errorf("%s: meta changed to %s", tc.name, out)
		}
	}
}
