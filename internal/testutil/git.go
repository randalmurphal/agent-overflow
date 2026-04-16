package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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

// CanonicalPath resolves symlinks and cleans the path, suitable for comparing
// filesystem paths that may go through /tmp symlinks on macOS.
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
