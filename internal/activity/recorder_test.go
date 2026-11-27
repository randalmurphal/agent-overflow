package activity

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// fakeStore holds the spans written, as the store would: an insert takes
// the next ID while jira is on.
type fakeStore struct {
	mu    sync.Mutex
	jira  bool
	fail  error
	spans map[int64]store.ActivitySpan
	days  []store.ThreadDay
	saves [][]store.ActivitySpan
	// during runs inside each save, without the lock.
	during func()
	prunes [][2]any
	nextID int64
}

func newFakeStore() *fakeStore { return &fakeStore{jira: true, spans: map[int64]store.ActivitySpan{}} }

func (f *fakeStore) SaveActivitySpans(spans []store.ActivitySpan) ([]int64, error) {
	if f.during != nil {
		f.during()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, slices.Clone(spans))
	if f.fail != nil {
		return nil, f.fail
	}
	ids := make([]int64, len(spans))
	for i, sp := range spans {
		switch {
		case sp.ID != 0:
			ids[i] = sp.ID
		case f.jira:
			f.nextID++
			ids[i] = f.nextID
		default:
			continue
		}
		sp.ID = ids[i]
		f.spans[sp.ID] = sp
	}
	return ids, nil
}

func (f *fakeStore) PruneActivity(beforeMs int64, beforeDay string) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunes = append(f.prunes, [2]any{beforeMs, beforeDay})
	return 0, 0, nil
}

func (f *fakeStore) ActivitySpans(fromMs, toMs int64) ([]store.ActivitySpan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ActivitySpan
	for _, sp := range f.spans {
		if sp.StartMs < toMs && sp.EndMs > fromMs {
			out = append(out, sp)
		}
	}
	return out, nil
}

func (f *fakeStore) ThreadDays(from, to string) ([]store.ThreadDay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ThreadDay
	for _, d := range f.days {
		if d.Day >= from && d.Day <= to {
			out = append(out, d)
		}
	}
	return out, nil
}

// stored is what the store holds, by start.
func (f *fakeStore) stored() []store.ActivitySpan {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Collect(func(yield func(store.ActivitySpan) bool) {
		for _, sp := range f.spans {
			if !yield(sp) {
				return
			}
		}
	})
	slices.SortFunc(out, func(a, b store.ActivitySpan) int { return int(a.StartMs - b.StartMs) })
	return out
}

// clock is a test's time, moved by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// nine is 09:00 on Tue Nov 24, local time.
var nine = time.Date(2026, 11, 24, 9, 0, 0, 0, time.Local)

func recorder(t *testing.T) (*Recorder, *fakeStore, *clock, *[]Status) {
	t.Helper()
	f := newFakeStore()
	c := &clock{t: nine}
	var changes []Status
	r := New(f, Config{Now: c.now, Changed: func(s Status) { changes = append(changes, s) }})
	t.Cleanup(r.Close)
	return r, f, c, &changes
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// Reports on one thread within the window are one span, which ends at
// the last report plus the window; a report on another thread ends it at
// once. Nothing is written until a flush, and a flush writes a span once,
// then only a moved end.
func TestReportsAreSpansWrittenAtAFlush(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Report("c1", "t1", false)
	c.add(time.Minute)
	r.Report("c1", "t1", false)
	c.add(2 * time.Minute)
	r.Report("c1", "t1", false) // 09:03
	if len(f.saves) != 0 {
		t.Fatalf("a report wrote %v", f.saves)
	}
	c.add(time.Minute)
	r.Flush() // 09:04
	want := []store.ActivitySpan{{ID: 1, ThreadID: "t1", StartMs: ms(nine), EndMs: ms(nine.Add(4 * time.Minute))}}
	if got := f.stored(); !slices.Equal(got, want) {
		t.Fatalf("stored %+v\nwant %+v", got, want)
	}
	c.add(time.Minute)
	r.Report("c1", "t2", true) // 09:05: t1 ends here
	c.add(time.Minute)
	r.Flush() // 09:06
	want = []store.ActivitySpan{
		{ID: 1, ThreadID: "t1", StartMs: ms(nine), EndMs: ms(nine.Add(5 * time.Minute))},
		{ID: 2, ThreadID: "t2", Phone: true, StartMs: ms(nine.Add(5 * time.Minute)), EndMs: ms(nine.Add(6 * time.Minute))},
	}
	if got := f.stored(); !slices.Equal(got, want) {
		t.Fatalf("stored %+v\nwant %+v", got, want)
	}
	// Idle past the window: the span ends at its last report plus the
	// window, written once more, and then nothing is written.
	c.add(10 * time.Minute)
	r.Flush()
	r.Flush()
	if got := f.stored()[1].EndMs; got != ms(nine.Add(8*time.Minute)) {
		t.Fatalf("the idle span ends at %v; want 09:08", time.UnixMilli(got))
	}
	if n := len(f.saves[len(f.saves)-1]); n != 0 {
		t.Fatalf("a flush with nothing changed wrote %d spans", n)
	}
}

// A report after the window opens a new span, and a span that would pass
// a day is closed and the next opened, so no span is longer than the
// store takes.
func TestASpanEndsAtAGapAndAtADay(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Report("c1", "t1", false)
	c.add(4 * time.Minute)
	r.Report("c1", "t1", false)
	for range 24 * 60 {
		c.add(time.Minute)
		r.Report("c1", "t1", false)
	}
	r.Flush()
	got := f.stored()
	if len(got) != 3 || got[0].EndMs != ms(nine.Add(3*time.Minute)) || got[1].StartMs != ms(nine.Add(4*time.Minute)) {
		t.Fatalf("stored %+v; want the first span ended by the gap", got)
	}
	for _, sp := range got {
		if sp.EndMs-sp.StartMs > store.MaxActivitySpanMs {
			t.Fatalf("a span of %v", time.Duration(sp.EndMs-sp.StartMs)*time.Millisecond)
		}
	}
}

// A connection's end ends its span; Close ends every span and writes
// them, and a report after Close records nothing.
func TestTheEndOfAConnectionAndOfTheRecorder(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Report("c1", "t1", false)
	r.Report("c2", "t2", false)
	c.add(time.Minute)
	r.End("c1")
	c.add(time.Minute)
	r.Close()
	r.Report("c3", "t3", false)
	r.Flush()
	want := []store.ActivitySpan{
		{ID: 1, ThreadID: "t1", StartMs: ms(nine), EndMs: ms(nine.Add(time.Minute))},
		{ID: 2, ThreadID: "t2", StartMs: ms(nine), EndMs: ms(nine.Add(2 * time.Minute))},
	}
	got := f.stored()
	slices.SortFunc(got, func(a, b store.ActivitySpan) int { return int(a.ID - b.ID) })
	if len(got) != 2 || got[0].EndMs != want[0].EndMs || got[1].EndMs != want[1].EndMs {
		t.Fatalf("stored %+v\nwant %+v", got, want)
	}
}

// A flush that fails keeps what it could not write and says since when
// once; the next flush that writes writes it and says so once. While no
// Jira connection is on, nothing is kept.
func TestAFailedFlushIsKeptAndSaid(t *testing.T) {
	t.Parallel()
	r, f, c, changes := recorder(t)
	r.Report("c1", "t1", false)
	c.add(time.Minute)
	r.Report("c1", "t2", false)
	f.fail = errors.New("disk I/O error")
	c.add(time.Minute)
	r.Flush()
	r.Flush()
	if st := r.Status(); st.Since != ms(nine) || st.Error != "disk I/O error" {
		t.Fatalf("the status is %+v; want since 09:00 with the error", st)
	}
	if len(*changes) != 1 {
		t.Fatalf("said %d changes; want the failure once", len(*changes))
	}
	f.fail = nil
	r.Flush()
	if st := r.Status(); st != (Status{}) || len(*changes) != 2 || (*changes)[1] != (Status{}) {
		t.Fatalf("the status is %+v after %v; want recording again, said once", st, *changes)
	}
	if got := f.stored(); len(got) != 2 {
		t.Fatalf("stored %+v; want both spans", got)
	}

	off, fo, co, _ := recorder(t)
	fo.jira = false
	off.Report("c1", "t1", false)
	co.add(time.Minute)
	off.Report("c1", "t2", false)
	off.Flush()
	if len(fo.stored()) != 0 || len(off.closed) != 0 {
		t.Fatalf("with no Jira connection stored %+v and kept %d", fo.stored(), len(off.closed))
	}
}

// A failing recorder keeps at most maxUnwritten closed spans, and still
// says since the earliest.
func TestAFailingRecorderIsBounded(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	f.fail = errors.New("disk full")
	for i := range maxUnwritten + 10 {
		r.Report("c1", []string{"t1", "t2"}[i%2], false)
		c.add(time.Second)
	}
	r.Flush()
	if len(r.closed) != maxUnwritten {
		t.Fatalf("kept %d closed spans; want %d", len(r.closed), maxUnwritten)
	}
	if st := r.Status(); st.Since == 0 {
		t.Fatalf("the status is %+v", st)
	}
}

// The prune runs once a day, deleting what is older than Keep.
func TestThePruneRunsOnceADay(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Flush()
	r.Flush()
	c.add(24 * time.Hour)
	r.Flush()
	cut := nine.Add(-Keep)
	if len(f.prunes) != 2 || f.prunes[0] != [2]any{ms(cut), cut.Format(time.DateOnly)} {
		t.Fatalf("pruned %v; want once a day before %s", f.prunes, cut)
	}
}

// Days answers each day's time with input once, however many screens
// had input at that instant, on the screen the person turned to last; a
// span that passes midnight counts on both days; what is not written yet
// counts, and a span written before counts at the end it has now. The
// threads that started a turn are named on their day.
func TestDaysCountEachInstantOnce(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	// c1 on t1 from 09:00; c2 (the phone) on t2 from 09:02 to 09:05: the
	// overlap is t2's.
	r.Report("c1", "t1", false)
	c.add(2 * time.Minute)
	r.Report("c1", "t1", false)
	r.Report("c2", "t2", true)
	c.add(3 * time.Minute)
	r.End("c2")
	r.Report("c1", "t1", false) // 09:05
	r.Flush()
	c.add(time.Minute)
	r.Report("c1", "t1", false) // 09:06, written to 09:05 so far
	f.days = []store.ThreadDay{{Day: "2026-11-24", ThreadID: "t1"}, {Day: "2026-11-24", ThreadID: "t3"}}
	// 23:59 to 00:01 the next night, on no thread.
	f.spans[99] = store.ActivitySpan{ID: 99, StartMs: ms(time.Date(2026, 11, 24, 23, 59, 0, 0, time.Local)),
		EndMs: ms(time.Date(2026, 11, 25, 0, 1, 0, 0, time.Local))}
	days, err := r.Days("2026-11-24", "2026-11-25")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days %+v", days)
	}
	d := days[0]
	// t1: 09:00-09:02 and 09:05-09:06, up to now; t2: 09:02-09:05.
	want := map[string]time.Duration{"t1": 3 * time.Minute, "t2": 3 * time.Minute, "": time.Minute}
	if d.Day != "2026-11-24" || d.Active != 7*time.Minute || d.Phone != 3*time.Minute || !mapsEqual(d.Threads, want) || !slices.Equal(d.Ran, []string{"t1", "t3"}) {
		t.Fatalf("the day is %+v; want 7m active, 3m on the phone, threads %v, ran t1 and t3", d, want)
	}
	if days[1].Day != "2026-11-25" || days[1].Active != time.Minute {
		t.Fatalf("the next day is %+v; want the minute after midnight", days[1])
	}
}

func mapsEqual(a, b map[string]time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A span the store already holds at its end is not written again when it
// closes, and a span that closes while a flush writes is written by the
// next.
func TestAFlushWritesOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Report("c1", "t1", false)
	c.add(3 * time.Minute)
	r.Flush() // written to 09:03, its last report plus the window, still open
	c.add(10 * time.Minute)
	r.Flush() // closed at 09:03, as written
	if n := len(f.saves[len(f.saves)-1]); n != 0 {
		t.Fatalf("closing a span written at its end wrote %d spans", n)
	}

	r.Report("c1", "t2", false)
	f.during = func() {
		f.during = nil
		c.add(time.Minute)
		r.Report("c1", "t3", false) // t2 closes while the flush writes
	}
	r.Flush()
	r.Flush()
	got := f.stored()
	if len(got) != 3 || got[1].ThreadID != "t2" || got[1].EndMs != got[1].StartMs+60_000 {
		t.Fatalf("stored %+v; want t2 written with its end once it closed", got)
	}
}
