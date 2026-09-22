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
	if !strings.Contains(err.Error(), `branch "main" already exists`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateWorktreeNormalizesBranchCreatedAfterPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock git is unix-only")
	}

	branchExistsMarker := filepath.Join(t.TempDir(), "branch-exists")
	installBranchRaceGit(t, branchExistsMarker)

	err := NewCore().CreateWorktreeFromBranch(t.TempDir(), filepath.Join(t.TempDir(), "wt"), "", "race")
	if err == nil {
		t.Fatal("expected duplicate branch error")
	}
	if !strings.Contains(err.Error(), `branch "race" already exists`) {
		t.Fatalf("error = %v, want duplicate branch message", err)
	}
}

func TestAttachWorktreeAttachesExistingBranch(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// Create a second branch on top of HEAD so we have something existing to
	// attach (the default branch is already checked out in `repo`).
	if err := core.CreateBranch(repo, "feature/existing"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	worktreePath := filepath.Join(t.TempDir(), "existing-wt")
	if err := core.AttachWorktree(repo, worktreePath, "feature/existing"); err != nil {
		t.Fatalf("AttachWorktree: %v", err)
	}

	worktrees, err := core.ListWorktrees(repo)
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	expected := testutil.CanonicalPath(t, worktreePath)
	var found bool
	for _, wt := range worktrees {
		if testutil.CanonicalPath(t, wt.Path) != expected {
			continue
		}
		found = true
		if wt.Branch != "feature/existing" {
			t.Fatalf("worktree.Branch = %q, want feature/existing", wt.Branch)
		}
	}
	if !found {
		t.Fatalf("expected worktree %q in list", worktreePath)
	}
}

func TestAttachWorktreeRequiresPath(t *testing.T) {
	core := NewCore()
	if err := core.AttachWorktree(t.TempDir(), "  ", "main"); err == nil ||
		!strings.Contains(err.Error(), "path is required") {
		t.Fatalf("expected path-required error, got %v", err)
	}
}

func TestAttachWorktreeRequiresBranch(t *testing.T) {
	core := NewCore()
	if err := core.AttachWorktree(t.TempDir(), filepath.Join(t.TempDir(), "wt"), "  "); err == nil ||
		!strings.Contains(err.Error(), "branch is required") {
		t.Fatalf("expected branch-required error, got %v", err)
	}
}

func TestAttachWorktreeRejectsFlagShapedBranch(t *testing.T) {
	core := NewCore()
	err := core.AttachWorktree(t.TempDir(), filepath.Join(t.TempDir(), "wt"), "--orphan")
	if err == nil || !strings.Contains(err.Error(), "must not start with -") {
		t.Fatalf("expected flag-shape rejection, got %v", err)
	}
}

func TestAttachWorktreeRefusesBranchAlreadyCheckedOut(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()

	// Default branch ("main") is already checked out in `repo`. Attaching a
	// second worktree to the same branch should fail with git's invariant.
	err := core.AttachWorktree(repo, filepath.Join(t.TempDir(), "wt"), "main")
	if err == nil {
		t.Fatal("expected error attaching a branch already checked out")
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

// Claude Code locks the worktrees it enters, and git refuses to remove a
// locked worktree with one --force. Removal lifts the lock first, on both
// the guarded and the forced path.
func TestRemoveWorktreeLiftsLock(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "guarded", true: "forced"}[force], func(t *testing.T) {
			repo := testutil.InitGitRepo(t)
			core := NewCore()
			worktreePath := filepath.Join(repo, ".claude", "worktrees", "locked-demo")
			if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := core.CreateWorktree(repo, worktreePath, "worktree-locked-demo"); err != nil {
				t.Fatalf("CreateWorktree: %v", err)
			}
			testutil.RunGit(t, repo, "worktree", "lock", "--", worktreePath)
			locked, err := core.worktreeLocked(repo, worktreePath)
			if err != nil || !locked {
				t.Fatalf("worktreeLocked = %v, %v; want locked", locked, err)
			}

			if err := core.RemoveWorktreeForce(repo, worktreePath, force); err != nil {
				t.Fatalf("RemoveWorktreeForce(force=%v) on a locked worktree: %v", force, err)
			}
			if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
				t.Fatalf("worktree still on disk after removal: %v", err)
			}
			worktrees, err := core.ListWorktrees(repo)
			if err != nil {
				t.Fatal(err)
			}
			for _, wt := range worktrees {
				if SameFilesystemPath(wt.Path, worktreePath) {
					t.Fatalf("worktree still listed after removal: %+v", wt)
				}
			}
		})
	}
}

// A dirty locked worktree still needs the caller's force: lifting the lock
// must not silently bypass git's dirty-tree guard.
func TestRemoveWorktreeLiftsLockButKeepsDirtyGuard(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	core := NewCore()
	worktreePath := filepath.Join(t.TempDir(), "locked-dirty")
	if err := core.CreateWorktree(repo, worktreePath, "feature/locked-dirty"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "scratch.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "worktree", "lock", "--", worktreePath)

	if err := core.RemoveWorktreeForce(repo, worktreePath, false); err == nil {
		t.Fatal("guarded removal of a dirty worktree succeeded; want git's dirty-tree refusal")
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("guarded refusal removed the worktree anyway: %v", err)
	}
	if err := core.RemoveWorktreeForce(repo, worktreePath, true); err != nil {
		t.Fatalf("forced removal after refusal: %v", err)
	}
}

func TestParseWorktreeEntriesReadsBareAndReasonedLocks(t *testing.T) {
	entries := parseWorktreeEntries("worktree /repo\nHEAD abc\nbranch refs/heads/main\n\n" +
		"worktree /repo/.claude/worktrees/x\nHEAD abc\nbranch refs/heads/worktree-x\nlocked\n\n" +
		"worktree /repo/.claude/worktrees/y\nHEAD abc\nbranch refs/heads/worktree-y\nlocked keep me\n\n")
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(entries), entries)
	}
	if entries[0].locked {
		t.Errorf("root reported locked: %+v", entries[0])
	}
	if !entries[1].locked || entries[1].lockReason != "" {
		t.Errorf("bare lock = %+v, want locked without reason", entries[1])
	}
	if !entries[2].locked || entries[2].lockReason != "keep me" {
		t.Errorf("reasoned lock = %+v, want locked with reason", entries[2])
	}
}
