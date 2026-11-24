package trackerapp

import (
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// An open ticket's view has one source, the issue's record in the mirror:
// a read of the ticket stores what it found and settles its outcome
// (ticket_settle.go), and every view its subscribers are told is built from
// the record by buildTickets. The change funnel tells each pump of a
// stored change to an input of its view (tellTicketLocked), counting it in
// the pump's dirty; a build installs only a view whose build began after
// the last such change, so no view built before a change it misses
// outlives it.

// tellTicketLocked counts in tp's dirty, and nudges it to build, a stored
// change in ch to an input of its view (affectsLocked). The caller holds
// subMu.
func (s *Service) tellTicketLocked(tp *ticketPump, ch Changes) {
	if tp.over || tp.core.DeadLocked() || !affectsLocked(tp, ch) {
		return
	}
	tp.dirty++
	nudge(tp.viewChanged)
}

// affectsLocked reports whether ch, what one store write changed, moves an
// input of tp's view, as detailViews reads them: the record of its issue
// (its card with its status time and history, its rows and whether the
// mirror keeps it, its body), the cards of the issues the view shows and
// whether the mirror keeps them (a row stored or removed, a keep begun or
// ended), and the connection's users and statuses. While a build of the
// view is in flight the issues it will show are not known yet (it may
// show an issue the installed view does not), so any card, row or keep of
// the connection counts. The caller holds subMu.
func affectsLocked(tp *ticketPump, ch Changes) bool {
	own := func(ids []tracker.IssueID) bool { return slices.Contains(ids, tp.key.issue) }
	shown := func(ids []tracker.IssueID) bool {
		return tp.building > 0 && len(ids) > 0 ||
			slices.ContainsFunc(tp.view.shows, func(id tracker.IssueID) bool { return slices.Contains(ids, id) })
	}
	return ch.Catalog || own(ch.Cards) || own(ch.History) || own(ch.Rows) || own(ch.Removed) || own(ch.Details) ||
		own(ch.Dropped) || own(ch.Visibility) || shown(ch.Cards) || shown(ch.Rows) || shown(ch.Removed) || shown(ch.Visibility)
}

// buildShown builds the views of connection conn's shown tickets that owe
// one and no read of which is in flight (a read builds its own as it
// settles): a pump's nudge or its handle shown again. One build serves
// every such ticket of the connection.
func (s *Service) buildShown(conn string) {
	s.subMu.Lock()
	var pumps []*ticketPump
	for k, tp := range s.subs.tickets {
		if k.connection == conn {
			pumps = append(pumps, tp)
		}
	}
	s.subMu.Unlock()
	if err := s.buildTickets(conn, 0, pumps, true); err != nil && !errors.Is(err, ErrStopped) {
		s.logf("trackerapp: build the views of the open tickets of %s: %v", conn, err)
	}
}

// buildTickets builds, from the record under root (zero: the connection's
// root now), the views of those of pumps, open tickets of connection conn,
// that owe one, installs each, and sends each pump's event with the
// outcome its reads left pending (flushTicketLocked). shown leaves out a
// ticket that is hidden or whose read is in flight, which a nudge does;
// otherwise a ticket that owes no view, and no build in flight will send
// its event, sends it now.
//
// Ordering: every tell (tellTicketLocked) is made after the store call
// whose change it reports returned, and a store call returns after its
// transaction committed (store mirrorSweptWrite). The build counts a
// ticket's dirty before its first statement, so a change told before the
// count is in what the build reads (a read begun after a commit returned
// sees it), and one told after moves dirty past the count: such a view is
// not installed and the ticket is built again. A tell that arrives late or
// twice costs a build, never a view older than the record.
//
// A ticket whose body the record does not hold keeps the view it showed
// (not loaded when it showed none) and, built on a nudge, is due a read at
// once (readNowLocked); a read's own build leaves that to its outcome. A
// build that fails is each ticket's failure outside the tracker, and is
// returned.
func (s *Service) buildTickets(conn string, root int64, pumps []*ticketPump, shown bool) error {
	s.subMu.Lock()
	claimed, counted := s.claimBuildsLocked(pumps, shown)
	s.subMu.Unlock()
	return s.buildClaimed(conn, root, claimed, counted, shown)
}

// claimBuildsLocked claims the builds of those of pumps that owe a view
// (buildTickets), each with the dirty its build covers, and sends the
// event of each other one when shown is not set. The caller holds subMu.
func (s *Service) claimBuildsLocked(pumps []*ticketPump, shown bool) (claimed []*ticketPump, counted []uint64) {
	for _, tp := range pumps {
		switch {
		case tp.over || tp.core.DeadLocked() || tp.building > 0:
		case tp.dirty == tp.built || shown && (tp.reading > 0 || !tp.shownLocked()):
			if !shown {
				s.flushTicketLocked(tp, false)
			}
		default:
			tp.building++
			claimed, counted = append(claimed, tp), append(counted, tp.dirty)
		}
	}
	return claimed, counted
}

// buildClaimed builds the views of claimed, whose builds claimBuildsLocked
// claimed with the dirty each covers in counted, and installs each whose
// dirty has not moved since; the others are claimed and built again.
func (s *Service) buildClaimed(conn string, root int64, claimed []*ticketPump, counted []uint64, shown bool) error {
	for len(claimed) > 0 {
		built, err := s.recordViews(conn, root, claimed)
		s.subMu.Lock()
		var again []*ticketPump
		for i, tp := range claimed {
			tp.building--
			switch {
			case tp.over || tp.core.DeadLocked():
			case err != nil:
				if tp.failure == nil {
					tp.tellFailureLocked(internalFailure(millis(s.now())))
				}
				s.flushTicketLocked(tp, false)
			case tp.dirty != counted[i]:
				again = append(again, tp)
			default:
				tp.built = counted[i]
				s.installViewLocked(tp, built[i], shown)
				s.flushTicketLocked(tp, false)
			}
		}
		if err != nil {
			s.subMu.Unlock()
			return err
		}
		claimed, counted = s.claimBuildsLocked(again, shown)
		s.subMu.Unlock()
	}
	return nil
}

// recordView is a view built from the record: the view (nil for a ticket
// whose record holds no body this version reads), the other issues it
// shows (showsOf), and the bookkeeping of the body it was built from.
type recordView struct {
	view  *DetailView
	shows []tracker.IssueID
	body  store.MirrorBodyState
}

// recordViews builds the views of pumps, open tickets of connection conn,
// from their records under root (zero: the connection's root now, none
// when it has no mirror), in the order of pumps. The bodies are read store.MirrorMaxBodies a statement,
// and the cards and names of each such batch's views together
// (detailViews).
func (s *Service) recordViews(conn string, root int64, pumps []*ticketPump) ([]recordView, error) {
	if root == 0 {
		var err error
		root, err = s.readRoot(conn)
		if errors.Is(err, ErrNotMirrored) {
			// A connection with no mirror holds no body of any ticket.
			return make([]recordView, len(pumps)), nil
		}
		if err != nil {
			return nil, err
		}
	} else if err := s.enter(); err != nil {
		return nil, err
	}
	defer s.leave()
	row, err := s.viewRow(conn)
	if err != nil {
		return nil, err
	}
	out := make([]recordView, len(pumps))
	for start := 0; start < len(pumps); start += store.MirrorMaxBodies {
		batch := pumps[start:min(start+store.MirrorMaxBodies, len(pumps))]
		ids := make([]string, len(batch))
		for i, tp := range batch {
			ids[i] = string(tp.key.issue)
		}
		held, err := s.store.ListMirrorBodies(root, ids)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: read the bodies of %d open tickets from the mirror: %w", len(ids), err)
		}
		var docs []ticketDoc
		var at []int
		for _, b := range held {
			doc, ok := bodyOf(b)
			if !ok {
				continue
			}
			i := start + slices.Index(ids, b.IssueID)
			docs, at = append(docs, doc), append(at, i)
			out[i].body = store.MirrorBodyState{UpdatedAt: b.UpdatedAt, ReadAt: b.ReadAt, Wall: b.Wall}
		}
		if len(docs) == 0 {
			continue
		}
		views, shows, err := s.detailViews(root, row, docs)
		if err != nil {
			return nil, err
		}
		for k, i := range at {
			out[i].view, out[i].shows = &views[k], shows[k]
		}
	}
	return out, nil
}

// installViewLocked makes b, a view built from the record (b.view nil: the
// record holds no body), tp's view: a view that differs from
// the one told, in content or in being loaded, is due to be told. A record
// that holds no body leaves the view as it was; built on a nudge (nudged),
// the ticket is then due a read at once. A view whose body is behind its
// card makes the ticket due a read at once, once for each card version, so
// a skew that persists costs one read, not a loop; a ticket whose last
// read failed is left to the retry schedule. The caller holds subMu.
func (s *Service) installViewLocked(tp *ticketPump, b recordView, nudged bool) {
	v := b.view
	if v == nil {
		if nudged {
			s.readNowLocked(tp)
		}
		return
	}
	digest := s.digestOf(*v)
	if tp.view.detail == nil || digest != tp.view.digest {
		tp.changed, tp.pending = true, true
	}
	tp.view = toldView{detail: v, digest: digest, shows: b.shows, body: b.body}
	if v.BodyBehind && v.Card.Updated > tp.owedFor && tp.failure == nil {
		tp.owedFor = v.Card.Updated
		s.readNowLocked(tp)
	}
}

// noteBodyLocked moves the read time of the body tp's view shows to held,
// the record's body as a read of the ticket left it, when held is the same
// version read later: the same body read again changes no view, but its
// read time is what the subscribers are shown and what a check measures
// its age from. The caller holds subMu.
func (tp *ticketPump) noteBodyLocked(held store.MirrorBodyState) {
	if b := &tp.view.body; tp.view.detail != nil && held.UpdatedAt == b.UpdatedAt && held.ReadAt > b.ReadAt {
		b.ReadAt, b.Wall = held.ReadAt, held.Wall
	}
}
