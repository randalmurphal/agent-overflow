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
//   - refreshed: a read covering it was made and stored;
//   - not shown: the mirror does not show it, so there is nothing to read
//     (a board no project lists or one retired during the refresh, an
//     issue the mirror does not keep, a detail it neither holds nor has
//     open);
//   - stale: the read covering it failed, or the refresh stopped before
//     making it (the write's context ended), with why.
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
	// ch is everything it stored, and own what its reads through each
	// board stored.
	ch  Changes
	own []boardChanges
	// pumps are the open tickets whose detail it read, began the read's
	// place among those settled into ticket pumps, and reads their reads
	// in the order of pumps, then those of the details only the mirror
	// holds; all set together when the details read begins, and nil when
	// the refresh stopped before it. details and detailsErr are how that
	// read ended.
	pumps      []*ticketPump
	began      uint64
	reads      []*ticketRead
	details    detailsRead
	detailsErr error
	// open are the resources named that the refresh has not accounted for
	// yet, and stale those it accounted for as stale.
	open  map[tracker.Resource]bool
	stale []staleRead
	// ended reports a mirror that ended during the refresh.
	ended bool
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
// read; nil when none failed.
func (f *refreshed) lastCallError() error {
	for _, st := range slices.Backward(f.stale) {
		if c, ok := errors.AsType[*callError](st.err); ok {
			return c.err
		}
	}
	return nil
}

// failedRead accounts for res, which a read that failed with err covered,
// and returns why the refresh stops: the mirror ended (f.ended), or the
// write's context did. It returns nil when the refresh goes on. A board
// retired during its read is the caller's to sort first (retired).
func (r *run) failedRead(f *refreshed, err error, res ...tracker.Resource) error {
	if errors.Is(err, store.ErrTrackerCacheGone) {
		f.ended = true
		return err
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
