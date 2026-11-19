package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

func ownPromptParser(issued ...string) *Parser {
	session := &Session{}
	for _, uuid := range issued {
		session.noteIssuedCommandUUID(uuid)
	}
	parser := NewParser()
	parser.peerTurns = session
	return parser
}

func parseOne(t *testing.T, parser *Parser, line string) []provider.ProviderEvent {
	t.Helper()
	events, err := parser.ParseLine(testThread, []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v\n%s", err, line)
	}
	return events
}

func userTextMeta(t *testing.T, evt provider.ProviderEvent) map[string]any {
	t.Helper()
	if evt.Kind != provider.EventUserText {
		t.Fatalf("kind = %s, want %s", evt.Kind, provider.EventUserText)
	}
	var meta map[string]any
	if err := json.Unmarshal(evt.Meta, &meta); err != nil {
		t.Fatalf("meta: %v", err)
	}
	return meta
}

// Shapes from the 2.1.284 spike (AO's flag set): the turn-opening prompt's
// main-transcript entry, a tool result, and the interrupt sentinel the CLI
// writes in the same file.
const (
	mirrorOwnPrompt = `{"type":"transcript_mirror","filePath":"/tmp/s.jsonl","entries":[` +
		`{"type":"queue-operation","operation":"dequeue"},` +
		`{"parentUuid":"leaf-1","isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"text","text":"see ` + "`<task-notification>`" + `"}]},"uuid":"u-own","promptSource":"sdk"},` +
		`{"type":"attachment","isSidechain":false,"uuid":"att-1","attachment":{"type":"date"}}]}`
	mirrorCLIEntries = `{"type":"transcript_mirror","filePath":"/tmp/s.jsonl","entries":[` +
		`{"parentUuid":"a-1","isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"x"}]},"uuid":"u-tool"},` +
		`{"parentUuid":"u-tool","isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]},"uuid":"u-sentinel"}]}`
)

func TestTranscriptMirrorConfirmsOwnPrompt(t *testing.T) {
	events := parseOne(t, ownPromptParser("u-own"), mirrorOwnPrompt)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want one confirm-only user text", events)
	}
	meta := userTextMeta(t, events[0])
	if meta["provider_item_id"] != "u-own" || meta["parent_uuid"] != "leaf-1" || meta[provider.MetaUserTextTranscriptRecordedKey] != true {
		t.Fatalf("meta = %v", meta)
	}
	// The same fingerprint the echo of these blocks carries.
	want := EchoBlockDigest(json.RawMessage(`[{"type":"text","text":"see ` + "`<task-notification>`" + `"}]`))
	got, _ := meta[provider.MetaUserContentBlockDigestKey].([]any)
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("digest = %v, want %v", got, want)
	}
}

func TestTranscriptMirrorIgnoresEntriesThisAppDidNotIssue(t *testing.T) {
	if events := parseOne(t, ownPromptParser("u-own"), mirrorCLIEntries); len(events) != 0 {
		t.Fatalf("CLI-written user entries reported: %+v", events)
	}
	if events := parseOne(t, ownPromptParser(), mirrorOwnPrompt); len(events) != 0 {
		t.Fatalf("unissued prompt reported: %+v", events)
	}
	// No session ledger: nothing is provably ours.
	if events := parseOne(t, NewParser(), mirrorOwnPrompt); len(events) != 0 {
		t.Fatalf("prompt reported without a ledger: %+v", events)
	}
}

// An AO send that quotes a balanced injected wrapper or a peer wrapper is
// still the user's message; reclassifying its echo strands the send.
func TestReplayOfOwnSendIsNeverReclassifiedByContent(t *testing.T) {
	for name, text := range map[string]string{
		"task notification": "<task-notification><task-id>t1</task-id><status>completed</status></task-notification> what does this mean?",
		"peer wrapper":      `<cross-session-message from="uds:/tmp/x.sock">hi</cross-session-message> explain this`,
	} {
		t.Run(name, func(t *testing.T) {
			line, err := json.Marshal(map[string]any{
				"type": "user", "isReplay": true, "uuid": "u-own",
				"message": map[string]any{"role": "user", "content": text},
			})
			if err != nil {
				t.Fatal(err)
			}
			events := parseOne(t, ownPromptParser("u-own"), string(line))
			if len(events) != 1 {
				t.Fatalf("events = %+v, want the send's own echo", events)
			}
			meta := userTextMeta(t, events[0])
			if meta["provider_item_id"] != "u-own" || meta["cross_session_message"] != nil || events[0].Content != text {
				t.Fatalf("echo = %q meta %v", events[0].Content, meta)
			}

			// The same text under a uuid this app never issued keeps the
			// content classification.
			foreign := parseOne(t, ownPromptParser(), string(line))
			if len(foreign) == 1 && foreign[0].Kind == provider.EventUserText {
				if m := userTextMeta(t, foreign[0]); m["cross_session_message"] != true {
					t.Fatalf("foreign echo lost its classification: %v", m)
				}
			}
		})
	}
}

// One main-transcript batch can hold both kinds of row stdout lacks: the
// record of this app's prompt and a turn the CLI started itself. Each is
// reported, in batch order, and a row under an issued uuid is the app's
// own send whatever turnOrigin it carries.
func TestTranscriptMirrorReportsOwnPromptsAndCLITurnStartsTogether(t *testing.T) {
	line := `{"type":"transcript_mirror","filePath":"/tmp/s.jsonl","entries":[` +
		`{"parentUuid":"leaf-1","isSidechain":false,"type":"user","message":{"role":"user","content":"typed"},"uuid":"u-own","promptSource":"sdk"},` +
		`{"isSidechain":false,"type":"user","isMeta":true,"promptSource":"system","turnOrigin":"scheduled","uuid":"cron-1","message":{"role":"user","content":"tick"}},` +
		`{"isSidechain":false,"type":"user","turnOrigin":"scheduled","uuid":"u-own-2","message":{"role":"user","content":"also typed"}}]}`
	events := parseOne(t, ownPromptParser("u-own", "u-own-2"), line)
	if len(events) != 3 {
		t.Fatalf("events = %+v, want own prompt, scheduled turn, own prompt", events)
	}
	for i, want := range []struct {
		uuid, origin string
		recorded     bool
	}{
		{"u-own", "", true},
		{"cron-1", ScheduledTurnOrigin, false},
		{"u-own-2", "", true},
	} {
		meta := userTextMeta(t, events[i])
		if meta["provider_item_id"] != want.uuid {
			t.Fatalf("event %d uuid = %v, want %s", i, meta["provider_item_id"], want.uuid)
		}
		if origin, _ := meta["origin"].(string); origin != want.origin {
			t.Fatalf("event %d origin = %q, want %q", i, origin, want.origin)
		}
		if recorded := meta[provider.MetaUserTextTranscriptRecordedKey] == true; recorded != want.recorded {
			t.Fatalf("event %d transcript-recorded = %v, want %v", i, recorded, want.recorded)
		}
	}
}
