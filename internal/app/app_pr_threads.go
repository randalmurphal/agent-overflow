package app

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The Pull requests page's "Open with agent" (docs/specs/pull-requests.md,
// "The page"): the threads already on a PR, and a new thread in a worktree
// on its head. A thread opened here is recorded in thread_pull_requests,
// which is how the badge, "Threads on this MR" and the merged-PR cleanup
// find it: git status reports no PR for a fork's pr/N branch.

// PRThreadsRequest names a PR and its head branch as the page's list row
// holds them: HeadRefName is the source branch, CrossRepository a head in
// another repository (a fork), whose local branch is pr/N.
type PRThreadsRequest struct {
	ProjectID       string             `json:"projectId"`
	PR              gitops.PRReference `json:"pr"`
	HeadRefName     string             `json:"headRefName"`
	CrossRepository bool               `json:"crossRepository"`
}

// PRThread is one thread on a PR: its state as the sidebar shows it, the
// branch it works on and when it last changed (unix milliseconds).
type PRThread struct {
	ThreadID       string `json:"threadId"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	WorktreeBranch string `json:"worktreeBranch"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// ListThreadsOnPR lists the threads tracking req.PR (thread_pull_requests,
// in any project) and the project's threads on its local head branch
// (HeadRefName, or pr/N for a fork's head), each once, most recently
// updated first. Archived threads are left out.
//
//ao:scope git:operate
//ao:route selected
func (a *App) ListThreadsOnPR(ctx context.Context, req PRThreadsRequest) ([]PRThread, error) {
	if err := a.requireScope(ctx, transport.ScopeThreadsRead, "ListThreadsOnPR lists threads"); err != nil {
		return nil, err
	}
	if err := req.PR.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		return nil, errors.New("list threads on PR: project is required")
	}
	branch := prLocalHead(req.HeadRefName, req.CrossRepository, req.PR.Number)
	tracked, err := a.store.ListThreadPullRequestsForPR(req.PR.Forge, req.PR.Host, req.PR.Project(), req.PR.Number)
	if err != nil {
		return nil, fmt.Errorf("list threads on PR: %w", err)
	}
	var threads []store.Thread
	seen := map[string]bool{}
	for _, row := range tracked {
		thread, err := a.store.GetOwnedThread(row.ThreadID)
		var moved *store.ThreadTransferError
		switch {
		case errors.As(err, &moved), errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("list threads on PR: %w", err)
		}
		if !thread.Archived && !seen[thread.ID] {
			seen[thread.ID] = true
			threads = append(threads, thread)
		}
	}
	if branch != "" {
		project, err := a.store.ListThreadsByProject(req.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("list threads on PR: %w", err)
		}
		for _, thread := range project {
			if thread.Branch == branch && !seen[thread.ID] {
				seen[thread.ID] = true
				threads = append(threads, thread)
			}
		}
	}
	tools := a.threadTools()
	projector := tools.newThreadProjector()
	out := make([]PRThread, 0, len(threads))
	for _, thread := range threads {
		live, err := tools.LiveState(ctx, thread.ID)
		if err != nil {
			return nil, fmt.Errorf("list threads on PR: %w", err)
		}
		out = append(out, PRThread{
			ThreadID:       thread.ID,
			Title:          thread.Title,
			Status:         threadtools.State(projector.thread(thread), live),
			WorktreeBranch: thread.Branch,
			UpdatedAt:      thread.UpdatedAt,
		})
	}
	slices.SortStableFunc(out, func(x, y PRThread) int { return cmp.Compare(y.UpdatedAt, x.UpdatedAt) })
	return out, nil
}

// prLocalHead is the local branch a PR's head is checked out on: its head
// branch, or pr/N for a fork's.
func prLocalHead(headRefName string, crossRepository bool, number int) string {
	if crossRepository {
		return gitops.PRLocalBranch(number)
	}
	return strings.TrimSpace(headRefName)
}

// OpenPRThreadRequest is "New thread on BRANCH": the project the PR's
// repository is checked out in, the PR as the list row holds it, and the
// identity the merged-PR cleanup tracks it by (URL; NodeID, which GitHub's
// state reads need).
type OpenPRThreadRequest struct {
	ProjectID       string             `json:"projectId"`
	PR              gitops.PRReference `json:"pr"`
	HeadRefName     string             `json:"headRefName"`
	CrossRepository bool               `json:"crossRepository"`
	URL             string             `json:"url"`
	NodeID          string             `json:"nodeId"`
	Title           string             `json:"title"`
}

// OpenPRThreadResult is the new thread, its worktree and its branch.
type OpenPRThreadResult struct {
	ThreadID     string `json:"threadId"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
}

// OpenPRThread creates a thread in a new worktree on req.PR's head and
// records the PR for it. A head in this repository is checked out as its
// branch, fetched from origin when only origin has it; a fork's head is
// fetched from the forge's head ref into the local branch pr/N, which is
// refused when pr/N exists at another commit. A failure once the thread
// exists removes the thread, its worktree and a pr/N this call created,
// and returns the cause. The frontend seeds the thread's draft.
//
//ao:scope git:operate
//ao:route selected
func (a *App) OpenPRThread(ctx context.Context, req OpenPRThreadRequest) (OpenPRThreadResult, error) {
	if a.shuttingDown.Load() {
		return OpenPRThreadResult{}, ErrShuttingDown
	}
	if err := a.requireScope(ctx, transport.ScopeThreadsOperate, "OpenPRThread creates a thread"); err != nil {
		return OpenPRThreadResult{}, err
	}
	if err := validateOpenPRThread(req); err != nil {
		return OpenPRThreadResult{}, err
	}
	project, err := a.store.GetProject(req.ProjectID)
	if err != nil {
		return OpenPRThreadResult{}, fmt.Errorf("open PR thread: %w", err)
	}
	repo, err := a.projectForgeRepository(ctx, project.ID)
	if err != nil {
		return OpenPRThreadResult{}, fmt.Errorf("open PR thread: %w", err)
	}
	if repo.Key() != req.PR.Repository().Key() {
		return OpenPRThreadResult{}, fmt.Errorf("open PR thread: %s is not a pull request of %s, the repository of %s", req.PR.Key(), repo.Key(), project.Name)
	}

	branch := strings.TrimSpace(req.HeadRefName)
	createdBranch := false
	if req.CrossRepository {
		branch, _, createdBranch, err = a.gitCore().FetchPRBranch(ctx, project.Path, req.PR)
		if err != nil {
			return OpenPRThreadResult{}, fmt.Errorf("open PR thread: %w", err)
		}
	} else {
		// The create path cuts a new branch when the named one does not
		// exist, which would be a thread on the wrong code.
		exists, fetchErr, err := a.gitCore().BranchToCheckOut(ctx, project.Path, branch)
		if err != nil {
			return OpenPRThreadResult{}, fmt.Errorf("open PR thread: %w", err)
		}
		if !exists {
			if fetchErr != nil {
				return OpenPRThreadResult{}, fmt.Errorf("open PR thread: branch %s is not on origin as last fetched, and fetching origin failed: %w", branch, fetchErr)
			}
			return OpenPRThreadResult{}, fmt.Errorf("open PR thread: branch %s is not on origin", branch)
		}
	}
	thread, err := a.createThread(ctx, CreateThreadOptions{ProjectID: project.ID, WorktreeBranch: branch}, false)
	if err != nil {
		return OpenPRThreadResult{}, errors.Join(fmt.Errorf("open PR thread: %w", err), a.dropPRBranch(project.Path, branch, createdBranch))
	}
	_, err = a.store.RecordOpenThreadPullRequest(store.ThreadPullRequest{
		ThreadID: thread.ID, Forge: req.PR.Forge, Host: req.PR.Host, Project: req.PR.Project(), Number: req.PR.Number,
		URL: strings.TrimSpace(req.URL), NodeID: strings.TrimSpace(req.NodeID), Branch: thread.Branch,
		WorktreePath: thread.WorktreePath, SeenOpenAt: time.Now().UnixMilli(),
	})
	if err != nil {
		cause := fmt.Errorf("open PR thread: record the pull request: %w", err)
		return OpenPRThreadResult{}, errors.Join(cause, a.rollBackPRThread(project, thread, createdBranch))
	}
	return OpenPRThreadResult{ThreadID: thread.ID, WorktreePath: thread.WorktreePath, Branch: thread.Branch}, nil
}

func validateOpenPRThread(req OpenPRThreadRequest) error {
	if strings.TrimSpace(req.ProjectID) == "" {
		return errors.New("open PR thread: project is required")
	}
	if err := req.PR.Validate(); err != nil {
		return fmt.Errorf("open PR thread: %w", err)
	}
	if strings.TrimSpace(req.URL) == "" {
		return errors.New("open PR thread: the pull request URL is required")
	}
	// The merged-PR cleanup reads a GitHub PR's state by its node id.
	if req.PR.Forge == "github" && strings.TrimSpace(req.NodeID) == "" {
		return errors.New("open PR thread: a GitHub pull request's node id is required")
	}
	if !req.CrossRepository {
		if err := gitops.ValidateBranchName(strings.TrimSpace(req.HeadRefName)); err != nil {
			return fmt.Errorf("open PR thread: head branch: %w", err)
		}
	}
	return nil
}

// rollBackPRThread undoes a thread OpenPRThread created: the thread, then
// its worktree (its setup cancelled and joined first), then a pr/N branch
// the call created. Every step runs; their failures are returned together.
func (a *App) rollBackPRThread(project store.Project, thread store.Thread, createdBranch bool) error {
	var errs []error
	if err := a.DeleteThread(thread.ID); err != nil {
		errs = append(errs, fmt.Errorf("delete thread %s: %w", thread.ID, err))
	}
	if thread.WorktreePath != "" {
		if _, err := a.removeProjectWorktree(project.ID, project.Path, thread.WorktreePath, worktreeRemovalPolicy{force: true}); err != nil {
			errs = append(errs, fmt.Errorf("remove worktree %s: %w", thread.WorktreePath, err))
		}
	}
	errs = append(errs, a.dropPRBranch(project.Path, thread.Branch, createdBranch))
	err := errors.Join(errs...)
	if err != nil {
		log.Printf("open PR thread: roll back thread %s: %v", thread.ID, err)
	}
	return err
}

// dropPRBranch deletes a pr/N branch this call created; nil otherwise.
func (a *App) dropPRBranch(projectPath, branch string, created bool) error {
	if !created {
		return nil
	}
	if err := a.gitCore().DeleteBranch(projectPath, branch, true); err != nil {
		return fmt.Errorf("delete branch %s: %w", branch, err)
	}
	return nil
}
