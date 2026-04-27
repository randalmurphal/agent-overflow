package sessionfork

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withToolResultsJSONL: 3 user prompts interspersed with assistant tool-call
// turns and tool-result echo (`type:"user"` but content is a tool_result
// block, not a real prompt).
const withToolResultsJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"please run ls"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","uuid":"echo1","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool1","content":"file1\nfile2"}]}}
{"type":"assistant","uuid":"a2","parentUuid":"echo1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"there are 2 files"}]}}
{"type":"user","uuid":"u2","parentUuid":"a2","sessionId":"src","message":{"role":"user","content":"now run cat"}}
{"type":"assistant","uuid":"a3","parentUuid":"u2","sessionId":"src","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool2","name":"Bash","input":{"command":"cat"}}]}}
{"type":"user","uuid":"echo2","parentUuid":"a3","sessionId":"src","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool2","content":"hello"}]}}
{"type":"assistant","uuid":"a4","parentUuid":"echo2","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"see hello"}]}}
{"type":"user","uuid":"u3","parentUuid":"a4","sessionId":"src","message":{"role":"user","content":"done thanks"}}
`

func TestFindUUIDBeforeUserTurn_ReturnsParentOfTargetPrompt(t *testing.T) {
	// withToolResultsJSONL has real prompts u1, u2, u3 at indexes 0, 1, 2.
	// u2's parentUuid is a2 (the assistant continuation after the first
	// tool_result echo). u3's parentUuid is a4.
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(withToolResultsJSONL), 1)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "a2" {
		t.Errorf("FindUUIDBeforeUserTurn(1): got %q, want a2 (u2's parent)", got)
	}

	got, err = FindUUIDBeforeUserTurn(strings.NewReader(withToolResultsJSONL), 2)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "a4" {
		t.Errorf("FindUUIDBeforeUserTurn(2): got %q, want a4 (u3's parent)", got)
	}
}

func TestFindUUIDBeforeUserTurn_FirstPromptReturnsEmpty(t *testing.T) {
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(withToolResultsJSONL), 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "" {
		t.Errorf("FindUUIDBeforeUserTurn(0): got %q, want empty (caller should clear session)", got)
	}
}

func TestFindUUIDBeforeUserTurn_NegativeIndex(t *testing.T) {
	_, err := FindUUIDBeforeUserTurn(strings.NewReader(withToolResultsJSONL), -1)
	if err == nil {
		t.Fatalf("expected error for negative index")
	}
}

func TestFindUUIDBeforeUserTurn_OutOfRange(t *testing.T) {
	_, err := FindUUIDBeforeUserTurn(strings.NewReader(withToolResultsJSONL), 99)
	if !errors.Is(err, ErrUserTurnOutOfRange) {
		t.Fatalf("err=%v, want ErrUserTurnOutOfRange", err)
	}
}

func TestFindUUIDBeforeUserTurn_SkipsSidechainPrompt(t *testing.T) {
	// A sidechain prompt (subagent transcript) must not count as a real
	// user turn — the user didn't type it.
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","isSidechain":false,"message":{"role":"user","content":"main"}}
{"type":"user","uuid":"sub-u1","parentUuid":"u1","sessionId":"src","isSidechain":true,"message":{"role":"user","content":"subagent prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","isSidechain":false,"message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"src","isSidechain":false,"message":{"role":"user","content":"second main"}}
`
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(src), 1)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// u2 is the 2nd real prompt (index 1); its parent is a1 — sidechain skipped.
	if got != "a1" {
		t.Errorf("got %q, want a1 (sidechain prompt should not affect count or chain)", got)
	}
}

func TestFindUUIDBeforeUserTurn_OnlyToolResultsIsNotAPrompt(t *testing.T) {
	// content array that is ENTIRELY tool_result blocks — echo, not a prompt.
	src := `{"type":"user","uuid":"echo","parentUuid":null,"sessionId":"src","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}
{"type":"assistant","uuid":"a0","parentUuid":"echo","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"r0"}]}}
{"type":"user","uuid":"u1","parentUuid":"a0","sessionId":"src","message":{"role":"user","content":"hi"}}
`
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(src), 0)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (first real prompt has no preceding turn)", got)
	}
}

func TestSliceUUIDForLastKeptTurn(t *testing.T) {
	// Round-trip via the disk-based helper. Slice through turn 0 keeps
	// only the first prompt's full turn — slice point is u1's parent (a2
	// in the with-tool-results fixture, but that JSONL has tool calls
	// so the simpler 3-turn fixture is clearer).
	dir := t.TempDir()
	path := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(path, []byte(simple3TurnsJSONL), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// simple3TurnsJSONL has prompts u1, u2, u3 (indexes 0, 1, 2) with
	// parents null, a1, a2.
	cases := []struct {
		lastKept int
		wantUUID string // empty means caller should clear session
	}{
		{-1, ""},   // caller should clear session
		{0, "a1"},  // keep through turn 0 → slice at u2's parent = a1
		{1, "a2"},  // keep through turn 1 → slice at u3's parent = a2
	}
	for _, c := range cases {
		got, err := SliceUUIDForLastKeptTurn(path, c.lastKept)
		if err != nil {
			t.Fatalf("lastKept=%d: %v", c.lastKept, err)
		}
		if got != c.wantUUID {
			t.Errorf("lastKept=%d: got %q, want %q", c.lastKept, got, c.wantUUID)
		}
	}
}
