package app

import (
	"os"
	"testing"

	"agent-overflow/internal/testutil"
)

// A failed plan seed rolls back the worktree its child was given by the path
// the cut answered with. Another client moving the child back to the source
// thread's worktree first must not turn the rollback against that worktree.
func TestCutRollbackByPathSparesTheWorktreeTheChildMovedTo(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)
	testutil.RunGit(t, repo, "branch", "src")
	testutil.RunGit(t, repo, "branch", "child")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	source := testThread("rollback-source")
	source.ProjectID, source.WorkspacePath = project.ID, repo
	if err := app.store.CreateThread(source); err != nil {
		t.Fatal(err)
	}
	onSource, err := app.AttachThreadWorktree(source.ID, "src")
	if err != nil {
		t.Fatalf("attach source: %v", err)
	}
	sourceWorktree := onSource.WorktreePath

	child := testThread("rollback-child")
	child.ProjectID, child.WorkspacePath, child.WorktreePath, child.Branch = project.ID, sourceWorktree, sourceWorktree, "src"
	if err := app.store.CreateThread(child); err != nil {
		t.Fatal(err)
	}
	cut, err := app.AttachThreadWorktree(child.ID, "child")
	if err != nil {
		t.Fatalf("attach child: %v", err)
	}
	if _, err := app.UpdateThreadWorkspace(child.ID, sourceWorktree); err != nil {
		t.Fatalf("move child back: %v", err)
	}

	if _, err := app.RemoveOtherWorktree(WorkspaceRef{ProjectID: project.ID}, cut.WorktreePath, false); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if _, err := os.Stat(cut.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("the cut worktree %s survived the rollback: %v", cut.WorktreePath, err)
	}
	if _, err := os.Stat(sourceWorktree); err != nil {
		t.Fatalf("the source worktree %s did not survive: %v", sourceWorktree, err)
	}
	stored, err := app.store.GetThread(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(stored.WorktreePath, sourceWorktree) {
		t.Fatalf("source thread moved to %q, want %q", stored.WorktreePath, sourceWorktree)
	}
}

// Deleting an archived thread whose project is archived too removes its
// worktree through the project root, then the thread.
func TestRemoveWorktreeOfArchivedThreadInArchivedProject(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	repo := initMainGitRepo(t)
	testutil.RunGit(t, repo, "branch", "feature")
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	thread := testThread("archived-worktree")
	thread.ProjectID, thread.WorkspacePath = project.ID, repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	attached, err := app.AttachThreadWorktree(thread.ID, "feature")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := app.ArchiveThread(thread.ID); err != nil {
		t.Fatalf("archive thread: %v", err)
	}
	if err := app.ArchiveProject(project.ID); err != nil {
		t.Fatalf("archive project: %v", err)
	}

	if _, err := app.RemoveOtherWorktree(WorkspaceRef{ProjectID: project.ID}, attached.WorktreePath, false); err != nil {
		t.Fatalf("remove worktree of archived project: %v", err)
	}
	if _, err := os.Stat(attached.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree %s survived: %v", attached.WorktreePath, err)
	}
	if err := app.DeleteThread(thread.ID); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
}
