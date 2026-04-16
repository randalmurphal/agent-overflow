package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestListOpenPRsParsesJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho '[{\"url\":\"https://example.com/pr/7\",\"number\":7,\"title\":\"Feature branch\",\"state\":\"OPEN\"}]'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	prs, err := core.ListOpenPRs(t.TempDir(), "feature/demo")
	if err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("len(prs) = %d, want 1", len(prs))
	}
	if prs[0].URL != "https://example.com/pr/7" {
		t.Fatalf("prs[0].URL = %q, want https://example.com/pr/7", prs[0].URL)
	}
	if prs[0].Title != "Feature branch" {
		t.Fatalf("prs[0].Title = %q, want Feature branch", prs[0].Title)
	}
	if prs[0].State != "OPEN" {
		t.Fatalf("prs[0].State = %q, want OPEN", prs[0].State)
	}
}

func TestCreatePRReturnsURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho 'https://example.com/pr/9'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	url, err := core.CreatePR(t.TempDir(), "Demo PR", "Body")
	if err != nil {
		t.Fatalf("CreatePR returned error: %v", err)
	}
	if url != "https://example.com/pr/9" {
		t.Fatalf("url = %q, want https://example.com/pr/9", url)
	}
}

func TestCreatePRRequiresTitle(t *testing.T) {
	core := NewCore()

	_, err := core.CreatePR(t.TempDir(), "  ", "body")
	if err == nil {
		t.Fatal("expected error for empty title")
	}
	if !strings.Contains(err.Error(), "title is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreatePRHandlesNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho 'auth required' 1>&2\nexit 1\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.CreatePR(t.TempDir(), "Test PR", "body")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "gh pr create failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreatePRHandlesEmptyURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho ''\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.CreatePR(t.TempDir(), "Test PR", "body")
	if err == nil {
		t.Fatal("expected error for empty URL output")
	}
	if !strings.Contains(err.Error(), "empty URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreatePRHandlesMissingGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	core := NewCore()
	_, err := core.CreatePR(t.TempDir(), "Test PR", "body")
	if err == nil {
		t.Fatal("expected missing gh error")
	}
	if !strings.Contains(err.Error(), "GitHub CLI (`gh`)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListOpenPRsRequiresHead(t *testing.T) {
	core := NewCore()

	_, err := core.ListOpenPRs(t.TempDir(), "  ")
	if err == nil {
		t.Fatal("expected error for empty head")
	}
	if !strings.Contains(err.Error(), "head branch is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListOpenPRsHandlesNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\necho 'no repo' 1>&2\nexit 1\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ListOpenPRs(t.TempDir(), "main")
	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	if !strings.Contains(err.Error(), "gh pr list failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListOpenPRsReturnsNilForEmptyOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	prs, err := core.ListOpenPRs(t.TempDir(), "main")
	if err != nil {
		t.Fatalf("ListOpenPRs returned error: %v", err)
	}
	if prs != nil {
		t.Fatalf("expected nil prs, got %v", prs)
	}
}

func TestCommandOutputMessage(t *testing.T) {
	if got := commandOutputMessage("", "stderr msg"); got != "stderr msg" {
		t.Fatalf("commandOutputMessage with stderr = %q, want stderr msg", got)
	}
	if got := commandOutputMessage("stdout msg", ""); got != "stdout msg" {
		t.Fatalf("commandOutputMessage with stdout = %q, want stdout msg", got)
	}
	if got := commandOutputMessage("", ""); got != "command failed" {
		t.Fatalf("commandOutputMessage with empty = %q, want command failed", got)
	}
}

func TestListOpenPRsHandlesMissingGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	core := NewCore()
	_, err := core.ListOpenPRs(t.TempDir(), "main")
	if err == nil {
		t.Fatal("expected missing gh error")
	}
	if !strings.Contains(err.Error(), "GitHub CLI (`gh`)") {
		t.Fatalf("expected missing gh message, got %v", err)
	}
}
