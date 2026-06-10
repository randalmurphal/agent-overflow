package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// offBranchAPIErrorTailTranscript replicates the checked-in fixture
// (docs/references/fixtures/claude/session_api_error_offbranch.jsonl;
// same row names, fields slimmed to what the scanner reads) minus its
// final user row — the shape the 2026-06-10 incident's pre-fix revert
// fork produced: the file ends with two deferred system/api_error rows
// whose parentUuid bypasses a2/a3-final. The active branch from the
// file's last transcript row (err2) is
// err2 → err1 → u2-toolresult → a1 → u1; the file-order content leaf
// a3-final is OFF that branch, and resume-at a3-final hard-fails in
// the real CLI ("No message found with message.uuid of: ...").
const offBranchAPIErrorTailTranscript = `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"please run the checks"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"msg_01","role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}]}}
{"type":"user","uuid":"u2-toolresult","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"checks passed"}]}}
{"type":"assistant","uuid":"a2","parentUuid":"u2-toolresult","message":{"id":"msg_02","role":"assistant","content":[{"type":"text","text":"working"}]}}
{"type":"assistant","uuid":"a3-final","parentUuid":"a2","message":{"id":"msg_03","role":"assistant","content":[{"type":"text","text":"All green"}]}}
{"type":"system","subtype":"api_error","uuid":"err1","parentUuid":"u2-toolresult","level":"error","retryAttempt":1,"error":{"message":"Connection error."}}
{"type":"system","subtype":"api_error","uuid":"err2","parentUuid":"err1","level":"error","retryAttempt":2,"error":{"message":"Connection error."}}`

// TestSessionLeafScannerOffBranchAPIErrorTailReturnsBranchLeaf is the
// incident regression: the scanner must not return the file-order
// content leaf (a3-final) when it is off the active branch — the
// deepest on-branch user/assistant row (u2-toolresult) is the only
// cursor claude will accept.
func TestSessionLeafScannerOffBranchAPIErrorTailReturnsBranchLeaf(t *testing.T) {
	state, err := scanSessionLeafReader(strings.NewReader(offBranchAPIErrorTailTranscript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID == "a3-final" {
		t.Fatalf("CanonicalLeafUUID = a3-final (off-branch file-order pick) — resume-at would hard-fail")
	}
	if state.CanonicalLeafUUID != "u2-toolresult" {
		t.Fatalf("CanonicalLeafUUID = %q, want deepest on-branch content row u2-toolresult", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerPoisonedFileWithTrailingUserStaysOnBranch: the
// UNforked incident file ends with a user row chained onto the
// api_errors — that row is both the file-order leaf and the branch
// leaf, so the scan returns it unchanged (the repair is a no-op when
// file order and graph agree at the tail).
func TestSessionLeafScannerPoisonedFileWithTrailingUserStaysOnBranch(t *testing.T) {
	transcript := offBranchAPIErrorTailTranscript + "\n" +
		`{"type":"user","uuid":"u3","parentUuid":"err2","message":{"role":"user","content":"Are you confident?"}}`
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "u3" {
		t.Fatalf("CanonicalLeafUUID = %q, want u3", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerOnBranchLeafUnchanged: linear healthy file —
// byte-identical behavior to the pre-branch-aware scanner.
func TestSessionLeafScannerOnBranchLeafUnchanged(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","message":{"role":"user","content":"more"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a2" {
		t.Fatalf("CanonicalLeafUUID = %q, want a2", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerCompactBoundaryRoot: after an idle /compact the
// active chain is rooted at a compact-boundary system row
// (parentUuid null). The branch walk must traverse it and keep the
// tail assistant as the leaf.
func TestSessionLeafScannerCompactBoundaryRoot(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"old"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"old reply"}]}}`,
		`{"type":"system","subtype":"compact_boundary","uuid":"sys-compact","parentUuid":null,"logicalParentUuid":"a1","content":"compaction"}`,
		`{"type":"user","uuid":"cs1","parentUuid":"sys-compact","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued..."}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"cs1","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"resumed"}]}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a2" {
		t.Fatalf("CanonicalLeafUUID = %q, want a2", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerSidechainTailNotBranchTip: trailing sidechain
// rows (subagent transcripts) must not become the branch tip — their
// parentUuid graphs are separate and would orphan the main chain.
func TestSessionLeafScannerSidechainTailNotBranchTip(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"main"}]}}`,
		`{"type":"user","uuid":"side-u","parentUuid":null,"isSidechain":true,"message":{"role":"user","content":"subagent prompt"}}`,
		`{"type":"assistant","uuid":"side-a","parentUuid":"side-u","isSidechain":true,"message":{"id":"sm1","role":"assistant","content":[{"type":"text","text":"subagent reply"}]}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "a1" {
		t.Fatalf("CanonicalLeafUUID = %q, want a1 (sidechain tail must not define the branch)", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerBrokenChainReturnsEmptyLeaf: when the active
// branch reaches no user/assistant row (tip's ancestry is broken and
// content hangs off the missing part), the scan returns an empty leaf
// — the app then omits --resume-session-at, which is claude's own
// default-leaf semantics and the safe degenerate.
func TestSessionLeafScannerBrokenChainReturnsEmptyLeaf(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":"missing-root","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"reply"}]}}`,
		`{"type":"system","subtype":"api_error","uuid":"err1","parentUuid":"ghost-uuid","level":"error","error":{"message":"Connection error."}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID != "" {
		t.Fatalf("CanonicalLeafUUID = %q, want empty (branch from err1 reaches no content row)", state.CanonicalLeafUUID)
	}
}

// TestSessionLeafScannerOffBranchFallbackSkipsUnresolvedServerTool: the
// branch fallback must respect the unresolved-server-tool exclusion —
// an assistant row awaiting a server-side tool result is transcript
// history but not a valid continuation leaf, even when it is the
// deepest content row on the branch.
func TestSessionLeafScannerOffBranchFallbackSkipsUnresolvedServerTool(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"advise"}}`,
		`{"type":"assistant","uuid":"a-advisor","parentUuid":"u1","message":{"id":"m1","role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_1","name":"advisor","input":{}}]}}`,
		// Off-branch content tail (bypassed by the api_error below).
		`{"type":"assistant","uuid":"a-final","parentUuid":"a-advisor","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"system","subtype":"api_error","uuid":"err1","parentUuid":"a-advisor","level":"error","error":{"message":"Connection error."}}`,
	}, "\n")
	state, err := scanSessionLeafReader(strings.NewReader(transcript))
	if err != nil {
		t.Fatalf("scanSessionLeafReader: %v", err)
	}
	if state.CanonicalLeafUUID == "a-advisor" {
		t.Fatalf("CanonicalLeafUUID = a-advisor, which has an unresolved server tool — not a valid continuation leaf")
	}
	if state.CanonicalLeafUUID != "u1" {
		t.Fatalf("CanonicalLeafUUID = %q, want u1 (deepest on-branch content row past the exclusion)", state.CanonicalLeafUUID)
	}
}

// TestResumeAtOnActiveBranch pins the exported validator used by the
// spawn path for explicit resume-at cursors: true only for
// user/assistant rows reachable from the file's last transcript row.
func TestResumeAtOnActiveBranch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	// Slug from the CANONICAL workspace path, mirroring
	// LocateSessionFile — on macOS t.TempDir lives under the /tmp
	// symlink, and an unresolved slug would only pass via the
	// scan-every-project-dir fallback instead of the primary lookup.
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("eval workspace symlinks: %v", err)
	}
	slug := strings.ReplaceAll(canonical, string(filepath.Separator), "-")
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "sess-branch.jsonl"), []byte(offBranchAPIErrorTailTranscript+"\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	cases := []struct {
		resumeAt string
		want     bool
	}{
		{"u2-toolresult", true}, // on-branch content
		{"u1", true},            // on-branch content (root)
		{"a3-final", false},     // content but OFF the active branch — the incident pick
		{"err1", false},         // on-branch but a system row, never a valid cursor
		{"no-such-uuid", false}, // unknown
	}
	for _, tc := range cases {
		got, err := ResumeAtOnActiveBranch("sess-branch", workspace, tc.resumeAt)
		if err != nil {
			t.Fatalf("ResumeAtOnActiveBranch(%q): %v", tc.resumeAt, err)
		}
		if got != tc.want {
			t.Errorf("ResumeAtOnActiveBranch(%q) = %v, want %v", tc.resumeAt, got, tc.want)
		}
	}

	if _, err := ResumeAtOnActiveBranch("sess-branch", workspace, ""); err == nil {
		t.Errorf("empty resume-at must error, not silently validate")
	}
}
