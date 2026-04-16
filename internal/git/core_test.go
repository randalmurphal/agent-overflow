package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
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
	repo := testutil.InitGitRepo(t)
	core := NewCore()
	worktreePath := filepath.Join(t.TempDir(), "feature-demo")

	if err := core.CreateWorktree(repo, worktreePath, "feature/demo"); err != nil {
		t.Fatalf("CreateWorktree returned error: %v", err)
	}

	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("expected worktree path to exist: %v", err)
	}
	expectedPath := testutil.CanonicalPath(t, worktreePath)

	worktrees, err := core.ListWorktrees(repo)
	if err != nil {
		t.Fatalf("ListWorktrees returned error: %v", err)
	}

	found := false
	for _, worktree := range worktrees {
		if testutil.CanonicalPath(t, worktree.Path) != expectedPath {
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
		if testutil.CanonicalPath(t, worktree.Path) == expectedPath {
			t.Fatalf("worktree %q still present after removal", worktreePath)
		}
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("expected worktree path to be removed, stat err = %v", err)
	}
}

func TestCreateWorktreeRequiresPath(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	err := core.CreateWorktree(repo, "  ", "feature/x")
	if err == nil {
		t.Fatal("expected error for empty worktree path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateWorktreeRequiresBranch(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	err := core.CreateWorktree(repo, filepath.Join(t.TempDir(), "wt"), "  ")
	if err == nil {
		t.Fatal("expected error for empty worktree branch")
	}
	if !strings.Contains(err.Error(), "branch is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateWorktreeRejectsInvalidBranch(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	err := core.CreateWorktree(repo, filepath.Join(t.TempDir(), "wt"), "--bad")
	if err == nil {
		t.Fatal("expected error for invalid branch name")
	}
	if !strings.Contains(err.Error(), "must not start with -") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateWorktreeFailsOnConflictingBranch(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// "main" already exists, so creating a worktree with branch "main" fails.
	err := core.CreateWorktree(repo, filepath.Join(t.TempDir(), "wt"), "main")
	if err == nil {
		t.Fatal("expected error for duplicate branch name")
	}
	if !strings.Contains(err.Error(), "worktree add failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRemoveWorktreeRequiresPath(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	err := core.RemoveWorktree(repo, "  ")
	if err == nil {
		t.Fatal("expected error for empty worktree path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRemoveWorktreeFailsOnNonExistent(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	err := core.RemoveWorktree(repo, filepath.Join(t.TempDir(), "no-such-wt"))
	if err == nil {
		t.Fatal("expected error for non-existent worktree")
	}
	if !strings.Contains(err.Error(), "worktree remove failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListWorktreesOnNonRepo(t *testing.T) {
	core := NewCore()

	_, err := core.ListWorktrees(t.TempDir())
	if err == nil {
		t.Fatal("expected error for non-repo directory")
	}
}

func TestFormatCommandQuotesSpecialChars(t *testing.T) {
	got := formatCommand("git", "commit", "-m", "hello world")
	if !strings.Contains(got, `"hello world"`) {
		t.Fatalf("expected quoted arg, got %q", got)
	}
}

func TestLimitedBufferMultipleWritesBeyondLimit(t *testing.T) {
	buf := newLimitedBuffer(6)

	if _, err := buf.Write([]byte("abc")); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if buf.Truncated() {
		t.Fatal("should not be truncated after first write")
	}
	if _, err := buf.Write([]byte("defgh")); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if !buf.Truncated() {
		t.Fatal("should be truncated after second write exceeds limit")
	}
	if got := buf.String(); got != "abcdef" {
		t.Fatalf("String() = %q, want abcdef", got)
	}
}

func TestLimitedBufferZeroMaxDropsEverything(t *testing.T) {
	buf := newLimitedBuffer(0)

	n, err := buf.Write([]byte("data"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 4 {
		t.Fatalf("Write returned %d, want 4", n)
	}
	if got := buf.String(); got != "" {
		t.Fatalf("String() = %q, want empty", got)
	}
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
