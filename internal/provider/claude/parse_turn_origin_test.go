package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

func wakeMirrorLine(uuid, body string) []byte {
	row := map[string]any{
		"type": "transcript_mirror", "filePath": "/p/s.jsonl",
		"entries": []any{map[string]any{
			"type": "user", "isSidechain": false, "uuid": uuid, "promptSource": "system",
			"turnOrigin": "task_notification", "message": map[string]any{"role": "user", "content": body},
		}},
	}
	out, _ := json.Marshal(row)
	return out
}

func backgroundWakes(t *testing.T, p *Parser, lines ...string) []provider.ProviderEvent {
	t.Helper()
	var out []provider.ProviderEvent
	for _, line := range lines {
		events, err := p.ParseLine(testThread, []byte(line))
		if err != nil {
			t.Fatalf("ParseLine(%.60s): %v", line, err)
		}
		for _, evt := range events {
			if evt.Kind == provider.EventNotification && strings.Contains(string(evt.Meta), backgroundWakeNotificationKind) {
				out = append(out, evt)
			}
		}
	}
	return out
}

const (
	resultLine       = `{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s"}`
	monitorEventBody = "<task-notification>\n<task-id>bwatch002</task-id>\n<summary>Monitor event: \"deploy log errors\"</summary>\n<event>ERROR step 3</event>\n</task-notification>"
	agentStopBody    = "<task-notification>\n<task-id>a1</task-id>\n<tool-use-id>toolu_1</tool-use-id>\n<status>completed</status>\n<summary>Agent \"Check the release notes\" finished</summary>\n<result>The notes cover every change.</result>\n</task-notification>"
)

// A background report that starts a turn on an idle session (claude-wire.md
// §E7) becomes one wake row naming the task, with the report or event line
// as its detail. The same row injected mid-turn is the replay echo's to
// report, so nothing is emitted for it here.
func TestTaskNotificationMirrorRowWakesOnlyBetweenTurns(t *testing.T) {
	p := NewParser()
	wakes := backgroundWakes(t, p,
		resultLine,
		// A background agent's forwarded output does not end the idle window.
		`{"type":"stream_event","parent_tool_use_id":"toolu_bg","event":{"type":"message_start","message":{"id":"m"}}}`,
		`{"type":"system","subtype":"init","session_id":"s","model":"m","cwd":"/"}`,
		string(wakeMirrorLine("wake-1", monitorEventBody)),
		string(wakeMirrorLine("wake-2", agentStopBody)),
	)
	if len(wakes) != 2 {
		t.Fatalf("wakes = %d, want 2", len(wakes))
	}
	for i, want := range []struct{ summary, detail, id string }{
		{`Monitor event: "deploy log errors"`, "ERROR step 3", "background-wake:wake-1"},
		{`Agent "Check the release notes" finished`, "The notes cover every change.", "background-wake:wake-2"},
	} {
		var meta map[string]any
		_ = json.Unmarshal(wakes[i].Meta, &meta)
		if wakes[i].Content != want.summary || meta["detail"] != want.detail || wakes[i].ItemID != want.id {
			t.Fatalf("wake %d = %q %v %s, want %q %q %s", i, wakes[i].Content, meta, wakes[i].ItemID, want.summary, want.detail, want.id)
		}
	}

	for name, opener := range map[string]string{
		"assistant":       `{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
		"stream start":    `{"type":"stream_event","event":{"type":"message_start","message":{"id":"m3"}}}`,
		"lifecycle start": `{"type":"command_lifecycle","command_uuid":"c1","state":"started"}`,
	} {
		p := NewParser()
		if got := backgroundWakes(t, p, resultLine, opener, string(wakeMirrorLine("mid", monitorEventBody))); len(got) != 0 {
			t.Fatalf("%s: a mid-turn notification row produced a wake: %+v", name, got)
		}
	}
	// Before the session's first result there is no idle session to wake.
	if got := backgroundWakes(t, NewParser(), string(wakeMirrorLine("early", monitorEventBody))); len(got) != 0 {
		t.Fatalf("a row before any turn ended produced a wake: %+v", got)
	}
}

func TestBackgroundWakeDetailIsBounded(t *testing.T) {
	long := strings.Repeat("é", maxBackgroundWakeDetailRunes*2)
	got := truncateWakeDetail(long)
	if n := len([]rune(got)); n != maxBackgroundWakeDetailRunes || !strings.HasSuffix(got, "…") {
		t.Fatalf("detail is %d runes (suffix %q), want %d ending in an ellipsis", n, got[len(got)-3:], maxBackgroundWakeDetailRunes)
	}
}
