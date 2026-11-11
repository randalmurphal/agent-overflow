package workflowapp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
)

// prGit is the disposition's git and forge, with the PR steps scripted.
type prGit struct {
	worktree, branch string
	createPR         func(ctx context.Context) (string, error)
	listOpenPRs      func(ctx context.Context) ([]gitops.GitPR, error)

	mu     sync.Mutex
	pushes int
}

func (g *prGit) ListWorktrees(string) ([]gitops.Worktree, error) {
	return []gitops.Worktree{{Path: g.worktree, Branch: g.branch}}, nil
}
func (g *prGit) WorkingTreeChanges(string, int) ([]string, int, error) { return nil, 0, nil }
func (g *prGit) CountWorkingTreeChanges(string) (int, error)           { return 0, nil }
func (g *prGit) RemoveWorktreeForce(string, string, bool) error {
	return errors.New("unexpected worktree removal")
}
func (g *prGit) DeleteBranch(string, string, bool) error {
	return errors.New("unexpected branch delete")
}
func (g *prGit) MergeBranch(string, string, string) (gitops.MergeResult, error) {
	return gitops.MergeResult{}, errors.New("unexpected merge")
}
func (g *prGit) HeadSHA(string) (string, error) { return "sha1", nil }
func (g *prGit) PushUnattended(string) error {
	g.mu.Lock()
	g.pushes++
	g.mu.Unlock()
	return nil
}
func (g *prGit) CreatePR(ctx context.Context, _, _, _, _ string, _ bool) (string, error) {
	return g.createPR(ctx)
}
func (g *prGit) ListOpenPRs(ctx context.Context, _, _ string) ([]gitops.GitPR, error) {
	if g.listOpenPRs == nil {
		return nil, errors.New("unexpected open PR lookup")
	}
	return g.listOpenPRs(ctx)
}
func (g *prGit) ReadPR(context.Context, gitops.PRReference, gitops.PRReadParts, *gitops.CIPipeline, []string) (gitops.PRRead, error) {
	return gitops.PRRead{}, errors.New("unexpected PR read")
}

func newPRDispositionFixture(t *testing.T, git *prGit) (*Service, *store.Store, store.WorkItem) {
	t.Helper()
	database := storetest.Clone(t)
	project := testutil.EnsureProject(t, database, t.TempDir())
	git.worktree, git.branch = t.TempDir(), "workflow/run"
	item := store.WorkItem{
		ID: "pr-run", ProjectID: project.ID, WorkflowID: "campaign", WorkflowScope: "shared",
		Goal: "ship it", State: string(engine.StateDone), Source: "manual", CreatedAt: 1, StartedAt: 2, EndedAt: 3,
		WorktreePath: git.worktree, Branch: git.branch, BaseBranch: "main",
	}
	if err := database.CreateWorkItem(item); err != nil {
		t.Fatal(err)
	}
	service := New(Deps{
		Store: database,
		Git:   func() Git { return git },
		ProjectProfile: func(string) (DispositionProfile, error) {
			return DispositionProfile{BaseBranch: "main"}, nil
		},
		ParkDisposition: func(string) error { return nil },
		Now:             func() time.Time { return time.UnixMilli(10_000) },
	})
	return service, database, item
}

func storedPRReceipt(t *testing.T, database *store.Store, itemID string) (DispositionReceipt, bool) {
	t.Helper()
	stored, err := database.GetWorkItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Disposition) == 0 {
		return DispositionReceipt{}, false
	}
	var receipt DispositionReceipt
	if err := json.Unmarshal(stored.Disposition, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt, true
}

// A PR disposition the caller cancels ends in one of two states: nothing
// pushed and no receipt, or the PR opened and its receipt written. The
// caller's context is cancelled before the call, while the PR is being
// created, and while an earlier attempt's PR is being looked up.
func TestPRDispositionCancelledByItsCallerNeverStopsBetweenPushAndReceipt(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/acme/repo/pull/9"
	for _, point := range []string{"before", "create", "lookup"} {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			git := &prGit{}
			git.createPR = func(prCtx context.Context) (string, error) {
				switch point {
				case "create":
					cancel()
				case "lookup":
					return "", errors.New("a pull request for branch workflow/run already exists")
				}
				return url, prCtx.Err()
			}
			git.listOpenPRs = func(lookupCtx context.Context) ([]gitops.GitPR, error) {
				if point == "lookup" {
					cancel()
				}
				return []gitops.GitPR{{URL: url}}, lookupCtx.Err()
			}
			service, database, item := newPRDispositionFixture(t, git)
			if point == "before" {
				cancel()
			}
			receipt, err := service.CreateItemPR(ctx, item.ID)
			stored, found := storedPRReceipt(t, database, item.ID)
			git.mu.Lock()
			pushes := git.pushes
			git.mu.Unlock()
			switch {
			case err == nil && found && stored.PRRef == url && receipt.PRRef == url:
			case err != nil && !found && pushes == 0:
			default:
				t.Fatalf("cancelled at %s: err=%v receipt=%+v stored=%+v found=%t pushes=%d; want not started or completed",
					point, err, receipt, stored, found, pushes)
			}
		})
	}
}

// A repeated PR disposition whose PR an earlier attempt opened records that
// PR instead of failing on the forge's duplicate refusal.
func TestPRDispositionAdoptsTheBranchsOpenPR(t *testing.T) {
	t.Parallel()
	git := &prGit{
		createPR: func(context.Context) (string, error) {
			return "", errors.New("a pull request for branch workflow/run into branch main already exists")
		},
		listOpenPRs: func(context.Context) ([]gitops.GitPR, error) {
			return []gitops.GitPR{{URL: "https://github.com/acme/repo/pull/7"}}, nil
		},
	}
	service, database, item := newPRDispositionFixture(t, git)
	if _, err := service.CreateItemPR(context.Background(), item.ID); err != nil {
		t.Fatalf("CreateItemPR = %v", err)
	}
	if stored, found := storedPRReceipt(t, database, item.ID); !found || stored.PRRef != "https://github.com/acme/repo/pull/7" {
		t.Fatalf("receipt = %+v (found %t), want the branch's open PR", stored, found)
	}

	// No open PR, or more than one, is not an answer: the refusal stands.
	for _, open := range [][]gitops.GitPR{nil, {{URL: "a"}, {URL: "b"}}} {
		git.listOpenPRs = func(context.Context) ([]gitops.GitPR, error) { return open, nil }
		service, database, item := newPRDispositionFixture(t, git)
		if _, err := service.CreateItemPR(context.Background(), item.ID); err == nil {
			t.Fatalf("CreateItemPR with %d open PRs succeeded", len(open))
		}
		if _, found := storedPRReceipt(t, database, item.ID); found {
			t.Fatalf("a receipt was written with %d open PRs", len(open))
		}
	}
}
