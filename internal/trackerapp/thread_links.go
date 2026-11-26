package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A thread's ticket link (store thread_tickets) names a ticket by tracker
// kind, site and issue, so a thread moved to another computer keeps it.
// The thread row shows the card the mirror of the site's link reader
// holds, which the link keeps there, and the link's last-seen copy while
// that mirror holds none (store ThreadTicketLink); the store writes the
// copy and names the rows a card write moves (store MirrorChanges.Threads).
// This file links and unlinks, and tells what a link's end leaves no
// longer kept.

// LinkRefusal is a link this computer did not make, or a linked ticket it
// cannot read, for a reason the person fixes outside the link: Message
// says what, in this package's words, and what to do then. Code names the
// reason for a surface. It unwraps to Err.
type LinkRefusal struct {
	Code    string
	Message string
	Err     error
}

func (e *LinkRefusal) Error() string { return e.Message }
func (e *LinkRefusal) Unwrap() error { return e.Err }

// PublicDetails is the refusal as a caller of a bound method reads it
// (errorsx.Detailed).
func (e *LinkRefusal) PublicDetails() (code, message string) { return e.Code, e.Message }

// The reasons of a LinkRefusal.
const (
	LinkNoConnection  = "thread_ticket_no_connection"
	LinkConnectionOff = "thread_ticket_connection_off"
	LinkNotFound      = "thread_ticket_not_found"
)

// siteName is site as a person names it: a Jira site without its scheme,
// a Linear workspace's key as it is.
func siteName(site string) string { return strings.TrimPrefix(site, "https://") }

// connectionRefusal is the refusal of a link to ticket, or of a read of
// the ticket a thread is linked to, when no connection of its tracker and
// site is on here: state is the link state the store gives that site
// (store TicketConnection), and then what to do once that is fixed.
func connectionRefusal(ticket store.TicketRef, state string, err error, then string) error {
	if state == store.TicketLinkTurnedOff {
		return &LinkRefusal{Code: LinkConnectionOff, Err: err, Message: fmt.Sprintf(
			"the %s connection to %s is turned off; turn it on in the Issues settings, then %s",
			trackerName(ticket.Kind), siteName(ticket.Site), then)}
	}
	return &LinkRefusal{Code: LinkNoConnection, Err: err, Message: fmt.Sprintf(
		"this computer has no %s connection to %s; add one in the Issues settings, then %s",
		trackerName(ticket.Kind), siteName(ticket.Site), then)}
}

// linkAgain is what a refused link says to do once its connection is on.
const linkAgain = "link the ticket again. The link was not changed"

// LinkedTicket is the ticket a thread is linked to as this computer reads
// it: the connection that reads its site (the link reader, store
// TicketConnection) and the site as a person names it, its issue, and the
// key the link last saw, which the tracker may have changed since.
type LinkedTicket struct {
	Connection string
	Site       string
	Issue      string
	Key        string
}

// LinkedTicket is the ticket thread threadID is linked to; ok is false for
// a thread with no link (or one this computer does not own). A link whose
// site no connection here reads, or reads only through connections turned
// off, fails with a *LinkRefusal that says which.
func (s *Service) LinkedTicket(threadID string) (_ LinkedTicket, ok bool, _ error) {
	if err := s.enter(); err != nil {
		return LinkedTicket{}, false, err
	}
	defer s.leave()
	link, ok, err := s.store.GetThreadTicket(threadID)
	if err != nil || !ok {
		return LinkedTicket{}, false, err
	}
	id, state, err := s.store.TicketConnection(link.Ticket)
	if err != nil {
		return LinkedTicket{}, false, fmt.Errorf("trackerapp: the connection of thread %s's ticket: %w", threadID, err)
	}
	if id == "" {
		return LinkedTicket{}, false, connectionRefusal(link.Ticket, state, nil, "read this thread's ticket "+link.IssueKey+" again")
	}
	return LinkedTicket{Connection: id, Site: siteName(link.Ticket.Site), Issue: link.Ticket.IssueID, Key: link.IssueKey}, true, nil
}

// LinkThread links thread threadID to ticket, replacing the link it had,
// with origin (store.TicketOrigin*). It reads through the connection of
// this computer that store TicketConnection names, and links the card
// that connection's mirror holds when it holds one no later read missed,
// with no request; otherwise it reads the card from the tracker, a read a
// person waits on, and stores it with the link in one write, which the
// link keeps from then on. A link the store refuses for its thread
// (moving, being deleted, a holder, missing), its input or its connection
// (store ErrThreadTicketNotReader: another connection of the site took
// over the links since) fails with the store's error, and one this
// computer cannot make fails with a *LinkRefusal; the link stays as it was
// either way. It reports what the write did to thread threadID's row
// (ThreadLinkWrite), which its caller announces. The other threads whose link
// the card it stored moved are told (Deps.LinksChanged), and the ticket a
// replaced link named is told as no longer kept when nothing else keeps it
// (LinksEnded).
func (s *Service) LinkThread(ctx context.Context, threadID string, ticket store.TicketRef, origin string) (ThreadLinkWrite, error) {
	id, state, err := s.store.TicketConnection(ticket)
	if err != nil {
		return ThreadLinkWrite{}, fmt.Errorf("trackerapp: the connection of %s ticket %s: %w", ticket.Kind, ticket.IssueID, err)
	}
	if id == "" {
		return ThreadLinkWrite{}, connectionRefusal(ticket, state, nil, linkAgain)
	}
	r, err := s.interact(ctx, id)
	switch {
	case errors.Is(err, ErrDisabled):
		return ThreadLinkWrite{}, connectionRefusal(ticket, store.TicketLinkTurnedOff, err, linkAgain)
	case errors.Is(err, ErrNotFound):
		return ThreadLinkWrite{}, connectionRefusal(ticket, store.TicketLinkNoConnection, err, linkAgain)
	case err != nil:
		return ThreadLinkWrite{}, err
	}
	set, asked, err := r.linkThread(threadID, ticket, origin)
	if asked {
		err = r.answer(err)
	}
	r.finish()
	if err != nil {
		return ThreadLinkWrite{}, err
	}
	if set.Replaced != nil {
		s.LinksEnded([]store.TicketRef{*set.Replaced})
	}
	return ThreadLinkWrite{Changed: set.Changed, Listing: set.Listing}, nil
}

// ThreadLinkWrite is what a link write did to its thread's row: whether
// the link the row shows changed, and how the write moved the thread
// across the sidebar listing rule (a link makes a never-sent thread real).
// Its caller announces both.
type ThreadLinkWrite struct {
	Changed bool
	Listing store.ListingChange
}

// linkThread writes the link of LinkThread on the run's connection: of
// the card the mirror holds when it holds one, else of one card read.
// asked reports that it made a tracker call, whose outcome the caller
// settles (run.answer).
func (r *run) linkThread(threadID string, ticket store.TicketRef, origin string) (set store.ThreadTicketSet, asked bool, err error) {
	if err := r.readState(); err != nil {
		return set, false, err
	}
	base := store.ThreadTicketWrite{ThreadID: threadID, Origin: origin, LinkedAt: millis(r.s.now()), Root: r.root, IssueID: ticket.IssueID}
	set, err = r.s.store.SetThreadTicket(base)
	if !errors.Is(err, store.ErrThreadTicketNotHeld) {
		return set, false, err
	}
	err = r.againIfSwept(func() error {
		began, err := r.beginRead()
		if err != nil {
			return err
		}
		issue := tracker.IssueID(ticket.IssueID)
		read, err := r.p.Cards(r.ctx, []tracker.IssueID{issue})
		if err != nil {
			return fmt.Errorf("trackerapp: read the card of %s: %w", ticket.IssueID, call(err))
		}
		if err := r.markMissing(began, read.Missing); err != nil {
			return err
		}
		switch {
		case len(read.Cards) == 0:
			return &LinkRefusal{Code: LinkNotFound, Err: ErrIssueNotFound, Message: trackerName(ticket.Kind) +
				" did not return this issue: it was deleted, or the token's user may not see it. The link was not changed"}
		case len(read.Cards) > 1 || read.Cards[0].ID != issue:
			return fmt.Errorf("trackerapp: the card of %s: %w", ticket.IssueID, call(fmt.Errorf("the tracker answered with %d cards", len(read.Cards))))
		}
		card, err := mirrorCard(read.Cards[0])
		if err != nil {
			return err
		}
		catalog, err := mirrorCatalog(read.Catalog)
		if err != nil {
			return err
		}
		w := base
		w.Card, w.Read, w.Catalog, w.Sweep = card, began, catalog, r.state.Sweep
		if set, err = r.s.store.SetThreadTicket(w); err != nil {
			return err
		}
		r.told("", changesOf(set.Cards))
		return nil
	})
	return set, true, err
}

// UnlinkThread removes thread threadID's ticket link and reports what it
// did to the row (ThreadLinkWrite; Changed when it had one), which its
// caller announces. A thread the store refuses a link write for fails with
// the store's error. A link removed tells its ticket as no longer kept
// when nothing else keeps it (LinksEnded).
func (s *Service) UnlinkThread(threadID string) (ThreadLinkWrite, error) {
	if err := s.enter(); err != nil {
		return ThreadLinkWrite{}, err
	}
	defer s.leave()
	cleared, err := s.store.ClearThreadTicket(threadID)
	if err != nil || !cleared.Had {
		return ThreadLinkWrite{}, err
	}
	s.LinksEnded([]store.TicketRef{cleared.Ticket})
	return ThreadLinkWrite{Changed: true, Listing: cleared.Listing}, nil
}

// LinksEnded tells, for tickets whose links ended (an unlink, a link
// replaced, a thread deleted or moved away), the issues an attached
// mirror of this computer holds and no longer keeps (store
// ListUnkeptLinkedIssues) to the subscriptions of each one's connection
// that show them as another's link (Changes.Visibility), as the
// drop of an open ticket's keep does. The root and its generation are
// read together under opMu, so a turn off that detached the root since
// reaches no subscription of a later mirror. A read that fails is logged:
// the views that show such an issue show its card until their board is
// read again.
func (s *Service) LinksEnded(tickets []store.TicketRef) {
	if len(tickets) == 0 {
		return
	}
	type unkept struct {
		root   int64
		gen    uint64
		issues []tracker.IssueID
	}
	s.opMu.Lock()
	refs, err := s.store.ListUnkeptLinkedIssues(tickets)
	byConnection := map[string]*unkept{}
	var order []string
	for _, ref := range refs {
		u := byConnection[ref.ConnectionID]
		if u == nil {
			u = &unkept{root: ref.Root, gen: s.gen(ref.ConnectionID)}
			byConnection[ref.ConnectionID] = u
			order = append(order, ref.ConnectionID)
		}
		u.issues = append(u.issues, tracker.IssueID(ref.IssueID))
	}
	s.opMu.Unlock()
	if err != nil {
		s.logf("trackerapp: read the issues %d ended links kept: %v; the views that link them show them until their boards are read again", len(tickets), err)
		return
	}
	for _, conn := range order {
		u := byConnection[conn]
		s.publish(u.root, u.gen, conn, Changes{Visibility: u.issues}, nil, nil)
	}
}

// siteLinksChanged tells the threads linked to a ticket of tracker kind's
// site (store ListThreadsLinkedToSite), after a connection of that site
// was added, turned on or off, or removed: the state and connection each
// one's row shows are derived from the site's connections. A Tempo
// connection reads no ticket. A read that fails is logged: the rows show
// the earlier state until they are read again.
func (s *Service) siteLinksChanged(row store.TrackerConnection) {
	if row.Kind == store.TrackerKindTempo {
		return
	}
	threads, err := s.store.ListThreadsLinkedToSite(row.Kind, row.Site)
	if err != nil {
		s.logf("trackerapp: read the threads linked to %s: %v; their rows show the link's earlier state until they are read again", siteName(row.Site), err)
		return
	}
	if len(threads) > 0 {
		s.linksChanged(threads)
	}
}
