package trackerapp

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// clockOffsets are this computer's clock against the tracker's.
var clockOffsets = map[string]time.Duration{"same clock": 0, "clock ahead": 3 * time.Minute, "clock behind": -3 * time.Minute}

// answerDates are whether the tracker's answers name its time (a Date
// header): without it no read is final by the tracker's clock, only by
// time elapsed since an earlier answer.
var answerDates = map[string]bool{"dated": true, "no Date": false}

// pollStep is the board pump's interval the tests tick at: Jira's.
const pollStep = 10 * time.Second

// useClock makes wall the service's wall clock and elapsed what its
// steady clock measures: the steady clock reads wall's time now plus how
// far elapsed has moved since, as Service.steady does with the monotonic
// clock, and now is the service's start. A test steps the wall clock by
// changing what wall returns; the steady clock does not move with it.
func (s *Service) useClock(wall, elapsed func() time.Time) {
	w0, e0 := wall(), elapsed()
	s.now = wall
	s.steady = func() time.Time { return w0.Add(elapsed().Sub(e0)) }
	s.steadyFrom = w0.UnixMilli()
}

// restart stops the fixture's service and makes another from the same
// deps over the same store, with the same provider: a restart of the app.
func (f *fixture) restart(t *testing.T) {
	t.Helper()
	old := f.svc
	old.Stop()
	svc, err := New(f.deps)
	if err != nil {
		t.Fatal(err)
	}
	svc.makeProvider = old.makeProvider
	t.Cleanup(svc.Stop)
	f.svc = svc
}

// historyTicks ticks every interval of clock from start for span, running
// each change at its time since start, and returns the time of each tick
// tick reports read history. Only advance and tick move clock.
func historyTicks(t *testing.T, clock *fakeClock, start time.Time, tick func(t *testing.T) bool, changes map[time.Duration]func(), span, every time.Duration) []time.Duration {
	t.Helper()
	at := slices.Sorted(func(yield func(time.Duration) bool) {
		for c := range changes {
			if !yield(c) {
				return
			}
		}
	})
	to := func(d time.Duration) {
		if now := clock.Now().Sub(start); d > now {
			clock.advance(d - now)
		}
	}
	var reads []time.Duration
	for next := every; next <= span; next += every {
		for len(at) > 0 && at[0] < next {
			to(at[0])
			changes[at[0]]()
			at = at[1:]
		}
		to(next)
		if tick(t) {
			reads = append(reads, next)
		}
	}
	return reads
}

// pumpTick runs one tick of scope as its board pump does and fails on
// anything but a clean outcome.
func pumpTick(t *testing.T, svc *Service, scope Scope) {
	t.Helper()
	out, err := svc.syncBoardPlanned(t.Context(), scope, Background, svc.boardDue, nil)
	if err != nil || out.Failure != nil || out.Ended {
		t.Fatalf("pump tick: %+v, %v", out, err)
	}
}

// historyScript is a scripted tracker on its own clock, with a settle
// margin of scriptedSettle, whose history answers are final through that
// margin before they are served when dated and through no time when not,
// and a delta that delivers each issue updated since its cursor.
type historyScript struct {
	m     *mirrorFixture
	clock *fakeClock
	dated bool

	mu      sync.Mutex
	updated map[tracker.IssueID]time.Time
	// gone are issues the tracker answers no history for (deleted or no
	// longer visible).
	gone map[tracker.IssueID]bool
	// recorded are the status changes each update made, with when the
	// tracker's history lists each: lag after the update.
	recorded map[tracker.IssueID][]lateChange
	lag      time.Duration
	// reads are the issues each history read named.
	reads [][]tracker.IssueID
	// slow is how long the next history read takes to be answered: the
	// tracker's clock, and this computer's with it, move that much during
	// the call, and the answer is served at its end.
	slow time.Duration
	// cs counts the store calls of the service run ticks, across a
	// restart; restarted is set until the first tick after one, which
	// also loads the connection (Service.hold).
	cs        *countingStore
	restarted bool
}

// lateChange is a status change the tracker's history lists from listed.
type lateChange struct {
	change tracker.StatusChange
	listed time.Time
}

const scriptedSettle = time.Minute

// newHistoryScript is a history script over issues 10001 and 10002,
// updated an hour before the start, whose first tick has read both
// histories once. The service's wall clock is the tracker's plus offset.
func newHistoryScript(t *testing.T, offset time.Duration, dated bool) *historyScript {
	t.Helper()
	return newHistoryScriptOf(t, offset, dated, ids("10001", "10002"), 1)
}

// newHistoryScriptOf is newHistoryScript over issues, whose first tick
// reads their histories in reads reads.
func newHistoryScriptOf(t *testing.T, offset time.Duration, dated bool, issues []tracker.IssueID, reads int) *historyScript {
	t.Helper()
	start := time.UnixMilli(1_800_000_000_000).UTC()
	h := &historyScript{clock: &fakeClock{now: start}, dated: dated, gone: map[tracker.IssueID]bool{"10002": true},
		updated: map[tracker.IssueID]time.Time{}, recorded: map[tracker.IssueID][]lateChange{}}
	for _, id := range issues {
		h.updated[id] = start.Add(-time.Hour)
	}
	f := newFixture(t)
	h.m = newMirrorFixtureOn(t, f)
	h.useClock(func() time.Time { return h.clock.Now().Add(offset) })
	h.m.p.set(func(p *scripted) {
		p.caps.HistorySettle = scriptedSettle
		p.changedRows = h.changedRows
		p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			return tracker.Listing{IDs: slices.Clone(issues), Complete: true}, nil
		}
		p.history = h.history
	})
	pumpTick(t, h.m.svc, h.m.scope())
	if got := h.takeReads(); len(got) != reads {
		t.Fatalf("the first tick read history %d times, want %d", len(got), reads)
	}
	return h
}

// useClock makes wall the service's wall clock, its steady clock moving
// with the tracker's.
func (h *historyScript) useClock(wall func() time.Time) { h.m.svc.useClock(wall, h.clock.Now) }

// changedRows delivers the rows of the issues updated after the cursor's
// time, oldest first, and a cursor at the newest it delivered.
func (h *historyScript) changedRows(q tracker.DeltaQuery) (tracker.RowPage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var after int64
	if q.From != nil {
		var err error
		if after, err = strconv.ParseInt(strings.TrimPrefix(string(q.From), "t:"), 10, 64); err != nil {
			return tracker.RowPage{}, err
		}
	}
	page := tracker.RowPage{Next: q.From}
	newest := after
	for id, updated := range h.updated {
		if at := updated.UnixMilli(); at > after {
			page.Rows = append(page.Rows, row(string(id), at, string(id)))
			newest = max(newest, at)
		}
	}
	slices.SortFunc(page.Rows, func(a, b tracker.Row) int {
		if c := a.Card.Updated.Compare(b.Card.Updated); c != 0 {
			return c
		}
		return strings.Compare(string(a.Card.ID), string(b.Card.ID))
	})
	if newest > after || q.From == nil {
		page.Next = tracker.Cursor("t:" + strconv.FormatInt(newest, 10))
	}
	return page, nil
}

func (h *historyScript) history(q tracker.HistoryQuery) (tracker.History, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, slices.Clone(q.Issues))
	h.clock.advance(h.slow)
	h.slow = 0
	served := h.clock.Now()
	var out tracker.History
	for _, id := range q.Issues {
		if h.gone[id] {
			continue
		}
		ih := tracker.IssueHistory{Issue: id}
		if h.dated {
			ih.CompleteTo = served.Add(-scriptedSettle)
		}
		for _, c := range h.recorded[id] {
			if !c.listed.After(served) {
				ih.Status = append(ih.Status, c.change)
			}
		}
		out.Issues = append(out.Issues, ih)
	}
	return out, nil
}

// update moves issue's update time to the tracker's clock now, with a
// status change its history lists lag later.
func (h *historyScript) update(issue tracker.IssueID) func() {
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		now := h.clock.Now()
		h.updated[issue] = now
		h.recorded[issue] = append(h.recorded[issue], lateChange{
			change: tracker.StatusChange{Change: tracker.Change{At: now, Author: "acc-user1"}, From: "1", To: "3"},
			listed: now.Add(h.lag),
		})
	}
}

// takeReads returns the issues of each history read since the last call.
func (h *historyScript) takeReads() [][]tracker.IssueID {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.reads
	h.reads = nil
	return out
}

// run ticks the board for span from now and returns, per issue, the
// times since now of the ticks whose history read named it. After each tick the two reads of the
// store's due rule agree (store GetMirrorSync's HistoryDue and
// ListMirrorHistoryDue), and a tick that asks the tracker only for the
// delta and stores no row costs what an idle tick does: the sync state's
// read and record.
func (h *historyScript) run(t *testing.T, changes map[time.Duration]func(), span time.Duration) map[tracker.IssueID][]time.Duration {
	t.Helper()
	cs := newCountingStore(h.m.st)
	h.cs, h.m.svc.store = cs, cs
	defer func() { h.m.svc.store = h.m.st }()
	start := h.clock.Now()
	out := map[tracker.IssueID][]time.Duration{}
	tick := func(t *testing.T) bool {
		t.Helper()
		before, at := len(h.m.p.callsInOrder()), h.clock.Now().Sub(start)
		pumpTick(t, h.m.svc, h.m.scope())
		provider, stored := countsOf(h.m.p.callsInOrder()[before:]), cs.take()
		reads := h.takeReads()
		for _, r := range reads {
			for _, id := range r {
				out[id] = append(out[id], at)
			}
		}
		restarted := h.restarted
		h.restarted = false
		if !restarted && describeCounts(provider) == "ChangedRows:1" && stored["WriteMirrorRowPage"] == 0 &&
			describeCounts(stored) != "GetMirrorSync:1 RecordMirrorSyncAttempt:1" {
			t.Errorf("tick at %v with nothing to read cost store %s; want an idle tick's", at, describeCounts(stored))
		}
		h.dueAgree(t, at)
		return len(reads) > 0
	}
	historyTicks(t, h.clock, start, tick, changes, span, pollStep)
	return out
}

// dueAgree fails unless the sync state's HistoryDue and the due listing
// agree on the steady clock now.
func (h *historyScript) dueAgree(t *testing.T, at time.Duration) {
	t.Helper()
	now := h.m.svc.steady().UnixMilli()
	st, _, err := h.m.st.GetMirrorSync(h.m.root(), string(testBoard), now)
	if err != nil {
		t.Fatal(err)
	}
	due, _, err := h.m.st.ListMirrorHistoryDue(h.m.root(), now)
	if err != nil {
		t.Fatal(err)
	}
	if st.HistoryDue != (len(due) > 0) {
		t.Errorf("after the tick at %v the sync state says history due %v, the listing %d issues", at, st.HistoryDue, len(due))
	}
}

func (h *historyScript) want(t *testing.T, got map[tracker.IssueID][]time.Duration, want map[tracker.IssueID][]time.Duration) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("history reads by issue, at ticks since the start:\n got %v\nwant %v", got, want)
	}
}

// sec is n seconds.
func sec(n float64) time.Duration { return time.Duration(n * float64(time.Second)) }

// eachHistoryScript runs test for each answer dating and clock offset.
func eachHistoryScript(t *testing.T, test func(t *testing.T, h *historyScript)) {
	for dating, dated := range answerDates {
		for name, offset := range clockOffsets {
			t.Run(dating+" "+name, func(t *testing.T) {
				t.Parallel()
				test(t, newHistoryScript(t, offset, dated))
			})
		}
	}
}
