package harnessrpc

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness/forgefake"
)

// forgeOriginRepo creates project's origin through origins, as
// HarnessForgeOrigin does, and pushes to it as publishPullRequest does:
// main, a feature branch one commit ahead, and the pull's head ref. It
// answers a git runner on that repository and the head sha.
func forgeOriginRepo(t *testing.T, origins *forgeOrigins, project, forge string, number int) (func(args ...string) string, string) {
	t.Helper()
	bare, err := origins.create(project)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	run := testGit(t)
	run(work, "init", "--quiet", "-b", "main")
	return forgeOriginPush(t, work, bare, forge, number)
}

// testGit runs git in dir with no system or global configuration and a
// fixed identity.
func testGit(t *testing.T) func(dir string, args ...string) string {
	return func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GIT_") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
}

// forgeOriginPush commits a.go and run.sh on main in the new repository
// work, then b.go on feature, pushes both to bare and sets the pull's
// head ref there.
func forgeOriginPush(t *testing.T, work, bare, forge string, number int) (func(args ...string) string, string) {
	t.Helper()
	run := testGit(t)
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
	origins := newForgeOrigins(filepath.Join(t.TempDir(), "forge-origin"))
	commit := origins.commit
	for _, forge := range []string{"github", "gitlab"} {
		project := "acme/" + forge
		git, head := forgeOriginRepo(t, origins, project, forge, 7)
		// A clone checks out main, as a forge's default branch.
		if got := git("symbolic-ref", "HEAD"); got != "refs/heads/main" {
			t.Fatalf("%s: the origin's HEAD = %s", forge, got)
		}
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

// treeDigest is every file and link under dir, by path, with its
// content or target: two digests are equal when nothing under dir was
// written.
func treeDigest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			out[rel] = "link " + target
			return err
		case d.IsDir():
			out[rel] = "dir"
		default:
			data, err := os.ReadFile(path)
			out[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// applyThroughTheFake seeds a fake forge whose commits go to origins with
// GitLab pull grp/s!3 on head (branch feature) and applies its
// suggestion, answering the status and the pull's new head.
func applyThroughTheFake(t *testing.T, origins *forgeOrigins, head string) (int, string) {
	t.Helper()
	const token = "origin-test-token"
	engine := forgefake.New(forgefake.Options{APIToken: token, Commit: origins.commit})
	line := 2
	fixture := forgefake.Fixture{Viewer: "octo", Repos: []forgefake.Repo{{Forge: "gitlab", Project: "grp/s", Pulls: []forgefake.Pull{{
		Number: 3, Title: "S", Author: "someone", HeadRef: "feature", HeadSHA: head,
		Files: map[string]string{"a.go": "one\ntwo\nthree\n"},
		Threads: []forgefake.Thread{{ID: "d1", Path: "a.go", Line: &line, Side: "right", Comments: []forgefake.Comment{
			{ID: 70, Author: "someone", Body: "```suggestion\nTWO\n```"},
		}}},
	}}}}}
	if _, err := engine.Seed(fixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	apply := func() *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), "PUT", srv.URL+"/gitlab/api/v4/suggestions/70000/apply", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "gitlab.com"
		req.Header.Set("Private-Token", token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	resp := apply()
	var mr struct {
		SHA string `json:"sha"`
	}
	read, err := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/gitlab/api/v4/projects/grp%2Fs/merge_requests/3", nil)
	if err != nil {
		t.Fatal(err)
	}
	read.Host = "gitlab.com"
	read.Header.Set("Private-Token", token)
	got, err := srv.Client().Do(read)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Body.Close() }()
	if err := json.NewDecoder(got.Body).Decode(&mr); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, mr.SHA
}

// The origin callback writes only to an origin the harness created: a
// symlink in place of the origin or of a parent directory, a directory
// it did not create, an origin with alternates, one whose git directory
// is redirected (core.bare off, a commondir file, a gitdir file) and git
// variables in the harness's environment each leave the other repository
// byte for byte as it was, and the apply still answers, with a generated
// head.
func TestForgeOriginCommitWritesOnlyToItsOwnOrigins(t *testing.T) {
	type rig struct {
		origins *forgeOrigins
		root    string // the origin root, as configured
		victim  string // the other repository, a bare one in the test's temp dir
		head    string // the head both share
	}
	setup := func(t *testing.T) rig {
		root := filepath.Join(t.TempDir(), "forge-origin")
		origins := newForgeOrigins(root)
		// The origin root exists and grp/s is the project under attack.
		if _, err := origins.create("grp/other"); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(t.TempDir(), "outside", "s.git")
		if err := os.MkdirAll(victim, 0o755); err != nil {
			t.Fatal(err)
		}
		testGit(t)(victim, "init", "--bare", "--quiet")
		work := t.TempDir()
		testGit(t)(work, "init", "--quiet", "-b", "main")
		_, head := forgeOriginPush(t, work, victim, "gitlab", 3)
		return rig{origins: origins, root: root, victim: victim, head: head}
	}
	// own creates grp/s as the harness does and pushes the victim's
	// history to it.
	own := func(t *testing.T, r rig) string {
		bare, err := r.origins.create("grp/s")
		if err != nil {
			t.Fatal(err)
		}
		testGit(t)(r.victim, "push", "--quiet", bare, "main", "feature")
		return bare
	}
	cases := map[string]func(t *testing.T, r rig) (watched string){
		"a symlinked repository directory": func(t *testing.T, r rig) string {
			bare := own(t, r)
			if err := os.RemoveAll(bare); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(r.victim, bare); err != nil {
				t.Fatal(err)
			}
			return r.victim
		},
		"a symlinked parent": func(t *testing.T, r rig) string {
			bare := own(t, r)
			if err := os.RemoveAll(filepath.Dir(bare)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Dir(r.victim), filepath.Dir(bare)); err != nil {
				t.Fatal(err)
			}
			return r.victim
		},
		"a symlink to another origin of its own": func(t *testing.T, r rig) string {
			other := filepath.Join(r.root, "grp", "other.git")
			testGit(t)(r.victim, "push", "--quiet", other, "main", "feature")
			if err := os.Symlink(other, filepath.Join(r.root, "grp", "s.git")); err != nil {
				t.Fatal(err)
			}
			return other
		},
		"a directory it did not create": func(t *testing.T, r rig) string {
			bare := filepath.Join(r.root, "grp", "s.git")
			if err := os.Rename(r.victim, bare); err != nil {
				t.Fatal(err)
			}
			return bare
		},
		"an origin with alternates": func(t *testing.T, r rig) string {
			bare := own(t, r)
			if err := os.WriteFile(filepath.Join(bare, "objects", "info", "alternates"), []byte(filepath.Join(r.victim, "objects")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return bare
		},
		"an origin git reads as not bare": func(t *testing.T, r rig) string {
			bare := own(t, r)
			testGit(t)(bare, "config", "core.bare", "false")
			return bare
		},
		"an origin whose common directory is elsewhere": func(t *testing.T, r rig) string {
			bare := own(t, r)
			if err := os.WriteFile(filepath.Join(bare, "commondir"), []byte(r.victim+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return r.victim
		},
		"an origin replaced by a gitdir file": func(t *testing.T, r rig) string {
			bare := own(t, r)
			if err := os.RemoveAll(bare); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bare, []byte("gitdir: "+r.victim+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return r.victim
		},
	}
	for name, attack := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := setup(t)
			watched := attack(t, r)
			before := treeDigest(t, watched)
			status, head := applyThroughTheFake(t, r.origins, r.head)
			if status != http.StatusOK || len(head) != 40 || head == r.head {
				t.Fatalf("apply = %d, head %q", status, head)
			}
			if after := treeDigest(t, watched); !maps.Equal(before, after) {
				t.Fatalf("%s was written: %d entries before, %d after", watched, len(before), len(after))
			}
		})
	}
}

// git variables in the harness's own environment do not redirect a
// commit to its own origin: the objects and refs land there and the
// repository they name is untouched.
func TestForgeOriginCommitIgnoresTheEnvironmentsGitVariables(t *testing.T) {
	root := filepath.Join(t.TempDir(), "forge-origin")
	origins := newForgeOrigins(root)
	victim := filepath.Join(t.TempDir(), "victim.git")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t)(victim, "init", "--bare", "--quiet")
	work := t.TempDir()
	testGit(t)(work, "init", "--quiet", "-b", "main")
	_, head := forgeOriginPush(t, work, victim, "gitlab", 3)
	bare, err := origins.create("grp/s")
	if err != nil {
		t.Fatal(err)
	}
	testGit(t)(victim, "push", "--quiet", bare, "main", "feature")
	before := treeDigest(t, victim)
	t.Setenv("GIT_DIR", victim)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(victim, "objects"))
	t.Setenv("GIT_COMMON_DIR", victim)
	status, sha := applyThroughTheFake(t, origins, head)
	if status != http.StatusOK || sha == head {
		t.Fatalf("apply = %d, head %q", status, sha)
	}
	if after := treeDigest(t, victim); !maps.Equal(before, after) {
		t.Fatalf("the repository the environment names was written")
	}
	if got := testGit(t)(bare, "rev-parse", "refs/heads/feature"); got != sha {
		t.Fatalf("the origin's feature = %s, want the commit %s", got, sha)
	}
}
