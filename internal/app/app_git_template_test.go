package app

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/testutil"
)

// Each git process costs tens of milliseconds on macOS, and a fixture
// repository takes several of them. A gitSnapshotTemplate runs a fixture's
// own builder once, in the first test that asks for it, and gives every
// test, that first one included, a copy. Copies of one fixture share their
// commit IDs; a test that needs unrelated histories, such as two computers
// in a transfer, calls the builder directly.
//
// A fixture can be several related repositories, such as a clone and its
// bare origin. Each copied repository gets its own t.TempDir, and the remote
// URLs and FETCH_HEAD that name the others by absolute path are rewritten to
// the copies.
type gitSnapshotTemplate struct {
	once  sync.Once
	dir   string
	count int
	built bool
	err   error
}

var fixtureTemplateRoot struct {
	sync.Mutex
	dir string
}

// removeFixtureTemplates deletes the fixture templates, git repositories and
// databases, built by this test binary. TestMain calls it after the tests
// finish.
func removeFixtureTemplates() error {
	fixtureTemplateRoot.Lock()
	defer fixtureTemplateRoot.Unlock()
	if fixtureTemplateRoot.dir == "" {
		return nil
	}
	return os.RemoveAll(fixtureTemplateRoot.dir)
}

func newFixtureTemplateDir() (string, error) {
	fixtureTemplateRoot.Lock()
	defer fixtureTemplateRoot.Unlock()
	if fixtureTemplateRoot.dir == "" {
		dir, err := os.MkdirTemp("", "agent-overflow-app-fixture-templates-")
		if err != nil {
			return "", err
		}
		fixtureTemplateRoot.dir = dir
	}
	return os.MkdirTemp(fixtureTemplateRoot.dir, "repo-")
}

// clone returns the copied repositories in the order build returned the
// originals.
func (tpl *gitSnapshotTemplate) clone(t *testing.T, build func(t *testing.T) []string) []string {
	t.Helper()
	tpl.once.Do(func() {
		sources := build(t)
		tpl.count = len(sources)
		tpl.dir, tpl.err = newFixtureTemplateDir()
		if tpl.err == nil {
			tpl.err = snapshotGitRepos(sources, tpl.dir)
		}
		tpl.built = true
	})
	if !tpl.built {
		t.Fatal("git snapshot template: its build failed in an earlier test")
	}
	if tpl.err != nil {
		t.Fatalf("build git snapshot template: %v", tpl.err)
	}
	dirs := make([]string, tpl.count)
	moves := make(map[string]string, tpl.count)
	for i := range dirs {
		dirs[i] = t.TempDir()
		source := filepath.Join(tpl.dir, fmt.Sprint(i))
		if err := copyTemplateTree(source, dirs[i]); err != nil {
			t.Fatalf("copy git snapshot template: %v", err)
		}
		moves[source] = dirs[i]
	}
	for _, dir := range dirs {
		if err := rewriteGitRepoPaths(dir, moves); err != nil {
			t.Fatalf("relocate git snapshot template: %v", err)
		}
		// The index records stat data of the template's files. Without a
		// refresh, plumbing such as `git diff-index` reports the copies
		// modified.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			testutil.RunGit(t, dir, "update-index", "-q", "--refresh")
		}
	}
	return dirs
}

// snapshotGitRepos copies each source to dir/<index>, without the inert
// sample hooks git init installs, and rewrites the sources' paths to the
// copies'.
func snapshotGitRepos(sources []string, dir string) error {
	moves := make(map[string]string, len(sources))
	for i, source := range sources {
		target := filepath.Join(dir, fmt.Sprint(i))
		if err := os.Mkdir(target, 0o755); err != nil {
			return err
		}
		if err := copyTemplateTree(source, target); err != nil {
			return err
		}
		for _, hooks := range []string{filepath.Join(target, ".git", "hooks"), filepath.Join(target, "hooks")} {
			samples, err := filepath.Glob(filepath.Join(hooks, "*.sample"))
			if err != nil {
				return err
			}
			for _, sample := range samples {
				if err := os.Remove(sample); err != nil {
					return err
				}
			}
		}
		moves[source] = target
	}
	return rewriteGitRepoPaths(dir, moves)
}

// rewriteGitRepoPaths replaces each old path with its new one in the git
// files under root that record other repositories' locations. Longer paths
// are replaced first so that a path that prefixes another cannot split it.
func rewriteGitRepoPaths(root string, moves map[string]string) error {
	olds := make([]string, 0, len(moves))
	for old := range moves {
		olds = append(olds, old)
	}
	sort.Slice(olds, func(i, j int) bool { return len(olds[i]) > len(olds[j]) })
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if name := entry.Name(); name != "config" && name != "FETCH_HEAD" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, old := range olds {
			text = strings.ReplaceAll(text, old, moves[old])
		}
		if text == string(data) {
			return nil
		}
		return os.WriteFile(path, []byte(text), 0o644)
	})
}

func copyTemplateTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("copy %s: unsupported file mode %v", path, info.Mode())
		}
		return copyTemplateFile(path, target, info.Mode().Perm())
	})
}

func copyTemplateFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var mainGitRepoTemplate gitSnapshotTemplate

// initMainGitRepo is testutil.InitGitRepo from a template: branch main,
// README.txt, one commit.
func initMainGitRepo(t *testing.T) string {
	t.Helper()
	return mainGitRepoTemplate.clone(t, func(t *testing.T) []string {
		return []string{testutil.InitGitRepo(t)}
	})[0]
}

var originGitRepoTemplate gitSnapshotTemplate

// initGitRepoWithOrigin is testutil.InitGitRepoWithOrigin from a template:
// the clone and its bare origin, with main pushed and tracked.
func initGitRepoWithOrigin(t *testing.T) (string, string) {
	t.Helper()
	dirs := originGitRepoTemplate.clone(t, func(t *testing.T) []string {
		repo, bare := testutil.InitGitRepoWithOrigin(t)
		return []string{repo, bare}
	})
	return dirs[0], dirs[1]
}

// TestGitSnapshotTemplateCopiesAreIndependent pins the relocation: a copy's
// remote is its own origin, so a push from one test cannot reach the
// template or another test's copy.
func TestGitSnapshotTemplateCopiesAreIndependent(t *testing.T) {
	t.Parallel()
	repo, bare := initGitRepoWithOrigin(t)
	if url := strings.TrimSpace(gitOutput(t, repo, "config", "--get", "remote.origin.url")); url != bare {
		t.Fatalf("remote.origin.url = %q, want the copy's origin %q", url, bare)
	}
	testutil.RunGit(t, repo, "push", "origin", "main:pushed-by-first-copy")

	_, otherBare := initGitRepoWithOrigin(t)
	if refs := gitOutput(t, otherBare, "branch", "--list"); strings.Contains(refs, "pushed-by-first-copy") {
		t.Fatalf("a push from one copy reached another copy's origin:\n%s", refs)
	}
	if refs := gitOutput(t, bare, "branch", "--list"); !strings.Contains(refs, "pushed-by-first-copy") {
		t.Fatalf("the push did not reach the copy's own origin:\n%s", refs)
	}
	if status := gitOutput(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("a fresh copy has a dirty work tree:\n%s", status)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
