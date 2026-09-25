package sessionfork

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// richSessionJSONL carries every row shape a slice keeps: a tool call and
// its result, a queued_command attachment consumed mid-loop, a compact
// boundary root with its logicalParentUuid backpointer, and plain turns
// on either side of the compaction.
const richSessionJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"build it"}}
{"type":"assistant","uuid":"a1-tool","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}
{"type":"user","uuid":"r1","parentUuid":"a1-tool","sessionId":"src","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"attachment","uuid":"qc1","parentUuid":"r1","sessionId":"src","attachment":{"type":"queued_command","prompt":[{"type":"text","text":"also this"}],"source_uuid":"ao-q1"}}
{"type":"assistant","uuid":"a1","parentUuid":"qc1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"system","subtype":"compact_boundary","uuid":"cb","parentUuid":null,"logicalParentUuid":"a1","sessionId":"src","content":"Conversation compacted"}
{"type":"user","uuid":"sum","parentUuid":"cb","sessionId":"src","isCompactSummary":true,"message":{"role":"user","content":"summary"}}
{"type":"user","uuid":"u2","parentUuid":"sum","sessionId":"src","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"second reply"}]}}
{"type":"user","uuid":"u3","parentUuid":"a2","sessionId":"src","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a3","parentUuid":"u3","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"third reply"}]}}
`

// transcriptRows reads a session file's transcript rows in file order.
func transcriptRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rows []map[string]any
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("%s line %d: %v", path, i, err)
		}
		typ, _ := e["type"].(string)
		if _, transcript := TranscriptTypes[typ]; transcript {
			rows = append(rows, e)
		}
	}
	return rows
}

// assertSliceKeepsSourceIdentity checks a written slice against its
// source file: the kept rows are wantKept in order, and each one keeps
// the source row's uuid, parentUuid and logicalParentUuid. Only session
// identity differs: the new sessionId and forkedFrom provenance naming
// the source session and the row's own uuid.
func assertSliceKeepsSourceIdentity(t *testing.T, srcPath, newID, newPath string, wantKept []string) {
	t.Helper()
	source := make(map[string]map[string]any)
	for _, row := range transcriptRows(t, srcPath) {
		id, _ := row["uuid"].(string)
		source[id] = row
	}
	rows := transcriptRows(t, newPath)
	var kept []string
	for _, row := range rows {
		id, _ := row["uuid"].(string)
		kept = append(kept, id)
		src, ok := source[id]
		if !ok {
			t.Errorf("slice row uuid %q does not exist in the source file", id)
			continue
		}
		if !reflect.DeepEqual(row["parentUuid"], src["parentUuid"]) {
			t.Errorf("row %s parentUuid = %v, want the source's %v", id, row["parentUuid"], src["parentUuid"])
		}
		gotLogical, gotHas := row["logicalParentUuid"]
		wantLogical, wantHas := src["logicalParentUuid"]
		if gotHas != wantHas || !reflect.DeepEqual(gotLogical, wantLogical) {
			t.Errorf("row %s logicalParentUuid = %v (present %v), want %v (present %v)", id, gotLogical, gotHas, wantLogical, wantHas)
		}
		if got, _ := row["sessionId"].(string); got != newID {
			t.Errorf("row %s sessionId = %q, want the slice's %q", id, got, newID)
		}
		wantFrom := map[string]any{"sessionId": SessionIDFromPath(srcPath), "messageUuid": id}
		if !reflect.DeepEqual(row["forkedFrom"], wantFrom) {
			t.Errorf("row %s forkedFrom = %v, want %v", id, row["forkedFrom"], wantFrom)
		}
	}
	if strings.Join(kept, ",") != strings.Join(wantKept, ",") {
		t.Fatalf("slice kept %v, want %v", kept, wantKept)
	}
}

// TestForkSlicesPreserveEveryUUIDAndParent pins the identity contract of
// every writer: a rollback slice, a fork slice and a whole clone mint a
// new session and keep every surviving row's uuid and parent chain, so
// provider ids AO stored against the source name the same rows in the
// new file.
func TestForkSlicesPreserveEveryUUIDAndParent(t *testing.T) {
	srcPath := writeSessionFixture(t, richSessionJSONL)
	throughA2 := []string{"u1", "a1-tool", "r1", "qc1", "a1", "cb", "sum", "u2", "a2"}
	cases := []struct {
		name     string
		write    func() (string, string, error)
		wantKept []string
	}{
		{"rollback slice before a user message", func() (string, string, error) {
			return WriteForkFileForUserMessageUUID(srcPath, "u3", "")
		}, throughA2},
		{"rollback slice before a queued message", func() (string, string, error) {
			return WriteForkFileForUserMessageUUID(srcPath, "ao-q1", "")
		}, []string{"u1", "a1-tool", "r1"}},
		{"fork slice at a turn boundary", func() (string, string, error) {
			return WriteForkFileForLastKeptTurn(srcPath, 0, "")
		}, []string{"u1", "a1-tool", "r1", "qc1", "a1", "cb", "sum"}},
		{"fork slice through an entry", func() (string, string, error) {
			return WriteForkFileThroughUUID(ForkCut{SourcePath: srcPath, LastKeptUUID: "a2"})
		}, throughA2},
		{"whole clone", func() (string, string, error) {
			return WriteForkFileFullTranscript(srcPath, "")
		}, append(append([]string(nil), throughA2...), "u3", "a3")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newID, newPath, err := tc.write()
			if err != nil {
				t.Fatalf("write slice: %v", err)
			}
			if newID == "" || newID == SessionIDFromPath(srcPath) {
				t.Fatalf("slice session id = %q, want a new one", newID)
			}
			assertSliceKeepsSourceIdentity(t, srcPath, newID, newPath, tc.wantKept)
		})
	}
}

// TestSliceOfSliceResolvesStoredIDs: ids stored against the original
// session keep anchoring through any chain of slices (a fork of a
// rollback, a rollback of a fork) with no rewrite in between.
func TestSliceOfSliceResolvesStoredIDs(t *testing.T) {
	srcPath := writeSessionFixture(t, richSessionJSONL)

	gen1ID, gen1, err := WriteForkFileForUserMessageUUID(srcPath, "u3", "")
	if err != nil {
		t.Fatalf("first slice: %v", err)
	}
	gen2ID, gen2, err := WriteForkFileForUserMessageUUID(gen1, "u2", "")
	if err != nil {
		t.Fatalf("second slice keyed by the source's u2: %v", err)
	}
	assertSliceKeepsSourceIdentity(t, gen1, gen2ID, gen2, []string{"u1", "a1-tool", "r1", "qc1", "a1", "cb", "sum"})

	gen3ID, gen3, err := WriteForkFileForUserMessageUUID(gen2, "ao-q1", "")
	if err != nil {
		t.Fatalf("third slice keyed by the queued message's send uuid: %v", err)
	}
	assertSliceKeepsSourceIdentity(t, gen2, gen3ID, gen3, []string{"u1", "a1-tool", "r1"})
	if gen1ID == gen2ID || gen2ID == gen3ID {
		t.Fatalf("slice generations share a session id: %s %s %s", gen1ID, gen2ID, gen3ID)
	}
}
