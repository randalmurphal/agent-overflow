package claude

import (
	"agent-overflow/internal/provider/claude/sessionfork"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crashDanglingToolUseTailTranscript replicates the 2026-08-03 BSOD
// incident tail (session 4a466e10, sanitized): the host died while a
// long Bash ran, so the file ends with the turn's thinking row and the
// Bash tool_use row — and no tool_result. On resume the CLI drops the
// tool_use row (filterUnresolvedToolUses) and then the thinking row
// (its only same-message.id sibling with non-thinking content is gone),
// so resume-at either row hard-fails ("No message found with
// message.uuid of: ..."). The last surviving row is the prior
// tool_result user row.
const crashDanglingToolUseTailTranscript = `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"wrap up the task"}}
{"type":"assistant","uuid":"a-edit","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"toolu_edit","name":"Edit","input":{}}]}}
{"type":"user","uuid":"u-edit-result","parentUuid":"a-edit","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_edit","content":"file updated"}]}}
{"type":"assistant","uuid":"a-think","parentUuid":"u-edit-result","message":{"id":"m2","role":"assistant","content":[{"type":"thinking","thinking":"running the full verification suite"}]}}
{"type":"assistant","uuid":"a-bash","parentUuid":"a-think","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"make verify"}}]}}`

// TestSessionLeafScannerCrashDanglingToolUseTail is the 2026-08-03
// incident regression: a crash mid-tool-execution must not leave the
// scan pointing at the dangling tool_use row (or its doomed thinking
// sibling) — both are on the active branch but neither survives the
// CLI's resume filters.
func TestSessionLeafScannerCrashDanglingToolUseTail(t *testing.T) {
	state, err := scanSessionLeafReader(strings.NewReader(crashDanglingToolUseTailTranscript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID == "a-bash" || state.CanonicalLeafUUID == "a-think" {
		t.Fatalf("CanonicalLeafUUID = %q — the CLI filters that row out and resume-at hard-fails", state.CanonicalLeafUUID)
	}
	if state.CanonicalLeafUUID != "u-edit-result" {
		t.Fatalf("CanonicalLeafUUID = %q, want u-edit-result (deepest row surviving the resume filters)", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerResolvedToolUseTailUnchanged: the healthy twin
// — once the tool_result lands (or is synthesized), the file-order
// leaf survives the filters and the repair is a no-op.
func TestSessionLeafScannerResolvedToolUseTailUnchanged(t *testing.T) {
	transcript := crashDanglingToolUseTailTranscript + "\n" +
		`{"type":"user","uuid":"u-bash-result","parentUuid":"a-bash","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_bash","content":"interrupted","is_error":true}]}}`
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "u-bash-result" {
		t.Fatalf("CanonicalLeafUUID = %q, want u-bash-result", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerMixedTextAndDanglingToolUseTail: text in the
// same message does NOT save a row whose tool_use blocks are all
// unresolved — filterUnresolvedToolUses drops the whole message.
func TestSessionLeafScannerMixedTextAndDanglingToolUseTail(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","uuid":"a-mixed","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"Running it now."},{"type":"tool_use","id":"toolu_x","name":"Bash","input":{}}]}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "u1" {
		t.Fatalf("CanonicalLeafUUID = %q, want u1 (a-mixed is dropped despite its text block)", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerOrphanedThinkingOnlyTail: a stream interrupted
// after the thinking flush but before any content block leaves a
// thinking-only row with no same-id sibling — the CLI drops it.
func TestSessionLeafScannerOrphanedThinkingOnlyTail(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"assistant","uuid":"a-think","parentUuid":"a1","message":{"id":"m2","role":"assistant","content":[{"type":"thinking","thinking":"planning"}]}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a1" {
		t.Fatalf("CanonicalLeafUUID = %q, want a1 (orphaned thinking-only tail is dropped)", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerThinkingWithSurvivingSiblingKept: a thinking
// row whose same-message.id sibling carries non-thinking content is
// NOT orphaned — the deeper sibling is the leaf and both are valid
// cursors.
func TestSessionLeafScannerThinkingWithSurvivingSiblingKept(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a-think","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"planning"}]}}`,
		`{"type":"assistant","uuid":"a-text","parentUuid":"a-think","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a-text" {
		t.Fatalf("CanonicalLeafUUID = %q, want a-text", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerWhitespaceOnlyTail: a cancel right after the
// model emitted "\n\n" leaves a whitespace-only text row — dropped by
// the CLI, so the scan falls back to the prior row.
func TestSessionLeafScannerWhitespaceOnlyTail(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a-ws","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"\n\n"}]}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "u1" {
		t.Fatalf("CanonicalLeafUUID = %q, want u1 (whitespace-only tail is dropped)", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerWhitespaceMergeCollapsesUserRun: removing a
// whitespace-only row makes the CLI merge every adjacent user run into
// its first row, erasing the later rows' uuids. The scan must fall
// back to the run head (whose uuid survives the merge when non-meta),
// never a later run member.
func TestSessionLeafScannerWhitespaceMergeCollapsesUserRun(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u0","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a-ws","parentUuid":"u0","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"\n"}]}}`,
		`{"type":"user","uuid":"uA","parentUuid":"a-ws","message":{"role":"user","content":"first"}}`,
		`{"type":"user","uuid":"uB","parentUuid":"uA","message":{"role":"user","content":"second"}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID == "uB" || state.CanonicalLeafUUID == "uA" {
		t.Fatalf("CanonicalLeafUUID = %q — merged away by the whitespace-triggered user merge", state.CanonicalLeafUUID)
	}
	if state.CanonicalLeafUUID != "u0" {
		t.Fatalf("CanonicalLeafUUID = %q, want run head u0", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerWhitespaceMergeMetaHeadedRunUnsafe: a
// meta-headed user run's surviving uuid depends on a CLI feature flag
// we cannot observe, so no row of the run is a safe cursor and the
// scan degrades to no cursor at all (claude's default-leaf semantics).
func TestSessionLeafScannerWhitespaceMergeMetaHeadedRunUnsafe(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u0","parentUuid":null,"isMeta":true,"message":{"role":"user","content":"caveat"}}`,
		`{"type":"assistant","uuid":"a-ws","parentUuid":"u0","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"\n"}]}}`,
		`{"type":"user","uuid":"uA","parentUuid":"a-ws","message":{"role":"user","content":"first"}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "" {
		t.Fatalf("CanonicalLeafUUID = %q, want empty (no row of a meta-headed merged run is a safe cursor)", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerEmptyContentAssistantKept: the CLI keeps
// assistant rows with an empty content array (patched later by
// ensureNonEmptyAssistantContent), so they remain valid cursors.
func TestSessionLeafScannerEmptyContentAssistantKept(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a-empty","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[]}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a-empty" {
		t.Fatalf("CanonicalLeafUUID = %q, want a-empty", state.CanonicalLeafUUID)
	}
}

// TestResumeAtOnActiveBranchRejectsFilteredRows: the explicit-cursor
// validator must reject rows the CLI's resume filters drop — the live
// tracker can hand the spawn path a wire-derived leaf whose
// tool_result never reached the file (process died mid-tool).
func TestResumeAtOnActiveBranchRejectsFilteredRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("eval workspace symlinks: %v", err)
	}
	slug := strings.ReplaceAll(canonical, string(filepath.Separator), "-")
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "sess-crash.jsonl"), []byte(crashDanglingToolUseTailTranscript+"\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	cases := []struct {
		resumeAt string
		want     bool
	}{
		{"u-edit-result", true}, // deepest surviving row
		{"a-edit", true},        // resolved tool_use — survives
		{"a-bash", false},       // dangling tool_use — CLI drops it
		{"a-think", false},      // orphaned with its sibling — CLI drops it
	}
	for _, tc := range cases {
		got, err := ResumeAtOnActiveBranch(sessionfork.ProjectsDirForHome(home), "sess-crash", workspace, tc.resumeAt)
		if err != nil {
			t.Fatalf("ResumeAtOnActiveBranch(%q): %v", tc.resumeAt, err)
		}
		if got != tc.want {
			t.Errorf("ResumeAtOnActiveBranch(%q) = %v, want %v", tc.resumeAt, got, tc.want)
		}
	}
}
