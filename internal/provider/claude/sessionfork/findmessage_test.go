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

// An index with no prompt is an error. One exactly past the last prompt is
// ErrUserTurnAtTranscriptEnd, which still wraps ErrUserTurnOutOfRange for
// callers matching the broader error. Real-world trigger: AO's checkpoint
// table tracks a user_text row at TurnIndex N, but the Claude subprocess died
// before persisting that prompt to its session JSONL, so the JSONL count is
// N. The revert pipeline catches this sentinel and falls back to
// WriteForkFileFullTranscript; without it, the user sees "Revert failed:
// Requested N, found N" instead of a successful revert. An index further out
// must not match the narrower sentinel.
func TestFindUUIDBeforeUserTurn_RefusesAnIndexWithNoPrompt(t *testing.T) {
	for _, tc := range []struct {
		name              string
		src               string
		index             int
		outOfRange, atEnd bool
	}{
		{"negative", withToolResultsJSONL, -1, false, false},
		// withToolResultsJSONL has 3 real prompts at indexes 0, 1, 2.
		{"one past the end", withToolResultsJSONL, 3, true, true},
		{"well past the end", withToolResultsJSONL, 99, true, false},
		// With the interrupt marker int1 correctly skipped there are 3 real
		// prompts; counting it would make index 3 a real slice point.
		{"one past the end after an interrupt", withSyntheticInterruptJSONL, 3, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FindUUIDBeforeUserTurn(strings.NewReader(tc.src), tc.index)
			if err == nil {
				t.Fatalf("FindUUIDBeforeUserTurn(%d) succeeded", tc.index)
			}
			if got := errors.Is(err, ErrUserTurnOutOfRange); got != tc.outOfRange {
				t.Errorf("err=%v matches ErrUserTurnOutOfRange = %v, want %v", err, got, tc.outOfRange)
			}
			if got := errors.Is(err, ErrUserTurnAtTranscriptEnd); got != tc.atEnd {
				t.Errorf("err=%v matches ErrUserTurnAtTranscriptEnd = %v, want %v", err, got, tc.atEnd)
			}
		})
	}
}

// A user-role entry the user did not type is not a prompt: it neither
// counts toward the index nor stops the walk, so u2 (index 1) still answers
// with its own parent.
func TestFindUUIDBeforeUserTurn_SkipsUserEntriesThatAreNotPrompts(t *testing.T) {
	const (
		u1 = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first real prompt"}}` + "\n"
		a1 = `{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"response"}]}}` + "\n"
	)
	u2 := func(parent string) string {
		return `{"type":"user","uuid":"u2","parentUuid":"` + parent + `","sessionId":"src","message":{"role":"user","content":"second real prompt"}}` + "\n"
	}
	for _, tc := range []struct{ name, src, want string }{
		// A subagent transcript's prompt; the user didn't type it.
		{"sidechain prompt", u1 +
			`{"type":"user","uuid":"sub-u1","parentUuid":"u1","sessionId":"src","isSidechain":true,"message":{"role":"user","content":"subagent prompt"}}` + "\n" +
			a1 + u2("a1"), "a1"},
		// A /compact or auto-compact summary: string content (not a
		// tool_result echo) whose parent is the compaction's system envelope,
		// not an assistant turn. Counting it lands the slice a turn early.
		{"compact summary", u1 + a1 +
			`{"type":"system","uuid":"sys-compact","parentUuid":null,"sessionId":"src","content":"compaction event"}` + "\n" +
			`{"type":"user","uuid":"cs1","parentUuid":"sys-compact","sessionId":"src","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued from a previous conversation..."}}` + "\n" +
			`{"type":"assistant","uuid":"a2","parentUuid":"cs1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"resumed response"}]}}` + "\n" +
			u2("a2"), "a2"},
		// A Skill-load or other system-injected entry.
		{"isMeta", u1 + a1 +
			`{"type":"user","uuid":"meta1","parentUuid":"a1","sessionId":"src","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /home/foo/.claude/skills/post-task-review"}]}}` + "\n" +
			u2("meta1"), "meta1"},
		// A task-notification echo the CLI may write when a backgrounded
		// subagent completes during a foreground tool_result. Plain content,
		// so the flag alone must skip it.
		{"isReplay", u1 + a1 +
			`{"type":"user","uuid":"replay1","parentUuid":"a1","sessionId":"src","isReplay":true,"message":{"role":"user","content":"replayed user-role envelope, plain content"}}` + "\n" +
			u2("replay1"), "replay1"},
		// Wrapper content with no synthetic flag: the wire-replay parser
		// suppresses the same wrapper set, so the on-disk filter must agree.
		{"injected XML", u1 + a1 +
			`{"type":"user","uuid":"inj1","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":"<system-reminder>some system context</system-reminder>"}}` + "\n" +
			u2("inj1"), "inj1"},
		// A completed subagent's report injected into the parent
		// conversation; counting it slices a fork or revert a turn too far.
		{"agent message", u1 + a1 +
			`{"type":"user","uuid":"am1","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":"<agent-message from=\"general-purpose\">\n# Report\nbody\n</agent-message>"}}` + "\n" +
			u2("am1"), "am1"},
		// A peer session's message through the cross-session inbox (Claude
		// Code 2.1.224+). AO refuses inbound by default, but a transcript
		// from the user's own `claude` CLI, resumed or imported, can hold one.
		{"cross-session message", u1 + a1 +
			`{"type":"user","uuid":"xs1","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":"<cross-session-message from=\"peer-session\">\nplease look at the build\n</cross-session-message>"}}` + "\n" +
			u2("xs1"), "xs1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindUUIDBeforeUserTurn(strings.NewReader(tc.src), 1)
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q (u2's parent: the %s entry must not count as a user turn)", got, tc.want, tc.name)
			}
		})
	}
}

// TestInjectedUserContentWrappersCoversCrossSessionMessage pins the entry
// in the CANONICAL set rather than only through one caller: the live-wire
// suppression in parse_user_replay.go ranges over the same slice, and the
// 2026-07 incident was exactly an entry that existed in one copy and not
// the other.
func TestInjectedUserContentWrappersCoversCrossSessionMessage(t *testing.T) {
	for _, w := range InjectedUserContentWrappers {
		if w.Open == "<cross-session-message" && w.Close == "</cross-session-message>" {
			return
		}
	}
	t.Fatalf("InjectedUserContentWrappers is missing the cross-session-message entry: %v", InjectedUserContentWrappers)
}

// withSyntheticInterruptJSONL reproduces the ordinal-walk off-by-one
// bug. A prior turn was interrupted, so Claude wrote a
// `[Request interrupted by user]` entry into the JSONL: a
// `type:"user"`, `role:"user"` row with NO synthetic boolean flags and
// array content whose first block is `{type:"text", text:"[Request
// interrupted by user]"}` (see createUserInterruptionMessage at
// claude-code-source-code/src/utils/messages.ts:545-560). Claude's own
// selectableUserMessagesFilter excludes it via isSyntheticMessage;
// AO's ordinal walk must too, or it counts one phantom prompt per prior
// interrupt and slices the resumed session a full turn too far back.
//
// Real prompts here are u1 (0), u2 (1), u3 (2). int1 sits in the chain
// between a1 and u2 and must NOT be counted.
const withSyntheticInterruptJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first real prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"first response"}]}}
{"type":"user","uuid":"int1","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}
{"type":"user","uuid":"u2","parentUuid":"int1","sessionId":"src","message":{"role":"user","content":"second real prompt"}}
{"type":"assistant","uuid":"a2","parentUuid":"u2","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"second response"}]}}
{"type":"user","uuid":"u3","parentUuid":"a2","sessionId":"src","message":{"role":"user","content":"third real prompt"}}
`

// TestFindUUIDBeforeUserTurn_SkipsSyntheticInterrupt is the regression
// guard for the ordinal-walk bug: a `[Request interrupted by
// user]` marker in kept history was counted as a real prompt, so the
// ordinal walk landed one turn too early and the resumed Claude session
// silently lost a turn (timeline/composer looked correct; only Claude's
// memory was short). Fails before the isSyntheticMessage port.
func TestFindUUIDBeforeUserTurn_SkipsSyntheticInterrupt(t *testing.T) {
	// u2 is the 2nd real prompt (index 1); its parent is int1 (the
	// interrupt marker physically precedes it in the chain — the marker
	// stays in history, it just isn't a countable turn). Counting int1
	// as a prompt would return a1 instead.
	got, err := FindUUIDBeforeUserTurn(strings.NewReader(withSyntheticInterruptJSONL), 1)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "int1" {
		t.Errorf("FindUUIDBeforeUserTurn(1): got %q, want int1 (u2's parent — the interrupt marker must not be counted as a real prompt)", got)
	}

	// u3 is the 3rd real prompt (index 2); its parent is a2. Counting
	// int1 would make index 2 land on u2 and return int1 instead.
	got, err = FindUUIDBeforeUserTurn(strings.NewReader(withSyntheticInterruptJSONL), 2)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "a2" {
		t.Errorf("FindUUIDBeforeUserTurn(2): got %q, want a2 (u3's parent — the interrupt marker must not shift the ordinal count)", got)
	}
}

// TestIsRealUserPrompt_AllSyntheticSentinelsFiltered pins the full
// SYNTHETIC_MESSAGES set as the spec. Every sentinel, in the array
// content shape Claude actually emits, must be rejected by
// isRealUserPrompt; the same text as a bare string must NOT be —
// Claude's isSyntheticMessage requires Array.isArray(content), so a user
// who literally types a sentinel is still a real prompt.
//
// This guards two drift surfaces the single-string test missed: a
// sentinel being dropped from syntheticUserContentMessages, and a typo
// in any constant other than claudeInterruptMessage. The interrupt and
// tool-use-interrupt markers genuinely reach this array path on the wire
// (createUserInterruptionMessage emits both as single text blocks); the
// cancel/reject/no-response strings are carried for byte-for-byte
// upstream parity even though Claude delivers them via tool_result or
// assistant-role entries that other branches already filter.
func TestIsRealUserPrompt_AllSyntheticSentinelsFiltered(t *testing.T) {
	// Explicit list, not a range over the map: if a constant is dropped
	// from syntheticUserContentMessages its row here flips to "real" and
	// fails, so the test stays a checklist of the intended set.
	sentinels := []string{
		claudeInterruptMessage,
		claudeInterruptMessageForToolUse,
		claudeCancelMessage,
		claudeRejectMessage,
		claudeNoResponseRequested,
	}
	for _, text := range sentinels {
		arrayForm := map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}},
		}
		if isRealUserPrompt(arrayForm) {
			t.Errorf("array-form sentinel %q must be filtered (not a real prompt)", text)
		}
		stringForm := map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": text},
		}
		if !isRealUserPrompt(stringForm) {
			t.Errorf("string-form sentinel %q must remain a real prompt (Claude's isSyntheticMessage requires array content)", text)
		}
	}

	// Positive control: a normal array-form prompt is still real, so the
	// filter isn't rejecting all text-first arrays.
	normal := map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "actually do the thing"}}},
	}
	if !isRealUserPrompt(normal) {
		t.Errorf("normal array-form prompt must count as a real prompt")
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

// mergedQueuedBatchJSONL is the on-disk shape of a boundary drain that
// handed the CLI three consecutive prompt-mode commands. The CLI merges
// them into ONE `type:"user"` entry whose content is the flat
// concatenation of the three commands' blocks, carrying only the LAST
// command's uuid (u2c). The two earlier uuids (u2a, u2b) are echoed on
// stdout as `user{isReplay:true}` envelopes and never reach this file.
// See docs/references/claude-wire.md §Queued-message consumption.
const mergedQueuedBatchJSONL = `{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"src","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"reply 1"}]}}
{"type":"user","uuid":"u2c","parentUuid":"a1","sessionId":"src","message":{"role":"user","content":[{"type":"text","text":"queued one"},{"type":"text","text":"queued two"},{"type":"text","text":"queued three"}]}}
{"type":"assistant","uuid":"a2","parentUuid":"u2c","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"reply 2"}]}}
{"type":"user","uuid":"u3","parentUuid":"a2","sessionId":"src","message":{"role":"user","content":"third"}}
{"type":"assistant","uuid":"a3","parentUuid":"u3","sessionId":"src","message":{"role":"assistant","content":[{"type":"text","text":"reply 3"}]}}
`

// TestMergedQueuedBatchIsOneUserPrompt pins the CLI facts the flush-join
// routes around: the merged entry is ONE prompt to every ordinal walk,
// only the last member's uuid can anchor a slice, and the dropped member
// uuids are simply absent.
func TestMergedQueuedBatchIsOneUserPrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "merged.jsonl")
	if err := os.WriteFile(path, []byte(mergedQueuedBatchJSONL), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Ordinal view: prompts are u1, u2c, u3 at indexes 0, 1, 2. Counting
	// the merged entry once per text block would push u3 to index 4 and
	// make every ordinal slice land a turn or more too far back.
	for _, c := range []struct {
		turn     int
		wantUUID string
	}{
		{0, ""},   // u1 opens the transcript
		{1, "a1"}, // merged entry's parent
		{2, "a2"},
	} {
		got, err := FindUUIDBeforeUserTurn(strings.NewReader(mergedQueuedBatchJSONL), c.turn)
		if err != nil {
			t.Fatalf("FindUUIDBeforeUserTurn(%d): %v", c.turn, err)
		}
		if got != c.wantUUID {
			t.Errorf("FindUUIDBeforeUserTurn(%d): got %q, want %q", c.turn, got, c.wantUUID)
		}
	}
	if _, err := FindUUIDBeforeUserTurn(strings.NewReader(mergedQueuedBatchJSONL), 3); err == nil {
		t.Error("FindUUIDBeforeUserTurn(3) succeeded; the merged entry must not count as three prompts")
	}

	// The last member's uuid anchors a real slice: everything from the
	// merged entry on is cut, and all three joined texts go with it
	// because they live in that one entry.
	newID, newPath, err := WriteForkFileForUserMessageUUID(path, "u2c", "")
	if err != nil {
		t.Fatalf("WriteForkFileForUserMessageUUID(u2c): %v", err)
	}
	if newID == "" {
		t.Fatal("fork session id is empty")
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read fork: %v", err)
	}
	forked := string(data)
	for _, want := range []string{"first", "reply 1"} {
		if !strings.Contains(forked, want) {
			t.Errorf("fork at u2c dropped kept content %q:\n%s", want, forked)
		}
	}
	for _, absent := range []string{"queued one", "queued two", "queued three", "reply 2", "third"} {
		if strings.Contains(forked, absent) {
			t.Errorf("fork at u2c kept cut content %q:\n%s", absent, forked)
		}
	}

	// The earlier members' uuids were acknowledged on stdout only. A row
	// stamped with one of them can never anchor a slice, which is why the
	// dispatcher joins the batch into one row under one uuid.
	for _, dropped := range []string{"u2a", "u2b"} {
		if _, _, err := WriteForkFileForUserMessageUUID(path, dropped, ""); !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("WriteForkFileForUserMessageUUID(%s): got %v, want ErrMessageNotFound", dropped, err)
		}
	}
}

// A shell command a host sends is one entry holding the command and its
// output, and it opens a turn the person started. The CLI's own `!` mode
// writes two entries, neither of which is a prompt.
func TestIsRealUserPrompt_CountsAHostShellRecordAndNotTheClisOwn(t *testing.T) {
	entry := func(content any) map[string]any {
		return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
	}
	hostRecord := "<bash-input>ls</bash-input>\n<bash-stdout>README.md</bash-stdout><bash-stderr></bash-stderr>"
	if !strings.Contains(hostRecord, HostShellRecordJoint) {
		t.Fatalf("fixture does not hold the joint %q", HostShellRecordJoint)
	}
	for name, content := range map[string]any{
		"alone":            hostRecord,
		"after other text": "first\n\n---\n\n" + hostRecord,
		"array form":       []any{map[string]any{"type": "text", "text": hostRecord}},
	} {
		if !isRealUserPrompt(entry(content)) {
			t.Errorf("a host shell record (%s) must count as a real prompt", name)
		}
	}
	for name, content := range map[string]any{
		"the CLI's command entry": "<bash-input>ls</bash-input>",
		"the CLI's output entry":  "<bash-stdout>README.md</bash-stdout><bash-stderr></bash-stderr>",
		"another wrapper":         "<system-reminder>context</system-reminder>",
	} {
		if isRealUserPrompt(entry(content)) {
			t.Errorf("%s must not count as a real prompt", name)
		}
	}
}
