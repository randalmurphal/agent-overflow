package trackerapp

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A write's refresh keeps an account of every resource the write named.
// Each resource leaves the account once, as one of:
//
//   - refreshed: every read it owes was made and stored, or found it not
//     shown; a card owes a rows read through each board the mirror shows
//     it on, or the cards read when no board returns it, and every other
//     resource owes one read;
//   - not shown: the mirror does not show it, so there is nothing to read
//     (a board no project lists or one retired during the refresh, an
//     issue the mirror does not keep, a detail it neither holds nor has
//     open);
//   - stale: a read it owes failed, or the refresh stopped before making
//     it (the write's context ended), with the first such failure; a
//     later failed read it owed reports nothing more, and its tracker
//     failure still settles the connection's problem.
//
// A refresh whose connection's mirror ended (the root detached) leaves
// the rest in the account: the write answers Ended, which covers every
// resource, since the mirror holds none of them any more. Whatever a
// refresh stored before it stopped, for any reason, is published.

// errUnaccounted is a resource a refresh ended without accounting for: a
// defect of the refresh, told as a failure outside the tracker.
var errUnaccounted = errors.New("trackerapp: the refresh after a write made no read of it")

// refreshed is what a write's refresh did.
type refreshed struct {
	// pumps are the open tickets whose detail it reads, began the read's
	// place among those settled into ticket pumps, and reads their reads
	// in the order of pumps, then those of the details only the mirror
	// holds: the pumps' reads begin with the refresh of the mirror, the
	// others' with the details read. details and detailsErr are how that
	// read ended, detailsErr why the refresh stopped before it.
	pumps      []*ticketPump
	began      uint64
	reads      []*ticketRead
	details    detailsRead
	detailsErr error
	// open are the resources named that the refresh has not accounted for
	// yet, and stale those it accounted for as stale.
	open  map[tracker.Resource]bool
	stale []staleRead
	// callErr is the tracker's failure of the last read that failed, kept
	// whether or not the resources it owed were already reported stale.
	callErr error
	// ended reports a mirror that ended during the refresh.
	ended bool
	// notOnBoard is the board a create was made from when the tracker
	// said it does not hold the new issue (write_place.go).
	notOnBoard tracker.BoardID
}

// staleRead is resources a refresh did not read again, and why: the
// tracker's failure (a *callError), the write's context ending, or a
// failure outside the tracker.
type staleRead struct {
	resources []tracker.Resource
	err       error
}

func newRefreshed(resources []tracker.Resource) *refreshed {
	f := &refreshed{open: make(map[tracker.Resource]bool, len(resources))}
	for _, res := range resources {
		f.open[res] = true
	}
	return f
}

// covered accounts for res as refreshed or not shown.
func (f *refreshed) covered(res ...tracker.Resource) {
	for _, r := range res {
		delete(f.open, r)
	}
}

// fail accounts for those of res still open as stale with err.
func (f *refreshed) fail(err error, res ...tracker.Resource) {
	var left []tracker.Resource
	for _, r := range res {
		if f.open[r] {
			left = append(left, r)
			delete(f.open, r)
		}
	}
	if len(left) > 0 {
		f.stale = append(f.stale, staleRead{resources: left, err: err})
	}
}

// openWhere are the open resources keep holds of, in a fixed order.
func (f *refreshed) openWhere(keep func(tracker.Resource) bool) []tracker.Resource {
	var out []tracker.Resource
	for r := range maps.Keys(f.open) {
		if keep(r) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, compareResources)
	return out
}

// close accounts for every resource still open as stale with err, the
// reason the refresh stopped, or errUnaccounted when it did not stop.
func (f *refreshed) close(err error) {
	if f.ended || len(f.open) == 0 {
		return
	}
	f.fail(cmp.Or(err, errUnaccounted), f.openWhere(func(tracker.Resource) bool { return true })...)
}

func compareResources(a, b tracker.Resource) int {
	aAuthor, aDay := a.Worklogs()
	bAuthor, bDay := b.Worklogs()
	return cmp.Or(cmp.Compare(a.Kind(), b.Kind()), cmp.Compare(a.Board(), b.Board()), cmp.Compare(a.Issue(), b.Issue()),
		cmp.Compare(aAuthor, bAuthor), aDay.Compare(bDay))
}

// lastCallError is the tracker's failure of the refresh's last failed
// read, which settles the connection's problem; nil when none failed.
func (f *refreshed) lastCallError() error { return f.callErr }

// failedRead accounts for res, the resources that owed the read that
// failed with err, and returns why the refresh stops: the mirror ended
// (f.ended), or the write's context did. It returns nil when the refresh
// goes on. A resource already reported stale is not reported again; the
// tracker's failure is kept for the problem either way. A board retired
// during its read is the caller's to sort first (retired).
func (r *run) failedRead(f *refreshed, err error, res ...tracker.Resource) error {
	if errors.Is(err, store.ErrTrackerCacheGone) {
		f.ended = true
		return err
	}
	if c, ok := errors.AsType[*callError](err); ok {
		f.callErr = c.err
	}
	f.fail(err, res...)
	if r.ctx.Err() != nil {
		return context.Cause(r.ctx)
	}
	return nil
}

// retired reports a read through a board that failed because the board
// is no longer listed or was retired since the refresh read its targets:
// that board's resources are not shown, and the refresh goes on. It is
// not the tick's rule (endsMirror), under which a board ends its own
// scope's tick.
func retired(err error) bool { return errors.Is(err, store.ErrTrackerCacheUnlisted) }
