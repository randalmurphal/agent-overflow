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
// The link holds the ticket's key, title and status as a mirror of this
// computer last stored its card, and keeps that card in the mirror while
// it exists. The store keeps the copy equal to the card in every card
// write (store MirrorChanges.Threads); this file links, unlinks and adopts
// links, and tells what a link's end leaves no longer kept.

// LinkRefusal is a link this computer did not make, for a reason the
// person fixes outside the link: Message says what, in this package's
// words, and that the link stayed as it was. Code names the reason for a
// surface. It unwraps to Err.
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

// connectionRefusal is the refusal of a link to ticket when no connection
// of its tracker and site is on here: state is the link state the store
// gives that site (store TicketConnection).
func connectionRefusal(ticket store.TicketRef, state string, err error) error {
	if state == store.TicketLinkTurnedOff {
		return &LinkRefusal{Code: LinkConnectionOff, Err: err, Message: fmt.Sprintf(
			"the %s connection to %s is turned off; turn it on in the Issues settings, then link the ticket again. The link was not changed",
			trackerName(ticket.Kind), siteName(ticket.Site))}
	}
	return &LinkRefusal{Code: LinkNoConnection, Err: err, Message: fmt.Sprintf(
		"this computer has no %s connection to %s; add one in the Issues settings, then link the ticket again. The link was not changed",
		trackerName(ticket.Kind), siteName(ticket.Site))}
}

// LinkThread links thread threadID to ticket, replacing the link it had,
// with origin (store.TicketOrigin*). It reads through the connection of
// this computer that store TicketConnection names, and takes the ticket's
// card from that connection's mirror when the mirror holds it, with no
// request; otherwise it reads the card from the tracker, a read a person
// waits on, and stores it with the link in one write, which the link
// keeps from then on. A link the store refuses for its thread (moving,
// being deleted, a holder, missing) or its input fails with the store's
// error, and one this computer cannot make fails with a *LinkRefusal; the
// link stays as it was either way. It reports whether thread threadID's
// link changed, which its caller announces. The other threads whose link
// the card it stored moved are told (Deps.LinksChanged), and the ticket a
// replaced link named is told as no longer kept when nothing else keeps it
// (LinksEnded).
func (s *Service) LinkThread(ctx context.Context, threadID string, ticket store.TicketRef, origin string) (changed bool, _ error) {
	id, state, err := s.store.TicketConnection(ticket)
	if err != nil {
		return false, fmt.Errorf("trackerapp: the connection of %s ticket %s: %w", ticket.Kind, ticket.IssueID, err)
	}
	if id == "" {
		return false, connectionRefusal(ticket, state, nil)
	}
	r, err := s.interact(ctx, id)
	switch {
	case errors.Is(err, ErrDisabled):
		return false, connectionRefusal(ticket, store.TicketLinkTurnedOff, err)
	case errors.Is(err, ErrNotFound):
		return false, connectionRefusal(ticket, store.TicketLinkNoConnection, err)
	case err != nil:
		return false, err
	}
	set, asked, err := r.linkThread(threadID, ticket, origin)
	if asked {
		err = r.answer(err)
	}
	r.finish()
	if err != nil {
		return false, err
	}
	if set.Replaced != nil {
		s.LinksEnded([]store.TicketRef{*set.Replaced})
	}
	return set.Changed, nil
}

// linkThread writes the link of LinkThread on the run's connection: from
// the card the mirror holds when it holds one, else from one card read.
// asked reports that it made a tracker call, whose outcome the caller
// settles (run.answer). A card the mirror replaced between its read and
// the write is read from the mirror once more.
func (r *run) linkThread(threadID string, ticket store.TicketRef, origin string) (set store.ThreadTicketSet, asked bool, err error) {
	if err := r.readState(); err != nil {
		return set, false, err
	}
	w := store.ThreadTicketWrite{ThreadID: threadID, Origin: origin, LinkedAt: millis(r.s.now()), Root: r.root}
	for again := false; ; again = true {
		held, err := r.s.store.ListMirrorCards(r.root, []string{ticket.IssueID})
		if err != nil {
			return set, false, fmt.Errorf("trackerapp: read the card of %s from the mirror of %s: %w", ticket.IssueID, r.row.ID, err)
		}
		if len(held) != 1 {
			break
		}
		card, ok := cardOf(held[0])
		if !ok {
			// A card whose blob this version cannot read has no title:
			// the tracker's card has.
			break
		}
		w.Card = held[0]
		w.Card.Title = card.Title
		set, err = r.s.store.SetThreadTicket(w)
		if errors.Is(err, store.ErrThreadTicketCardMoved) && !again {
			continue
		}
		if err != nil {
			return set, false, err
		}
		r.told("", changesOf(set.Cards))
		return set, false, nil
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
		if _, err := r.markMissing(began, read.Missing); err != nil {
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
		w.Card, w.Read, w.Catalog, w.Sweep = card, began, catalog, r.state.Sweep
		if set, err = r.s.store.SetThreadTicket(w); err != nil {
			return err
		}
		r.told("", changesOf(set.Cards))
		return nil
	})
	return set, true, err
}

// UnlinkThread removes thread threadID's ticket link and reports whether
// it had one, which its caller announces. A thread the store refuses a
// link write for fails with the store's error. A link removed tells its
// ticket as no longer kept when nothing else keeps it (LinksEnded).
func (s *Service) UnlinkThread(threadID string) (had bool, _ error) {
	if err := s.enter(); err != nil {
		return false, err
	}
	defer s.leave()
	ticket, had, err := s.store.ClearThreadTicket(threadID)
	if err != nil || !had {
		return false, err
	}
	s.LinksEnded([]store.TicketRef{ticket})
	return true, nil
}

// AdoptLinks takes, for the links of threadIDs, the card a mirror of
// this computer holds of each one's ticket, as a link made here would,
// with no request: for links another computer's thread carried here,
// whose copy names no mirror of this one. A link whose ticket no mirror
// here holds stays not checked until a read stores its card. It tells
// Deps.LinksChanged nothing: the caller announces the rows it received.
// It returns every failure, having tried every link; a link it could not
// adopt stays as it was.
func (s *Service) AdoptLinks(threadIDs []string) error {
	if err := s.enter(); err != nil {
		return err
	}
	defer s.leave()
	links, err := s.store.ListThreadTickets(threadIDs)
	if err != nil {
		return fmt.Errorf("trackerapp: read the links of %d threads: %w", len(threadIDs), err)
	}
	var errs []error
	for _, link := range links {
		if err := s.adoptLink(link); err != nil {
			errs = append(errs, fmt.Errorf("trackerapp: adopt the link of thread %s: %w", link.ThreadID, err))
		}
	}
	return errors.Join(errs...)
}

// adoptLink writes link from the card the mirror of its connection holds,
// once more when the mirror replaced the card meanwhile.
func (s *Service) adoptLink(link store.ThreadTicket) error {
	id, _, err := s.store.TicketConnection(link.Ticket)
	if err != nil || id == "" {
		return err
	}
	root, ok, err := s.store.GetMirrorRoot(id)
	if err != nil || !ok {
		return err
	}
	for again := false; ; again = true {
		held, err := s.store.ListMirrorCards(root, []string{link.Ticket.IssueID})
		if err != nil || len(held) != 1 {
			return err
		}
		card, ok := cardOf(held[0])
		if !ok {
			return nil
		}
		w := store.ThreadTicketWrite{ThreadID: link.ThreadID, Origin: link.Origin, LinkedAt: link.LinkedAt, Root: root, Card: held[0]}
		w.Card.Title = card.Title
		_, err = s.store.SetThreadTicket(w)
		if errors.Is(err, store.ErrThreadTicketCardMoved) && !again {
			continue
		}
		return err
	}
}

// LinksEnded tells, for tickets whose links ended (an unlink, a link
// replaced, a thread deleted or moved away), the issues an attached
// mirror of this computer holds and no longer keeps (store
// ListUnkeptLinkedIssues) to the subscriptions of each one's connection
// that show them as another's parent or link (Changes.Visibility), as the
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
		s.logf("trackerapp: read the issues %d ended links kept: %v; their parents' and links' views show them until their boards are read again", len(tickets), err)
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
