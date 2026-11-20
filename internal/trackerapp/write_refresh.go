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
// each through the read sync uses (resources.go), batched by read:
//
//   - a board's structure: one structure read;
//   - rows: one rows read per board for every issue the write names on it,
//     whether named by its row or by its card;
//   - a card: through every board the mirror shows the issue on, joined
//     with those boards' rows above; one cards read for the issues the
//     mirror keeps on no board; nothing for an issue it does not keep;
//   - a detail: when a ticket is open on it or the mirror holds it, all of
//     them in one full read (verifiedDetails), the open tickets settled
//     with what it found;
//   - worklogs: one read per author and span of named days.
//
// Where each lives in the mirror, and the board lifetimes and root sweep
// its writes name, come from one store read made before any of them
// (store ReadMirrorRefreshTargets). A resource the mirror does not show (a
// board no project lists, an issue it does not keep) is not read. A read
// that fails leaves its resources stale and the others still run; a
// mirror that ended (the connection turned off) stops the refresh.

// worklogSpan is the most days one worklogs read stores (the store's
// bound on one ReplaceMirrorWorklogs).
const worklogSpan = 31

// refreshed is what a write's refresh did.
type refreshed struct {
	// ch is everything it stored, and own what its reads through each
	// board stored.
	ch  Changes
	own []boardChanges
	// pumps are the open tickets whose detail it read, began the read's
	// place among those settled into ticket pumps, and reads their reads
	// in the order of pumps, then those of the details only the mirror
	// holds. details and detailsErr are how that read ended.
	pumps      []*ticketPump
	began      uint64
	reads      []*ticketRead
	details    detailsRead
	detailsErr error
	// stale are the resources it could not read again.
	stale []staleRead
	// ended reports a mirror that ended during the refresh.
	ended bool
}

// staleRead is resources a refresh did not read again, and why: the
// tracker's failure (a *callError) or one outside the tracker.
type staleRead struct {
	resources []tracker.Resource
	err       error
}

// lastCallError is the tracker's failure of the refresh's last failed
// read; nil when none failed.
func (f *refreshed) lastCallError() error {
	for _, st := range slices.Backward(f.stale) {
		if c, ok := errors.AsType[*callError](st.err); ok {
			return c.err
		}
	}
	return nil
}

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

// fail records resources left stale by err and reports whether the
// refresh goes on: not once the mirror ended or the run's context did.
func (r *run) fail(f *refreshed, err error, resources []tracker.Resource) bool {
	if endsMirror(err) {
		f.ended = true
		return false
	}
	f.stale = append(f.stale, staleRead{resources: resources, err: err})
	return r.ctx.Err() == nil
}

// refresh reads resources again and stores them.
func (r *run) refresh(resources []tracker.Resource) *refreshed {
	f := &refreshed{}
	p := planRefresh(resources)
	if len(p.cards)+len(p.details)+len(p.rows)+len(p.structures) > 0 && !r.refreshMirrored(f, p) {
		return f
	}
	for _, author := range slices.Sorted(maps.Keys(p.worklogs)) {
		for span := range daySpans(p.worklogs[author]) {
			if err := r.worklogs(author, span[0], span[len(span)-1]); err != nil {
				stale := make([]tracker.Resource, len(span))
				for i, d := range span {
					stale[i] = tracker.WorklogsOf(author, d)
				}
				if !r.fail(f, err, stale) {
					return f
				}
			}
		}
	}
	return f
}

// refreshMirrored refreshes p's structures, rows, cards and details, and
// reports whether the refresh goes on.
func (r *run) refreshMirrored(f *refreshed, p refreshPlan) bool {
	// The open tickets are taken before the targets are read: a ticket
	// opened after the connection was turned off and on again is one of a
	// later mirror, which the targets read then refuses.
	f.pumps = r.s.openTickets(r.row.ID, p.details)
	boards := slices.Concat(slices.Collect(maps.Keys(p.rows)), p.structures)
	t, err := r.refreshTargets(slices.Concat(p.cards, p.details), boards)
	if err != nil {
		var all []tracker.Resource
		for _, id := range p.cards {
			all = append(all, tracker.CardOf(id))
		}
		for b, ids := range p.rows {
			for _, id := range ids {
				all = append(all, tracker.RowOf(b, id))
			}
		}
		for _, b := range p.structures {
			all = append(all, tracker.StructureOf(b))
		}
		for _, id := range p.details {
			all = append(all, tracker.DetailOf(id))
		}
		f.pumps = nil
		return r.fail(f, err, all)
	}
	shown := map[tracker.BoardID]bool{}
	for _, b := range t.Boards {
		shown[tracker.BoardID(b.BoardID)] = true
	}
	for _, b := range p.structures {
		if !shown[b] {
			continue
		}
		ch, err := r.structure(b)
		f.add(b, ch)
		if err != nil && !r.fail(f, err, []tracker.Resource{tracker.StructureOf(b)}) {
			return false
		}
	}
	rows, stale := map[tracker.BoardID][]tracker.IssueID{}, map[tracker.BoardID][]tracker.Resource{}
	for b, ids := range p.rows {
		if shown[b] {
			rows[b] = ids
			for _, id := range ids {
				stale[b] = append(stale[b], tracker.RowOf(b, id))
			}
		}
	}
	onBoard := map[tracker.IssueID]bool{}
	for _, row := range t.Rows {
		id, b := tracker.IssueID(row.IssueID), tracker.BoardID(row.BoardID)
		if _, ok := slices.BinarySearch(p.cards, id); ok {
			onBoard[id] = true
			rows[b] = append(rows[b], id)
			stale[b] = append(stale[b], tracker.CardOf(id))
		}
	}
	for _, b := range slices.Sorted(maps.Keys(rows)) {
		ch, err := r.boardRows(b, sortedUnique(rows[b]))
		f.add(b, ch)
		if err != nil && !r.fail(f, err, stale[b]) {
			return false
		}
	}
	var offBoard []tracker.IssueID
	for _, id := range t.Kept {
		if _, named := slices.BinarySearch(p.cards, tracker.IssueID(id)); named && !onBoard[tracker.IssueID(id)] {
			offBoard = append(offBoard, tracker.IssueID(id))
		}
	}
	if len(offBoard) > 0 {
		ch, err := r.cards(offBoard)
		f.ch.add(ch)
		if err != nil && !r.fail(f, err, cardResources(offBoard)) {
			return false
		}
	}
	return r.refreshDetails(f, t.Details)
}

// refreshDetails reads in full, together, the details of f's open tickets
// and of cached, the issues named whose detail the mirror holds, and
// reports whether the refresh goes on.
func (r *run) refreshDetails(f *refreshed, cached []string) bool {
	f.began, f.reads = r.s.ticketReads(f.pumps)
	for _, id := range cached {
		if !slices.ContainsFunc(f.pumps, func(tp *ticketPump) bool { return string(tp.key.issue) == id }) {
			f.reads = append(f.reads, &ticketRead{issue: tracker.IssueID(id)})
		}
	}
	if len(f.reads) == 0 {
		return true
	}
	ch, err := r.verifiedDetails(f.reads, true, 0)
	f.ch.add(ch)
	f.details = detailsRead{ch: ch, root: r.root, kind: r.row.Kind}
	if err == nil {
		return true
	}
	f.detailsErr = err
	var o Outcome
	if _, _, ferr := r.failed(&o, err); ferr == nil && o.Failure != nil {
		f.details.failure, f.details.retryAt = o.Failure, o.RetryAt
	}
	return r.fail(f, err, detailResources(f.reads))
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
