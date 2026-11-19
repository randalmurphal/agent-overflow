package triage

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func aoResultOf(t *testing.T, meta string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(meta), &got); err != nil {
		t.Fatalf("stamped meta is not JSON: %v", err)
	}
	result, _ := got["ao_result"].(map[string]any)
	return result
}

func TestStampAoToolResultRecordsWhatTheReplySaid(t *testing.T) {
	t.Parallel()
	const spawn = `{"mcp":{"server":"ao-thread-tools","tool":"thread_spawn"},"input":{"prompt":"Write the notes."}}`
	for _, tc := range []struct {
		name, meta, reply string
		want              map[string]any
	}{
		{
			name:  "a spawn ack",
			meta:  spawn,
			reply: `{"token":"tok-1","kind":"spawn","thread_id":"t-1","title":"Release notes","state":"running","outcome":"backgrounded","notify":true,"note":"Still working."}`,
			want:  map[string]any{"token": "tok-1", "thread_id": "t-1", "title": "Release notes", "state": "running", "outcome": "backgrounded"},
		},
		{
			name:  "a reply names its sender",
			meta:  `{"mcp":{"server":"ao-thread-tools","tool":"thread_reply"}}`,
			reply: `{"token":"tok-2","accepted":true,"state":"replied","source_thread_id":"t-2","source_computer":"Desk"}`,
			want:  map[string]any{"token": "tok-2", "thread_id": "t-2", "computer": "Desk", "state": "replied"},
		},
		{
			name:  "a run start that waited",
			meta:  `{"mcp":{"server":"ao-workflow-tools","tool":"run_start"}}`,
			reply: `{"start":{"itemId":"run-1","workflowId":"digest","state":"queued"},"run":{"itemId":"run-1","workflowId":"digest","state":"running"},"outcome":"backgrounded"}`,
			want:  map[string]any{"run_id": "run-1", "workflow_id": "digest", "state": "running", "outcome": "backgrounded"},
		},
		{
			name:  "a run status",
			meta:  `{"mcp":{"server":"ao-workflow-tools","tool":"run_status"}}`,
			reply: `{"run":{"itemId":"run-1","workflowId":"digest","state":"completed"},"outcome":"resting"}`,
			want:  map[string]any{"run_id": "run-1", "workflow_id": "digest", "state": "completed", "outcome": "resting"},
		},
		{
			name:  "a bare run document",
			meta:  `{"mcp":{"server":"ao-workflow-tools","tool":"run_output"}}`,
			reply: `{"itemId":"run-1","state":"completed","outputs":{}}`,
			want:  map[string]any{"run_id": "run-1", "state": "completed"},
		},
	} {
		got := aoResultOf(t, StampAoToolResult(tc.meta, tc.reply, false, 1))
		if len(got) != len(tc.want) {
			t.Errorf("%s: ao_result = %v, want %v", tc.name, got, tc.want)
			continue
		}
		for key, want := range tc.want {
			if got[key] != want {
				t.Errorf("%s: ao_result.%s = %v, want %v", tc.name, key, got[key], want)
			}
		}
	}
}

func TestStampAoToolResultLeavesOtherRowsAlone(t *testing.T) {
	t.Parallel()
	const spawn = `{"mcp":{"server":"ao-thread-tools","tool":"thread_spawn"}}`
	const ack = `{"token":"tok-1","title":"Release notes","state":"running","outcome":"backgrounded"}`
	for _, tc := range []struct {
		name, meta, reply string
		failed            bool
	}{
		{"a failed call", spawn, ack, true},
		{"an empty reply", spawn, "", false},
		{"an error text", spawn, "[thread_not_found] No thread matches.", false},
		{"another server's tool", `{"mcp":{"server":"elsewhere","tool":"thread_spawn"}}`, ack, false},
		{"a native tool", `{"input":{"command":"ls"}}`, ack, false},
		{"a reply naming nothing the header shows", `{"mcp":{"server":"ao-thread-tools","tool":"thread_status"}}`, `{"requests":[{"token":"a","state":"running"}]}`, false},
	} {
		if out := StampAoToolResult(tc.meta, tc.reply, tc.failed, 1); out != tc.meta {
			t.Errorf("%s: meta changed to %s", tc.name, out)
		}
	}
}

func TestStampAoToolResultKeepsTheNextFireStamp(t *testing.T) {
	t.Parallel()
	const loop = `{"mcp":{"server":"ao-thread-tools","tool":"thread_loop"}}`
	out := StampAoToolResult(loop, `{"kind":"loop","label":"board","state":"running","next_fire":"2026-09-19T12:10:00Z"}`, false, 1789812000000)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["next_fire_at"] != float64(1789819800000) {
		t.Errorf("next_fire_at lost: %v", got)
	}
	if aoResultOf(t, out)["state"] != "running" {
		t.Errorf("ao_result missing beside next_fire_at: %v", got)
	}
}

func TestStampAoToolResultBoundsEveryString(t *testing.T) {
	t.Parallel()
	title := strings.Repeat("é", 4000)
	reply, _ := json.Marshal(map[string]any{"token": "t", "title": title, "state": "running"})
	got := aoResultOf(t, StampAoToolResult(`{"mcp":{"server":"ao-thread-tools","tool":"thread_spawn"}}`, string(reply), false, 1))
	stamped, _ := got["title"].(string)
	if utf8.RuneCountInString(stamped) != aoResultTextMax || !strings.HasSuffix(stamped, "…") {
		t.Errorf("title stamped at %d runes, want %d ending in an ellipsis", utf8.RuneCountInString(stamped), aoResultTextMax)
	}
}
