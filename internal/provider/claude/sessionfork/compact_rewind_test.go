package sessionfork

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compactEchoContent mirrors the command-input metadata echo the CLI
// persists for a `/compact` send (captured from claude 2.1.219).
const compactEchoContent = "<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"

// successfulCompactJSONL mirrors the post-/compact session layout
// captured from a real 2.1.219 run: the compact_boundary is a
// parentUuid:null chain ROOT carrying logicalParentUuid → the
// pre-compact leaf, and the command echo's ancestors are the
// compaction's effects (boundary → summary → caveat).
const successfulCompactJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"build the feature"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"system","subtype":"compact_boundary","uuid":"cb","parentUuid":null,"logicalParentUuid":"a1","sessionId":"src","content":"Conversation compacted"}
{"type":"user","uuid":"sum","parentUuid":"cb","sessionId":"src","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation..."}}
{"type":"user","uuid":"cav","parentUuid":"sum","sessionId":"src","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>Caveat: local commands</local-command-caveat>"}}
{"type":"user","uuid":"echo","parentUuid":"cav","sessionId":"src","message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"}}
{"type":"user","uuid":"out","parentUuid":"echo","sessionId":"src","message":{"role":"user","content":"<local-command-stdout>Compacted</local-command-stdout>"}}
`

func writeSessionFixture(t *testing.T, jsonl string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// forkedSourceUUIDs runs WriteForkFileForUserMessageUUID and returns
// the forkedFrom.messageUuid provenance of every transcript row in the
// fork, in file order (custom-title excluded).
func forkedSourceUUIDs(t *testing.T, srcPath, anchorUUID string) []string {
	t.Helper()
	_, newPath, _, err := WriteForkFileForUserMessageUUID(srcPath, anchorUUID, "")
	if err != nil {
		t.Fatalf("WriteForkFileForUserMessageUUID: %v", err)
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read fork: %v", err)
	}
	var sources []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse fork line: %v", err)
		}
		if t, _ := e["type"].(string); t == "custom-title" {
			continue
		}
		ff, _ := e["forkedFrom"].(map[string]any)
		u, _ := ff["messageUuid"].(string)
		sources = append(sources, u)
	}
	return sources
}

// Reverting to the /compact echo of a SUCCESSFUL compaction must slice
// at the boundary's logicalParentUuid (the pre-compact leaf), cutting
// the boundary, summary, and caveat — otherwise the fork keeps the
// compacted provider state while the caller's timeline deletes the
// compaction divider.
func TestWriteForkFileForUserMessageUUID_CompactEchoRewindsToPreCompactLeaf(t *testing.T) {
	src := writeSessionFixture(t, successfulCompactJSONL)
	got := forkedSourceUUIDs(t, src, "echo")
	want := []string{"u1", "a1"}
	if len(got) != len(want) {
		t.Fatalf("fork rows=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fork rows=%v, want %v", got, want)
		}
	}
}

// A canceled compaction writes no compact_boundary, so the echo's
// ancestor walk hits a real content row and the anchor stays at the
// echo's parent — the pre-fix behavior, which is correct there.
func TestWriteForkFileForUserMessageUUID_CanceledCompactKeepsParentAnchor(t *testing.T) {
	const canceled = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"build the feature"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"user","uuid":"cav","parentUuid":"a1","sessionId":"src","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>Caveat: local commands</local-command-caveat>"}}
{"type":"user","uuid":"echo","parentUuid":"cav","sessionId":"src","message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"}}
`
	src := writeSessionFixture(t, canceled)
	got := forkedSourceUUIDs(t, src, "echo")
	want := []string{"u1", "a1", "cav"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fork rows=%v, want %v", got, want)
	}
}

// A NORMAL user message sent after a compaction must not rewind — the
// user is reverting their own message, and the compacted state is the
// correct context to keep.
func TestWriteForkFileForUserMessageUUID_PostCompactMessageDoesNotRewind(t *testing.T) {
	postCompact := successfulCompactJSONL +
		`{"type":"user","uuid":"u2","parentUuid":"out","sessionId":"src","message":{"role":"user","content":"next question"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}
`
	src := writeSessionFixture(t, postCompact)
	got := forkedSourceUUIDs(t, src, "u2")
	want := []string{"u1", "a1", "cb", "sum", "cav", "echo", "out"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fork rows=%v, want %v", got, want)
	}
}

// A boundary without a usable logicalParentUuid (absent, empty, or
// pointing outside this transcript) falls back to the echo's parent —
// degrading to the pre-fix slice instead of failing the revert.
func TestWriteForkFileForUserMessageUUID_CompactRewindFallbacks(t *testing.T) {
	cases := []struct {
		name        string
		boundaryRow string
	}{
		{
			name:        "no logicalParentUuid",
			boundaryRow: `{"type":"system","subtype":"compact_boundary","uuid":"cb","parentUuid":null,"sessionId":"src","content":"Conversation compacted"}`,
		},
		{
			name:        "null logicalParentUuid",
			boundaryRow: `{"type":"system","subtype":"compact_boundary","uuid":"cb","parentUuid":null,"logicalParentUuid":null,"sessionId":"src","content":"Conversation compacted"}`,
		},
		{
			name:        "logicalParentUuid not in transcript",
			boundaryRow: `{"type":"system","subtype":"compact_boundary","uuid":"cb","parentUuid":null,"logicalParentUuid":"gone","sessionId":"src","content":"Conversation compacted"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jsonl := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"build the feature"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
` + tc.boundaryRow + `
{"type":"user","uuid":"sum","parentUuid":"cb","sessionId":"src","isCompactSummary":true,"message":{"role":"user","content":"summary"}}
{"type":"user","uuid":"cav","parentUuid":"sum","sessionId":"src","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>Caveat</local-command-caveat>"}}
{"type":"user","uuid":"echo","parentUuid":"cav","sessionId":"src","message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"}}
`
			src := writeSessionFixture(t, jsonl)
			got := forkedSourceUUIDs(t, src, "echo")
			want := []string{"u1", "a1", "cb", "sum", "cav"}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("fork rows=%v, want %v", got, want)
			}
		})
	}
}

func TestCompactCommandSliceAnchor_Table(t *testing.T) {
	transcript := parseFixture(t, successfulCompactJSONL)
	byUUID := make(map[string]map[string]any, len(transcript))
	for _, e := range transcript {
		u, _ := e["uuid"].(string)
		byUUID[u] = e
	}

	echoWith := func(content any) map[string]any {
		return map[string]any{
			"type":       "user",
			"uuid":       "echo",
			"parentUuid": "cav",
			"message":    map[string]any{"role": "user", "content": content},
		}
	}

	cases := []struct {
		name     string
		anchored map[string]any
		parent   string
		want     string
	}{
		{
			name:     "compact echo rewinds to pre-compact leaf",
			anchored: byUUID["echo"],
			parent:   "cav",
			want:     "a1",
		},
		{
			name:     "different command does not rewind",
			anchored: echoWith("<command-name>/usage</command-name>\n<command-message>usage</command-message>"),
			parent:   "cav",
			want:     "cav",
		},
		{
			name:     "plain prompt does not rewind",
			anchored: echoWith("please /compact this"),
			parent:   "cav",
			want:     "cav",
		},
		{
			name: "array-content echo rewinds",
			anchored: echoWith([]any{
				map[string]any{"type": "text", "text": compactEchoContent},
			}),
			parent: "cav",
			want:   "a1",
		},
		{
			name:     "attachment-shaped anchor does not rewind",
			anchored: map[string]any{"type": "attachment", "uuid": "att", "parentUuid": "cav"},
			parent:   "cav",
			want:     "cav",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactCommandSliceAnchor(transcript, tc.anchored, tc.parent); got != tc.want {
				t.Fatalf("anchor=%q, want %q", got, tc.want)
			}
		})
	}
}

func parseFixture(t *testing.T, jsonl string) []map[string]any {
	t.Helper()
	transcript, _, err := parseTranscript(strings.NewReader(jsonl), "src")
	if err != nil {
		t.Fatalf("parseTranscript: %v", err)
	}
	return transcript
}
