package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transferfiles"
	"github.com/google/uuid"
)

func prepareTestTransferWorktree(t *testing.T, branch string) (*Core, TransferWorktree) {
	t.Helper()
	ctx, core := context.Background(), NewCore()
	source := testutil.InitGitRepo(t)
	writeWorkspaceFile(t, source, "staged", "staged only bytes")
	testutil.RunGit(t, source, "add", "staged")
	capture, err := core.CaptureTransferWorkspace(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	var pack bytes.Buffer
	if err := core.WriteTransferObjects(ctx, source, capture.Workspace.Head, capture.Workspace.Index, nil, &pack); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snapshot.tar")
	digest, err := transferfiles.Create(ctx, archive, capture.Sources)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	stage := filepath.Join(t.TempDir(), "archive")
	if _, err := transferfiles.Extract(ctx, input, digest, stage); err != nil {
		t.Fatal(err)
	}
	request := TransferWorktreeRequest{OperationID: uuid.NewString(), Repository: testutil.InitGitRepo(t), Path: filepath.Join(t.TempDir(), "published"), Branch: branch, Workspace: capture.Workspace, ArchiveRoot: stage}
	plan, err := core.PrepareTransferWorktree(ctx, request, &pack)
	if err != nil {
		t.Fatal(err)
	}
	return core, plan
}

func TestTransferWorktreeRetainsObjectsAndRecoversRenameBeforeRepair(t *testing.T) {
	core, plan := prepareTestTransferWorktree(t, "copied-work")
	worktrees, err := core.ListWorktrees(plan.Repository)
	if err != nil || len(worktrees) != 1 {
		t.Fatalf("preparation leaked into workspace selector: %+v %v", worktrees, err)
	}
	staged, _, err := core.Execute(plan.Stage, "rev-parse", ":staged")
	if err != nil {
		t.Fatal(err)
	}
	staged = strings.TrimSpace(staged)
	testutil.RunGit(t, plan.Repository, "gc", "--prune=now", "--quiet")
	if got, _, err := core.Execute(plan.Repository, "cat-file", "blob", staged); err != nil || got != "staged only bytes" {
		t.Fatalf("GC lost prepared index content: %q", got)
	}
	// Crash point: the directory is published but Git still names its old
	// location. Recreate Core too, so no cached cwd/path data can save the retry.
	if err := atomicfile.RenameNoReplace(plan.Stage, plan.Path); err != nil {
		t.Fatal(err)
	}
	core = NewCore()
	if err := core.PublishTransferWorktree(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if err := core.PublishTransferWorktree(context.Background(), plan); err != nil {
		t.Fatalf("second acknowledgment retry: %v", err)
	}
	if got, _, err := core.Execute(plan.Path, "symbolic-ref", "--short", "HEAD"); err != nil || strings.TrimSpace(got) != "copied-work" {
		t.Fatalf("lost branch: %s", got)
	}
	if _, err := os.Stat(filepath.Join(plan.GitDir, "locked")); !os.IsNotExist(err) {
		t.Fatalf("left completed worktree locked: %v", err)
	}
	worktrees, err = core.ListWorktrees(plan.Repository)
	if err != nil || len(worktrees) != 2 {
		t.Fatalf("completed workspace unavailable: %+v %v", worktrees, err)
	}
	if got, err := os.ReadFile(filepath.Join(plan.Path, "staged")); err != nil || string(got) != "staged only bytes" {
		t.Fatalf("lost staged bytes: %q %v", got, err)
	}
}

func TestTransferPreparationCleanupRecoversAfterWorktreeRemoval(t *testing.T) {
	core, plan := prepareTestTransferWorktree(t, "cancel-copy")
	marker := plan.Stage + ".cleanup.json"
	if err := atomicfile.WriteJSON(marker, plan); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash between removing the checkout and releasing its
	// reserved branch. The remaining private marker proves the exact owner.
	testutil.RunGit(t, plan.Repository, "worktree", "remove", "--force", "--force", plan.Stage)
	core = NewCore()
	for range 2 {
		if err := core.DiscardTransferPreparation(context.Background(), plan.OperationID, plan.Repository, plan.Path, plan.Head, plan.Branch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cleanup marker remains: %v", err)
	}
	result, err := core.run(plan.Repository, "show-ref", "--verify", "--quiet", "refs/heads/"+plan.Branch)
	if err != nil || result.exitCode != 1 {
		t.Fatalf("reserved branch remains: %+v %v", result, err)
	}
}

func TestTransferPreparationCleanupPreservesPublishedWorkspace(t *testing.T) {
	core, plan := prepareTestTransferWorktree(t, "published-copy")
	if err := core.PublishTransferWorktree(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if err := core.DiscardTransferPreparation(context.Background(), plan.OperationID, plan.Repository, plan.Path, plan.Head, plan.Branch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plan.Path, "staged")); err != nil {
		t.Fatal("cleanup removed published work", err)
	}
	if _, _, err := core.Execute(plan.Path, "rev-parse", "refs/heads/"+plan.Branch); err != nil {
		t.Fatal("cleanup removed published branch", err)
	}
}

func TestTransferWorktreePublicationRefusesAnotherDirectory(t *testing.T) {
	core, plan := prepareTestTransferWorktree(t, "")
	if err := os.Mkdir(plan.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := core.PublishTransferWorktree(context.Background(), plan); err == nil {
		t.Fatal("replaced another directory")
	}
	if _, err := os.Stat(plan.Stage); err != nil {
		t.Fatal("lost preparation on conflict")
	}
	if err := os.Remove(plan.Path); err != nil {
		t.Fatal(err)
	}
	if err := core.PublishTransferWorktree(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	other := plan
	other.OperationID = uuid.NewString()
	other.Stage = filepath.Join(filepath.Dir(other.Path), transferWorktreePrefix+other.OperationID)
	if err := core.PublishTransferWorktree(context.Background(), other); err == nil {
		t.Fatal("accepted another operation's published worktree")
	}
}

func TestTransferWorktreePublicationChecksPreparedContent(t *testing.T) {
	for _, change := range []string{"working bytes", "new file", "deleted file", "index content", "index flags", "branch", "missing fingerprint"} {
		t.Run(change, func(t *testing.T) {
			core, plan := prepareTestTransferWorktree(t, "")
			switch change {
			case "working bytes":
				writeWorkspaceFile(t, plan.Stage, "staged", "changed after preparation")
			case "new file":
				writeWorkspaceFile(t, plan.Stage, "unexpected", "added after preparation")
			case "deleted file":
				if err := os.Remove(filepath.Join(plan.Stage, "staged")); err != nil {
					t.Fatal(err)
				}
			case "index content":
				writeWorkspaceFile(t, plan.Stage, "staged", "different staged bytes")
				testutil.RunGit(t, plan.Stage, "add", "staged")
				writeWorkspaceFile(t, plan.Stage, "staged", "staged only bytes")
			case "index flags":
				testutil.RunGit(t, plan.Stage, "update-index", "--assume-unchanged", "staged")
			case "branch":
				testutil.RunGit(t, plan.Stage, "update-ref", "refs/heads/unrelated", plan.Head)
				testutil.RunGit(t, plan.Stage, "symbolic-ref", "HEAD", "refs/heads/unrelated")
			case "missing fingerprint":
				plan.Fingerprint = ""
			}
			if err := core.PublishTransferWorktree(context.Background(), plan); err == nil {
				t.Fatal("published changed preparation")
			}
			if _, err := os.Stat(plan.Stage); err != nil {
				t.Fatal("lost preparation", err)
			}
			if _, err := os.Stat(plan.Path); !os.IsNotExist(err) {
				t.Fatal("published before verification", err)
			}
		})
	}
}

func TestTransferWorktreePublicationAllowsIndexStatRefresh(t *testing.T) {
	core, plan := prepareTestTransferWorktree(t, "")
	// This updates expendable index stat data; the staged and working bytes
	// remain the prepared snapshot. Publication must not compare opaque indexes.
	testutil.RunGit(t, plan.Stage, "update-index", "--refresh")
	if err := core.PublishTransferWorktree(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, plan.Path, "staged", "changed after rename")
	if err := core.PublishTransferWorktree(context.Background(), plan); err == nil {
		t.Fatal("retry accepted changed published workspace")
	}
}
