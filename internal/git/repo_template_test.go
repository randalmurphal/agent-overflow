package git

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agent-overflow/internal/testutil"
)

// The repository testutil.InitGitRepo builds is identical for every test,
// so the package builds it once and copies it: a few dozen small files and
// one index refresh cost a fraction of the five git processes that create
// them.
var (
	repoTemplateOnce sync.Once
	repoTemplateDir  string
	repoTemplateErr  error
)

// initGitRepo returns a fresh copy of testutil.InitGitRepo's repository:
// branch main, a configured identity and one commit of README.txt.
func initGitRepo(t *testing.T) string {
	t.Helper()
	repoTemplateOnce.Do(func() { repoTemplateDir, repoTemplateErr = buildRepoTemplate() })
	if repoTemplateErr != nil {
		t.Fatalf("build template repository: %v", repoTemplateErr)
	}
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS(repoTemplateDir)); err != nil {
		t.Fatalf("copy template repository: %v", err)
	}
	// The copy gives README.txt a new inode and mtime. Refresh the index so
	// Git sees a clean tree, as it would in a repository built in place.
	testutil.RunGit(t, repo, "update-index", "--refresh", "-q")
	return repo
}

func buildRepoTemplate() (string, error) {
	dir, err := os.MkdirTemp("", "ao-git-template-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("hello\n"), 0o644); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Agent Overflow"},
		{"config", "user.email", "agent-overflow@example.com"},
		{"add", "README.txt"},
		{"commit", "-m", "initial commit"},
	} {
		if err := testutil.RunGitAllowError(dir, args...); err != nil {
			return "", fmt.Errorf("git %v: %w", args, err)
		}
	}
	return dir, nil
}

// removeRepoTemplate deletes the template after the package's tests ran.
func removeRepoTemplate() error {
	if repoTemplateDir == "" {
		return nil
	}
	return os.RemoveAll(repoTemplateDir)
}
