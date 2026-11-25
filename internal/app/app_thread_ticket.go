package app

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"slices"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// A thread's ticket link: the bound methods that set, clear and list it,
// and the app's part of keeping each thread row's link current
// (trackerapp thread_links.go owns the rest). A link's own row is
// announced by the method that changed it; the rows of other threads a
// card write or a connection change moved arrive through
// announceThreadLinks.

// ThreadTicketRef names the ticket a thread links to: its tracker kind
// (jira or linear), the site as the tracker connections name it, and the
// tracker's own issue ID. It names no connection: the computer that holds
// the thread reads the ticket through its own.
type ThreadTicketRef struct {
	Kind    string `json:"kind"`
	Site    string `json:"site"`
	IssueID string `json:"issueId"`
}

func (r ThreadTicketRef) ticket() store.TicketRef {
	return store.TicketRef{Kind: r.Kind, Site: r.Site, IssueID: r.IssueID}
}

// SetThreadTicket links the thread to ticket, replacing the link it had,
// and returns its row. The ticket is read through this computer's
// connection to its site: from the issue mirror when it holds the ticket,
// with no request, otherwise with one card read, bounded as a form read
// is. A link that cannot be made leaves the link as it was. Reading the
// ticket needs the ticket read grant as well as the thread's.
//
//ao:scope threads:operate
func (a *App) SetThreadTicket(ctx context.Context, threadID string, ticket ThreadTicketRef) (store.Thread, error) {
	if err := a.requireScope(ctx, transport.ScopeTrackerRead, "SetThreadTicket reads the ticket it links"); err != nil {
		return store.Thread{}, err
	}
	svc, err := a.trackerService()
	if err != nil {
		return store.Thread{}, err
	}
	ctx, cancel := a.trackerRun(ctx, "SetThreadTicket", "Linking the ticket", trackerSlow)
	defer cancel()
	changed, err := svc.LinkThread(ctx, threadID, ticket.ticket(), store.TicketOriginManual)
	if err != nil {
		return store.Thread{}, threadTicketError(ctx, err)
	}
	if changed {
		a.announceThread(triage.ThreadActionFull, threadID)
	}
	return a.store.GetThread(threadID)
}

// ClearThreadTicket removes the thread's ticket link and returns its row.
//
//ao:scope threads:operate
func (a *App) ClearThreadTicket(ctx context.Context, threadID string) (store.Thread, error) {
	svc, err := a.trackerService()
	if err != nil {
		return store.Thread{}, err
	}
	had, err := svc.UnlinkThread(threadID)
	if err != nil {
		return store.Thread{}, threadTicketError(ctx, err)
	}
	if had {
		a.announceThread(triage.ThreadActionFull, threadID)
	}
	return a.store.GetThread(threadID)
}

// ThreadsForTicket lists the rows of this computer's threads linked to
// ticket, newest first, as search lists them: an ask's copy and a side
// chat before Keep left out. Its cost grows with the ticket's links, not
// with the threads.
//
//ao:scope threads:read
//ao:route selected
func (a *App) ThreadsForTicket(ctx context.Context, ticket ThreadTicketRef) ([]store.Thread, error) {
	rows, err := a.store.ListThreadsForTicket(ticket.ticket())
	if errors.Is(err, store.ErrInvalidThreadTicket) {
		return nil, errorsx.Public("thread_ticket_invalid", "a ticket is named by its tracker (Jira or Linear), its site and its issue ID", err)
	}
	return rows, err
}

// threadTicketError is a link write's error as its caller reads it: the
// store's refusals of the thread in this feature's words, and otherwise as
// a tracker call's (trackerRunError), whose refusals include
// trackerapp.LinkRefusal's own sentence.
func threadTicketError(ctx context.Context, err error) error {
	var transfer *store.ThreadTransferError
	switch {
	case errors.As(err, &transfer):
		return errorsx.Public("thread_ticket_thread_moving",
			"this conversation is moving, or moved, to another computer; link the ticket on the computer that has it. The link was not changed", err)
	case errors.Is(err, store.ErrThreadDeleting):
		return errorsx.Public("thread_ticket_thread_deleting", "this conversation is being deleted. The link was not changed", err)
	case errors.Is(err, sql.ErrNoRows):
		return errorsx.Public("thread_ticket_thread_missing", "this conversation is not on this computer", err)
	case errors.Is(err, store.ErrInvalidThreadTicket):
		return errorsx.Public("thread_ticket_invalid",
			"this link cannot be stored: a ticket is named by its tracker (Jira or Linear), its site and its issue ID, and only a conversation can hold one", err)
	case errors.Is(err, store.ErrThreadTicketNotReader):
		return errorsx.Public("thread_ticket_connection_changed",
			"another connection to this site started reading its tickets while the ticket was being linked; link it again. The link was not changed", err)
	case errors.Is(err, store.ErrThreadTicketNotHeld):
		return errorsx.Public("thread_ticket_not_found_since",
			"a later read did not return this issue: it was deleted, or the token's user may not see it. The link was not changed", err)
	}
	return trackerRunError(ctx, err)
}

// announceThreadLinks announces the rows of threadIDs, whose ticket link
// the tracker service moved, on one serial queue: one job per call, its
// rows in order. It never blocks the service (trackerapp
// Deps.LinksChanged).
func (a *App) announceThreadLinks(threadIDs []string) {
	ids := slices.Clone(threadIDs)
	a.threadLinkAnnounces.Go(func() {
		ctx := a.lifeCtx()
		for _, id := range ids {
			a.threadAnnouncer().AnnounceWithin(ctx, a.emitEvent, triage.ThreadActionFull, id)
		}
	})
}

// threadLinksEnding reads the tickets of the links of threadIDs before a
// write that ends them (a delete's commit, a move away), for
// endThreadLinks after it. A read that fails is logged and ends none: the
// views that show such a ticket as another's parent or link show its card
// until their board is read again.
func (a *App) threadLinksEnding(threadIDs []string) map[string]store.TicketRef {
	if a.trackers == nil || len(threadIDs) == 0 {
		return nil
	}
	links, err := a.store.ListThreadTickets(threadIDs)
	if err != nil {
		log.Printf("thread links: read the links of %d threads before they end: %v", len(threadIDs), err)
		return nil
	}
	out := make(map[string]store.TicketRef, len(links))
	for id, link := range links {
		out[id] = link.Ticket
	}
	return out
}

// endThreadLinks tells the tracker service the tickets of ending (from
// threadLinksEnding) whose threads ended are in ended, so the issues
// nothing keeps any more leave the views that show them
// (trackerapp.Service.LinksEnded).
func (a *App) endThreadLinks(ending map[string]store.TicketRef, ended []string) {
	if a.trackers == nil || len(ending) == 0 {
		return
	}
	var tickets []store.TicketRef
	for _, id := range ended {
		if t, ok := ending[id]; ok {
			tickets = append(tickets, t)
		}
	}
	a.trackers.LinksEnded(tickets)
}
