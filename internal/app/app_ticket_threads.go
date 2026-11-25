package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// StartTicketThreadRequest is a ticket's Ask or Start work: the ticket, by
// the connection that reads it and its issue ID, and the branch a Start
// work thread runs on. Ask names no branch: its thread runs in the
// project's checkout.
type StartTicketThreadRequest struct {
	ConnectionID string `json:"connectionId"`
	IssueID      string `json:"issueId"`
	// Origin is ask or start_work.
	Origin string `json:"origin"`
	Branch string `json:"branch,omitempty"`
}

// StartTicketThread makes the thread of a ticket's Ask or Start work in
// projectID and answers its row: titled with the ticket's key and title,
// linked to the ticket with the request's origin, with the ticket file
// (trackerapp.TicketFileText, KEY.md) attached to its draft, unsent, and,
// for Start work, the draft's text "Work on KEY: title." and a worktree on
// exactly the branch named: the worktree that already has the branch
// checked out, else a new one on the branch, made from the project's
// current branch when the branch does not exist; never a tag of that name.
// A Start work for a ticket, project and branch that already has a Start
// work thread, not archived, answers that thread and makes nothing, so
// asking again after a lost reply is safe; one ticket's Start works in a
// project run one at a time, so two at once make one thread. An archived
// one leaves the branch and its worktree as they are: the new thread runs
// in the worktree that still has the branch checked out, or in a new one
// on it. The ticket is read before anything is made (from
// the mirror, or once from the tracker when it holds no body); a failure
// at any later step deletes the thread, which ends its link and tells the
// views that showed the ticket as kept by it, and takes back the worktree
// and branch the call cut while they are still its own. Nothing is written
// to the tracker.
//
//ao:scope threads:operate
//ao:budget 12m
func (a *App) StartTicketThread(ctx context.Context, projectID string, req StartTicketThreadRequest) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	if err := a.requireScope(ctx, transport.ScopeTrackerRead, "StartTicketThread reads the ticket it links"); err != nil {
		return store.Thread{}, err
	}
	branch := strings.TrimSpace(req.Branch)
	switch req.Origin {
	case store.TicketOriginAsk:
		if branch != "" {
			return store.Thread{}, errorsx.Public("ticket_thread_invalid", "an Ask thread runs in the project's checkout, so it names no branch", nil)
		}
	case store.TicketOriginStartWork:
		if err := a.requireScope(ctx, transport.ScopeGitOperate, "Start work makes a worktree"); err != nil {
			return store.Thread{}, err
		}
		if branch == "" {
			return store.Thread{}, errorsx.Public("ticket_thread_branch_invalid", "Start work names a branch for its worktree", nil)
		}
		if err := gitops.ValidateBranchName(branch); err != nil {
			return store.Thread{}, errorsx.Public("ticket_thread_branch_invalid", fmt.Sprintf("git cannot name a branch %q", branch), err)
		}
	default:
		return store.Thread{}, errorsx.Public("ticket_thread_invalid", fmt.Sprintf("a ticket's thread is an Ask or a Start work, not %q", req.Origin), nil)
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("start ticket thread: %w", err)
	}
	svc, err := a.trackerService()
	if err != nil {
		return store.Thread{}, err
	}
	conn, err := a.store.GetTrackerConnection(req.ConnectionID)
	if err != nil {
		return store.Thread{}, trackerError(err)
	}
	ticket := store.TicketRef{Kind: conn.Kind, Site: conn.Site, IssueID: req.IssueID}
	if req.Origin == store.TicketOriginStartWork {
		release, err := a.startWork.acquire(ctx, strings.Join([]string{project.ID, ticket.Kind, ticket.Site, ticket.IssueID}, "\n"))
		if err != nil {
			return store.Thread{}, err
		}
		defer release()
		started, ok, err := a.startedWorkThread(project.ID, ticket, branch)
		if err != nil || ok {
			return started, err
		}
	}
	readCtx, cancel := withinBudget(ctx, budgetExceeded{doing: "Reading the ticket", budget: transport.DefaultMethodBudget, then: trackerSlow})
	detail, err := svc.DetailForThread(readCtx, req.ConnectionID, tracker.IssueID(req.IssueID))
	cancel()
	if err != nil {
		return store.Thread{}, trackerRunError(readCtx, err)
	}
	key, title := detail.Card.Key, detail.Card.Title

	opts := CreateThreadOptions{ProjectID: project.ID, Title: strings.TrimSpace(key + " " + title)}
	if branch != "" {
		held, err := a.branchCheckout(project, branch)
		if err != nil {
			return store.Thread{}, err
		}
		if held != "" {
			opts.WorktreePath, opts.Branch = held, branch
		} else {
			// Spelled in full, the branch is exactly this name: git's
			// refusal and no tidying, and never a tag of that name.
			opts.WorktreeBranch = "refs/heads/" + branch
		}
	}
	thread, cut, err := a.createThreadWithWorktree(ctx, opts, false)
	if err != nil {
		return store.Thread{}, fmt.Errorf("start ticket thread: %w", err)
	}
	undo := func(cause error) (store.Thread, error) {
		return store.Thread{}, errors.Join(cause, a.rollBackCreatedThread("start ticket thread", project, thread, cut))
	}
	if branch != "" && thread.Branch != branch {
		return undo(errorsx.Public("ticket_thread_branch_changed",
			fmt.Sprintf("the worktree came out on %q, not on the branch %q named; nothing was made", thread.Branch, branch), nil))
	}

	write, err := svc.LinkThread(ctx, thread.ID, ticket, req.Origin)
	if err != nil {
		return undo(threadTicketError(ctx, err))
	}
	a.announceThreadLink(thread.ID, write)
	text := trackerapp.TicketFileText(detail)
	file, err := a.storeAttachment(ctx, thread.ID, key+".md", "text/markdown", int64(len(text)), strings.NewReader(text))
	if err != nil {
		return undo(fmt.Errorf("start ticket thread: write the ticket file: %w", err))
	}
	seed := ""
	if req.Origin == store.TicketOriginStartWork {
		seed = "Work on " + key + ": " + title + "."
	}
	if err := a.SaveDraft(ctx, thread.ID, seed, []string{file.ID}, nil, nil); err != nil {
		return undo(fmt.Errorf("start ticket thread: seed the draft: %w", err))
	}
	return a.store.GetThread(thread.ID)
}

// startedWorkThread is the Start work thread already linked to ticket in
// projectID on branch, which a Start work asked again answers: its reply
// may never have arrived, or another window asked at the same moment. An
// archived one is the person's to leave, so it never answers.
func (a *App) startedWorkThread(projectID string, ticket store.TicketRef, branch string) (store.Thread, bool, error) {
	threads, err := a.store.ListThreadsForTicket(ticket)
	if err != nil {
		return store.Thread{}, false, fmt.Errorf("start ticket thread: the threads of the ticket: %w", err)
	}
	for _, t := range threads {
		if t.ProjectID == projectID && t.Branch == branch && !t.Archived && t.TicketLink != nil && t.TicketLink.Origin == store.TicketOriginStartWork {
			return t, true, nil
		}
	}
	return store.Thread{}, false, nil
}

// branchCheckout is the worktree of project that has branch checked out,
// "" when none has it. The project's own checkout on it is refused: a
// Start work thread runs in a worktree of its own.
func (a *App) branchCheckout(project store.Project, branch string) (string, error) {
	branches, err := a.gitCore().ListBranches(project.Path)
	if err != nil {
		return "", fmt.Errorf("start ticket thread: list the branches of %s: %w", project.Name, err)
	}
	for _, b := range branches {
		if b.Name != branch || b.WorktreePath == "" {
			continue
		}
		if gitops.SameFilesystemPath(b.WorktreePath, project.Path) {
			return "", errorsx.Public("ticket_thread_branch_checked_out",
				fmt.Sprintf("%s is checked out in %s's own checkout; check out another branch there, or name another branch", branch, project.Name), nil)
		}
		return b.WorktreePath, nil
	}
	return "", nil
}
