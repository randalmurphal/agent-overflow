package app

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/testutil"
)

func TestWriteWorkspaceFile(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := testutil.InitGitRepo(t)
	ref := testWorkspaceRef(t, app, workspace)

	writtenPath, err := app.WriteWorkspaceFile(ref, "plans/ship-it.md", "# Ship it\n")
	if err != nil {
		t.Fatalf("WriteWorkspaceFile() error = %v", err)
	}
	if writtenPath != filepath.Join("plans", "ship-it.md") {
		t.Fatalf("writtenPath = %q, want %q", writtenPath, filepath.Join("plans", "ship-it.md"))
	}

	data, err := os.ReadFile(filepath.Join(workspace, writtenPath))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "# Ship it\n" {
		t.Fatalf("file contents = %q, want %q", string(data), "# Ship it\n")
	}
	// File mode is 0o600 — workspace writes can carry user content
	// the user wouldn't want world-readable on a shared host. Pin
	// the mode so a future change can't loosen it without surfacing.
	info, err := os.Stat(filepath.Join(workspace, writtenPath))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 0o600 (workspace writes are user-private)", got)
	}
}

// A registered worktree is the second thing a ref may name, and the resolve
// path that accepts it must not need a git binary — this write happens from
// the plan-save dialog, not a git action.
func TestWriteWorkspaceFileAcceptsRegisteredWorktree(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}
	workspace := filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature/plans", workspace)
	t.Cleanup(func() { _ = app.gitCore().RemoveWorktreeForce(repo, workspace, true) })
	ref := WorkspaceRef{ProjectID: project.ID, WorkspacePath: workspace}

	t.Setenv("PATH", "")
	writtenPath, err := app.WriteWorkspaceFile(ref, "plan.md", "# Plan\n")
	if err != nil {
		t.Fatalf("WriteWorkspaceFile() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, writtenPath)); err != nil {
		t.Fatalf("Stat(%s) error = %v", writtenPath, err)
	}
}

// The ref is the trust boundary: a directory that is not a checkout of the
// named project can never be written to, however it is spelled.
func TestWriteWorkspaceFileRefusesNonCheckout(t *testing.T) {
	app := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace() error = %v", err)
	}

	outside := t.TempDir()
	cases := map[string]WorkspaceRef{
		"plain directory":      {ProjectID: project.ID, WorkspacePath: outside},
		"subdirectory of root": {ProjectID: project.ID, WorkspacePath: filepath.Join(repo, "src")},
		"unknown project":      {ProjectID: "no-such-project", WorkspacePath: repo},
		"zero ref":             {},
	}
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := app.WriteWorkspaceFile(ref, "plan.md", "nope"); err == nil {
				t.Fatal("expected refusal for a non-checkout workspace ref")
			}
			if _, err := os.Stat(filepath.Join(outside, "plan.md")); !os.IsNotExist(err) {
				t.Fatalf("file written outside the checkout: err = %v", err)
			}
		})
	}
}

func TestWriteWorkspaceFileRejectsParentEscape(t *testing.T) {
	app := newTestAppWithStore(t)
	workspace := testutil.InitGitRepo(t)
	ref := testWorkspaceRef(t, app, workspace)

	if _, err := app.WriteWorkspaceFile(ref, "../outside.md", "nope"); err == nil {
		t.Fatal("expected parent escape path to fail")
	}
}
