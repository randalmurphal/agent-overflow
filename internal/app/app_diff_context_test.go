package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/testutil"
)

func TestReadWorkspaceFile(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	got, err := readWorkspaceFile(regular, 64)
	if err != nil {
		t.Fatalf("readWorkspaceFile(regular) error = %v", err)
	}
	if got != "hello\nworld\n" {
		t.Errorf("content = %q, want %q", got, "hello\nworld\n")
	}

	// maxBytes <= 0 is unbounded but still type-checked.
	if _, err := readWorkspaceFile(regular, 0); err != nil {
		t.Errorf("readWorkspaceFile(regular, 0) error = %v", err)
	}

	if _, err := readWorkspaceFile(regular, 5); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized read error = %v, want exceeds", err)
	}

	if _, err := readWorkspaceFile(dir, 64); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory read error = %v, want not a regular file", err)
	}
}

func TestGetDiffContextLinesWorkspaceScope(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-diff-context")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var lines []string
	for i := 1; i <= 50; i += 1 {
		lines = append(lines, strings.Repeat("x", 3)+" line")
	}
	for i := range lines {
		lines[i] = lines[i] + " " + string(rune('0'+i%10))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "main.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope:     "workspace",
		Path:      "src/main.go",
		StartLine: 11,
		EndLine:   30,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines() error = %v", err)
	}
	if len(result.Lines) != 20 {
		t.Fatalf("len(Lines) = %d, want 20", len(result.Lines))
	}
	if result.Lines[0] != lines[10] || result.Lines[19] != lines[29] {
		t.Fatalf("slice mismatch: got [%q..%q], want [%q..%q]", result.Lines[0], result.Lines[19], lines[10], lines[29])
	}
	if result.StartLine != 11 || result.EOF || result.TotalLines != 50 {
		t.Fatalf("meta = {start:%d eof:%v total:%d}, want {11 false 50}", result.StartLine, result.EOF, result.TotalLines)
	}

	// Range past EOF clamps and flags EOF; trailing newline does not
	// count as an extra line.
	tail, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope:     "workspace",
		Path:      "src/main.go",
		StartLine: 41,
		EndLine:   60,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines(tail) error = %v", err)
	}
	if len(tail.Lines) != 10 || !tail.EOF {
		t.Fatalf("tail = {%d lines, eof:%v}, want {10, true}", len(tail.Lines), tail.EOF)
	}

	// Fully past EOF: empty but EOF-flagged, not an error (a trailing
	// gap probe when the last hunk already reached the file end).
	past, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope:     "workspace",
		Path:      "src/main.go",
		StartLine: 51,
		EndLine:   70,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines(past) error = %v", err)
	}
	if len(past.Lines) != 0 || !past.EOF || past.TotalLines != 50 {
		t.Fatalf("past = {%d lines, eof:%v, total:%d}, want {0, true, 50}", len(past.Lines), past.EOF, past.TotalLines)
	}
}

func TestGetDiffContextLinesCommitScope(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := testutil.InitGitRepo(t)
	thread := testThread("thread-diff-context-commit")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	writeAndCommit := func(content, message string) {
		if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		testutil.RunGit(t, workspace, "add", "notes.txt")
		testutil.RunGit(t, workspace, "commit", "-m", message)
	}
	writeAndCommit("one\ntwo\nthree\n", "first")
	sha, _, err := gitops.NewCore().Execute(workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	// The worktree moves on: commit scope must read the SELECTED
	// commit's tree, not the file on disk.
	writeAndCommit("one\nedited\nthree\nfour\n", "second")

	result, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope:     "commit",
		CommitSHA: strings.TrimSpace(sha),
		Path:      "notes.txt",
		StartLine: 1,
		EndLine:   10,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines(commit) error = %v", err)
	}
	if got := strings.Join(result.Lines, "\n"); got != "one\ntwo\nthree" {
		t.Fatalf("commit-scope lines = %q, want the first commit's content", got)
	}
	if !result.EOF || result.TotalLines != 3 {
		t.Fatalf("meta = {eof:%v total:%d}, want {true 3}", result.EOF, result.TotalLines)
	}

	if _, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope:     "commit",
		Path:      "notes.txt",
		StartLine: 1,
		EndLine:   10,
	}); err == nil || !strings.Contains(err.Error(), "commit SHA is required") {
		t.Fatalf("missing-SHA error = %v, want commit SHA is required", err)
	}
}

func TestGetDiffContextLinesValidation(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-diff-context-validate")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	cases := []DiffContextRequest{
		{Scope: "workspace", Path: "a.txt", StartLine: 0, EndLine: 5},
		{Scope: "workspace", Path: "a.txt", StartLine: 10, EndLine: 5},
		{Scope: "workspace", Path: "a.txt", StartLine: 1, EndLine: maxDiffContextLines + 1},
		{Scope: "workspace", Path: "../escape.txt", StartLine: 1, EndLine: 5},
		{Scope: "workspace", Path: "/etc/passwd", StartLine: 1, EndLine: 5},
		{Scope: "nonsense", Path: "a.txt", StartLine: 1, EndLine: 5},
		{Scope: "pr", HeadSHA: "", Path: "a.txt", StartLine: 1, EndLine: 5},
	}
	for _, req := range cases {
		if _, err := app.GetDiffContextLines(thread.ID, req); err == nil {
			t.Fatalf("GetDiffContextLines(%+v) expected error", req)
		}
	}
}

func TestSplitContentLines(t *testing.T) {
	cases := []struct {
		content string
		want    int
	}{
		{"", 0},
		{"\n", 1},
		{"one", 1},
		{"one\n", 1},
		{"one\ntwo", 2},
		{"one\ntwo\n", 2},
	}
	for _, tc := range cases {
		if got := len(splitContentLines(tc.content)); got != tc.want {
			t.Fatalf("splitContentLines(%q) len = %d, want %d", tc.content, got, tc.want)
		}
	}
}

func TestGetDiffContextLinesEditsScope(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-diff-context-edits")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	writeFile := func(content string) {
		if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	writeFile(strings.Join(lines, "\n") + "\n")

	verifyPatch := "diff --git a/notes.txt b/notes.txt\n" +
		"--- a/notes.txt\n" +
		"+++ b/notes.txt\n" +
		"@@ -10,3 +10,3 @@\n" +
		" line 10\n" +
		"-old eleven\n" +
		"+line 11\n" +
		" line 12\n"

	// Verified: the workspace still matches this historical patch, so
	// its lines serve as context.
	result, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope:       "edits",
		Path:        "notes.txt",
		StartLine:   1,
		EndLine:     5,
		VerifyPatch: verifyPatch,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines(verified) error = %v", err)
	}
	if len(result.Lines) != 5 || result.Lines[0] != "line 1" || result.TotalLines != 20 {
		t.Fatalf("verified result = %+v", result)
	}

	// No verification patch → default-closed refusal.
	if _, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope: "edits", Path: "notes.txt", StartLine: 1, EndLine: 5,
	}); err == nil {
		t.Fatal("missing VerifyPatch must refuse")
	}

	// The file drifted (line 11 rewritten since the edit) → refusal, so
	// current lines can never masquerade as historical context.
	writeFile(strings.Join(lines[:10], "\n") + "\nrewritten since\n" + strings.Join(lines[11:], "\n") + "\n")
	if _, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope: "edits", Path: "notes.txt", StartLine: 1, EndLine: 5, VerifyPatch: verifyPatch,
	}); err == nil {
		t.Fatal("drifted workspace must refuse edits-scope expansion")
	}
}

// VerifyEditDiffs runs the serving path's exact resolution per file, so
// its verdicts are the ones expansion clicks would get: workspace-
// verified and snapshot-backed files pass, drifted and out-of-workspace
// files don't.
func TestVerifyEditDiffs(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-verify-edit-diffs")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "ok.txt"), []byte("line 1\nline 2\nline 3\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	patchFor := func(path string) string {
		return "diff --git a/" + path + " b/" + path + "\n" +
			"--- a/" + path + "\n" +
			"+++ b/" + path + "\n" +
			"@@ -2,1 +2,1 @@\n" +
			"-old two\n" +
			"+line 2\n"
	}

	// gone.txt only survives as a snapshot; drifted.txt has neither a
	// matching workspace file nor a snapshot.
	insertDiffSpanPayload(t, app, thread.ID, "item-v", "payload-v", "tool_result", patchFor("gone.txt"))
	if err := app.store.PutEditFileSnapshot(thread.ID, "payload-v", "gone.txt", "line 1\nline 2\nline 3\n", time.Now().UnixMilli()); err != nil {
		t.Fatalf("PutEditFileSnapshot() error = %v", err)
	}

	result, err := app.VerifyEditDiffs(thread.ID, VerifyEditDiffsRequest{
		EditPayloadID: "payload-v",
		EditTurnIndex: -1,
		Files: []EditDiffVerifyFile{
			{Path: "ok.txt", VerifyPatch: patchFor("ok.txt")},
			{Path: "gone.txt", VerifyPatch: patchFor("gone.txt")},
			{Path: "drifted.txt", VerifyPatch: patchFor("drifted.txt")},
			{Path: "/abs/outside.txt", VerifyPatch: patchFor("/abs/outside.txt")},
			{Path: "empty.txt", VerifyPatch: ""},
		},
	})
	if err != nil {
		t.Fatalf("VerifyEditDiffs() error = %v", err)
	}
	if len(result.ExpandablePaths) != 2 ||
		result.ExpandablePaths[0] != "ok.txt" || result.ExpandablePaths[1] != "gone.txt" {
		t.Fatalf("ExpandablePaths = %v, want [ok.txt gone.txt]", result.ExpandablePaths)
	}
}

// A persisted edit-file snapshot outranks the workspace: once captured,
// the edit stays expandable no matter how far the workspace drifts —
// including the file being deleted outright.
func TestGetDiffContextLinesEditsScopeSnapshotFirst(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-diff-context-snapshot")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	historical := strings.Join(lines, "\n") + "\n"
	verifyPatch := "diff --git a/notes.txt b/notes.txt\n" +
		"--- a/notes.txt\n" +
		"+++ b/notes.txt\n" +
		"@@ -10,3 +10,3 @@\n" +
		" line 10\n" +
		"-old eleven\n" +
		"+line 11\n" +
		" line 12\n"

	// Two edits of the same turn touched the file; the later snapshot is
	// the state the whole-turn merged section describes. The workspace
	// copy is GONE — only snapshots can serve.
	insertDiffSpanPayload(t, app, thread.ID, "item-early", "payload-early", "tool_result", verifyPatch)
	insertDiffSpanPayload(t, app, thread.ID, "item-late", "payload-late", "tool_result", verifyPatch)
	now := time.Now().UnixMilli()
	stale := strings.Join(lines[:10], "\n") + "\nsuperseded eleven\n" + strings.Join(lines[11:], "\n") + "\n"
	if err := app.store.PutEditFileSnapshot(thread.ID, "payload-early", "notes.txt", stale, now); err != nil {
		t.Fatalf("PutEditFileSnapshot(early) error = %v", err)
	}
	if err := app.store.PutEditFileSnapshot(thread.ID, "payload-late", "notes.txt", historical, now); err != nil {
		t.Fatalf("PutEditFileSnapshot(late) error = %v", err)
	}

	// Single-edit selection resolves that payload's snapshot.
	result, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope: "edits", Path: "notes.txt", StartLine: 1, EndLine: 5,
		VerifyPatch: verifyPatch, EditPayloadID: "payload-late", EditTurnIndex: -1,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines(snapshot) error = %v", err)
	}
	if len(result.Lines) != 5 || result.Lines[0] != "line 1" || result.TotalLines != 20 {
		t.Fatalf("snapshot result = %+v", result)
	}

	// Whole-turn selection resolves the LAST matching snapshot of the
	// path in item order (both payloads sit in the same turn; the later
	// one wins and is the one that verifies).
	result, err = app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope: "edits", Path: "notes.txt", StartLine: 10, EndLine: 12,
		VerifyPatch: verifyPatch, EditTurnIndex: 0,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines(turn snapshot) error = %v", err)
	}
	if len(result.Lines) != 3 || result.Lines[1] != "line 11" {
		t.Fatalf("turn snapshot result = %+v", result)
	}

	// A snapshot that does not match the verification patch (the early
	// edit's state predates the verified section) must not serve — and
	// with no workspace file either, the request refuses.
	if _, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope: "edits", Path: "notes.txt", StartLine: 1, EndLine: 5,
		VerifyPatch: verifyPatch, EditPayloadID: "payload-early", EditTurnIndex: -1,
	}); err == nil {
		t.Fatal("mismatched snapshot with no workspace file must refuse")
	}

	// A mismatched snapshot must FALL THROUGH to the workspace, not
	// refuse outright: once the file exists on disk in matching state,
	// the same request serves from it.
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte(historical), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	result, err = app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope: "edits", Path: "notes.txt", StartLine: 1, EndLine: 5,
		VerifyPatch: verifyPatch, EditPayloadID: "payload-early", EditTurnIndex: -1,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines(workspace fallthrough) error = %v", err)
	}
	if len(result.Lines) != 5 || result.Lines[0] != "line 1" {
		t.Fatalf("workspace fallthrough result = %+v", result)
	}
}

// Claude's structuredPatch ships leading tabs as two spaces per tab, so
// a tab-indented file (all gofmt'd Go) never byte-matches its own edit
// diff. Verification must tolerate exactly that transform, and the
// served context lines must come back tab-expanded so they indent like
// the hunk lines they sit between.
func TestGetDiffContextLinesEditsScopeTabMangledPatch(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := t.TempDir()
	thread := testThread("thread-diff-context-edits-tabs")
	thread.WorkspacePath = workspace
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}

	content := "package x\n" +
		"\n" +
		"func f() {\n" +
		"\tbefore()\n" +
		"\tcall()\n" +
		"\tafter()\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(workspace, "x.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	verifyPatch := "diff --git a/x.go b/x.go\n" +
		"--- a/x.go\n" +
		"+++ b/x.go\n" +
		"@@ -4,3 +4,3 @@\n" +
		"   before()\n" +
		"-  old()\n" +
		"+  call()\n" +
		"   after()\n"

	result, err := app.GetDiffContextLines(thread.ID, DiffContextRequest{
		Scope:       "edits",
		Path:        "x.go",
		StartLine:   1,
		EndLine:     7,
		VerifyPatch: verifyPatch,
	})
	if err != nil {
		t.Fatalf("GetDiffContextLines(tab-mangled) error = %v", err)
	}
	if len(result.Lines) != 7 {
		t.Fatalf("len(Lines) = %d, want 7", len(result.Lines))
	}
	if result.Lines[3] != "  before()" || result.Lines[4] != "  call()" {
		t.Fatalf("served lines not tab-expanded: %q", result.Lines[3:5])
	}
}
