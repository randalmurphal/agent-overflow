package sessionfork

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// simple3TurnsJSONL: 3 user prompts + 3 assistant responses, no tools,
// no sidechains. Used for the "happy path" slice tests.
const simple3TurnsJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","timestamp":"2026-01-01T00:00:01.000Z","cwd":"/tmp/x","gitBranch":"main","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src","timestamp":"2026-01-01T00:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"second reply"}]}}
{"type":"user","uuid":"u3","parentUuid":"a2","sessionId":"src","timestamp":"2026-01-01T00:00:05.000Z","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a3","parentUuid":"u3","sessionId":"src","timestamp":"2026-01-01T00:00:06.000Z","message":{"role":"assistant","content":[{"type":"text","text":"third reply"}]}}
`

func TestBuildForkLines_FullClone(t *testing.T) {
	newID, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(simple3TurnsJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	if newID == "" {
		t.Fatalf("expected non-empty newID")
	}
	// 6 transcript entries + 1 custom-title (no content-replacement input).
	if got, want := len(lines), 7; got != want {
		t.Fatalf("lines=%d, want %d", got, want)
	}
	// Verify every transcript line carries the new sessionId.
	for i, line := range lines[:6] {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if got, _ := e["sessionId"].(string); got != newID {
			t.Errorf("line %d sessionId=%q, want %q", i, got, newID)
		}
		// forkedFrom stamp.
		ff, _ := e["forkedFrom"].(map[string]any)
		if got, _ := ff["sessionId"].(string); got != "src" {
			t.Errorf("line %d forkedFrom.sessionId=%q, want src", i, got)
		}
		// isSidechain explicitly set to false.
		if got, _ := e["isSidechain"].(bool); got {
			t.Errorf("line %d isSidechain=true, want false", i)
		}
	}
}

func TestBuildForkLines_SliceAtMidpoint(t *testing.T) {
	// Slice up to u2 inclusive — should have u1, a1, u2 (3 entries) + custom-title.
	newID, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(simple3TurnsJSONL), "src", "u2", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	if got, want := len(lines), 4; got != want {
		t.Fatalf("lines=%d, want %d", got, want)
	}

	// Verify the parent chain is well-formed: u1 has no parent, a1's parent
	// is the new uuid for u1, u2's parent is the new uuid for a1.
	uuidByOldUUID := make(map[string]string, 3)
	originalUUIDs := []string{"u1", "a1", "u2"}
	for i, line := range lines[:3] {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		ff, _ := e["forkedFrom"].(map[string]any)
		oldID, _ := ff["messageUuid"].(string)
		if oldID != originalUUIDs[i] {
			t.Errorf("line %d forkedFrom.messageUuid=%q, want %q", i, oldID, originalUUIDs[i])
		}
		newU, _ := e["uuid"].(string)
		uuidByOldUUID[oldID] = newU
	}

	// Verify chain: a1.parent → u1 (new), u2.parent → a1 (new).
	var a1Entry, u2Entry map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &a1Entry)
	_ = json.Unmarshal([]byte(lines[2]), &u2Entry)
	if got, _ := a1Entry["parentUuid"].(string); got != uuidByOldUUID["u1"] {
		t.Errorf("a1.parentUuid=%q, want %q", got, uuidByOldUUID["u1"])
	}
	if got, _ := u2Entry["parentUuid"].(string); got != uuidByOldUUID["a1"] {
		t.Errorf("u2.parentUuid=%q, want %q", got, uuidByOldUUID["a1"])
	}

	// Verify newID is exposed and consistent.
	if newID != a1Entry["sessionId"] {
		t.Errorf("newID=%q does not match line sessionId=%v", newID, a1Entry["sessionId"])
	}
}

func TestBuildForkLines_MessageNotFound(t *testing.T) {
	_, _, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(simple3TurnsJSONL), "src", "no-such-uuid", "")
	if !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("err=%v, want ErrMessageNotFound", err)
	}
}

func TestBuildForkLines_EmptySession(t *testing.T) {
	_, _, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(""), "src", "", "")
	if !errors.Is(err, ErrSessionEmpty) {
		t.Fatalf("err=%v, want ErrSessionEmpty", err)
	}
}

const sidechainJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","isSidechain":false,"message":{"role":"user","content":"main"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}
{"type":"user","uuid":"sub-u1","parentUuid":null,"sessionId":"src","isSidechain":true,"message":{"role":"user","content":"subagent question"}}
{"type":"assistant","uuid":"sub-a1","parentUuid":"sub-u1","sessionId":"src","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent reply"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src","isSidechain":false,"message":{"role":"user","content":"after subagent"}}
`

func TestBuildForkLines_FiltersSidechains(t *testing.T) {
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(sidechainJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	// 3 main-chain entries + custom-title, sidechain entries dropped.
	if got, want := len(lines), 4; got != want {
		t.Fatalf("lines=%d, want %d (sidechain entries should be dropped)", got, want)
	}
	for i, line := range lines[:3] {
		var e map[string]any
		_ = json.Unmarshal([]byte(line), &e)
		ff, _ := e["forkedFrom"].(map[string]any)
		oldID, _ := ff["messageUuid"].(string)
		if strings.HasPrefix(oldID, "sub-") {
			t.Errorf("line %d kept sidechain uuid %s", i, oldID)
		}
	}
}

const contentReplacementJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"hi"}}
{"type":"content-replacement","sessionId":"src","replacements":[{"old":"a","new":"b"},{"old":"c","new":"d"}],"uuid":"cr1","timestamp":"2026-01-01T00:00:01.000Z"}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
{"type":"content-replacement","sessionId":"other-session","replacements":[{"old":"x","new":"y"}],"uuid":"cr2","timestamp":"2026-01-01T00:00:02.000Z"}
`

func TestBuildForkLines_ReEmitsContentReplacement(t *testing.T) {
	newID, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(contentReplacementJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	// Expected: u1, a1, content-replacement (only the one targeting "src"), custom-title = 4
	if got, want := len(lines), 4; got != want {
		t.Fatalf("lines=%d, want %d", got, want)
	}
	var crEntry map[string]any
	_ = json.Unmarshal([]byte(lines[2]), &crEntry)
	if got, _ := crEntry["type"].(string); got != "content-replacement" {
		t.Fatalf("expected content-replacement entry at index 2, got type=%q", got)
	}
	if got, _ := crEntry["sessionId"].(string); got != newID {
		t.Errorf("content-replacement sessionId=%q, want newID=%q", got, newID)
	}
	reps, _ := crEntry["replacements"].([]any)
	if got, want := len(reps), 2; got != want {
		t.Errorf("replacements=%d, want %d (only src-targeted should pass through)", got, want)
	}
}

const progressJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"hi"}}
{"type":"progress","uuid":"p1","parentUuid":"u1","sessionId":"src"}
{"type":"progress","uuid":"p2","parentUuid":"p1","sessionId":"src"}
{"type":"assistant","uuid":"a1","parentUuid":"p2","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"yo"}]}}
`

func TestBuildForkLines_ProgressEntriesSkippedInOutputButUsedInChain(t *testing.T) {
	newID, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(progressJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	// 2 writable entries (u1, a1) + custom-title = 3. Progress dropped.
	if got, want := len(lines), 3; got != want {
		t.Fatalf("lines=%d, want %d (progress entries should be dropped from output)", got, want)
	}
	// a1's parentUuid should resolve to u1's NEW uuid, not the progress
	// entry's uuid (which is dropped). Walk the chain.
	var u1, a1 map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &u1)
	_ = json.Unmarshal([]byte(lines[1]), &a1)
	if got, _ := a1["parentUuid"].(string); got != u1["uuid"] {
		t.Errorf("a1.parentUuid=%q, want u1.uuid=%v (chain skipped progress)", got, u1["uuid"])
	}
	_ = newID
}

const truncatedJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src","message":{"role":"user","cont`

func TestBuildForkLines_ToleratesTruncatedFinalLine(t *testing.T) {
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(truncatedJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	// Truncated last line silently dropped; should still produce u1+a1+title.
	if got, want := len(lines), 3; got != want {
		t.Fatalf("lines=%d, want %d", got, want)
	}
}

func TestBuildForkLines_StripsLeakageFields(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","teamName":"team-a","agentName":"alice","slug":"old-slug","sourceToolAssistantUUID":"x","message":{"role":"user","content":"hi"}}` + "\n"
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	var e map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &e)
	for _, k := range stripFields {
		if _, present := e[k]; present {
			t.Errorf("forked entry retained leakage field %q", k)
		}
	}
}

func TestBuildForkLines_PreservesUnknownFields(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","cwd":"/repo","gitBranch":"main","version":"1.2.3","customField":"keep-me","message":{"role":"user","content":"hi"}}` + "\n"
	_, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	var e map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &e)
	// Assert exact values, not just presence — a regression that
	// re-serialized through a different type or coerced to interface{}
	// would still pass a presence-only check.
	wantValues := map[string]string{
		"cwd":         "/repo",
		"gitBranch":   "main",
		"version":     "1.2.3",
		"customField": "keep-me",
	}
	for k, want := range wantValues {
		got, ok := e[k].(string)
		if !ok {
			t.Errorf("forked entry dropped or retyped %q (got %#v)", k, e[k])
			continue
		}
		if got != want {
			t.Errorf("forked entry mutated %q: got %q, want %q", k, got, want)
		}
	}
}

// TestWriteForkFileForLastKeptTurn_AtTranscriptEnd pins the
// recoverable boundary that the revert pipeline relies on: asking
// for the slice point past the last persisted user prompt returns
// the wrapped ErrUserTurnAtTranscriptEnd sentinel (and still matches
// ErrUserTurnOutOfRange for any broad callers).
func TestWriteForkFileForLastKeptTurn_AtTranscriptEnd(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(srcPath, []byte(simple3TurnsJSONL), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// simple3TurnsJSONL has 3 real prompts. lastKeptTurn=2 asks for
	// the slice point at the 3rd user prompt's end-of-turn — that's
	// FindUUIDBeforeUserTurn(3) under the hood (lastKept+1), exactly
	// one past the last persisted prompt.
	_, _, _, err := WriteForkFileForLastKeptTurn(srcPath, 2, "")
	if !errors.Is(err, ErrUserTurnAtTranscriptEnd) {
		t.Fatalf("err=%v, want ErrUserTurnAtTranscriptEnd", err)
	}
	if !errors.Is(err, ErrUserTurnOutOfRange) {
		t.Fatalf("err=%v should still match ErrUserTurnOutOfRange for broad callers", err)
	}
}

// TestWriteForkFileFullTranscript_HappyPath covers the slice-at-EOF
// fallback the revert pipeline routes to when the anchor is missing
// from the JSONL: clone every transcript entry under a fresh session
// id, returning a new file the caller can adopt as SessionRef. The
// uuidMap covers every transcript entry so the same helpers that
// remap UUIDs for normal forks continue to work.
func TestWriteForkFileFullTranscript_HappyPath(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(srcPath, []byte(simple3TurnsJSONL), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	newID, newPath, uuidMap, err := WriteForkFileFullTranscript(srcPath, "")
	if err != nil {
		t.Fatalf("WriteForkFileFullTranscript: %v", err)
	}
	if newID == "" {
		t.Fatal("expected non-empty new session id")
	}
	if newPath == srcPath {
		t.Fatalf("new path = src path %q, want a fresh sibling file", srcPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected new transcript at %s: %v", newPath, err)
	}
	// All six original transcript entries should be remapped. The
	// helper is byUuid so we just check the count — value content is
	// covered by the BuildForkLines suite above.
	if got, want := len(uuidMap), 6; got != want {
		t.Errorf("uuidMap entries = %d, want %d", got, want)
	}
	// The new file must carry the new sessionId on every transcript
	// line so `claude --resume <newID>` finds it.
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read new transcript: %v", err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		entryType, _ := e["type"].(string)
		// Custom-title rows carry sessionId on a different shape; skip
		// the strict assertion for those and rely on the BuildForkLines
		// tests to cover them.
		if entryType == "" {
			continue
		}
		if got, _ := e["sessionId"].(string); got != newID {
			t.Errorf("line %d sessionId=%q, want %q", i, got, newID)
		}
	}
}

func TestBuildForkLines_RejectsEmptyUUID(t *testing.T) {
	// An entry with empty uuid would collide in the uuid map. Verify
	// it's silently dropped rather than corrupting the chain.
	src := `{"type":"user","uuid":"","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"second"}}
`
	newID, lines, _, err := BuildForkLinesWithUUIDMap(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLinesWithUUIDMap: %v", err)
	}
	if newID == "" {
		t.Fatal("expected non-empty newID")
	}
	// Only u1 + custom-title should appear (empty-uuid entry skipped).
	if got, want := len(lines), 2; got != want {
		t.Errorf("lines = %d, want %d (empty-uuid entry should be dropped)", got, want)
	}
}
