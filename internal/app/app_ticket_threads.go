package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	attachmentstore "agent-overflow/internal/attachment"
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
// current branch when the branch does not exist. A name only a tag has
// makes a branch of that name and checks it out, never the tag.
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
	return a.makeTicketThread(ctx, ticketThread{project: project, conn: conn, issues: []string{req.IssueID}, origin: req.Origin, branch: branch})
}

// MaxThreadTickets is the most tickets one thread is started on: each
// ticket's file is one attachment of the draft, and a message carries at
// most attachment.DefaultMaxCount.
const MaxThreadTickets = attachmentstore.DefaultMaxCount

// StartTicketsThreadRequest is a thread started on tickets selected
// together (a dashboard's ticket list): the tickets, by the connection that
// reads them and their issue IDs, and the text its draft starts with.
type StartTicketsThreadRequest struct {
	ConnectionID string   `json:"connectionId"`
	IssueIDs     []string `json:"issueIds"`
	Text         string   `json:"text,omitempty"`
}

// StartTicketsThread makes one thread in projectID on the tickets req
// names, at most MaxThreadTickets, each named once, and answers its row:
// in the project's checkout, with every ticket's file attached to its
// draft in the order named, and the draft's text req.Text, unsent. A
// thread of one ticket is titled and linked as that ticket's Ask is
// (StartTicketThread); a thread of several is linked to none, since a
// thread links one ticket, and titled with their keys ("WEB-1, WEB-2,
// WEB-3 and 2 more"). Every ticket is read before anything is made; a
// failure at any later step deletes the thread, which ends its link.
// Nothing is written to the tracker.
//
//ao:scope threads:operate
//ao:budget 2m
func (a *App) StartTicketsThread(ctx context.Context, projectID string, req StartTicketsThreadRequest) (store.Thread, error) {
	if a.shuttingDown.Load() {
		return store.Thread{}, ErrShuttingDown
	}
	if err := a.requireScope(ctx, transport.ScopeTrackerRead, "StartTicketsThread reads the tickets it attaches"); err != nil {
		return store.Thread{}, err
	}
	switch n := len(req.IssueIDs); {
	case n == 0:
		return store.Thread{}, errorsx.Public("ticket_thread_invalid", "a thread on tickets names at least one ticket", nil)
	case n > MaxThreadTickets:
		return store.Thread{}, errorsx.Public("ticket_thread_invalid", fmt.Sprintf("a thread is started on at most %d tickets, since each is an attachment of its first message; %d were named", MaxThreadTickets, n), nil)
	}
	for i, id := range req.IssueIDs {
		if strings.TrimSpace(id) == "" {
			return store.Thread{}, errorsx.Public("ticket_thread_invalid", "a thread on tickets names each ticket by its issue ID; one was empty", nil)
		}
		if slices.Contains(req.IssueIDs[:i], id) {
			return store.Thread{}, errorsx.Public("ticket_thread_invalid", fmt.Sprintf("a thread on tickets names each ticket once; issue %s was named twice", id), nil)
		}
	}
	project, err := a.store.GetProject(projectID)
	if err != nil {
		return store.Thread{}, fmt.Errorf("start tickets thread: %w", err)
	}
	conn, err := a.store.GetTrackerConnection(req.ConnectionID)
	if err != nil {
		return store.Thread{}, trackerError(err)
	}
	return a.makeTicketThread(ctx, ticketThread{project: project, conn: conn, issues: req.IssueIDs, origin: store.TicketOriginAsk, text: req.Text})
}

// ticketThread is a thread to make on tickets: in project, the tickets of
// conn by issue ID, read and attached in that order; its link's origin,
// which only a thread of one ticket is linked with; the branch a Start
// work thread runs on; and the draft's text.
type ticketThread struct {
	project store.Project
	conn    store.TrackerConnection
	issues  []string
	origin  string
	branch  string
	text    string
}

// makeTicketThread makes t's thread: it reads every ticket first (from the
// mirror, and those it holds no body of from the tracker together, once),
// then the thread
// and, on a branch, its worktree, the link of a thread of one ticket, each
// ticket's file and the draft. A failure after the thread is made deletes
// it and takes back the worktree and branch it cut.
func (a *App) makeTicketThread(ctx context.Context, t ticketThread) (store.Thread, error) {
	svc, err := a.trackerService()
	if err != nil {
		return store.Thread{}, err
	}
	issues := make([]tracker.IssueID, len(t.issues))
	for i, issue := range t.issues {
		issues[i] = tracker.IssueID(issue)
	}
	readCtx, cancel := withinBudget(ctx, budgetExceeded{doing: "Reading the tickets", budget: transport.DefaultMethodBudget, then: trackerSlow})
	details, err := svc.DetailsForThread(readCtx, t.conn.ID, issues)
	cancel()
	if err != nil {
		return store.Thread{}, trackerRunError(readCtx, err)
	}

	opts := CreateThreadOptions{ProjectID: t.project.ID, Title: ticketThreadTitle(details)}
	if t.branch != "" {
		held, err := a.branchCheckout(t.project, t.branch)
		if err != nil {
			return store.Thread{}, err
		}
		if held != "" {
			opts.WorktreePath, opts.Branch = held, t.branch
		} else {
			// Spelled in full, the branch is exactly this name: git's
			// refusal and no tidying, and a branch even where a tag
			// has the name.
			opts.WorktreeBranch = "refs/heads/" + t.branch
		}
	}
	thread, cut, err := a.createThreadWithWorktree(ctx, opts, false)
	if err != nil {
		return store.Thread{}, fmt.Errorf("start ticket thread: %w", err)
	}
	undo := func(cause error) (store.Thread, error) {
		return store.Thread{}, errors.Join(cause, a.rollBackCreatedThread("start ticket thread", t.project, thread, cut))
	}
	if t.branch != "" && thread.Branch != t.branch {
		return undo(errorsx.Public("ticket_thread_branch_changed",
			fmt.Sprintf("the worktree came out on %q, not on the branch %q named; nothing was made", thread.Branch, t.branch), nil))
	}

	if len(t.issues) == 1 {
		ticket := store.TicketRef{Kind: t.conn.Kind, Site: t.conn.Site, IssueID: t.issues[0]}
		write, err := svc.LinkThread(ctx, thread.ID, ticket, t.origin)
		if err != nil {
			return undo(threadTicketError(ctx, err))
		}
		a.announceThreadLink(thread.ID, write)
	}
	files := make([]string, len(details))
	for i, detail := range details {
		text := trackerapp.TicketFileText(detail)
		file, err := a.storeAttachment(ctx, thread.ID, detail.Card.Key+".md", "text/markdown", int64(len(text)), strings.NewReader(text))
		if err != nil {
			return undo(fmt.Errorf("start ticket thread: write the ticket file of %s: %w", detail.Card.Key, err))
		}
		files[i] = file.ID
	}
	seed := t.text
	if t.origin == store.TicketOriginStartWork {
		seed = "Work on " + details[0].Card.Key + ": " + details[0].Card.Title + "."
	}
	if err := a.SaveDraft(ctx, thread.ID, seed, files, nil, nil); err != nil {
		return undo(fmt.Errorf("start ticket thread: seed the draft: %w", err))
	}
	return a.store.GetThread(thread.ID)
}

// ticketThreadTitle is a ticket thread's title: one ticket's key and
// title, or the keys of several, the first three named ("WEB-1, WEB-2,
// WEB-3 and 2 more").
func ticketThreadTitle(details []trackerapp.DetailView) string {
	if len(details) == 1 {
		return strings.TrimSpace(details[0].Card.Key + " " + details[0].Card.Title)
	}
	keys := make([]string, 0, 3)
	for _, d := range details[:min(3, len(details))] {
		keys = append(keys, d.Card.Key)
	}
	switch rest := len(details) - len(keys); {
	case rest > 0:
		return strings.Join(keys, ", ") + fmt.Sprintf(" and %d more", rest)
	default:
		return strings.Join(keys[:len(keys)-1], ", ") + " and " + keys[len(keys)-1]
	}
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
