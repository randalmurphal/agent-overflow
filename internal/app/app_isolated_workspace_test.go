package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// An isolated boot pins every spawn to the mock provider, but the mock still
// runs in the thread's workspace. A project outside the harness data root is
// therefore a real repository a scenario's writeFile step could edit.
func TestCreateProjectRefusesAWorkspaceOutsideTheIsolatedRoot(t *testing.T) {
	app := newTestAppWithStore(t)
	root := t.TempDir()
	ConfigureIsolation(app, IsolationConfig{WorkspaceRoot: root})

	outside := t.TempDir()
	_, err := app.CreateProject(outside)
	if err == nil {
		t.Fatal("CreateProject() accepted a workspace outside the harness data root")
	}
	if !strings.Contains(err.Error(), "outside the harness data root") ||
		!strings.Contains(err.Error(), outside) ||
		!strings.Contains(err.Error(), root) {
		t.Fatalf("CreateProject() error = %v, want the harness-root refusal naming both paths", err)
	}
	if !strings.Contains(err.Error(), "ao-harness clone") {
		t.Fatalf("CreateProject() error = %v, want the operator instructions", err)
	}

	// A seeded fixture workspace under <dataRoot>/workspaces is the shape the
	// harness generates, and it must still be accepted.
	inside := filepath.Join(root, "workspaces", "fixture-repo")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := app.CreateProject(inside); err != nil {
		t.Fatalf("CreateProject(%s) error = %v, want nil", inside, err)
	}

	// App-created worktrees live under <dataRoot>/agent-overflow/worktrees.
	worktree := filepath.Join(root, "agent-overflow", "worktrees", "fixture-repo", "feature")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := app.CreateProject(worktree); err != nil {
		t.Fatalf("CreateProject(%s) error = %v, want nil", worktree, err)
	}
}

// macOS reaches /tmp through a symlink to /private/tmp, so the configured
// root and the real paths under it are spelled differently. Both sides are
// resolved before the prefix comparison.
func TestIsolatedWorkspaceRootIsComparedThroughSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	linkedRoot := newTestAppWithStore(t)
	ConfigureIsolation(linkedRoot, IsolationConfig{WorkspaceRoot: link})
	inside := filepath.Join(real, "workspaces", "repo")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := linkedRoot.CreateProject(inside); err != nil {
		t.Fatalf("CreateProject(%s) with a symlinked root error = %v, want nil", inside, err)
	}

	// The same equivalence in the other direction: a real root reached
	// through a linked path.
	realRoot := newTestAppWithStore(t)
	ConfigureIsolation(realRoot, IsolationConfig{WorkspaceRoot: real})
	if _, err := realRoot.CreateProject(filepath.Join(link, "workspaces", "repo")); err != nil {
		t.Fatalf("CreateProject through a symlinked path error = %v, want nil", err)
	}
	if _, err := realRoot.CreateProject(t.TempDir()); err == nil {
		t.Fatal("a real root still accepted a workspace outside it")
	}
}

// The start path refuses before it tears anything down, so a thread that
// cannot legally run keeps the session it already had.
func TestStartSessionRefusesAWorkspaceOutsideTheIsolatedRoot(t *testing.T) {
	app := newTestAppWithStore(t)
	t.Cleanup(func() { _ = app.ServiceShutdown() })
	root := t.TempDir()
	ConfigureIsolation(app, IsolationConfig{WorkspaceRoot: root})

	thread := testThread("thread-outside-harness-root")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	// A prior session on the same thread: a refused start must not stop it.
	app.sessionManager().put(thread.ID, session{
		Provider: string(provider.Claude),
		Token:    "keep-me",
	})

	err := app.startSessionNow(thread.ID)
	if err == nil {
		t.Fatal("startSessionNow() spawned a session in a workspace outside the harness data root")
	}
	if !strings.Contains(err.Error(), "start session:") || !strings.Contains(err.Error(), "outside the harness data root") {
		t.Fatalf("startSessionNow() error = %v, want the harness-root refusal on the start-session path", err)
	}
	existing, ok := app.sessionManager().get(thread.ID)
	if !ok || existing.Token != "keep-me" {
		t.Fatalf("refused start disturbed the existing session: %+v (registered=%v)", existing, ok)
	}
}

// Unit tests build an App with no isolation config at all, and every path on
// the developer's machine must stay reachable there.
func TestRequireIsolatedWorkspaceIsInertWithoutARoot(t *testing.T) {
	app := newTestAppWithStore(t)
	if err := app.requireIsolatedWorkspace("/definitely/not/under/any/root"); err != nil {
		t.Fatalf("requireIsolatedWorkspace() with no root error = %v, want nil", err)
	}
	if _, err := app.CreateProject(t.TempDir()); err != nil {
		t.Fatalf("CreateProject() with no root error = %v, want nil", err)
	}
}

// The root itself is a legal workspace, and a sibling whose name merely
// starts with the root's spelling is not.
func TestIsolatedWorkspaceBoundaryIsPathComponentWise(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "data")
	sibling := root + "-other"
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	app := newTestAppWithStore(t)
	ConfigureIsolation(app, IsolationConfig{WorkspaceRoot: root})

	if err := app.requireIsolatedWorkspace(root); err != nil {
		t.Fatalf("requireIsolatedWorkspace(root) error = %v, want nil", err)
	}
	if err := app.requireIsolatedWorkspace(sibling); err == nil {
		t.Fatalf("requireIsolatedWorkspace(%s) accepted a sibling of the root", sibling)
	}
}
