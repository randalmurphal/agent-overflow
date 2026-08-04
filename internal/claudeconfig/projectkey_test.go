package claudeconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// writeWorktreeLayout hand-crafts the exact on-disk layout `git
// worktree add` produces (no git binary needed): a main checkout with
// .git/worktrees/<name>/{commondir,gitdir} and a linked worktree whose
// .git is a pointer file. Returns (mainRoot, worktreeRoot).
func writeWorktreeLayout(t *testing.T, name string) (string, string) {
	t.Helper()
	base := t.TempDir()
	mainRoot := filepath.Join(base, "main")
	worktree := filepath.Join(base, name)
	adminDir := filepath.Join(mainRoot, ".git", "worktrees", name)
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatalf("mkdir admin dir: %v", err)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+adminDir+"\n")
	writeFile(t, filepath.Join(adminDir, "commondir"), "../..\n")
	writeFile(t, filepath.Join(adminDir, "gitdir"), filepath.Join(worktree, ".git")+"\n")
	return mainRoot, worktree
}

func TestProjectKey_regularCheckout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	sub := filepath.Join(root, "cmd", "tool")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if got := ProjectKey(root); got != root {
		t.Errorf("ProjectKey(root) = %q, want %q", got, root)
	}
	// A cwd below the root resolves to the root, like the CLI.
	if got := ProjectKey(sub); got != root {
		t.Errorf("ProjectKey(subdir) = %q, want %q", got, root)
	}
}

func TestProjectKey_linkedWorktreeResolvesToMainRoot(t *testing.T) {
	mainRoot, worktree := writeWorktreeLayout(t, "feature-x")
	if got := ProjectKey(worktree); got != mainRoot {
		t.Errorf("ProjectKey(worktree) = %q, want main root %q", got, mainRoot)
	}
}

func TestProjectKey_nonGitDirFallsBackToItself(t *testing.T) {
	dir := t.TempDir()
	if got := ProjectKey(dir); got != dir {
		t.Errorf("ProjectKey = %q, want %q", got, dir)
	}
	if got := ProjectKey(""); got != "" {
		t.Errorf("ProjectKey(\"\") = %q, want empty", got)
	}
}

// TestProjectKey_invalidWorktreeStructureFallsBack pins the two
// validation checks: a commondir pointing outside the expected
// worktrees layout, or a gitdir back-link naming a different checkout,
// must not be trusted — the worktree's own root is used instead.
func TestProjectKey_invalidWorktreeStructureFallsBack(t *testing.T) {
	t.Run("commondir escapes layout", func(t *testing.T) {
		mainRoot, worktree := writeWorktreeLayout(t, "wt")
		adminDir := filepath.Join(mainRoot, ".git", "worktrees", "wt")
		// Point commondir at an unrelated directory.
		elsewhere := t.TempDir()
		if err := os.MkdirAll(filepath.Join(elsewhere, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFile(t, filepath.Join(adminDir, "commondir"), filepath.Join(elsewhere, ".git")+"\n")
		if got := ProjectKey(worktree); got != worktree {
			t.Errorf("ProjectKey = %q, want fallback to worktree %q", got, worktree)
		}
	})
	t.Run("gitdir back-link mismatch", func(t *testing.T) {
		mainRoot, worktree := writeWorktreeLayout(t, "wt")
		adminDir := filepath.Join(mainRoot, ".git", "worktrees", "wt")
		other := filepath.Join(t.TempDir(), "other")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFile(t, filepath.Join(other, ".git"), "gitdir: nowhere\n")
		writeFile(t, filepath.Join(adminDir, "gitdir"), filepath.Join(other, ".git")+"\n")
		if got := ProjectKey(worktree); got != worktree {
			t.Errorf("ProjectKey = %q, want fallback to worktree %q", got, worktree)
		}
	})
}

// TestProjectKey_bareRepoWorktreeUsesCommonDir pins the bare-repo
// case: when the shared dir is not a working directory's .git, the
// common dir itself is the stable identity.
func TestProjectKey_bareRepoWorktreeUsesCommonDir(t *testing.T) {
	base := t.TempDir()
	bare := filepath.Join(base, "repo.git")
	worktree := filepath.Join(base, "wt")
	adminDir := filepath.Join(bare, "worktrees", "wt")
	if err := os.MkdirAll(adminDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(worktree, ".git"), "gitdir: "+adminDir+"\n")
	writeFile(t, filepath.Join(adminDir, "commondir"), "../..\n")
	writeFile(t, filepath.Join(adminDir, "gitdir"), filepath.Join(worktree, ".git")+"\n")
	if got := ProjectKey(worktree); got != bare {
		t.Errorf("ProjectKey = %q, want bare common dir %q", got, bare)
	}
}
