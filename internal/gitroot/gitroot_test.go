package gitroot

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The layouts below are hand-written rather than produced by the git CLI:
// they are exactly the four shapes this package has to tell apart, they pin
// the file contents git writes (which is what the resolver reads), and they
// spawn nothing.

func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// want is the canonical form of path, which is what MainRoot returns (macOS
// temp dirs live behind a /private symlink).
func want(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("evalsymlinks %s: %v", path, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		t.Fatalf("abs %s: %v", resolved, err)
	}
	return abs
}

// plainRepo writes the primary-checkout shape: `.git` is a directory.
func plainRepo(t *testing.T, root string) string {
	t.Helper()
	mkdirAll(t, filepath.Join(root, gitDirName))
	return root
}

// linkedWorktree writes the shape `git worktree add` produces: a `.git` FILE
// in the worktree pointing at a private gitdir under the repository, and the
// registration pointing back. commonDir is the `commondir` file's content;
// empty means "write no commondir file" (git before 2.13).
func linkedWorktree(t *testing.T, repoRoot, name, worktreePath, commonDir string) string {
	t.Helper()
	private := filepath.Join(repoRoot, gitDirName, "worktrees", name)
	mkdirAll(t, private)
	writeFile(t, filepath.Join(worktreePath, gitDirName), "gitdir: "+private+"\n")
	writeFile(t, filepath.Join(private, "gitdir"), filepath.Join(worktreePath, gitDirName)+"\n")
	if commonDir != "" {
		writeFile(t, filepath.Join(private, "commondir"), commonDir+"\n")
	}
	return worktreePath
}

// linkedWorktreeRelative writes the same shape with every pointer RELATIVE to
// the file holding it — what git ≥2.48 produces under `worktree add
// --relative-paths` / `worktree.useRelativePaths`.
func linkedWorktreeRelative(t *testing.T, repoRoot, name, worktreePath string) string {
	t.Helper()
	private := mkdirAll(t, filepath.Join(repoRoot, gitDirName, "worktrees", name))
	mkdirAll(t, worktreePath)
	writeFile(t, filepath.Join(worktreePath, gitDirName), "gitdir: "+rel(t, worktreePath, private)+"\n")
	writeFile(t, filepath.Join(private, "gitdir"),
		rel(t, private, filepath.Join(worktreePath, gitDirName))+"\n")
	writeFile(t, filepath.Join(private, "commondir"), "../..\n")
	return worktreePath
}

func rel(t *testing.T, base, target string) string {
	t.Helper()
	path, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("rel %s -> %s: %v", base, target, err)
	}
	return path
}

func TestMainRootResolvesEveryRepositoryLayout(t *testing.T) {
	tmp := t.TempDir()

	repo := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "repo")))
	nested := mkdirAll(t, filepath.Join(repo, "internal", "deep"))

	// A worktree parked well outside the repository, exactly like AO's own
	// (`<configDir>/worktrees/<project>/<branch>`).
	worktree := linkedWorktree(t, repo, "feature",
		mkdirAll(t, filepath.Join(tmp, "worktrees", "repo", "feature")), "../..")
	worktreeSub := mkdirAll(t, filepath.Join(worktree, "cmd"))

	// Same shape, no commondir file: the main root has to come off the
	// gitdir path's `/.git/worktrees/` segment instead.
	legacy := linkedWorktree(t, repo, "legacy",
		mkdirAll(t, filepath.Join(tmp, "worktrees", "repo", "legacy")), "")

	// An ABSOLUTE commondir, which git accepts and older tooling wrote.
	absolute := linkedWorktree(t, repo, "absolute",
		mkdirAll(t, filepath.Join(tmp, "worktrees", "repo", "absolute")),
		filepath.Join(repo, gitDirName))

	// Every pointer written relative, which is git ≥2.48's opt-in layout.
	relative := linkedWorktreeRelative(t, repo, "relative",
		filepath.Join(tmp, "worktrees", "repo", "relative"))

	// A submodule: `.git` is a file pointing into the superproject's
	// modules/ store, which is a repository of its own with no commondir.
	// Its own working tree is the root, which is what git answers too.
	super := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "super")))
	submodule := mkdirAll(t, filepath.Join(super, "vendor", "lib"))
	mkdirAll(t, filepath.Join(super, gitDirName, "modules", "lib"))
	writeFile(t, filepath.Join(submodule, gitDirName),
		"gitdir: "+filepath.Join(super, gitDirName, "modules", "lib")+"\n")

	// A worktree of a BARE repository: the common dir is `repo.git`, whose
	// parent is not a working tree, so the worktree is its own root.
	bare := mkdirAll(t, filepath.Join(tmp, "bare.git", "worktrees", "only"))
	bareWorktree := mkdirAll(t, filepath.Join(tmp, "bare-checkout"))
	writeFile(t, filepath.Join(bareWorktree, gitDirName), "gitdir: "+bare+"\n")
	writeFile(t, filepath.Join(bare, "gitdir"), filepath.Join(bareWorktree, gitDirName)+"\n")
	writeFile(t, filepath.Join(bare, "commondir"), "../..\n")

	cases := []struct {
		name string
		in   string
		out  string
		ok   bool
	}{
		{name: "primary checkout", in: repo, out: repo, ok: true},
		{name: "directory inside a primary checkout", in: nested, out: repo, ok: true},
		{name: "linked worktree root", in: worktree, out: repo, ok: true},
		{name: "directory inside a linked worktree", in: worktreeSub, out: repo, ok: true},
		{name: "linked worktree with no commondir", in: legacy, out: repo, ok: true},
		{name: "linked worktree with an absolute commondir", in: absolute, out: repo, ok: true},
		{name: "linked worktree with relative pointers", in: relative, out: repo, ok: true},
		{name: "submodule resolves to its own root", in: submodule, out: submodule, ok: true},
		{name: "worktree of a bare repository is its own root", in: bareWorktree, out: bareWorktree, ok: true},
		{name: "superproject is unaffected", in: super, out: super, ok: true},
		{name: "not a repository", in: tmp},
		{name: "path that does not exist", in: filepath.Join(tmp, "worktrees", "repo", "deleted")},
		{name: "empty path", in: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MainRoot(tc.in)
			if ok != tc.ok {
				t.Fatalf("MainRoot(%q) ok = %v, want %v (got %q)", tc.in, ok, tc.ok, got)
			}
			if !tc.ok {
				return
			}
			if got != want(t, tc.out) {
				t.Fatalf("MainRoot(%q) = %q, want %q", tc.in, got, want(t, tc.out))
			}
		})
	}
}

// A file under a deleted worktree's path must not walk up into whatever
// repository happens to contain it — a dotfiles repo in $HOME is enough to
// make that answer wrong, and the registry is what covers dead paths.
func TestMainRootRefusesToWalkUpFromAMissingPath(t *testing.T) {
	tmp := t.TempDir()
	repo := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "home")))
	dead := filepath.Join(repo, "worktrees", "project", "branch")

	if root, ok := MainRoot(dead); ok {
		t.Fatalf("MainRoot(%q) = %q, true — a missing path must not resolve", dead, root)
	}
}

// Git terminates every one of these files with a newline, and a transfer
// through a Windows tool leaves CRLF behind. Each value is trimmed on read, so
// no layout may be sensitive to the line ending or to surrounding padding.
func TestMainRootToleratesCRLFAndPaddedPointers(t *testing.T) {
	tmp := t.TempDir()
	repo := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "repo")))
	worktree := mkdirAll(t, filepath.Join(tmp, "worktree"))
	private := mkdirAll(t, filepath.Join(repo, gitDirName, "worktrees", "crlf"))

	writeFile(t, filepath.Join(worktree, gitDirName), "gitdir: "+private+"  \r\n")
	writeFile(t, filepath.Join(private, "gitdir"), " "+filepath.Join(worktree, gitDirName)+"\r\n")
	writeFile(t, filepath.Join(private, "commondir"), "../..\r\n")

	if root, ok := MainRoot(worktree); !ok || root != want(t, repo) {
		t.Fatalf("MainRoot(%q) = %q, %v; want %q", worktree, root, ok, want(t, repo))
	}
	got, err := RegisteredWorktrees(repo)
	if err != nil || !slices.Equal(got, []string{filepath.Clean(worktree)}) {
		t.Fatalf("RegisteredWorktrees(%q) = %v, %v; want the padded registration read", repo, got, err)
	}
}

// A worktree resolves to a repository only because that repository REGISTERED
// it — git writes the `gitdir` back-pointer, and nobody else does. Without
// that check two files anybody can write name an arbitrary path as a main
// root, and the path becomes an auto-created project row: the commondir path
// needs a `.git` pointer plus a `commondir`, and the pre-2.13 marker fallback
// needs only the `.git` pointer, naming a gitdir that need not even exist.
func TestMainRootRefusesAGitDirTheRepositoryNeverRegistered(t *testing.T) {
	tmp := t.TempDir()
	target := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "victim")))

	// The commondir path: a fabricated private gitdir whose commondir names
	// the victim's `.git`, and no registration back-pointer beside it.
	viaCommonDir := mkdirAll(t, filepath.Join(tmp, "via-commondir"))
	fabricated := mkdirAll(t, filepath.Join(tmp, "fabricated", gitDirName, "worktrees", "x"))
	writeFile(t, filepath.Join(viaCommonDir, gitDirName), "gitdir: "+fabricated+"\n")
	writeFile(t, filepath.Join(fabricated, "commondir"), filepath.Join(target, gitDirName)+"\n")

	// The marker fallback: one `.git` file whose gitdir path merely CONTAINS
	// `/.git/worktrees/`. Nothing under the victim is written at all.
	viaMarker := mkdirAll(t, filepath.Join(tmp, "via-marker"))
	writeFile(t, filepath.Join(viaMarker, gitDirName),
		"gitdir: "+filepath.Join(target, gitDirName, "worktrees", "made-up")+"\n")

	// A back-pointer naming a DIFFERENT worktree is no better than none: it is
	// some other checkout's registration, and this one is still unconfirmed.
	mismatched := mkdirAll(t, filepath.Join(tmp, "mismatched"))
	registration := mkdirAll(t, filepath.Join(target, gitDirName, "worktrees", "elsewhere"))
	writeFile(t, filepath.Join(mismatched, gitDirName), "gitdir: "+registration+"\n")
	writeFile(t, filepath.Join(registration, "gitdir"),
		filepath.Join(tmp, "another-worktree", gitDirName)+"\n")
	writeFile(t, filepath.Join(registration, "commondir"), "../..\n")

	// A COMPLETE fake registration — correct back-pointer and all — whose
	// commondir redirects to the victim. The back-pointer only proves the
	// registration names this worktree; an author who controls the
	// registration directory controls both files, so the commondir must also
	// be the directory that physically holds the gitdir (commonDirHoldsGitDir)
	// or the resolved root is whatever the author chose.
	redirected := mkdirAll(t, filepath.Join(tmp, "redirected"))
	fakeReg := mkdirAll(t, filepath.Join(tmp, "fake-repo", "worktrees", "x"))
	writeFile(t, filepath.Join(redirected, gitDirName), "gitdir: "+fakeReg+"\n")
	writeFile(t, filepath.Join(fakeReg, "gitdir"), filepath.Join(redirected, gitDirName)+"\n")
	writeFile(t, filepath.Join(fakeReg, "commondir"), filepath.Join(target, gitDirName)+"\n")

	for _, spoofed := range []string{viaCommonDir, viaMarker, mismatched, redirected} {
		root, ok := MainRoot(spoofed)
		if !ok || root != want(t, spoofed) {
			t.Errorf("MainRoot(%q) = %q, %v; want the checkout itself, never %q",
				spoofed, root, ok, want(t, target))
		}
	}
}

// A `.git` entry that EXISTS but cannot be interpreted is not "no repository
// here". Walking past it attributes the checkout to whatever repository
// happens to contain it — the same false positive the missing-path refusal
// exists for — so the walk stops, which is also where git itself stops and
// errors rather than searching upward.
func TestMainRootStopsAtAnUninterpretableGitEntry(t *testing.T) {
	tmp := t.TempDir()
	container := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "home")))

	cases := map[string]string{
		"empty":   "",
		"garbage": "not a gitdir pointer\n",
		"blank":   "gitdir:   \n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := mkdirAll(t, filepath.Join(container, name))
			writeFile(t, filepath.Join(dir, gitDirName), body)

			root, ok := MainRoot(dir)
			if !ok || root != want(t, dir) {
				t.Fatalf("MainRoot(%q) = %q, %v; want the checkout itself, never the containing repo %q",
					dir, root, ok, want(t, container))
			}
		})
	}
}

func TestRegisteredWorktreesListsLiveAndDeletedCheckouts(t *testing.T) {
	tmp := t.TempDir()
	repo := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "repo")))

	live := linkedWorktree(t, repo, "live",
		mkdirAll(t, filepath.Join(tmp, "worktrees", "live")), "../..")

	// A registration whose worktree directory is GONE: nothing about it can
	// be discovered from the filesystem side, which is why the registry is
	// the only way to place it.
	deleted := filepath.Join(tmp, "worktrees", "deleted")
	writeFile(t, filepath.Join(repo, gitDirName, "worktrees", "deleted", "gitdir"),
		filepath.Join(deleted, gitDirName)+"\n")

	// A RELATIVE registration (git ≥2.48's `worktree.useRelativePaths`) is
	// relative to the registration directory holding it. Left as written it
	// could never match an absolute session cwd, which would silently drop
	// every deleted worktree of such a repository from the grouping.
	relativeWorktree := filepath.Join(tmp, "worktrees", "relative")
	relativeRegistration := mkdirAll(t, filepath.Join(repo, gitDirName, "worktrees", "relative"))
	writeFile(t, filepath.Join(relativeRegistration, "gitdir"),
		rel(t, relativeRegistration, filepath.Join(relativeWorktree, gitDirName))+"\n")

	// A corrupt registration naming something that is not a `.git` file is
	// skipped rather than guessed at.
	writeFile(t, filepath.Join(repo, gitDirName, "worktrees", "corrupt", "gitdir"),
		filepath.Join(tmp, "worktrees", "corrupt")+"\n")

	got, err := RegisteredWorktrees(repo)
	if err != nil {
		t.Fatalf("RegisteredWorktrees: %v", err)
	}
	slices.Sort(got)
	expected := []string{filepath.Clean(deleted), filepath.Clean(live), filepath.Clean(relativeWorktree)}
	slices.Sort(expected)
	if !slices.Equal(got, expected) {
		t.Fatalf("RegisteredWorktrees = %v, want %v", got, expected)
	}
}

// A repository with no worktrees, a path that is not a checkout at all, and a
// path that does not exist are all "no registrations", never an error: the
// scan calls this once per project row and a project on a stale path must not
// fail a listing.
func TestRegisteredWorktreesIsEmptyWithoutARegistry(t *testing.T) {
	tmp := t.TempDir()
	for _, root := range []string{
		plainRepo(t, mkdirAll(t, filepath.Join(tmp, "repo"))),
		mkdirAll(t, filepath.Join(tmp, "plain")),
		filepath.Join(tmp, "missing"),
		"",
	} {
		got, err := RegisteredWorktrees(root)
		if err != nil || len(got) != 0 {
			t.Fatalf("RegisteredWorktrees(%q) = %v, %v; want no paths and no error", root, got, err)
		}
	}
}

// A project row pointing at a linked worktree still enumerates the
// REPOSITORY's registrations — the registry lives with the common dir, not
// with whichever checkout was asked.
func TestRegisteredWorktreesFollowsAWorktreeToItsRepository(t *testing.T) {
	tmp := t.TempDir()
	repo := plainRepo(t, mkdirAll(t, filepath.Join(tmp, "repo")))
	worktree := linkedWorktree(t, repo, "feature",
		mkdirAll(t, filepath.Join(tmp, "worktrees", "feature")), "../..")

	got, err := RegisteredWorktrees(worktree)
	if err != nil {
		t.Fatalf("RegisteredWorktrees: %v", err)
	}
	if !slices.Equal(got, []string{filepath.Clean(worktree)}) {
		t.Fatalf("RegisteredWorktrees(%q) = %v, want the repository's own registration", worktree, got)
	}
}
