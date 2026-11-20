package trackerapp

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A write's refresh reads again every resource the write may have changed,
// each through the read sync uses (resources.go), batched by read, and
// accounts for each (write_account.go):
//
//   - a board's structure: one structure read;
//   - rows: one rows read per board for every issue the write names on it,
//     whether named by its row or by its card;
//   - a card: through every board the mirror shows the issue on, joined
//     with those boards' rows above; one cards read for the issues the
//     mirror keeps that no rows read answered with a row (on no board, or
//     answered missing by every board it was on, or on boards retired
//     meanwhile); nothing for an issue it does not keep;
//   - a detail: when a ticket is open on it or the mirror holds it, all of
//     them in one full read (verifiedDetails), the open tickets settled
//     with what it found;
//   - worklogs: one read per author and span of named days.
//
// Where each lives in the mirror, and the board lifetimes and root sweep
// its writes name, come from one store read made before any of them
// (store ReadMirrorRefreshTargets). A read that fails leaves its resources
// stale and the others still run; a board retired meanwhile ends that
// board's reads alone; the write's context ending stops the refresh, and
// so does the mirror ending (the connection turned off).

// worklogSpan is the most days one worklogs read stores (the store's
// bound on one ReplaceMirrorWorklogs).
const worklogSpan = 31

// refreshPlan is a refresh's resources by kind, each list sorted without
// repeats.
type refreshPlan struct {
	cards, details []tracker.IssueID
	rows           map[tracker.BoardID][]tracker.IssueID
	structures     []tracker.BoardID
	worklogs       map[tracker.UserID][]tracker.Date
}

func planRefresh(resources []tracker.Resource) refreshPlan {
	p := refreshPlan{rows: map[tracker.BoardID][]tracker.IssueID{}, worklogs: map[tracker.UserID][]tracker.Date{}}
	for _, res := range resources {
		switch res.Kind() {
		case tracker.ResourceCard:
			p.cards = append(p.cards, res.Issue())
		case tracker.ResourceDetail:
			p.details = append(p.details, res.Issue())
		case tracker.ResourceRow:
			p.rows[res.Board()] = append(p.rows[res.Board()], res.Issue())
		case tracker.ResourceStructure:
			p.structures = append(p.structures, res.Board())
		case tracker.ResourceWorklogs:
			author, day := res.Worklogs()
			p.worklogs[author] = append(p.worklogs[author], day)
		}
	}
	p.cards, p.details, p.structures = sortedUnique(p.cards), sortedUnique(p.details), sortedUnique(p.structures)
	for b, ids := range p.rows {
		p.rows[b] = sortedUnique(ids)
	}
	for a, days := range p.worklogs {
		slices.SortFunc(days, tracker.Date.Compare)
		p.worklogs[a] = slices.Compact(days)
	}
	return p
}

func sortedUnique[T cmp.Ordered](s []T) []T {
	slices.Sort(s)
	return slices.Compact(s)
}

// refresh reads resources again, stores them and accounts for each.
func (r *run) refresh(resources []tracker.Resource) *refreshed {
	f := newRefreshed(resources)
	p := planRefresh(resources)
	err := r.refreshMirrored(f, p)
	if err == nil {
		err = r.refreshWorklogs(f, p)
	}
	f.close(err)
	return f
}

// refreshMirrored refreshes p's structures, rows, cards and details. It
// returns why the refresh stops, nil to go on.
func (r *run) refreshMirrored(f *refreshed, p refreshPlan) error {
	if len(p.cards)+len(p.details)+len(p.rows)+len(p.structures) == 0 {
		return nil
	}
	// The open tickets are taken before the targets are read: a ticket
	// opened after the connection was turned off and on again is one of a
	// later mirror, which the targets read then refuses.
	open := r.s.openTickets(r.row.ID, p.details)
	boards := slices.Concat(slices.Collect(maps.Keys(p.rows)), p.structures)
	t, err := r.refreshTargets(slices.Concat(p.cards, p.details), boards)
	if errors.Is(err, store.ErrTrackerCacheGone) {
		f.ended = true
		return err
	}
	if err != nil {
		// Nothing mirrored is read; the worklogs, which need no target, are.
		f.fail(err, f.openWhere(func(res tracker.Resource) bool { return res.Kind() != tracker.ResourceWorklogs })...)
		return nil
	}
	shown, gone := map[tracker.BoardID]bool{}, map[tracker.BoardID]bool{}
	for _, b := range t.Boards {
		shown[tracker.BoardID(b.BoardID)] = true
	}
	for _, b := range p.structures {
		if !shown[b] {
			f.covered(tracker.StructureOf(b))
			continue
		}
		ch, err := r.structure(b)
		f.add(b, ch)
		switch {
		case err == nil:
			f.covered(tracker.StructureOf(b))
		case retired(err):
			gone[b] = true
			f.covered(tracker.StructureOf(b))
		default:
			if stop := r.failedRead(f, err, tracker.StructureOf(b)); stop != nil {
				return stop
			}
		}
	}
	if stop := r.refreshRows(f, p, t, shown, gone); stop != nil {
		return stop
	}
	return r.refreshDetails(f, open, t.Details, p.details)
}

// refreshRows reads p's rows on the boards asked that the mirror shows
// (shown), and p's cards through every shown board holding a row of them
// (t.Rows), except the boards retired since (gone); a card the mirror
// keeps that no rows read answered with a row is read through the cards
// read. It returns why the refresh stops, nil to go on.
func (r *run) refreshRows(f *refreshed, p refreshPlan, t store.MirrorRefreshTargets, shown, gone map[tracker.BoardID]bool) error {
	asked := map[tracker.BoardID][]tracker.IssueID{}
	for b, ids := range p.rows {
		if shown[b] && !gone[b] {
			asked[b] = slices.Clone(ids)
		} else {
			f.covered(rowResources(b, ids)...)
		}
	}
	for _, row := range t.Rows {
		id, b := tracker.IssueID(row.IssueID), tracker.BoardID(row.BoardID)
		if _, named := slices.BinarySearch(p.cards, id); named && !gone[b] {
			asked[b] = append(asked[b], id)
		}
	}
	kept := map[tracker.IssueID]bool{}
	for _, id := range t.Kept {
		kept[tracker.IssueID(id)] = true
	}
	for _, id := range p.cards {
		if !kept[id] {
			f.covered(tracker.CardOf(id))
		}
	}
	// A card owes a read through every board it was asked on: it is
	// refreshed only when each of them answered, and stale when any failed
	// or was never made. A board retired meanwhile owes nothing.
	returned := map[tracker.IssueID]bool{}
	for _, b := range slices.Sorted(maps.Keys(asked)) {
		ids := sortedUnique(asked[b])
		ch, read, err := r.boardRows(b, ids)
		f.add(b, ch)
		for _, id := range read {
			returned[id] = true
		}
		switch {
		case err == nil, retired(err):
			f.covered(rowResources(b, p.rows[b])...)
		default:
			owed := slices.Concat(rowResources(b, p.rows[b]), cardResources(ids))
			if stop := r.failedRead(f, err, owed...); stop != nil {
				return stop
			}
		}
	}
	// Every board a card still open was asked on answered. A card one of
	// them returned is refreshed; a kept card none returned (on no board,
	// answered missing on every board, or on boards retired meanwhile) is
	// read by itself.
	var unread []tracker.IssueID
	for _, id := range p.cards {
		switch {
		case !f.open[tracker.CardOf(id)]:
		case returned[id]:
			f.covered(tracker.CardOf(id))
		default:
			unread = append(unread, id)
		}
	}
	if len(unread) == 0 {
		return nil
	}
	ch, err := r.cards(unread)
	f.ch.add(ch)
	if err != nil {
		return r.failedRead(f, err, cardResources(unread)...)
	}
	f.covered(cardResources(unread)...)
	return nil
}

// refreshDetails reads in full, together, the details of the open
// tickets open and of cached, the issues named whose detail the mirror
// holds; a detail named that is neither is not shown. It returns why the
// refresh stops, nil to go on.
func (r *run) refreshDetails(f *refreshed, open []*ticketPump, cached []string, named []tracker.IssueID) error {
	for _, id := range named {
		if !slices.Contains(cached, string(id)) && !slices.ContainsFunc(open, func(tp *ticketPump) bool { return tp.key.issue == id }) {
			f.covered(tracker.DetailOf(id))
		}
	}
	f.pumps = open
	f.began, f.reads = r.s.ticketReads(open)
	for _, id := range cached {
		if !slices.ContainsFunc(open, func(tp *ticketPump) bool { return string(tp.key.issue) == id }) {
			f.reads = append(f.reads, &ticketRead{issue: tracker.IssueID(id)})
		}
	}
	if len(f.reads) == 0 {
		return nil
	}
	ch, err := r.verifiedDetails(f.reads, true, 0)
	f.ch.add(ch)
	f.details = detailsRead{ch: ch, root: r.root, kind: r.row.Kind}
	if err == nil {
		f.covered(detailResources(f.reads)...)
		return nil
	}
	f.detailsErr = err
	var o Outcome
	if _, _, ferr := r.failed(&o, err); ferr == nil && o.Failure != nil {
		f.details.failure, f.details.retryAt = o.Failure, o.RetryAt
	}
	return r.failedRead(f, err, detailResources(f.reads)...)
}

// refreshWorklogs reads p's worklogs, one read per author and span of
// days. It returns why the refresh stops, nil to go on.
func (r *run) refreshWorklogs(f *refreshed, p refreshPlan) error {
	for _, author := range slices.Sorted(maps.Keys(p.worklogs)) {
		for span := range daySpans(p.worklogs[author]) {
			res := make([]tracker.Resource, len(span))
			for i, d := range span {
				res[i] = tracker.WorklogsOf(author, d)
			}
			if err := r.worklogs(author, span[0], span[len(span)-1]); err != nil {
				if stop := r.failedRead(f, err, res...); stop != nil {
					return stop
				}
				continue
			}
			f.covered(res...)
		}
	}
	return nil
}

func rowResources(b tracker.BoardID, ids []tracker.IssueID) []tracker.Resource {
	out := make([]tracker.Resource, len(ids))
	for i, id := range ids {
		out[i] = tracker.RowOf(b, id)
	}
	return out
}

// add merges ch, stored by a read through board b.
func (f *refreshed) add(b tracker.BoardID, ch Changes) {
	f.ch.add(ch)
	i := slices.IndexFunc(f.own, func(o boardChanges) bool { return o.board == b })
	if i < 0 {
		f.own = append(f.own, boardChanges{board: b})
		i = len(f.own) - 1
	}
	f.own[i].ch.add(ch)
}

// refreshTargets reads where the mirror holds issues and boards (store
// ReadMirrorRefreshTargets), lookupBatch issues a read, and captures the
// lifetimes and the sweep the refresh's writes name.
func (r *run) refreshTargets(issues []tracker.IssueID, boards []tracker.BoardID) (store.MirrorRefreshTargets, error) {
	issues, boards = sortedUnique(issues), sortedUnique(boards)
	var out store.MirrorRefreshTargets
	for i := 0; i == 0 || i < len(issues); i += lookupBatch {
		batch := issues[i:min(i+lookupBatch, len(issues))]
		var asked []string
		if i == 0 {
			asked = idStrings(boards)
		}
		t, err := r.s.store.ReadMirrorRefreshTargets(r.root, idStrings(batch), asked)
		if err != nil {
			return out, fmt.Errorf("trackerapp: read where the mirror of %s holds what a write changed: %w", r.row.ID, err)
		}
		if i == 0 {
			out.Sweep, out.Boards = t.Sweep, t.Boards
		}
		out.Rows = append(out.Rows, t.Rows...)
		out.Kept = append(out.Kept, t.Kept...)
		out.Details = append(out.Details, t.Details...)
	}
	r.state.Sweep = out.Sweep
	r.lifetimes = map[tracker.BoardID]int64{}
	for _, b := range out.Boards {
		r.lifetimes[tracker.BoardID(b.BoardID)] = b.Lifetime
	}
	for _, row := range out.Rows {
		r.lifetimes[tracker.BoardID(row.BoardID)] = row.Lifetime
	}
	return out, nil
}

// openTickets are the open tickets of connection conn on issues.
func (s *Service) openTickets(conn string, issues []tracker.IssueID) []*ticketPump {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	var out []*ticketPump
	for _, id := range issues {
		if tp := s.subs.tickets[ticketKey{connection: conn, issue: id}]; tp != nil && !tp.over && !tp.core.DeadLocked() {
			out = append(out, tp)
		}
	}
	return out
}

// daySpans are days, sorted, in runs of at most worklogSpan days from the
// first to the last: each one worklogs read.
func daySpans(days []tracker.Date) func(yield func([]tracker.Date) bool) {
	return func(yield func([]tracker.Date) bool) {
		for start := 0; start < len(days); {
			end := start + 1
			for end < len(days) && daysBetween(days[start], days[end]) < worklogSpan {
				end++
			}
			if !yield(days[start:end]) {
				return
			}
			start = end
		}
	}
}

// daysBetween is the number of days from a to b.
func daysBetween(a, b tracker.Date) int {
	return int(dayTime(b).Sub(dayTime(a)) / (24 * time.Hour))
}

func dayTime(d tracker.Date) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

func cardResources(ids []tracker.IssueID) []tracker.Resource {
	out := make([]tracker.Resource, len(ids))
	for i, id := range ids {
		out[i] = tracker.CardOf(id)
	}
	return out
}

func detailResources(reads []*ticketRead) []tracker.Resource {
	out := make([]tracker.Resource, len(reads))
	for i, rd := range reads {
		out[i] = tracker.DetailOf(rd.issue)
	}
	return out
}
