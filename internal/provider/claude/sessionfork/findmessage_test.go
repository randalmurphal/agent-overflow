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

func TestFindParentUUIDForUserMessageUUID(t *testing.T) {
	got, err := FindParentUUIDForUserMessageUUID(strings.NewReader(withToolResultsJSONL), "u2")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "a2" {
		t.Fatalf("got %q, want a2", got)
	}
}

func TestFindParentUUIDForUserMessageUUID_FirstPromptReturnsEmpty(t *testing.T) {
	got, err := FindParentUUIDForUserMessageUUID(strings.NewReader(withToolResultsJSONL), "u1")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty parent for first prompt", got)
	}
}

func TestFindParentUUIDForUserMessageUUID_SkipsToolResultEcho(t *testing.T) {
	_, err := FindParentUUIDForUserMessageUUID(strings.NewReader(withToolResultsJSONL), "echo1")
	if !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("err=%v, want ErrMessageNotFound", err)
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
	// 99 is well past the last prompt (count=3); the more specific
	// at-transcript-end sentinel must NOT match — that's reserved for
	// the off-by-exactly-one case the revert fallback consumes.
	if errors.Is(err, ErrUserTurnAtTranscriptEnd) {
		t.Fatalf("err=%v matched ErrUserTurnAtTranscriptEnd, want only ErrUserTurnOutOfRange (gap is %d, not 1)", err, 99-3)
	}
}

// TestFindUUIDBeforeUserTurn_AtTranscriptEnd pins the boundary where
// userTurnIndex is one past the last persisted prompt. Real-world
// trigger: AO's checkpoint table tracks a user_text row at TurnIndex
// N, but the Claude subprocess died before persisting that prompt to
// its session JSONL, so the JSONL count is N. The revert pipeline
// catches this sentinel and falls back to WriteForkFileFullTranscript;
// without the distinct sentinel, the user sees "Revert failed:
// Requested N, found N" instead of a successful revert + composer
// rehydration.
func TestFindUUIDBeforeUserTurn_AtTranscriptEnd(t *testing.T) {
	// withToolResultsJSONL has 3 real prompts (u1, u2, u3 at indexes
	// 0, 1, 2). Index 3 is exactly one past the end.
	_, err := FindUUIDBeforeUserTurn(strings.NewReader(withToolResultsJSONL), 3)
	if !errors.Is(err, ErrUserTurnAtTranscriptEnd) {
		t.Fatalf("err=%v, want ErrUserTurnAtTranscriptEnd", err)
	}
	// The narrower sentinel still wraps ErrUserTurnOutOfRange so any
	// existing callers that match on the broader error continue to
	// work.
	if !errors.Is(err, ErrUserTurnOutOfRange) {
		t.Fatalf("err=%v must also match ErrUserTurnOutOfRange (errors.Is chain)", err)
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

// TestFindUUIDBeforeUserTurn_SkipsCompactSummary is the regression
// guard for the off-by-N revert/fork bug. A /compact or auto-compact
// writes a `user{isCompactSummary:true, isVisibleInTranscriptOnly:true}`
// entry to the JSONL. That entry's `content` is a string (not a
// tool_result echo) and its `parentUuid` points at a `system`
// envelope from the compaction event — NOT at any real assistant
// turn. Counting it as a real prompt makes the slice point land one
// turn earlier than intended.
func TestFindUUIDBeforeUserTurn_SkipsCompactSummary(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first real prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"first response"}]}}
{"type":"system","uuid":"sys-compact","parentUuid":null,"sessionId":"src","content":"compaction event"}
{"type":"user","uuid":"cs1","parentUuid":"sys-compact","sessionId":"src","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation..."}}
{"type":"assistant","uuid":"a2","parentUuid":"cs1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"resumed response"}]}}
{"type":"user","uuid":"u2","parentUuid":"a2","sessionId":"src","message":{"role":"user","content":"second real prompt"}}
`
	// Two real prompts: u1 (index 0) and u2 (index 1). The compact
	// summary cs1 is NOT a real prompt and must be skipped.
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(src), 1)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "a2" {
		t.Errorf("got %q, want a2 (u2's parent — compact summary cs1 must not be counted as a real prompt)", got)
	}
}

// TestFindUUIDBeforeUserTurn_SkipsIsMeta covers the Skill-load /
// system-injected user-role entries Claude writes to JSONL.
func TestFindUUIDBeforeUserTurn_SkipsIsMeta(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first real prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"response"}]}}
{"type":"user","uuid":"meta1","parentUuid":"a1","sessionId":"src","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /home/foo/.claude/skills/post-task-review"}]}}
{"type":"user","uuid":"u2","parentUuid":"meta1","sessionId":"src","message":{"role":"user","content":"second real prompt"}}
`
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(src), 1)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "meta1" {
		t.Errorf("got %q, want meta1 (u2's parent — isMeta user entries must not be counted)", got)
	}
}

// TestFindUUIDBeforeUserTurn_SkipsIsReplay covers the synthetic
// task-notification echoes the CLI may write into the JSONL when a
// backgrounded subagent completes during a concurrent foreground
// tool_result. The boolean `isReplay:true` flag is the load-bearing
// signal — fixture content is deliberately plain text so the
// wrapper-XML filter does NOT fire here; we want to prove the flag
// alone is enough to skip the entry.
func TestFindUUIDBeforeUserTurn_SkipsIsReplay(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first real prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"response"}]}}
{"type":"user","uuid":"replay1","parentUuid":"a1","sessionId":"src","isReplay":true,"message":{"role":"user","content":"replayed user-role envelope, plain content"}}
{"type":"user","uuid":"u2","parentUuid":"replay1","sessionId":"src","message":{"role":"user","content":"second real prompt"}}
`
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(src), 1)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "replay1" {
		t.Errorf("got %q, want replay1 (u2's parent — isReplay user entries must not be counted)", got)
	}
}

// TestFindUUIDBeforeUserTurn_SkipsInjectedXMLContent covers
// task-notification / system-reminder / bash-stdout / etc. wrapper
// content even when none of the boolean synthetic flags are set —
// the wire-replay parser uses the same wrapper set to suppress
// emission, so the on-disk slice filter must agree.
func TestFindUUIDBeforeUserTurn_SkipsInjectedXMLContent(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first real prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"response"}]}}
{"type":"user","uuid":"inj1","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":"<system-reminder>some system context</system-reminder>"}}
{"type":"user","uuid":"u2","parentUuid":"inj1","sessionId":"src","message":{"role":"user","content":"second real prompt"}}
`
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(src), 1)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "inj1" {
		t.Errorf("got %q, want inj1 (u2's parent — injected XML wrapper content must not be counted)", got)
	}
}

// TestFindParentUUIDForUserMessageUUID_ImmuneToSynthetics demonstrates
// that the UUID-keyed lookup is structurally immune to the
// over-counting bug — it matches by UUID, not by ordinal, so any
// number of synthetic entries between real prompts is irrelevant.
func TestFindParentUUIDForUserMessageUUID_ImmuneToSynthetics(t *testing.T) {
	src := `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first real prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"first response"}]}}
{"type":"system","uuid":"sys-compact","parentUuid":null,"sessionId":"src","content":"compaction event"}
{"type":"user","uuid":"cs1","parentUuid":"sys-compact","sessionId":"src","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued..."}}
{"type":"assistant","uuid":"a2","parentUuid":"cs1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"resumed response"}]}}
{"type":"user","uuid":"u2","parentUuid":"a2","sessionId":"src","message":{"role":"user","content":"second real prompt"}}
`
	got, err := FindParentUUIDForUserMessageUUID(strings.NewReader(src), "u2")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "a2" {
		t.Fatalf("got %q, want a2 (u2's parent — UUID-keyed lookup must land correctly regardless of synthetic entries between prompts)", got)
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
		{-1, ""},  // caller should clear session
		{0, "a1"}, // keep through turn 0 → slice at u2's parent = a1
		{1, "a2"}, // keep through turn 1 → slice at u3's parent = a2
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
