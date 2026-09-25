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
	newID, lines, err := BuildForkLines(strings.NewReader(simple3TurnsJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
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
	// Slice up to u2 inclusive: u1, a1, u2 (3 entries) + custom-title.
	newID, lines, err := BuildForkLines(strings.NewReader(simple3TurnsJSONL), "src", "u2", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
	}
	if got, want := len(lines), 4; got != want {
		t.Fatalf("lines=%d, want %d", got, want)
	}

	// Every kept row keeps its source uuid and parent chain.
	want := []struct{ uuid, parent string }{{"u1", ""}, {"a1", "u1"}, {"u2", "a1"}}
	for i, w := range want {
		var e map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if got, _ := e["uuid"].(string); got != w.uuid {
			t.Errorf("line %d uuid=%q, want %q", i, got, w.uuid)
		}
		if got, _ := e["parentUuid"].(string); got != w.parent {
			t.Errorf("line %d parentUuid=%q, want %q", i, got, w.parent)
		}
		if got, _ := e["sessionId"].(string); got != newID {
			t.Errorf("line %d sessionId=%q, want newID %q", i, got, newID)
		}
	}
}

func TestBuildForkLines_MessageNotFound(t *testing.T) {
	_, _, err := BuildForkLines(strings.NewReader(simple3TurnsJSONL), "src", "no-such-uuid", "")
	if !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("err=%v, want ErrMessageNotFound", err)
	}
}

func TestBuildForkLines_EmptySession(t *testing.T) {
	_, _, err := BuildForkLines(strings.NewReader(""), "src", "", "")
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
	_, lines, err := BuildForkLines(strings.NewReader(sidechainJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
	}
	// 3 main-chain entries + custom-title, sidechain entries dropped.
	if got, want := len(lines), 4; got != want {
		t.Fatalf("lines=%d, want %d (sidechain entries should be dropped)", got, want)
	}
	for i, line := range lines[:3] {
		var e map[string]any
		_ = json.Unmarshal([]byte(line), &e)
		if id, _ := e["uuid"].(string); strings.HasPrefix(id, "sub-") {
			t.Errorf("line %d kept sidechain uuid %s", i, id)
		}
	}
}

const contentReplacementJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"hi"}}
{"type":"content-replacement","sessionId":"src","replacements":[{"old":"a","new":"b"},{"old":"c","new":"d"}],"uuid":"cr1","timestamp":"2026-01-01T00:00:01.000Z"}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
{"type":"content-replacement","sessionId":"other-session","replacements":[{"old":"x","new":"y"}],"uuid":"cr2","timestamp":"2026-01-01T00:00:02.000Z"}
`

func TestBuildForkLines_ReEmitsContentReplacement(t *testing.T) {
	newID, lines, err := BuildForkLines(strings.NewReader(contentReplacementJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
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
	_, lines, err := BuildForkLines(strings.NewReader(progressJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
	}
	// 2 writable entries (u1, a1) + custom-title = 3. Progress dropped.
	if got, want := len(lines), 3; got != want {
		t.Fatalf("lines=%d, want %d (progress entries should be dropped from output)", got, want)
	}
	// a1's parentUuid resolves to u1, not the dropped progress entry.
	var a1 map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &a1)
	if got, _ := a1["parentUuid"].(string); got != "u1" {
		t.Errorf("a1.parentUuid=%q, want u1 (chain skipped progress)", got)
	}
}

const truncatedJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src","message":{"role":"user","cont`

func TestBuildForkLines_ToleratesTruncatedFinalLine(t *testing.T) {
	_, lines, err := BuildForkLines(strings.NewReader(truncatedJSONL), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
	}
	// Truncated last line silently dropped; should still produce u1+a1+title.
	if got, want := len(lines), 3; got != want {
		t.Fatalf("lines=%d, want %d", got, want)
	}
}

func TestBuildForkLines_StripsLeakageFields(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","teamName":"team-a","agentName":"alice","slug":"old-slug","sourceToolAssistantUUID":"x","message":{"role":"user","content":"hi"}}` + "\n"
	_, lines, err := BuildForkLines(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
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
	_, lines, err := BuildForkLines(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
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
	_, _, err := WriteForkFileForLastKeptTurn(srcPath, 2, "")
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
// id, returning a new file the caller can adopt as SessionRef.
func TestWriteForkFileFullTranscript_HappyPath(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(srcPath, []byte(simple3TurnsJSONL), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	newID, newPath, err := WriteForkFileFullTranscript(srcPath, "")
	if err != nil {
		t.Fatalf("WriteForkFileFullTranscript: %v", err)
	}
	if newID == "" {
		t.Fatal("expected non-empty new session id")
	}
	if newPath == srcPath {
		t.Fatalf("new path = src path %q, want a fresh sibling file", srcPath)
	}
	// All six transcript entries are cloned under their own uuids, and
	// every line carries the new sessionId so `claude --resume <newID>`
	// finds it.
	if got := strings.Join(keptUUIDs(t, newPath), ","); got != "u1,a1,u2,a2,u3,a3" {
		t.Errorf("cloned entries = %s, want u1,a1,u2,a2,u3,a3", got)
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read new transcript: %v", err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if got, _ := e["sessionId"].(string); got != newID {
			t.Errorf("line %d sessionId=%q, want %q", i, got, newID)
		}
	}
}

// keptUUIDs returns the uuid of every transcript row in a written fork,
// in file order. The fork keeps source uuids, so these are the fixture's
// own row names.
func keptUUIDs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fork: %v", err)
	}
	var kept []string
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		typ, _ := e["type"].(string)
		if _, transcript := TranscriptTypes[typ]; !transcript {
			continue
		}
		id, _ := e["uuid"].(string)
		kept = append(kept, id)
	}
	return kept
}

// queuedCommandJSONL mirrors the shape claude 2.1.205 persists when it
// consumes a queued message MID-LOOP (queued while a turn was running):
// a `type:"attachment"` entry with a CLI-minted uuid; the AO-minted send
// uuid survives only as `attachment.source_uuid`. The follow-up
// assistant entry parents to the attachment entry — it sits on the
// active branch. (Consumption at turn pickup instead persists a real
// `type:"user"` entry carrying the AO uuid verbatim, like u2 here.)
const queuedCommandJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}
{"type":"attachment","uuid":"qc1","parentUuid":"a1","sessionId":"src","isSidechain":false,"attachment":{"type":"queued_command","prompt":[{"type":"text","text":"queued follow-up"}],"source_uuid":"ao-queued-1","commandMode":"prompt","timestamp":"2026-01-01T00:00:03.000Z"}}
{"type":"assistant","uuid":"a2","parentUuid":"qc1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"queued reply"}]}}
{"type":"user","uuid":"u2","parentUuid":"a2","sessionId":"src","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a3","parentUuid":"u2","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"second reply"}]}}
`

// TestWriteForkFileForUserMessageUUID_QueuedCommandAttachment — reverting
// to a queued message the CLI consumed mid-loop. The AO uuid never
// appears as a `type:"user"` entry, only as the queued_command
// attachment's source_uuid; the slice must anchor there and keep
// exactly the rows before the attachment entry.
func TestWriteForkFileForUserMessageUUID_QueuedCommandAttachment(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(srcPath, []byte(queuedCommandJSONL), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	newID, newPath, err := WriteForkFileForUserMessageUUID(srcPath, "ao-queued-1", "")
	if err != nil {
		t.Fatalf("WriteForkFileForUserMessageUUID: %v", err)
	}
	if newID == "" || newPath == "" {
		t.Fatalf("newID=%q newPath=%q, want both non-empty", newID, newPath)
	}
	if kept := keptUUIDs(t, newPath); strings.Join(kept, ",") != "u1,a1" {
		t.Fatalf("kept entries = %v, want [u1 a1] (everything before the queued_command attachment)", kept)
	}
}

// TestParentUUIDForUserMessageUUID_MatchPreference — a direct
// `type:"user"` match wins over a queued_command attachment carrying
// the same source_uuid, and attachment types other than queued_command
// never match.
func TestParentUUIDForUserMessageUUID_MatchPreference(t *testing.T) {
	transcript := []map[string]any{
		{"type": "user", "uuid": "u1", "parentUuid": ""},
		{"type": "attachment", "uuid": "at1", "parentUuid": "u1",
			"attachment": map[string]any{"type": "queued_command", "source_uuid": "shared"}},
		{"type": "attachment", "uuid": "at2", "parentUuid": "at1",
			"attachment": map[string]any{"type": "other_kind", "source_uuid": "other-ao"}},
		{"type": "user", "uuid": "shared", "parentUuid": "at2"},
	}

	parent, err := parentUUIDForUserMessageUUIDInTranscript(transcript, "shared")
	if err != nil {
		t.Fatalf("shared: %v", err)
	}
	if parent != "at2" {
		t.Errorf("shared parent = %q, want at2 (the real user entry, not the attachment)", parent)
	}
	if _, err := parentUUIDForUserMessageUUIDInTranscript(transcript, "other-ao"); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("non-queued_command attachment matched: err=%v, want ErrMessageNotFound", err)
	}
}

// TestWriteForkFileThroughUUID: the already-cut retry re-slice must
// keep everything through the identified entry INCLUSIVE (cutting rows
// appended after a failed revert, round-5 R5-6), match ANY entry
// type's uuid (a checkpoint's parent uuid is usually an ASSISTANT
// entry), and fail loudly on absent or empty ids.
func TestWriteForkFileThroughUUID(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(srcPath, []byte(queuedCommandJSONL), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Keep through the assistant entry a1: the shape of the already-cut
	// retry, where a1 was the cut-away anchor's parent. Rows after a1
	// (the attachment and both later turns) must NOT survive.
	_, throughPath, err := WriteForkFileThroughUUID(ForkCut{SourcePath: srcPath, LastKeptUUID: "a1"})
	if err != nil {
		t.Fatalf("through a1: %v", err)
	}
	if got := keptUUIDs(t, throughPath); strings.Join(got, ",") != "u1,a1" {
		t.Fatalf("kept entries = %v, want [u1 a1] (inclusive through the parent, later rows cut)", got)
	}

	if _, _, err := WriteForkFileThroughUUID(ForkCut{SourcePath: srcPath, LastKeptUUID: "nope"}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("absent uuid: err=%v, want ErrMessageNotFound", err)
	}
	if _, _, err := WriteForkFileThroughUUID(ForkCut{SourcePath: srcPath}); err == nil {
		t.Fatal("empty uuid must error")
	}

	// The already-cut shape itself: a slice at u2 keeps u2's parent a2
	// under its own uuid, so a retry keyed by the stored parent resolves
	// on the slice, while the cut-away anchor u2 matches nothing there.
	_, slicePath, err := WriteForkFileForUserMessageUUID(srcPath, "u2", "")
	if err != nil {
		t.Fatalf("slice at u2: %v", err)
	}
	_, retryPath, err := WriteForkFileThroughUUID(ForkCut{SourcePath: slicePath, LastKeptUUID: "a2"})
	if err != nil {
		t.Fatalf("through the stored parent a2 on the slice: %v", err)
	}
	if got := keptUUIDs(t, retryPath); strings.Join(got, ",") != "u1,a1,qc1,a2" {
		t.Fatalf("retry kept %v, want [u1 a1 qc1 a2]", got)
	}
	if _, _, err := WriteForkFileForUserMessageUUID(slicePath, "u2", ""); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("cut-away uuid on the slice: err=%v, want ErrMessageNotFound", err)
	}
}

func TestBuildForkLines_RejectsEmptyUUID(t *testing.T) {
	// An entry with an empty uuid can be neither a slice point nor a
	// parent. Verify it's silently dropped rather than corrupting the
	// chain.
	src := `{"type":"user","uuid":"","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"second"}}
`
	newID, lines, err := BuildForkLines(strings.NewReader(src), "src", "", "")
	if err != nil {
		t.Fatalf("BuildForkLines: %v", err)
	}
	if newID == "" {
		t.Fatal("expected non-empty newID")
	}
	// Only u1 + custom-title should appear (empty-uuid entry skipped).
	if got, want := len(lines), 2; got != want {
		t.Errorf("lines = %d, want %d (empty-uuid entry should be dropped)", got, want)
	}
}
