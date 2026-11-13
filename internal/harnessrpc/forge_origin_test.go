package harnessrpc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness/forgefake"
)

// forgeOriginRepo builds <root>/<project>.git as publishPullRequest
// does: main, a feature branch one commit ahead, and the pull's head
// ref. It answers a git runner on that repository and the head sha.
func forgeOriginRepo(t *testing.T, root, project, forge string, number int) (func(args ...string) string, string) {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(root, filepath.FromSlash(project)+".git")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(forgeOriginGitEnv(filepath.Join(dir, ".git"), filepath.Join(dir, ".git", "index")), "GIT_WORK_TREE="+dir)
		if dir == bare {
			cmd.Env = forgeOriginGitEnv(bare, filepath.Join(t.TempDir(), "index"))
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	run(bare, "init", "--bare", "--quiet")
	run(work, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "a.go"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "run.sh"), []byte("echo\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "--quiet", "-m", "init")
	run(work, "checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "b.go"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "--quiet", "-m", "feature work")
	head := run(work, "rev-parse", "HEAD")
	run(work, "push", "--quiet", bare, "main", "feature")
	run(bare, "update-ref", forgePullHeadRef(forge, number), head)
	return func(args ...string) string { return run(bare, args...) }, head
}

// A commit on a pull bound to an origin repository is a real commit
// there: the file spliced on the head, its mode kept, the message, and
// the head branch and pull ref moved; an unbound pull, or one whose head
// the repository lacks, gets none.
func TestForgeOriginCommit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	commit := forgeOriginCommit(root)
	for _, forge := range []string{"github", "gitlab"} {
		project := "acme/" + forge
		git, head := forgeOriginRepo(t, root, project, forge, 7)
		req := forgefake.CommitRequest{Forge: forge, Project: project, Number: 7, Branch: "feature", Parent: head,
			Path: "a.go", Content: "one\nTWO\nthree\n", Message: "Apply suggestion from code review\n\nCo-authored-by: Octo <1+octo@users.noreply.github.com>"}
		sha, err := commit(req)
		if err != nil || len(sha) != 40 || sha == head {
			t.Fatalf("%s: commit = %q, %v", forge, sha, err)
		}
		switch {
		case git("rev-parse", "refs/heads/feature") != sha:
			t.Fatalf("%s: the head branch did not move", forge)
		case git("rev-parse", forgePullHeadRef(forge, 7)) != sha:
			t.Fatalf("%s: the pull's head ref did not move", forge)
		case git("rev-parse", sha+"^") != head:
			t.Fatalf("%s: parent is not the head", forge)
		case git("show", sha+":a.go") != "one\nTWO\nthree":
			t.Fatalf("%s: a.go = %q", forge, git("show", sha+":a.go"))
		case git("log", "-1", "--format=%B", sha) != req.Message:
			t.Fatalf("%s: message = %q", forge, git("log", "-1", "--format=%B", sha))
		case git("diff", "--name-only", head, sha) != "a.go":
			t.Fatalf("%s: changed %q", forge, git("diff", "--name-only", head, sha))
		}
		// An executable keeps its mode.
		exe := req
		exe.Parent, exe.Path, exe.Content = sha, "run.sh", "echo hi\n"
		next, err := commit(exe)
		if err != nil || !strings.HasPrefix(git("ls-tree", next, "--", "run.sh"), "100755 ") {
			t.Fatalf("%s: executable = %q, %v", forge, git("ls-tree", next, "--", "run.sh"), err)
		}
		// A branch that moved since the parent is not overwritten.
		stale := req
		if _, err := commit(stale); err == nil || git("rev-parse", "refs/heads/feature") != next {
			t.Fatalf("%s: a commit on a stale parent = %v", forge, err)
		}
	}
	for name, req := range map[string]forgefake.CommitRequest{
		"no origin repository": {Forge: "github", Project: "acme/none", Number: 1, Branch: "feature", Parent: strings.Repeat("a", 40), Path: "a.go"},
		"a head it lacks":      {Forge: "github", Project: "acme/github", Number: 7, Branch: "feature", Parent: strings.Repeat("b", 40), Path: "a.go"},
		"a path escaping root": {Forge: "github", Project: "../x", Number: 7, Branch: "feature", Parent: strings.Repeat("b", 40), Path: "a.go"},
	} {
		if sha, err := commit(req); sha != "" || err != nil {
			t.Fatalf("%s: commit = %q, %v", name, sha, err)
		}
	}
}
