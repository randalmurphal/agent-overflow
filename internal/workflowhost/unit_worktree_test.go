package workflowhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
	"agent-overflow/internal/worktreesetup"
)

// seedWorktreeSetup writes a project's worktree-setup recipe straight onto the
// row the runner reads it from. `App.SetProjectWorktreeSetup` validates the
// same config before writing it; validation is App policy, and what these tests
// need is the recipe in place.
func seedWorktreeSetup(t *testing.T, dataStore *store.Store, projectID string, config worktreesetup.Config) {
	t.Helper()
	if err := dataStore.UpdateProjectWorktreeSetup(projectID, &config); err != nil {
		t.Fatalf("seed worktree setup: %v", err)
	}
}

// A unit setup failure must remove a worktree THIS provisioning call cut —
// otherwise every failed try leaks its checkout until run discard — while an
// ADOPTED worktree (a re-entered try, whose crash may have been mid-run) can
// hold prior turns of work and is never this call's to destroy.
func TestUnitWorktreeSetupFailureRollsBackOnlyFreshCut(t *testing.T) {
	dataStore := newTestStore(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, dataStore, repo)
	seedWorktreeSetup(t, dataStore, projectRow.ID, worktreesetup.Config{
		Run:     [][]string{{"/bin/sh", "-c", "exit 7"}},
		Timeout: "5s",
	})

	host := &fakeHost{configDir: t.TempDir()}
	core := host.GitCore()
	itemBranch := "workflow-unit-rollback-item"
	itemPath := filepath.Join(t.TempDir(), "item")
	if err := core.CreateWorktreeFromBranch(repo, itemPath, "main", itemBranch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.RemoveWorktreeForce(repo, itemPath, true) })

	item := store.WorkItem{
		ID: "unit-rollback", ProjectID: projectRow.ID, Goal: "unit rollback",
		WorkflowID: "wf", WorkflowScope: "shared", State: string(engine.StateRunning),
		Source: "manual", CreatedAt: 1,
	}
	if err := dataStore.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.CreateWorkItemUnits([]store.WorkItemUnit{{
		ItemID: item.ID, PhaseID: "fan", Attempt: 1, UnitID: "u1", UnitIndex: 0,
		Kind: store.WorkItemUnitKindUnit, Status: store.WorkItemUnitPending, UnitAttempt: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	runner := newTestRunner(t, host, dataStore, nil)
	primary := PreparedWorkspace{
		Path: itemPath, Branch: itemBranch, BaseBranch: "main", Project: projectRow,
	}
	ref := UnitWorkspaceRef{
		ProjectID: projectRow.ID, ItemID: item.ID, PhaseID: "fan",
		Attempt: 1, UnitID: "u1", UnitAttempt: 1,
	}
	unitBranch := UnitBranch(itemBranch, ref)

	// Fresh cut: the failed setup's worktree is removed, the branch and the
	// unit's registration survive (run discard enumerates them from rows).
	if _, err := runner.provisionUnitWorktree(context.Background(), ref, primary); err == nil {
		t.Fatal("fresh-cut provisioning should fail with the setup error")
	}
	worktrees, err := core.ListWorktrees(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, worktree := range worktrees {
		if worktree.Branch == unitBranch {
			t.Fatalf("failed fresh-cut setup left worktree %q on branch %q", worktree.Path, unitBranch)
		}
	}
	unit, found, err := dataStore.GetWorkItemUnit(item.ID, "fan", 1, "u1")
	if err != nil || !found {
		t.Fatalf("unit row after fresh-cut failure: found=%v err=%v", found, err)
	}
	if unit.Branch != unitBranch {
		t.Fatalf("unit registration branch = %q, want %q", unit.Branch, unitBranch)
	}

	// Adopted: a checkout already on the unit's branch is a re-entered try.
	// Give it uncommitted prior work; a failed setup must leave both alone.
	adoptedPath := filepath.Join(t.TempDir(), "adopted")
	if err := core.AttachWorktree(repo, adoptedPath, unitBranch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.RemoveWorktreeForce(repo, adoptedPath, true) })
	marker := filepath.Join(adoptedPath, "prior-work.txt")
	if err := os.WriteFile(marker, []byte("turns of work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.provisionUnitWorktree(context.Background(), ref, primary); err == nil {
		t.Fatal("adopted provisioning should fail with the setup error")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failed setup destroyed the adopted worktree's prior work: %v", err)
	}
}

// One item branch is not one fan-out's private namespace: a called run executes
// in its caller's workspace (§9), so every wave of a self-calling campaign fans
// out from the SAME branch, and a re-expanded phase opens an attempt whose unit
// tries restart at 1. Retirement removes a lane's checkout and never its
// branch, so lanes that share a name do not fail cleanly — they either refuse
// the cut or adopt the earlier lane's checkout. Each of these must provision
// its own, whether or not the earlier lane's checkout is still on disk.
func TestUnitWorktreesOfSeparateFanOutsShareOneItemBranch(t *testing.T) {
	dataStore := newTestStore(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, dataStore, repo)

	host := &fakeHost{configDir: t.TempDir()}
	core := host.GitCore()
	const itemBranch = "workflow-campaign-root"
	itemPath := filepath.Join(t.TempDir(), "root")
	if err := core.CreateWorktreeFromBranch(repo, itemPath, "main", itemBranch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.RemoveWorktreeForce(repo, itemPath, true) })

	runner := newTestRunner(t, host, dataStore, nil)
	primary := PreparedWorkspace{
		Path: itemPath, Branch: itemBranch, BaseBranch: "main", Project: projectRow,
	}
	// Every lane below names the same phase, unit, and try. Only the fan-out
	// owner and the phase attempt differ — the two coordinates a wave and a
	// re-expansion move.
	lanes := []struct {
		name string
		ref  UnitWorkspaceRef
	}{
		{"wave 2", UnitWorkspaceRef{
			ProjectID: projectRow.ID, ItemID: "wave-2", PhaseID: "implement",
			Attempt: 1, UnitID: "port-0", UnitAttempt: 1,
		}},
		{"wave 3", UnitWorkspaceRef{
			ProjectID: projectRow.ID, ItemID: "wave-3", PhaseID: "implement",
			Attempt: 1, UnitID: "port-0", UnitAttempt: 1,
		}},
		{"wave 2 re-expanded", UnitWorkspaceRef{
			ProjectID: projectRow.ID, ItemID: "wave-2", PhaseID: "implement",
			Attempt: 2, UnitID: "port-0", UnitAttempt: 1,
		}},
	}
	seeded := map[string]bool{}
	for _, lane := range lanes {
		if !seeded[lane.ref.ItemID] {
			if err := dataStore.CreateWorkItem(store.WorkItem{
				ID: lane.ref.ItemID, ProjectID: projectRow.ID, Goal: "campaign wave",
				WorkflowID: "campaign", WorkflowScope: "shared", State: string(engine.StateRunning),
				Source: "manual", CreatedAt: 1,
			}); err != nil {
				t.Fatal(err)
			}
			seeded[lane.ref.ItemID] = true
		}
		if err := dataStore.CreateWorkItemUnits([]store.WorkItemUnit{{
			ItemID: lane.ref.ItemID, PhaseID: lane.ref.PhaseID, Attempt: lane.ref.Attempt,
			UnitID: lane.ref.UnitID, UnitIndex: 0, Kind: store.WorkItemUnitKindUnit,
			Status: store.WorkItemUnitPending, UnitAttempt: lane.ref.UnitAttempt,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	provisioned := map[string]string{}
	for i, lane := range lanes {
		sub, err := runner.provisionUnitWorktree(context.Background(), lane.ref, primary)
		if err != nil {
			t.Fatalf("%s failed to provision its lane: %v", lane.name, err)
		}
		if want := UnitBranch(itemBranch, lane.ref); sub.Branch != want {
			t.Fatalf("%s branch = %q, want %q", lane.name, sub.Branch, want)
		}
		if other, clash := provisioned[sub.Branch]; clash {
			t.Fatalf("%s reused branch %q, already checked out at %q", lane.name, sub.Branch, other)
		}
		provisioned[sub.Branch] = sub.Path
		unit, found, err := dataStore.GetWorkItemUnit(
			lane.ref.ItemID, lane.ref.PhaseID, lane.ref.Attempt, lane.ref.UnitID,
		)
		if err != nil || !found {
			t.Fatalf("%s unit row: found=%v err=%v", lane.name, found, err)
		}
		if unit.Branch != sub.Branch || unit.WorktreePath != sub.Path {
			t.Fatalf("%s registered %q/%q, want %q/%q", lane.name, unit.Branch, unit.WorktreePath, sub.Branch, sub.Path)
		}
		// Retirement takes the earlier lane's checkout and leaves its branch, so
		// the next wave meets a branch that exists with nothing checked out on
		// it — which is exactly what refused the cut in the live incident.
		if i == 0 {
			if err := core.RemoveWorktreeForce(repo, sub.Path, true); err != nil {
				t.Fatal(err)
			}
			continue
		}
		path := sub.Path
		t.Cleanup(func() { _ = core.RemoveWorktreeForce(repo, path, true) })
	}
	if len(provisioned) != len(lanes) {
		t.Fatalf("provisioned lanes = %+v, want one per fan-out", provisioned)
	}

	// Re-entering a try is the case adoption exists for: the same coordinates
	// must land back in the checkout that try already owns.
	last := lanes[len(lanes)-1]
	again, err := runner.provisionUnitWorktree(context.Background(), last.ref, primary)
	if err != nil {
		t.Fatal(err)
	}
	if again.Path != provisioned[again.Branch] {
		t.Fatalf("re-entered try moved to %q, want its own checkout %q", again.Path, provisioned[again.Branch])
	}
}

// A queued sibling cancelled while waiting for the item's workspace lock has
// done no work, so its abandonment must read as a setup failure that still
// carries the cancellation cause — losing `context.Canceled` from the chain
// would make the engine treat an operator's cancel as a provisioning defect,
// and losing `ErrSetupFailed` would park it as an agent error. Both sentinels
// and the item's name ride one error.
func TestWorkflowWorkspaceLockCancelKeepsBothSentinels(t *testing.T) {
	runner := newTestRunner(t, nil, newTestStore(t), staticWorkflowProfileSource{})
	request := engine.RunRequest{Key: engine.RunKey{ItemID: "item-wedge", PhaseID: "work", Attempt: 1}}

	requireBothSentinels := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, engine.ErrSetupFailed) {
			t.Fatalf("error = %v, want engine.ErrSetupFailed in chain", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled in chain", err)
		}
		if !strings.Contains(err.Error(), "item-wedge") {
			t.Fatalf("error = %v, want the item named", err)
		}
	}

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := runner.prepareWorkspace(ctx, request)
		requireBothSentinels(t, err)
	})

	t.Run("cancelled while queued behind the holder", func(t *testing.T) {
		unlock := runner.workspaceLocks.Lock(request.Key.ItemID)
		defer unlock()
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			_, err := runner.prepareWorkspace(ctx, request)
			errCh <- err
		}()
		// The waiter must be parked on the lock, not failed fast: give it no way
		// to have returned before the cancel is what releases it.
		select {
		case err := <-errCh:
			t.Fatalf("prepareWorkspace returned %v before cancel while the lock was held", err)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		requireBothSentinels(t, <-errCh)
	})
}
