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

	newID, newPath, uuidMap, err := WriteForkFileForUserMessageUUID(srcPath, "ao-queued-1", "")
	if err != nil {
		t.Fatalf("WriteForkFileForUserMessageUUID: %v", err)
	}
	if newID == "" || newPath == "" {
		t.Fatalf("newID=%q newPath=%q, want both non-empty", newID, newPath)
	}

	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read fork: %v", err)
	}
	var kept []string
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		ff, _ := e["forkedFrom"].(map[string]any)
		if oldID, _ := ff["messageUuid"].(string); oldID != "" {
			kept = append(kept, oldID)
		}
	}
	if strings.Join(kept, ",") != "u1,a1" {
		t.Fatalf("kept entries = %v, want [u1 a1] (everything before the queued_command attachment)", kept)
	}
	if _, ok := uuidMap["qc1"]; ok {
		t.Error("attachment entry survived the slice; it should be dropped with the queued message")
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

// TestWriteForkFileThroughUUID — the already-cut retry re-slice must
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

	keptProvenance := func(t *testing.T, path string) []string {
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
			ff, _ := e["forkedFrom"].(map[string]any)
			if oldID, _ := ff["messageUuid"].(string); oldID != "" {
				kept = append(kept, oldID)
			}
		}
		return kept
	}

	// Keep through the assistant entry a1: the shape of the already-cut
	// retry, where a1 was the cut-away anchor's parent. Rows after a1
	// (the attachment and both later turns) must NOT survive.
	_, throughPath, _, err := WriteForkFileThroughUUID(ForkCut{SourcePath: srcPath, LastKeptUUID: "a1"})
	if err != nil {
		t.Fatalf("through a1: %v", err)
	}
	if got := keptProvenance(t, throughPath); strings.Join(got, ",") != "u1,a1" {
		t.Fatalf("kept entries = %v, want [u1 a1] (inclusive through the parent, later rows cut)", got)
	}

	if _, _, _, err := WriteForkFileThroughUUID(ForkCut{SourcePath: srcPath, LastKeptUUID: "nope"}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("absent uuid: err=%v, want ErrMessageNotFound", err)
	}
	if _, _, _, err := WriteForkFileThroughUUID(ForkCut{SourcePath: srcPath}); err == nil {
		t.Fatal("empty uuid must error")
	}

	// Fork provenance (CT4-5): after a slice remints every uuid, a
	// PRE-fork id must still resolve via forkedFrom.messageUuid — the
	// state a failed remap (or a crash between the SessionRef update and
	// the remap) leaves stored checkpoint ids in. Re-slicing the fork
	// through pre-fork "a1" keeps the fork's remints of [u1 a1].
	_, forkPath, uuidMap, err := WriteForkFileForUserMessageUUID(srcPath, "u2", "")
	if err != nil {
		t.Fatalf("fork at u2: %v", err)
	}
	_, secondPath, _, err := WriteForkFileThroughUUID(ForkCut{SourcePath: forkPath, LastKeptUUID: "a1"})
	if err != nil {
		t.Fatalf("through pre-fork a1 on the fork: %v", err)
	}
	want := []string{uuidMap["u1"], uuidMap["a1"]}
	if got := keptProvenance(t, secondPath); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("provenance re-slice kept %v, want the fork's remints of [u1 a1] = %v", got, want)
	}
	// The cut-away anchor's id maps to nothing on the fork — the
	// already-cut detector relies on that asymmetry.
	if _, _, _, err := WriteForkFileThroughUUID(ForkCut{SourcePath: forkPath, LastKeptUUID: "u2"}); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("cut-away uuid on fork: err=%v, want ErrMessageNotFound", err)
	}
}

// TestWriteForkFileForUserMessageUUID_ForkedFromProvenance pins the
// CT4-5 anchor half: slicing an already-forked transcript by a
// PRE-fork user uuid — the id a failed remap leaves on AO rows and
// checkpoints — anchors on the kept row whose forkedFrom.messageUuid
// matches, cutting at exactly the same message as a current-uuid
// slice would.
func TestWriteForkFileForUserMessageUUID_ForkedFromProvenance(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.jsonl")
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"second reply"}]}}
{"type":"user","uuid":"u3","parentUuid":"a2","sessionId":"src","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a3","parentUuid":"u3","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"third reply"}]}}
`
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// First slice: keep everything before u3. Every kept row remints
	// with forkedFrom provenance; assume the remap of stored ids FAILED.
	_, forkPath, uuidMap, err := WriteForkFileForUserMessageUUID(srcPath, "u3", "")
	if err != nil {
		t.Fatalf("first slice: %v", err)
	}

	// Second slice keyed by the PRE-fork uuid "u2": must anchor via
	// provenance and keep exactly the rows before u2 — the fork's
	// remints of u1 and a1.
	_, secondPath, _, err := WriteForkFileForUserMessageUUID(forkPath, "u2", "")
	if err != nil {
		t.Fatalf("second slice by pre-fork uuid: %v", err)
	}
	data, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("read second fork: %v", err)
	}
	var keptProvenance []string
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		ff, _ := e["forkedFrom"].(map[string]any)
		if oldID, _ := ff["messageUuid"].(string); oldID != "" {
			keptProvenance = append(keptProvenance, oldID)
		}
	}
	want := []string{uuidMap["u1"], uuidMap["a1"]}
	if strings.Join(keptProvenance, ",") != strings.Join(want, ",") {
		t.Fatalf("second slice kept %v, want the first fork's remints of [u1 a1] = %v", keptProvenance, want)
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
