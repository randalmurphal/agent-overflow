package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExecuteTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock git is unix-only")
	}

	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nsleep 2\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock git: %v", err)
	}

	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+originalPath)

	core := &Core{timeout: 50 * time.Millisecond, maxOutputBytes: defaultMaxOutputBytes}
	_, _, err := core.Execute(t.TempDir(), "status")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestExecuteReturnsStdoutAndStderrOnNonZeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock git is unix-only")
	}

	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\necho 'out'\necho 'err' 1>&2\nexit 4\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock git: %v", err)
	}

	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+originalPath)

	core := NewCore()
	stdout, stderr, err := core.Execute(t.TempDir(), "status")
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if strings.TrimSpace(stdout) != "out" {
		t.Fatalf("stdout = %q, want out", stdout)
	}
	if strings.TrimSpace(stderr) != "err" {
		t.Fatalf("stderr = %q, want err", stderr)
	}
}

func TestParseWorktreeList(t *testing.T) {
	worktrees := parseWorktreeList(
		"worktree /tmp/repo\nHEAD abc123\nbranch refs/heads/main\n\nworktree /tmp/repo-feature\nHEAD def456\nbranch refs/heads/feature/demo\n",
	)

	if len(worktrees) != 2 {
		t.Fatalf("len(worktrees) = %d, want 2", len(worktrees))
	}
	if worktrees[0].Path != "/tmp/repo" {
		t.Fatalf("worktrees[0].Path = %q, want /tmp/repo", worktrees[0].Path)
	}
	if worktrees[0].Branch != "main" {
		t.Fatalf("worktrees[0].Branch = %q, want main", worktrees[0].Branch)
	}
	if worktrees[1].Branch != "feature/demo" {
		t.Fatalf("worktrees[1].Branch = %q, want feature/demo", worktrees[1].Branch)
	}
	if worktrees[1].HEAD != "def456" {
		t.Fatalf("worktrees[1].HEAD = %q, want def456", worktrees[1].HEAD)
	}
}

func TestCreateListAndRemoveWorktree(t *testing.T) {
	repo := initGitRepo(t)
	core := NewCore()
	worktreePath := filepath.Join(t.TempDir(), "feature-demo")

	if err := core.CreateWorktree(repo, worktreePath, "feature/demo"); err != nil {
		t.Fatalf("CreateWorktree returned error: %v", err)
	}

	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected worktree path to exist: %v", err)
	}
	expectedPath := canonicalPath(t, worktreePath)

	worktrees, err := core.ListWorktrees(repo)
	if err != nil {
		t.Fatalf("ListWorktrees returned error: %v", err)
	}

	found := false
	for _, worktree := range worktrees {
		if canonicalPath(t, worktree.Path) != expectedPath {
			continue
		}
		found = true
		if worktree.Branch != "feature/demo" {
			t.Fatalf("worktree.Branch = %q, want feature/demo", worktree.Branch)
		}
		if worktree.HEAD == "" {
			t.Fatal("expected worktree HEAD to be populated")
		}
	}
	if !found {
		t.Fatalf("expected worktree %q in list", worktreePath)
	}

	if err := core.RemoveWorktree(repo, worktreePath); err != nil {
		t.Fatalf("RemoveWorktree returned error: %v", err)
	}

	worktrees, err = core.ListWorktrees(repo)
	if err != nil {
		t.Fatalf("ListWorktrees after remove returned error: %v", err)
	}
	for _, worktree := range worktrees {
		if canonicalPath(t, worktree.Path) == expectedPath {
			t.Fatalf("worktree %q still present after removal", worktreePath)
		}
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree path to be removed, stat err = %v", err)
	}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func TestLimitedBufferTruncates(t *testing.T) {
	buf := newLimitedBuffer(4)

	if _, err := buf.Write([]byte("hello")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if got := buf.String(); got != "hell" {
		t.Fatalf("String() = %q, want hell", got)
	}
	if !buf.Truncated() {
		t.Fatal("expected buffer to report truncation")
	}
}
