package activity

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"agent-overflow/internal/store"
)

// fakeStore holds the spans written, as the store would: an insert takes
// the next ID.
type fakeStore struct {
	mu    sync.Mutex
	fail  error
	spans map[int64]store.ActivitySpan
	days  []store.ThreadDay
	saves [][]store.ActivitySpan
	// daySaves are the thread days each save was given.
	daySaves [][]store.ThreadDay
	// during runs inside each save, without the lock, and duringRead
	// after each read of the spans, without the lock.
	during     func()
	duringRead func()
	prunes     [][2]any
	nextID     int64
}

func newFakeStore() *fakeStore { return &fakeStore{spans: map[int64]store.ActivitySpan{}} }

func (f *fakeStore) SaveActivity(spans []store.ActivitySpan, days []store.ThreadDay) ([]int64, error) {
	if f.during != nil {
		f.during()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves = append(f.saves, slices.Clone(spans))
	f.daySaves = append(f.daySaves, slices.Clone(days))
	if f.fail != nil {
		return nil, f.fail
	}
	for _, d := range days {
		if !slices.Contains(f.days, d) {
			f.days = append(f.days, d)
		}
	}
	ids := make([]int64, len(spans))
	for i, sp := range spans {
		if sp.ID != 0 {
			ids[i] = sp.ID
		} else {
			f.nextID++
			ids[i] = f.nextID
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
	var out []store.ActivitySpan
	for _, sp := range f.spans {
		if sp.StartMs < toMs && sp.EndMs > fromMs {
			out = append(out, sp)
		}
	}
	f.mu.Unlock()
	if f.duringRead != nil {
		f.duringRead()
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
	r.SetOn(true)
	// The cleanup only stops the recorder; a test that fails its store
	// checks Close's answer itself.
	t.Cleanup(func() { _ = r.Close() })
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
	r.Report("c1", "t1", false, 0, nil)
	c.add(time.Minute)
	r.Report("c1", "t1", false, 0, nil)
	c.add(2 * time.Minute)
	r.Report("c1", "t1", false, 0, nil) // 09:03
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
	r.Report("c1", "t2", true, 0, nil) // 09:05: t1 ends here
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
	r.Report("c1", "t1", false, 0, nil)
	c.add(4 * time.Minute)
	r.Report("c1", "t1", false, 0, nil)
	for range 24 * 60 {
		c.add(time.Minute)
		r.Report("c1", "t1", false, 0, nil)
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
	r.Report("c1", "t1", false, 0, nil)
	r.Report("c2", "t2", false, 0, nil)
	c.add(time.Minute)
	r.End("c1")
	c.add(time.Minute)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r.Report("c3", "t3", false, 0, nil)
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
// once; the next flush that writes writes it and says so once.
func TestAFailedFlushIsKeptAndSaid(t *testing.T) {
	t.Parallel()
	r, f, c, changes := recorder(t)
	r.Report("c1", "t1", false, 0, nil)
	c.add(time.Minute)
	r.Report("c1", "t2", false, 0, nil)
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
	// A Close whose last write fails says what was not recorded.
	failing, ff, _, _ := recorder(t)
	ff.fail = errors.New("disk I/O error")
	failing.Report("c1", "t1", false, 0, nil)
	if err := failing.Close(); err == nil || !strings.Contains(err.Error(), "disk I/O error") || !strings.Contains(err.Error(), nine.Format(time.DateTime)) {
		t.Fatalf("a Close whose write failed answers %v", err)
	}
	f.fail = nil
	r.Flush()
	if st := r.Status(); st != (Status{}) || len(*changes) != 2 || (*changes)[1] != (Status{}) {
		t.Fatalf("the status is %+v after %v; want recording again, said once", st, *changes)
	}
	if got := f.stored(); len(got) != 2 {
		t.Fatalf("stored %+v; want both spans", got)
	}

}

// A failing recorder keeps at most maxUnwritten closed spans, and still
// says since the earliest.
func TestAFailingRecorderIsBounded(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	f.fail = errors.New("disk full")
	for i := range maxUnwritten + 10 {
		r.Report("c1", []string{"t1", "t2"}[i%2], false, 0, nil)
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
	r.Report("c1", "t1", false, 0, nil)
	c.add(2 * time.Minute)
	r.Report("c1", "t1", false, 0, nil)
	r.Report("c2", "t2", true, 0, nil)
	c.add(3 * time.Minute)
	r.End("c2")
	r.Report("c1", "t1", false, 0, nil) // 09:05
	r.Flush()
	c.add(time.Minute)
	r.Report("c1", "t1", false, 0, nil) // 09:06, written to 09:05 so far
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
	r.Report("c1", "t1", false, 0, nil)
	c.add(3 * time.Minute)
	r.Flush() // written to 09:03, its last report plus the window, still open
	c.add(10 * time.Minute)
	r.Flush() // closed at 09:03, as written
	if n := len(f.saves[len(f.saves)-1]); n != 0 {
		t.Fatalf("closing a span written at its end wrote %d spans", n)
	}

	r.Report("c1", "t2", false, 0, nil)
	f.during = func() {
		f.during = nil
		c.add(time.Minute)
		r.Report("c1", "t3", false, 0, nil) // t2 closes while the flush writes
	}
	r.Flush()
	r.Flush()
	got := f.stored()
	if len(got) != 3 || got[1].ThreadID != "t2" || got[1].EndMs != got[1].StartMs+60_000 {
		t.Fatalf("stored %+v; want t2 written with its end once it closed", got)
	}
}

// Each span opened is tied to its connection's end, and its end unties
// it: at another thread, idle at a flush, and at the connection's end,
// which ends the span. A tie that fails opens nothing.
func TestASpanIsTiedToItsConnection(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	var ends []func()
	tied := 0
	tie := func(end func()) (func(), error) {
		ends = append(ends, end)
		tied++
		return func() { tied-- }, nil
	}
	if err := r.Report("c1", "t1", false, 0, tie); err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	if err := r.Report("c1", "t1", false, 0, tie); err != nil || len(ends) != 1 {
		t.Fatalf("a report on an open span tied %d times, %v", len(ends), err)
	}
	if err := r.Report("c1", "t2", false, 0, tie); err != nil || len(ends) != 2 || tied != 1 {
		t.Fatalf("another thread tied %d times and holds %d ties, %v; want the first untied and one held", len(ends), tied, err)
	}
	c.add(10 * time.Minute)
	r.Flush()
	if tied != 0 {
		t.Fatalf("a span that ended idle holds %d ties", tied)
	}
	c.add(time.Minute)
	if err := r.Report("c1", "t1", false, 0, tie); err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	ends[2]()
	r.Flush()
	if got := f.stored(); len(got) != 3 || got[2].EndMs != got[2].StartMs+60_000 || tied != 0 {
		t.Fatalf("stored %+v holding %d ties; want the last span ended by its connection's end", got, tied)
	}
	refused := errors.New("the connection closed")
	if err := r.Report("c2", "t1", false, 0, func(func()) (func(), error) { return nil, refused }); !errors.Is(err, refused) {
		t.Fatalf("a refused tie answered %v", err)
	}
	if len(r.open) != 0 {
		t.Fatalf("a refused tie opened %d spans", len(r.open))
	}
}

// A report counts the input it names, sinceInput before it: the span's
// last input is then, clamped to the window, and an age below 0 is now.
func TestAReportCountsItsInputsTime(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Report("c1", "t1", false, 0, nil)
	c.add(time.Minute)
	r.Report("c1", "t1", false, 10*time.Second, nil) // input at 09:00:50
	c.add(10 * time.Minute)
	r.Flush()
	if got := f.stored(); len(got) != 1 || got[0].EndMs != ms(nine.Add(50*time.Second+Window)) {
		t.Fatalf("stored %+v; want the span ended at the input at 09:00:50 plus the window", got)
	}
	r.Report("c2", "t2", false, time.Hour, nil) // 09:11, clamped to 09:08
	r.Report("c3", "t3", false, -time.Hour, nil)
	r.mu.Lock()
	c2, c3 := r.open["c2"].StartMs, r.open["c3"].StartMs
	r.mu.Unlock()
	if c2 != ms(c.now().Add(-Window)) || c3 != ms(c.now()) {
		t.Fatalf("an hour ago starts at %v and an hour ahead at %v; want the window ago and now", time.UnixMilli(c2), time.UnixMilli(c3))
	}
}

// Turned off, the recorder ends every open span then and writes it with
// the turn starts before, and records nothing until it is on again; on, it
// records from then.
func TestTheRecorderRecordsWhileOn(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Report("c1", "t1", false, 0, nil)
	r.Ran("t1", ms(nine))
	c.add(2 * time.Minute)
	r.SetOn(false)
	if got := f.stored(); len(got) != 1 || got[0].EndMs != ms(nine.Add(2*time.Minute)) || len(f.days) != 1 || len(r.open) != 0 {
		t.Fatalf("turned off, stored %+v and %+v with %d open; want the two minutes before, t1's day and none open", got, f.days, len(r.open))
	}
	c.add(time.Minute)
	r.Report("c1", "t1", false, 0, nil)
	r.Ran("t2", ms(c.now()))
	r.Flush()
	if len(f.stored()) != 1 || len(f.days) != 1 || len(r.open) != 0 || len(r.ran) != 0 {
		t.Fatalf("while off, recorded %+v, %+v", f.stored(), f.days)
	}
	r.SetOn(true)
	r.Report("c1", "t1", false, 0, nil)
	c.add(time.Minute)
	r.Flush()
	if got := f.stored(); len(got) != 2 || got[1].StartMs != ms(nine.Add(3*time.Minute)) {
		t.Fatalf("on again, stored %+v; want a span from 09:03", got)
	}
}

// A turn start is held until the flush, which writes its thread's day
// once; Days answers it before then. A failed flush keeps it and says
// since its turn; a failing recorder holds at most maxUnwritten.
func TestTurnStartsAreWrittenAtTheFlush(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Ran("t1", ms(nine))
	r.Ran("t1", ms(nine.Add(time.Hour)))
	r.Ran("t2", ms(nine.Add(24*time.Hour)))
	if len(f.days) != 0 {
		t.Fatalf("a turn start wrote %+v before the flush", f.days)
	}
	days, err := r.Days("2026-11-24", "2026-11-25")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || !slices.Equal(days[0].Ran, []string{"t1"}) || !slices.Equal(days[1].Ran, []string{"t2"}) {
		t.Fatalf("before the flush the days are %+v; want t1 Tuesday and t2 Wednesday", days)
	}
	f.fail = errors.New("disk I/O error")
	c.add(time.Minute)
	r.Flush()
	if st := r.Status(); st.Since != ms(nine) {
		t.Fatalf("a failed flush of turn starts says %+v; want since 09:00", st)
	}
	f.fail = nil
	r.Flush()
	want := []store.ThreadDay{{Day: "2026-11-24", ThreadID: "t1"}, {Day: "2026-11-25", ThreadID: "t2"}}
	got := slices.Clone(f.days)
	slices.SortFunc(got, func(a, b store.ThreadDay) int { return strings.Compare(a.Day, b.Day) })
	if !slices.Equal(got, want) || r.Status().Since != 0 {
		t.Fatalf("the flush wrote %+v, status %+v; want %+v", got, r.Status(), want)
	}
	r.Flush()
	if last := f.daySaves[len(f.daySaves)-1]; len(last) != 0 {
		t.Fatalf("a flush after the days were written wrote %+v again", last)
	}

	failing, ff, _, _ := recorder(t)
	ff.fail = errors.New("disk full")
	for i := range maxUnwritten + 10 {
		failing.Ran(fmt.Sprintf("t%d", i), ms(nine))
	}
	failing.Flush()
	if len(failing.ran) != maxUnwritten {
		t.Fatalf("a failing recorder holds %d thread days; want %d", len(failing.ran), maxUnwritten)
	}
}

// A day whose midnight its zone skips starts at the moment after it, and
// a read of it ends: a span from 23:30 the night before, an hour long, is
// half an hour on each day. Not parallel: it sets time.Local.
func TestADayWhoseMidnightIsSkippedIsCounted(t *testing.T) {
	for _, tc := range []struct {
		zone        string
		before, day string
		y           int
		m           time.Month
		d           int
	}{
		{"America/Santiago", "2026-09-05", "2026-09-06", 2026, time.September, 5},
		{"America/Havana", "2026-03-07", "2026-03-08", 2026, time.March, 7},
	} {
		loc, err := time.LoadLocation(tc.zone)
		if err != nil {
			t.Fatal(err)
		}
		saved := time.Local
		time.Local = loc
		f := newFakeStore()
		r := New(f, Config{Now: func() time.Time { return time.Date(tc.y, tc.m, tc.d+2, 9, 0, 0, 0, loc) }})
		start := time.Date(tc.y, tc.m, tc.d, 23, 30, 0, 0, loc)
		f.spans[1] = store.ActivitySpan{ID: 1, ThreadID: "t1", StartMs: ms(start), EndMs: ms(start.Add(time.Hour))}
		type answer struct {
			days []Day
			err  error
		}
		done := make(chan answer, 1)
		go func() {
			days, err := r.Days(tc.before, tc.day)
			done <- answer{days, err}
		}()
		var got answer
		select {
		case got = <-done:
		case <-time.After(5 * time.Second):
			time.Local = saved
			t.Fatalf("%s: the read of %s to %s did not end in 5s", tc.zone, tc.before, tc.day)
		}
		time.Local = saved
		_ = r.Close()
		if got.err != nil || len(got.days) != 2 || got.days[0].Day != tc.before || got.days[0].Active != 30*time.Minute ||
			got.days[1].Day != tc.day || got.days[1].Active != 30*time.Minute {
			t.Fatalf("%s: the days are %+v, %v; want half an hour on each", tc.zone, got.days, got.err)
		}
	}
}

// A past week's read counts only what reaches it: the spans held in
// memory today are not in it, before a flush or after.
func TestAPastWeekCountsNothingOfToday(t *testing.T) {
	t.Parallel()
	r, _, c, _ := recorder(t)
	r.Report("c1", "t1", false, 0, nil)
	c.add(2 * time.Minute)
	r.Report("c1", "t2", false, 0, nil)
	c.add(time.Minute)
	read := func(when string) {
		days, err := r.Days("2026-11-16", "2026-11-22")
		if err != nil || len(days) != 0 {
			t.Fatalf("%s the week before reads %+v, %v; want nothing", when, days, err)
		}
	}
	read("before a flush")
	r.Flush()
	read("after a flush")
}

// A read beside a flush misses nothing: a flush cannot write and retire a
// span between the read of the store and the read of memory.
func TestAReadBesideAFlushMissesNothing(t *testing.T) {
	t.Parallel()
	r, f, c, _ := recorder(t)
	r.Report("c1", "t1", false, 0, nil)
	c.add(2 * time.Minute)
	r.Report("c1", "t2", false, 0, nil) // t1's two minutes are closed, not written
	f.duringRead = func() {
		f.duringRead = nil
		// A flush that can run now, after the store was read, does.
		if r.flushMu.TryLock() {
			r.flushMu.Unlock()
			r.Flush()
		}
	}
	days, err := r.Days("2026-11-24", "2026-11-24")
	if err != nil || len(days) != 1 || days[0].Threads["t1"] != 2*time.Minute {
		t.Fatalf("the read beside a flush is %+v, %v; want t1's two minutes", days, err)
	}
}
