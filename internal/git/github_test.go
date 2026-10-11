package git

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
)

// These tests target the github forge's gh CLI path (CreatePR) directly
// via ForgeByID("github"); its HTTP reads are in github_api_test.go.

func TestCreatePRReturnsURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	argLog := filepath.Join(binDir, "args.log")
	ghPath := filepath.Join(binDir, "gh")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" > %q\necho 'https://example.com/pr/9'\n", argLog)
	mockexec.Write(t, ghPath, script)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	url, err := core.ForgeByID("github").CreatePR(t.Context(), t.TempDir(), "Demo PR", "Body", "release", false)
	if err != nil {
		t.Fatalf("CreatePR returned error: %v", err)
	}
	if url != "https://example.com/pr/9" {
		t.Fatalf("url = %q, want https://example.com/pr/9", url)
	}
	args, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatal(err)
	}
	if argv := string(args); !strings.Contains(argv, "--base release") {
		t.Fatalf("argv = %q, missing base", argv)
	}
	if _, err := core.ForgeByID("github").CreatePR(t.Context(), t.TempDir(), "Default base", "Body", "", false); err != nil {
		t.Fatal(err)
	}
	args, err = os.ReadFile(argLog)
	if err != nil {
		t.Fatal(err)
	}
	if argv := string(args); strings.Contains(argv, "--base") {
		t.Fatalf("argv = %q, unexpected base", argv)
	}
}

func TestCreatePRRequiresTitle(t *testing.T) {
	t.Parallel()
	core := NewCore()

	_, err := core.ForgeByID("github").CreatePR(t.Context(), t.TempDir(), "  ", "body", "", false)
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
	mockexec.Write(t, ghPath, script)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("github").CreatePR(t.Context(), t.TempDir(), "Test PR", "body", "", false)
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
	mockexec.Write(t, ghPath, script)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	_, err := core.ForgeByID("github").CreatePR(t.Context(), t.TempDir(), "Test PR", "body", "", false)
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
	_, err := core.ForgeByID("github").CreatePR(t.Context(), t.TempDir(), "Test PR", "body", "", false)
	if err == nil {
		t.Fatal("expected missing gh error")
	}
	if !strings.Contains(err.Error(), "GitHub CLI (`gh`)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListOpenPRsRequiresHead(t *testing.T) {
	t.Parallel()
	core := NewCore()

	_, err := core.ForgeByID("github").ListOpenPRs(t.Context(), t.TempDir(), "  ")
	if err == nil {
		t.Fatal("expected error for empty head")
	}
	if !strings.Contains(err.Error(), "head branch is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGitHubForgeIDAndBinary(t *testing.T) {
	t.Parallel()
	core := NewCore()
	f := core.ForgeByID("github")
	if f.ID() != "github" {
		t.Errorf("ID() = %q, want github", f.ID())
	}
	if f.BinaryName() != "gh" {
		t.Errorf("BinaryName() = %q, want gh", f.BinaryName())
	}
}
