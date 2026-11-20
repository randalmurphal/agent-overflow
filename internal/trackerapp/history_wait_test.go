package trackerapp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// An issue that changed has its history read at once and, when that read
// is not final for the change, once more a settle margin after its answer
// arrived: two reads per change whatever the poll interval, whether the
// tracker names its time on the answer and whatever this computer's clock
// says against the tracker's or does while it waits. The ticks are the
// board pump's (syncBoardPlanned with boardDue): the tick that reads the
// sync state learns the wait is over, with no timer of its own.

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

// A change at 5 s that the tracker's history lists only a whole margin
// later (65 s) is read on the next tick after the delta stored it (20 s),
// which is not final for it, and once more a margin after that answer
// (80 s), which is: by the tracker's clock when the answer is dated, by
// the elapsed margin when not. That read holds the change; nothing is read
// after it. An issue the tracker answers no history for is read once per
// change: the empty history recorded is final for its update.
func TestHistoryIsReadTwicePerChange(t *testing.T) {
	t.Parallel()
	eachHistoryScript(t, func(t *testing.T, h *historyScript) {
		h.lag = scriptedSettle
		var held bool
		h.m.p.set(func(p *scripted) {
			p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
				out, err := h.history(q)
				for _, ih := range out.Issues {
					held = held || (ih.Issue == "10001" && len(ih.Status) == 1)
				}
				return out, err
			}
		})
		got := h.run(t, map[time.Duration]func(){sec(5): func() { h.update("10001")(); h.update("10002")() }}, 3*time.Minute)
		h.want(t, got, map[tracker.IssueID][]time.Duration{"10001": {sec(20), sec(80)}, "10002": {sec(20)}})
		if !held {
			t.Error("no read held the change the tracker listed late")
		}
	})
}

// A change that arrives during the wait is read at once again (the tick
// after the delta stored it, 50 s), then once a margin after that answer
// (110 s). The first change's end of wait (80 s) reads nothing.
func TestHistoryChangedDuringTheWaitIsReadAtOnce(t *testing.T) {
	t.Parallel()
	eachHistoryScript(t, func(t *testing.T, h *historyScript) {
		got := h.run(t, map[time.Duration]func(){sec(5): h.update("10001"), sec(35): h.update("10001")}, 3*time.Minute)
		h.want(t, got, map[tracker.IssueID][]time.Duration{"10001": {sec(20), sec(50), sec(110)}})
	})
}

// A change stored by the tick that ends the first change's wait (75 s,
// stored at 80 s) is a version no read was made for: the read that tick
// makes is not final for it, however long after the earlier answer it
// began, and the change the tracker lists a margin late (135 s) is read
// a margin after that answer (140 s).
func TestHistoryChangedAsTheWaitEndsIsReadAgain(t *testing.T) {
	t.Parallel()
	eachHistoryScript(t, func(t *testing.T, h *historyScript) {
		h.lag = scriptedSettle
		got := h.run(t, map[time.Duration]func(){sec(5): h.update("10001"), sec(75): h.update("10001")}, 4*time.Minute)
		h.want(t, got, map[tracker.IssueID][]time.Duration{"10001": {sec(20), sec(80), sec(140)}})
	})
}

// An update half a second after the answer to a read, in the same second
// of the tracker's clock, is a later version than the one read: it is
// read on the first tick after the delta stored it (40 s), not at the end
// of the first change's wait (80 s), and once more a margin after (100 s).
func TestHistoryUpdatedWithinTheAnswersSecond(t *testing.T) {
	t.Parallel()
	eachHistoryScript(t, func(t *testing.T, h *historyScript) {
		got := h.run(t, map[time.Duration]func(){sec(5): h.update("10001"), sec(20.5): h.update("10001")}, 3*time.Minute)
		h.want(t, got, map[tracker.IssueID][]time.Duration{"10001": {sec(20), sec(40), sec(100)}})
	})
}

// The wait counts from when the answer arrived, not from when its read
// began. The first read, begun at 20 s, is answered 9 s later (29 s); the
// next read is on the first tick a margin after that, 90 s, and is final.
// Counted from the read's start, the wait would end at 80 s.
func TestHistoryWaitCountsFromTheAnswer(t *testing.T) {
	t.Parallel()
	eachHistoryScript(t, func(t *testing.T, h *historyScript) {
		got := h.run(t, map[time.Duration]func(){sec(5): h.update("10001"), sec(19): func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.slow = 9 * time.Second
		}}, 3*time.Minute)
		h.want(t, got, map[tracker.IssueID][]time.Duration{"10001": {sec(20), sec(90)}})
	})
}

// A step of this computer's wall clock during the wait, an hour forward
// or back, moves no wait: the change is read at exactly the two ticks it
// is without the step.
func TestHistoryWallClockStepMovesNoWait(t *testing.T) {
	t.Parallel()
	for _, step := range []time.Duration{time.Hour, -time.Hour} {
		for dating, dated := range answerDates {
			t.Run(fmt.Sprintf("%s %v", dating, step), func(t *testing.T) {
				t.Parallel()
				h := newHistoryScript(t, 0, dated)
				var mu sync.Mutex
				var offset time.Duration
				h.useClock(func() time.Time {
					mu.Lock()
					defer mu.Unlock()
					return h.clock.Now().Add(offset)
				})
				got := h.run(t, map[time.Duration]func(){sec(5): h.update("10001"), sec(25): func() {
					mu.Lock()
					defer mu.Unlock()
					offset = step
				}}, 3*time.Minute)
				h.want(t, got, map[tracker.IssueID][]time.Duration{"10001": {sec(20), sec(80)}})
			})
		}
	}
}

// A restart during the wait (35 s, after the read at 20 s) re-anchors the
// steady clock on the new process's wall clock, and an answer stored
// before it proves no elapsed time. The bound: the next read is never more
// than a margin after the restart, and the restart costs at most one more
// read, never a read taken as final early.
//   - No step: the wait ends when it would have (80 s). A dated read is
//     final then; an undated one is not (its wait began in the earlier
//     process), so one more read a margin later (140 s).
//   - Wall clock stepped an hour forward or back across the restart: due
//     at once (40 s), since the stored wait looks over or its answer looks
//     to come after now. That read is not final and waits a whole margin
//     (100 s).
func TestHistoryRestartDuringTheWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		step  time.Duration
		dated bool
		want  []time.Duration
	}{
		{0, true, []time.Duration{sec(20), sec(80)}},
		{0, false, []time.Duration{sec(20), sec(80), sec(140)}},
		{time.Hour, true, []time.Duration{sec(20), sec(40), sec(100)}},
		{time.Hour, false, []time.Duration{sec(20), sec(40), sec(100)}},
		{-time.Hour, true, []time.Duration{sec(20), sec(40), sec(100)}},
		{-time.Hour, false, []time.Duration{sec(20), sec(40), sec(100)}},
	} {
		t.Run(fmt.Sprintf("dated %v step %v", tc.dated, tc.step), func(t *testing.T) {
			t.Parallel()
			h := newHistoryScript(t, 0, tc.dated)
			got := h.run(t, map[time.Duration]func(){sec(5): h.update("10001"), sec(35): func() {
				h.m.restart(t)
				h.useClock(func() time.Time { return h.clock.Now().Add(tc.step) })
				h.m.svc.store, h.restarted = h.cs, true
			}}, 3*time.Minute)
			h.want(t, got, map[tracker.IssueID][]time.Duration{"10001": tc.want})
		})
	}
}

// The first sync of issues updated long ago reads each history once, in
// as many reads as batches, and every read is final at once: no issue is
// left waiting and no tick after reads history. 120 issues are three
// reads of 50.
func TestHistoryFirstSyncOfOldIssuesIsFinal(t *testing.T) {
	t.Parallel()
	issues := make([]tracker.IssueID, 120)
	for i := range issues {
		issues[i] = tracker.IssueID(strconv.Itoa(20001 + i))
	}
	h := newHistoryScriptOf(t, 0, true, issues, 3)
	due, _, err := h.m.st.ListMirrorHistoryDue(h.m.root(), h.m.svc.steady().Add(24*time.Hour).UnixMilli())
	if err != nil || len(due) != 0 {
		t.Fatalf("issues waiting a day after the first sync: %+v, %v", due, err)
	}
	h.want(t, h.run(t, nil, 3*time.Minute), map[tracker.IssueID][]time.Duration{})
}

// The fakes, through the real providers and clients, with and without a
// Date on their answers: an edit half a poll before a tick is read on the
// tick after the delta stored it and on the first tick at least the
// provider's margin after that read's answer, Jira's one minute and
// Linear's two, at a 10 s poll and at Linear's own 45 s. Undated, the
// second read is final by the elapsed margin.
func TestFakeHistoryIsReadTwicePerChange(t *testing.T) {
	t.Parallel()
	jira := func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{jiraSeed(at)}}
	}
	linear := func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
	}
	for _, kind := range []struct {
		name   string
		margin time.Duration
		every  time.Duration
		route  string
		issue  tracker.IssueID
		seed   func(time.Time) trackerfake.Fixture
		sites  []string
		setup  func(s *fakeScenario) (Scope, func())
	}{
		{"jira", time.Minute, pollStep, "POST rest/api/3/changelog/bulkfetch", "10102", jira, []string{"example.atlassian.net"}, jiraHistoryScope},
		{"linear", 2 * time.Minute, pollStep, "IssueHistories", tracker.IssueID(lid(61)), linear, nil, linearHistoryScope},
		{"linear at its poll", 2 * time.Minute, LinearPollEvery, "IssueHistories", tracker.IssueID(lid(61)), linear, nil, linearHistoryScope},
	} {
		for dating, dated := range answerDates {
			for name, offset := range clockOffsets {
				t.Run(kind.name+" "+dating+" "+name, func(t *testing.T) {
					t.Parallel()
					s := newFakeScenario(t, kind.seed, kind.sites...)
					if !dated {
						s.withoutDates(t)
					}
					s.svc.useClock(func() time.Time { return s.clock.Now().Add(offset) }, s.clock.Now)
					scope, edit := kind.setup(s)
					p := s.provider(scope.Connection)
					if got := p.Capabilities().HistorySettle; got != kind.margin {
						t.Fatalf("the provider's settle margin is %v, want %v", got, kind.margin)
					}
					// The answers name the tracker's time only when dated.
					h, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{kind.issue}, Boards: []tracker.BoardID{scope.Board}})
					if err != nil || len(h.Issues) != 1 || h.Issues[0].CompleteTo.IsZero() == dated {
						t.Fatalf("history %+v, %v; want a final time only when dated", h, err)
					}
					pumpTick(t, s.svc, scope)
					// An hour on, any seeded issue the first tick read within a
					// margin of its update, or without a Date, has been read
					// final, so only the edit is due in the ticks measured.
					s.clock.advance(time.Hour)
					pumpTick(t, s.svc, scope)
					start := s.clock.Now()
					tick := func(t *testing.T) bool {
						t.Helper()
						return s.requestsOf(t, func() { pumpTick(t, s.svc, scope) })[kind.route] > 0
					}
					first := 2 * kind.every
					second := (first + kind.margin + kind.every - 1) / kind.every * kind.every
					reads := historyTicks(t, s.clock, start, tick, map[time.Duration]func(){kind.every / 2: edit}, second+2*kind.margin, kind.every)
					if want := []time.Duration{first, second}; !slices.Equal(reads, want) {
						t.Errorf("history reads at %v since %v; want %v", reads, start, want)
					}
				})
			}
		}
	}
}

// withoutDates puts a proxy that strips the Date header from every answer
// in front of the fakes, so the clients learn no tracker time.
func (s *fakeScenario) withoutDates(t *testing.T) {
	t.Helper()
	origin := s.origin
	proxies := map[string]string{}
	s.origin = func(kind, site string) (string, error) {
		target, err := origin(kind, site)
		if err != nil {
			return "", err
		}
		if p, ok := proxies[target]; ok {
			return p, nil
		}
		u, err := url.Parse(target)
		if err != nil {
			return "", err
		}
		rp := httputil.NewSingleHostReverseProxy(u)
		rp.ModifyResponse = func(r *http.Response) error {
			r.Header.Del("Date")
			return nil
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Date"] = nil // keeps net/http from adding the wall clock's
			rp.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		proxies[target] = srv.URL
		return srv.URL, nil
	}
}

func jiraHistoryScope(s *fakeScenario) (Scope, func()) {
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	scope := Scope{Connection: conn.ID, Board: "1"}
	s.listBoards(conn.ID, scope.Board)
	return scope, func() {
		applied(s.t, "edit", s.provider(conn.ID).Edit(s.t.Context(), tracker.EditInput{Issue: "10102",
			Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Second task, renamed")}}}))
	}
}

func linearHistoryScope(s *fakeScenario) (Scope, func()) {
	conn := s.addLinear()
	s.writeToken(conn, "lin_api_fake00001")
	scope := Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
	s.listBoards(conn.ID, scope.Board)
	return scope, func() {
		applied(s.t, "edit", s.provider(conn.ID).Edit(s.t.Context(), tracker.EditInput{Issue: tracker.IssueID(lid(61)),
			Fields: []tracker.FieldEdit{{Field: tracker.FieldTitle, Value: tracker.TypedValues("Add search, renamed")}}}))
	}
}

// An issue whose read was final and that gains a board is read once, at
// once, for the estimate changes of the new board, and that read is final
// although no answer names the tracker's time: it is for the version a
// final read was stored for, whose changes had settled. No tick after
// reads it again.
func TestHistoryOfABoardGainedIsReadOnceFinal(t *testing.T) {
	t.Parallel()
	for dating, dated := range answerDates {
		t.Run(dating, func(t *testing.T) {
			t.Parallel()
			h := newHistoryScript(t, 0, dated)
			// Undated, the first sync is final only by a second read, at
			// 60 s; the board is gained right after it, well inside a
			// margin of that answer.
			h.run(t, nil, scriptedSettle)
			second := Scope{Connection: h.m.conn.ID, Board: "2"}
			must[Outcome](t)(h.m.svc.tickDelta(t.Context(), second, Background))
			must[Outcome](t)(h.m.svc.tickHistory(t.Context(), second, Background))
			if reads := h.takeReads(); len(reads) != 1 || !slices.Equal(reads[0], ids("10001", "10002")) {
				t.Fatalf("history reads after the issues gained board 2: %v; want one naming both", reads)
			}
			h.want(t, h.run(t, nil, 3*time.Minute), map[tracker.IssueID][]time.Duration{})
		})
	}
}
