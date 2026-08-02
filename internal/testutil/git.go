package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// InitGitRepo creates a temporary git repository with an initial commit and
// returns its path. The repo uses "main" as the default branch.
func InitGitRepo(t *testing.T) string {
	t.Helper()

	repo := t.TempDir()
	if err := RunGitAllowError(repo, "init", "-b", "main"); err != nil {
		RunGit(t, repo, "init")
		RunGit(t, repo, "checkout", "-b", "main")
	}
	RunGit(t, repo, "config", "user.name", "Agent Overflow")
	RunGit(t, repo, "config", "user.email", "agent-overflow@example.com")

	filePath := filepath.Join(repo, "README.txt")
	if err := os.WriteFile(filePath, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write repo file: %v", err)
	}
	RunGit(t, repo, "add", "README.txt")
	RunGit(t, repo, "commit", "-m", "initial commit")

	return repo
}

// InitGitRepoWithOrigin creates a bare repository plus a working clone
// that has it as `origin` with `main` pushed and tracking. Returns
// (workingRepo, barePath).
//
// The bare repo is a local path, so nothing here touches a network: it
// behaves like a real remote for fetch/push/ahead-behind purposes while
// staying inside t.TempDir().
func InitGitRepoWithOrigin(t *testing.T) (string, string) {
	t.Helper()

	bare := t.TempDir()
	if err := RunGitAllowError(bare, "init", "--bare", "-b", "main"); err != nil {
		RunGit(t, bare, "init", "--bare")
	}
	repo := InitGitRepo(t)
	RunGit(t, repo, "remote", "add", "origin", bare)
	RunGit(t, repo, "push", "-u", "origin", "main")
	return repo, bare
}

// AdvanceOriginMain pushes one commit to bare's main branch through a
// throwaway sibling clone, simulating a collaborator pushing while the
// app's clone isn't looking. Returns once the bare repo has the new tip;
// clones of it are one commit behind until they fetch.
func AdvanceOriginMain(t *testing.T, bare string) {
	t.Helper()

	sibling := t.TempDir()
	RunGit(t, sibling, "clone", bare, ".")
	// Fixed filename (tests assert on it), fresh content every call so a
	// second advance against the same bare still has something to commit.
	const name = "outside.txt"
	body := fmt.Sprintf("upstream %d\n", time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(sibling, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	RunGit(t, sibling, "add", name)
	RunGit(t, sibling, "-c", "user.email=outside@example.com", "-c", "user.name=Outside",
		"commit", "-m", "upstream commit")
	RunGit(t, sibling, "push", "origin", "main")
}

// RunGit executes a git command and fails the test if it returns an error.
func RunGit(t *testing.T, cwd string, args ...string) {
	t.Helper()

	if err := RunGitAllowError(cwd, args...); err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
}

// RunGitAllowError executes a git command, returning any error instead of
// failing the test.
func RunGitAllowError(cwd string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, string(output))
	}
	return nil
}

// GonePruneRepo builds a repo with a local bare "origin" so upstream
// tracking (and its "gone" state after remote deletion) behaves exactly
// as against a real remote:
//
//	merged-gone    pushed, merged into main (merge commit), deleted on remote
//	squashed-gone  pushed, remote deleted WITHOUT merging (tip not on main)
//	local-only     never pushed — has no upstream to be "gone"
//	main           default (origin/HEAD set); checked out
func GonePruneRepo(t *testing.T) string {
	t.Helper()
	repo := InitGitRepo(t)
	origin := t.TempDir()
	RunGit(t, origin, "init", "--bare")
	RunGit(t, repo, "remote", "add", "origin", origin)
	RunGit(t, repo, "push", "-u", "origin", "main")
	RunGit(t, repo, "remote", "set-head", "origin", "main")

	addBranchCommit := func(branch, file, subject string) {
		RunGit(t, repo, "checkout", "-b", branch, "main")
		if err := os.WriteFile(filepath.Join(repo, file), []byte(file), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		RunGit(t, repo, "add", file)
		RunGit(t, repo, "commit", "-m", subject)
	}

	addBranchCommit("merged-gone", "merged.txt", "work on merged-gone")
	RunGit(t, repo, "push", "-u", "origin", "merged-gone")
	RunGit(t, repo, "checkout", "main")
	RunGit(t, repo, "merge", "--no-ff", "merged-gone")
	RunGit(t, repo, "push", "origin", "--delete", "merged-gone")

	// Subject carries a '|' so delimiter handling in ref-listing parsers
	// stays exercised.
	addBranchCommit("squashed-gone", "squashed.txt", "squash | pipes kept")
	RunGit(t, repo, "push", "-u", "origin", "squashed-gone")
	RunGit(t, repo, "push", "origin", "--delete", "squashed-gone")

	addBranchCommit("local-only", "local.txt", "work on local-only")
	RunGit(t, repo, "checkout", "main")
	return repo
}

// CanonicalPath resolves symlinks and cleans the path, suitable for comparing
// filesystem paths that may go through /tmp symlinks on macOS.
//
// This duplicates git.CanonicalPath intentionally to avoid a circular import
// (internal/git test files import testutil, so testutil cannot import
// internal/git). Production code should use git.CanonicalPath or
// git.SameFilesystemPath directly.
func CanonicalPath(t *testing.T, path string) string {
	if t != nil {
		t.Helper()
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}
