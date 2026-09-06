package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"github.com/google/uuid"
)

func TestTransferredWorkspaceCleanupWaitsForConfirmationAndPreservesRetirement(t *testing.T) {
	a := newTestAppWithStore(t)
	repo := testutil.InitGitRepo(t)
	project, err := a.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "old-worktree")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "old-branch", workspace)
	retired := testThread(uuid.NewString())
	retired.ProjectID, retired.WorkspacePath, retired.WorktreePath, retired.Branch = project.ID, workspace, workspace, "old-branch"
	retired.Provider, retired.SessionRef = "claude", uuid.NewString()
	if err := a.store.CreateThread(retired); err != nil {
		t.Fatal(err)
	}
	sibling := retired
	sibling.ID, sibling.SessionRef = uuid.NewString(), ""
	if err := a.store.CreateThread(sibling); err != nil {
		t.Fatal(err)
	}
	row, err := a.store.CreateThreadTransfer(store.ThreadTransfer{ID: uuid.NewString(), ThreadID: retired.ID, PeerBackendID: uuid.NewString(), Kind: "move", Direction: "outgoing", ActivationHash: strings.Repeat("a", 64), PrivateState: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.BindThreadTransferSessions(row.ID, []store.TransferSession{{Provider: "claude", Ref: retired.SessionRef}}); err != nil {
		t.Fatal(err)
	}
	ref := WorkspaceRef{ProjectID: project.ID, WorkspacePath: repo}
	for _, phase := range []string{"preparing", "prepared", "committed", "complete"} {
		if phase != "preparing" {
			if _, err := a.store.AdvanceThreadTransfer(row.ID, phase, strings.Repeat("b", 64)); err != nil {
				t.Fatal(err)
			}
		}
		_, err := a.RemoveOtherWorktree(ref, workspace, true)
		if phase != "complete" {
			var pending *store.ThreadTransferError
			if !errors.As(err, &pending) {
				t.Fatalf("%s worktree cleanup escaped reservation: %v", phase, err)
			}
			if _, err := os.Stat(workspace); err != nil {
				t.Fatal("pending checkout removed", err)
			}
			if _, err := a.DeleteProject(project.ID); !errors.As(err, &pending) {
				t.Fatalf("%s project cleanup escaped reservation: %v", phase, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("confirmed old worktree retained", err)
	}
	old, err := a.store.GetThread(retired.ID)
	if err != nil || old.WorkspacePath != workspace || old.SessionRef != retired.SessionRef {
		t.Fatal("retired native cache was reattached", old, err)
	}
	current, err := a.store.GetThread(sibling.ID)
	if err != nil || !samePath(current.WorkspacePath, repo) || current.WorktreePath != "" {
		t.Fatal("local sibling was not reattached", current, err)
	}
	var moved *store.ThreadTransferError
	if _, err := a.GetThread(retired.ID); !errors.As(err, &moved) || !moved.Moved {
		t.Fatal("public metadata read revived a retired conversation", err)
	}
	if err := a.DeleteThread(retired.ID); err == nil {
		t.Fatal("stale thread action silently deleted a retired cache")
	}
	result, err := a.DeleteProject(project.ID)
	if err != nil {
		t.Fatal("confirmed move blocked project cleanup", err)
	}
	if !slices.Equal(result.ThreadIDs, []string{sibling.ID}) {
		t.Fatal("project cleanup would close a remote conversation", result.ThreadIDs)
	}
	if _, err := a.store.GetThread(retired.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("retained obsolete history cache", err)
	}
	if err := a.store.CheckThreadExecutionAccess(retired); err == nil {
		t.Fatal("cache cleanup revived moved conversation")
	}
	if err := a.store.CheckNativeThreadTransferAccess("claude", retired.SessionRef); err == nil {
		t.Fatal("project deletion erased native retirement")
	}
}
