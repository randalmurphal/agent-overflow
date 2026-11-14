package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
)

// seedRepo creates a git repo with one committed file so tests have a
// stable baseline. Mirrors the helper in root-package commit tests.
func seedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	run("add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=Tester", "commit", "-q", "-m", "init")
	return dir
}

func TestRecentCommitSubjectsNewestFirstSkippingMerges(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	commit := func(name, subject string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		testutil.RunGit(t, repo, "add", "-A")
		testutil.RunGit(t, repo, "commit", "-q", "-m", subject)
	}
	commit("a.txt", "feat: add a")
	testutil.RunGit(t, repo, "checkout", "-q", "-b", "side")
	commit("b.txt", "fix: add b")
	testutil.RunGit(t, repo, "checkout", "-q", "main")
	commit("c.txt", "chore: add c")
	testutil.RunGit(t, repo, "merge", "-q", "--no-ff", "-m", "merge side", "side")

	core := NewCore()
	got := core.RecentCommitSubjects(repo, 3)
	want := []string{"chore: add c", "fix: add b", "feat: add a"}
	if len(got) != len(want) {
		t.Fatalf("subjects = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subjects = %q, want %q", got, want)
		}
	}
}

func TestRecentCommitSubjectsBestEffortOnEmptyOrInvalid(t *testing.T) {
	t.Parallel()
	core := NewCore()
	if got := core.RecentCommitSubjects(t.TempDir(), 20); got != nil {
		t.Fatalf("non-repo: got %q, want nil", got)
	}
	repo := t.TempDir()
	testutil.RunGit(t, repo, "init", "-q")
	if got := core.RecentCommitSubjects(repo, 20); got != nil {
		t.Fatalf("repo without commits: got %q, want nil", got)
	}
	if got := core.RecentCommitSubjects(repo, 0); got != nil {
		t.Fatalf("n=0: got %q, want nil", got)
	}
}
