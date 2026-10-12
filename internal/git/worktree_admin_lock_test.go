package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/mockexec"
)

// Two worktrees cut at once from one repository run their `git worktree`
// commands one at a time. Git 2.43 dies when one add reads the admin
// directory another add has created but not yet filled, so overlapping adds
// fail intermittently with "failed to read .../commondir".
func TestWorktreeCommandsOfOneRepositoryDoNotOverlap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script git wrapper is unix-only")
	}
	repo := initGitRepo(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper runs real git and records a `worktree` command that starts
	// while another is still running.
	marks := t.TempDir()
	overlaps := filepath.Join(marks, "overlaps")
	binDir := t.TempDir()
	mockexec.Write(t, filepath.Join(binDir, "git"), `#!/bin/sh
if [ "$1" = worktree ]; then
	if ! mkdir "$AO_WORKTREE_MARK" 2>/dev/null; then
		echo "$*" >> "$AO_WORKTREE_OVERLAPS"
		"$AO_REAL_GIT" "$@"
		exit $?
	fi
	sleep 0.3
	"$AO_REAL_GIT" "$@"
	status=$?
	rmdir "$AO_WORKTREE_MARK"
	exit $status
fi
exec "$AO_REAL_GIT" "$@"
`)
	t.Setenv("AO_REAL_GIT", realGit)
	t.Setenv("AO_WORKTREE_MARK", filepath.Join(marks, "running"))
	t.Setenv("AO_WORKTREE_OVERLAPS", overlaps)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	worktrees := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i, branch := range []string{"unit-alpha", "unit-beta"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = core.CreateWorktreeFromBranch(repo, filepath.Join(worktrees, branch), "main", branch)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errs[2] = core.ListWorktrees(repo)
	}()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if data, err := os.ReadFile(overlaps); err == nil {
		t.Fatalf("worktree commands of one repository overlapped:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	listed, err := core.ListWorktrees(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 {
		t.Fatalf("worktrees = %+v, want the main one and both units", listed)
	}
	for _, branch := range []string{"unit-alpha", "unit-beta"} {
		testutil.RunGit(t, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	}
}
