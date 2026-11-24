package git

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
	"agent-overflow/internal/testutil/fixturedir"
)

// The repository testutil.InitGitRepo builds is identical for every test,
// so the package builds it once, in the run's fixture directory, and copies
// it: a few dozen small files and one index refresh cost a fraction of the
// five git processes that create them.
var repoTemplate = fixturedir.New("git-repo", func(tb testing.TB, dir string) {
	if err := buildRepoTemplate(dir); err != nil {
		tb.Fatalf("build template repository: %v", err)
	}
})

// initGitRepo returns a fresh copy of testutil.InitGitRepo's repository:
// branch main, a configured identity and one commit of README.txt.
func initGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS(repoTemplate.Path(t))); err != nil {
		t.Fatalf("copy template repository: %v", err)
	}
	// The copy gives README.txt a new inode and mtime. Refresh the index so
	// Git sees a clean tree, as it would in a repository built in place.
	testutil.RunGit(t, repo, "update-index", "--refresh", "-q")
	return repo
}

func buildRepoTemplate(dir string) error {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("hello\n"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Agent Overflow"},
		{"config", "user.email", "agent-overflow@example.com"},
		{"add", "README.txt"},
		{"commit", "-m", "initial commit"},
	} {
		if err := testutil.RunGitAllowError(dir, args...); err != nil {
			return fmt.Errorf("git %v: %w", args, err)
		}
	}
	return nil
}
