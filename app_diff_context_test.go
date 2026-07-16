package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
