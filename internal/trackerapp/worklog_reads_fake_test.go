package trackerapp

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/trackerdash"
)

// The reads of the logged time the dashboards count (worklog_reads.go),
// against the fake Jira site through the real provider, with the
// service's steady clock on the fake's clock. An evaluation's need is
// given as the evaluator gives it (Board.Worklogs from worklogsOf), and
// the end of a run is heard as the page hears it: a board event of the
// connection's board with Logged set.

// dashTime is a time scenario whose connection is verified as user 1,
// whose steady clock is the fake's, and which holds a hidden subscription
// of the connection's board (no reads of its own), whose events tell the
// end of each run. today is the service's day in UTC.
func dashTime(t *testing.T) (*timeScenario, string) {
	t.Helper()
	s := newTimeScenario(t)
	s.conn = must[Connection](t)(s.svc.Verify(t.Context(), s.conn.ID))
	s.svc.useClock(s.svc.now, s.clock.Now)
	scope := Scope{Connection: s.conn.ID, Board: "1"}
	s.listBoards(s.conn.ID, scope.Board)
	must[BoardSubscription](t)(s.svc.SubscribeBoard("boards-"+s.conn.ID, scope, []Section{{Kind: SectionBacklog}}, false, untied))
	return s, s.svc.now().UTC().Format(time.DateOnly)
}

// window is the days the dashboards hold of the logged time, to today.
func (s *timeScenario) window(t *testing.T, today string) []string {
	t.Helper()
	first := must[tracker.Date](t)(s.svc.loggedFrom())
	end, err := time.Parse(time.DateOnly, today)
	if err != nil {
		t.Fatal(err)
	}
	var days []string
	for d := dayTime(first); !d.After(end); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format(time.DateOnly))
	}
	return days
}

// unread is the window's days the mirror holds no read of, as an
// evaluation finds them.
func (s *timeScenario) unread(t *testing.T, today string) []string {
	t.Helper()
	days := s.window(t, today)
	got, err := s.st.ReadMirrorWorklogDays(s.conn.ID, "acc-user1", days[0], today)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(days, func(d string) bool { _, read := slices.BinarySearch(got.ReadDays, d); return read })
}

// shown is an evaluation of a dashboard whose widget counts the logged
// time, today: it tells the reads what it finds not read and answers
// their state.
func (s *timeScenario) shown(t *testing.T, today string) trackerdash.WorklogState {
	t.Helper()
	return s.shownNeeding(t, trackerdash.WorklogNeed{Unread: s.unread(t, today), Today: today})
}

func (s *timeScenario) shownNeeding(t *testing.T, need trackerdash.WorklogNeed) trackerdash.WorklogState {
	t.Helper()
	row, err := s.st.GetTrackerConnection(s.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s.svc.worklogsOf(row)(need)
}

// told waits until the connection's board was told n ends of runs.
func (s *timeScenario) told(t *testing.T, n int) {
	t.Helper()
	s.events.waitFor(t, fmt.Sprintf("%d ends of the reads of logged time", n), func(boards []BoardEvent, _ []IssueEvent, _ []error) bool {
		return loggedEnds(boards) >= n
	})
}

func loggedEnds(boards []BoardEvent) int {
	n := 0
	for _, e := range boards {
		if e.Logged {
			n++
		}
	}
	return n
}

func (s *timeScenario) ends() int {
	boards, _, _ := s.events.snapshot()
	return loggedEnds(boards)
}

// spansSince is the replaces recorded after the first n.
func (s *timeScenario) spansSince(n int) []string {
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	return slices.Clone(s.gate.spans[n:])
}

func (s *timeScenario) spanCount() int {
	s.gate.mu.Lock()
	defer s.gate.mu.Unlock()
	return len(s.gate.spans)
}

// refuseOnce has the fake Jira site refuse its next worklog search.
func (s *timeScenario) refuseOnce(t *testing.T) {
	t.Helper()
	jira := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
	if err := s.e.AddFault(trackerfake.Fault{Origin: jira, Route: "POST rest/api/3/search/jql", Kind: trackerfake.FaultRefuse, Times: 1, Status: 400,
		Body: json.RawMessage(`{"errorMessages":["refused"],"errors":{}}`)}); err != nil {
		t.Fatal(err)
	}
}

// newestSpan is the newest worklogSpan days to today, as a replace
// records them.
func newestSpan(today string) string {
	end, _ := time.Parse(time.DateOnly, today)
	return end.AddDate(0, 0, 1-worklogSpan).Format(time.DateOnly) + ".." + today
}

// A first show of a dashboard whose widget counts the logged time reads
// every day of the window in one run, newest first, 31 days a read, each
// once, and tells the board once; the next show within 5 minutes reads
// nothing. The read days then cover the window.
func TestAShownDashboardReadsItsWindowOfLoggedTimeOnce(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	n := len(s.window(t, today))
	start := time.Now()
	routes := s.requestsOf(t, func() {
		if st := s.shown(t, today); !st.Reading || st.Failure != "" {
			t.Fatalf("the first show answered %+v; want reading", st)
		}
		s.told(t, 1)
	})
	took := time.Since(start)
	spans := s.spansSince(0)
	if want := (n + worklogSpan - 1) / worklogSpan; len(spans) != want || total(routes) != want {
		t.Fatalf("the first show read %v with requests %s for %d days; want %d reads, one request each", spans, describe(routes), n, want)
	}
	if spans[0] != newestSpan(today) || !slices.IsSortedFunc(spans, func(a, b string) int { return strings.Compare(b, a) }) {
		t.Fatalf("the reads were %v; want the newest 31 days first, then older", spans)
	}
	t.Logf("first show, cold: %d tracker reads, requests %s, %v from the show to the board told", len(spans), describe(routes), took.Round(time.Millisecond))
	if left := s.unread(t, today); len(left) != 0 {
		t.Fatalf("days left unread: %v", left)
	}
	again := s.requestsOf(t, func() {
		if st := s.shown(t, today); st.Reading || st.Failure != "" {
			t.Fatalf("the next show answered %+v; want nothing to read", st)
		}
	})
	if total(again) != 0 || s.ends() != 1 {
		t.Fatalf("the next show made requests %s and told the board %d times", describe(again), s.ends())
	}
}

// After an automatic read failed, nothing is read on its own: not the
// days the evaluations find unread, and not the newest days 5 minutes on,
// as a board's change would read them. The state says why until a person
// asks; the person's ask reads the whole window again and clears it.
func TestAnAutomaticReadThatFailedWaitsForAPerson(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	n := len(s.window(t, today))
	s.refuseOnce(t)
	s.shown(t, today)
	s.told(t, 1)
	if st := s.shown(t, today); st.Reading || !strings.Contains(st.Failure, "refused") {
		t.Fatalf("a show after the failure answered %+v; want its failure and no read", st)
	}
	s.clock.advance(worklogRereadEvery)
	routes := s.requestsOf(t, func() {
		if st := s.shown(t, today); st.Reading || st.Failure == "" {
			t.Fatalf("a show 5 minutes after the failure answered %+v", st)
		}
	})
	if total(routes) != 0 || s.spanCount() != 0 || s.ends() != 1 {
		t.Fatalf("after the failure the shows read %v with requests %s", s.spansSince(0), describe(routes))
	}
	if err := s.svc.ReadWorklogDays(s.conn.ID, "UTC"); err != nil {
		t.Fatal(err)
	}
	if st := s.shown(t, today); !st.Reading || st.Failure != "" {
		t.Fatalf("a show during the person's read answered %+v; want reading and no failure", st)
	}
	s.told(t, 2)
	if spans := s.spansSince(0); len(spans) != (n+worklogSpan-1)/worklogSpan || spans[0] != newestSpan(today) {
		t.Fatalf("the person's ask read %v; want the whole window, newest first", spans)
	}
}

// logOutside logs seconds of user 1's time on the fake Jira site today,
// as a person logging time in Jira does: the app is told nothing.
func (s *timeScenario) logOutside(t *testing.T, id string, seconds int64) {
	t.Helper()
	site := s.e.Snapshot().Jira[0]
	site.Token = "fake-jira-token-1"
	if site.Tempo != nil {
		site.Tempo.Token = tempoTokenFake
	}
	at := s.clock.Now()
	site.Issues[0].Worklogs = append(site.Issues[0].Worklogs, trackerfake.JiraWorklog{ID: id, Author: "acc-user1", Started: at.Add(-time.Minute), TimeSpent: seconds, Created: at, Updated: at})
	if err := s.e.Seed(trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}); err != nil {
		t.Fatal(err)
	}
}

// holds reports whether the mirror holds worklog id of user 1 today.
func (s *timeScenario) holds(t *testing.T, today, id string) bool {
	t.Helper()
	got := must[WorklogDays](t)(s.svc.Worklogs(s.conn.ID, "acc-user1", today, today))
	return slices.ContainsFunc(got.Worklogs, func(w WorklogView) bool { return w.ID == id })
}

// Time logged outside the app on a day of the newest 31 is read by the
// first show once 5 minutes have passed since the newest days were last
// read, whatever the board did, and by no show sooner. A person's ask
// reads it at once, with the whole window.
func TestTheNewestLoggedTimeIsReadAgainEveryFiveMinutes(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	n := len(s.window(t, today))
	s.shown(t, today)
	s.told(t, 1)
	mark := s.spanCount()

	s.logOutside(t, "20009", 3600)
	s.clock.advance(worklogRereadEvery - time.Minute)
	if st := s.shown(t, today); st.Reading || s.holds(t, today, "20009") {
		t.Fatalf("a show 4 minutes later answered %+v; want no read", st)
	}
	s.clock.advance(time.Minute)
	if st := s.shown(t, today); !st.Reading {
		t.Fatalf("a show 5 minutes later answered %+v; want the newest days read", st)
	}
	s.told(t, 2)
	if spans := s.spansSince(mark); !slices.Equal(spans, []string{newestSpan(today)}) || !s.holds(t, today, "20009") {
		t.Fatalf("5 minutes later the show read %v; want %s with worklog 20009", spans, newestSpan(today))
	}

	s.logOutside(t, "20010", 900)
	mark = s.spanCount()
	if err := s.svc.ReadWorklogDays(s.conn.ID, "UTC"); err != nil {
		t.Fatal(err)
	}
	s.told(t, 3)
	if spans := s.spansSince(mark); len(spans) != (n+worklogSpan-1)/worklogSpan || spans[0] != newestSpan(today) || !s.holds(t, today, "20010") {
		t.Fatalf("a person's ask right after read %v; want the whole window, newest first", spans)
	}
	if st := s.shown(t, today); st.Reading {
		t.Fatalf("a show right after the person's ask answered %+v; want no read", st)
	}
}

// A show while a read is in flight, which finds that read's days not read
// yet, does not queue them again: the first show's run reads each day
// once.
func TestAShowDuringAReadQueuesNotItsDays(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	n := len(s.window(t, today))
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	s.gate.mu.Lock()
	s.gate.gate = func() {
		close(arrived)
		<-release
	}
	s.gate.mu.Unlock()
	s.shown(t, today)
	select {
	case <-arrived:
	case <-time.After(gateWait):
		t.Fatal("the first read never reached its store")
	}
	if st := s.shown(t, today); !st.Reading {
		t.Fatalf("a show during the read answered %+v", st)
	}
	let()
	s.told(t, 1)
	if spans := s.spansSince(0); len(spans) != (n+worklogSpan-1)/worklogSpan {
		t.Fatalf("the run read %v; want each day once", spans)
	}
}

// One hour of a dashboard shown with the logged time read, on the fake
// clock: counted every 10 minutes (the page's recount, the board idle),
// the newest days are read 6 times; counted every 5 s (a busy board), 12.
func TestAnHourOfAShownDashboardReadsTheNewestDaysAtMostTwelveTimes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		every time.Duration
		want  int
	}{{"idle", 10 * time.Minute, 6}, {"busy", 5 * time.Second, 12}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, today := dashTime(t)
			s.shown(t, today)
			s.told(t, 1)
			mark, told := s.spanCount(), 1
			routes := s.requestsOf(t, func() {
				for at := c.every; at <= time.Hour; at += c.every {
					s.clock.advance(c.every)
					if st := s.shownNeeding(t, trackerdash.WorklogNeed{Today: today}); st.Reading {
						told++
						s.told(t, told)
					}
				}
			})
			spans := s.spansSince(mark)
			if len(spans) != c.want || total(routes) != c.want {
				t.Fatalf("an hour %s read %d spans with requests %s; want %d", c.name, len(spans), describe(routes), c.want)
			}
			t.Logf("an hour %s, counted every %v: %d tracker reads, requests %s", c.name, c.every, len(spans), describe(routes))
		})
	}
}

// A person's Refresh reads every day of the window, the newest first,
// whatever the dashboard shows: one call, 6 tracker reads.
func TestARefreshReadsTheWholeWindowAgain(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	n := len(s.window(t, today))
	s.shown(t, today)
	s.told(t, 1)
	mark := s.spanCount()
	start := time.Now()
	routes := s.requestsOf(t, func() {
		if err := s.svc.ReadWorklogDays(s.conn.ID, "UTC"); err != nil {
			t.Fatal(err)
		}
		s.told(t, 2)
	})
	if spans := s.spansSince(mark); len(spans) != (n+worklogSpan-1)/worklogSpan || total(routes) != len(spans) {
		t.Fatalf("Refresh read %v with requests %s", spans, describe(routes))
	}
	t.Logf("Refresh: 1 call, %d tracker reads, requests %s, %v to the board told", (n+worklogSpan-1)/worklogSpan, describe(routes), time.Since(start).Round(time.Millisecond))
}

// The reads of one connection take one turn, which nothing replaces: while
// a run's read is held between its answer and its store, a change of the
// time source (Tempo added) starts the reads over, and the new run, a
// refresh of another author and one of the viewer's all wait for the
// held read; the turn is the same before and after. The other author's
// refresh leaves the viewer's reads as they were: a show after it reads
// nothing.
func TestTheReadsOfLoggedTimeTakeOneTurn(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	gate := s.svc.worklogs.gate(s.conn.ID)
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	s.gate.mu.Lock()
	s.gate.gate = func() {
		close(arrived)
		<-release
	}
	s.gate.mu.Unlock()
	s.shown(t, today)
	select {
	case <-arrived:
	case <-time.After(gateWait):
		t.Fatal("the first read never reached its store")
	}
	s.addTempoOn(t)
	if st := s.shown(t, today); !st.Reading {
		t.Fatalf("a show under Tempo answered %+v; want its reads started", st)
	}
	done := make(chan string, 2)
	for _, author := range []string{"acc-user2", "acc-user1"} {
		go func() {
			if _, err := s.svc.RefreshWorklogs(t.Context(), s.conn.ID, author, s.from, s.to); err != nil {
				t.Errorf("%s's refresh: %v", author, err)
			}
			done <- author
		}()
	}
	select {
	case a := <-done:
		t.Fatalf("%s's refresh answered while the first read held the turn", a)
	case <-time.After(time.Second):
	}
	if got := s.spanCount(); got != 1 {
		t.Fatalf("%d reads stored while the first held the turn: %v", got, s.spansSince(0))
	}
	let()
	for range 2 {
		select {
		case <-done:
		case <-time.After(gateWait):
			t.Fatal("a refresh never answered after the first read was released")
		}
	}
	s.told(t, 1)
	if again := s.svc.worklogs.gate(s.conn.ID); again != gate {
		t.Fatal("the connection's turn was replaced")
	}
	if st := s.shown(t, today); st.Reading {
		t.Fatalf("a show after the other author's refresh answered %+v; want nothing read", st)
	}
}

// A failure goes with the time source: once Tempo becomes the
// connection's, the next show reads every day of the window on its own,
// the ones whose read from Jira failed included.
func TestAnotherTimeSourceStartsTheReadsOver(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	n := len(s.window(t, today))
	s.refuseOnce(t)
	s.shown(t, today)
	s.told(t, 1)
	s.addTempoOn(t)
	if st := s.shown(t, today); !st.Reading || st.Failure != "" {
		t.Fatalf("a show under Tempo answered %+v; want the reads started over", st)
	}
	s.told(t, 2)
	if spans := s.spansSince(0); len(spans) != (n+worklogSpan-1)/worklogSpan || spans[0] != newestSpan(today) {
		t.Fatalf("under Tempo the show read %v; want the whole window, newest first", spans)
	}
}

// A run reads nothing more once the time source changes under it, though
// no evaluation came to start the reads over: the read in flight ends, and
// the next span is not read for the old source.
func TestARunStopsWhenTheTimeSourceChanges(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	s.gate.mu.Lock()
	s.gate.gate = func() {
		close(arrived)
		<-release
	}
	s.gate.mu.Unlock()
	s.shown(t, today)
	select {
	case <-arrived:
	case <-time.After(gateWait):
		t.Fatal("the first read never reached its store")
	}
	s.addTempoOn(t)
	let()
	s.told(t, 1)
	if got := s.spanCount(); got != 1 {
		t.Fatalf("the run read %v after the time source changed; want the read in flight alone", s.spansSince(0))
	}
}

// Turning a connection off stops its run and forgets its reads but the
// turn: a failure is forgotten, and once it is on again a show reads the
// window again on its own.
func TestTurningAConnectionOffForgetsItsReads(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	n := len(s.window(t, today))
	s.refuseOnce(t)
	s.shown(t, today)
	s.told(t, 1)
	gate := s.svc.worklogs.gate(s.conn.ID)
	must[Connection](t)(s.svc.SetEnabled(s.conn.ID, false))
	must[Connection](t)(s.svc.SetEnabled(s.conn.ID, true))
	// Turned off, the connection's board subscription ended; the page
	// subscribes again.
	must[BoardSubscription](t)(s.svc.SubscribeBoard("boards-"+s.conn.ID, Scope{Connection: s.conn.ID, Board: "1"}, []Section{{Kind: SectionBacklog}}, false, untied))
	if st := s.shown(t, today); !st.Reading || st.Failure != "" {
		t.Fatalf("a show after off and on answered %+v; want the reads started again", st)
	}
	if s.svc.worklogs.gate(s.conn.ID) != gate {
		t.Fatal("off and on replaced the connection's turn")
	}
	s.told(t, 2)
	if spans := s.spansSince(0); len(spans) != (n+worklogSpan-1)/worklogSpan || spans[0] != newestSpan(today) {
		t.Fatalf("after off and on the show read %v; want the whole window, the failed days first", spans)
	}
}

// A person's read is refused before anything is read for a time zone that
// is none and an account the connection has not verified.
func TestAPersonsReadIsRefusedWhatItCannotRead(t *testing.T) {
	t.Parallel()
	s, _ := dashTime(t)
	for _, zone := range []string{"", "Mars/Olympus"} {
		if err := s.svc.ReadWorklogDays(s.conn.ID, zone); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("zone %q: %v; want ErrInvalidRequest", zone, err)
		}
	}
	s.writeToken(s.conn, "another-token")
	if err := s.svc.ReadWorklogDays(s.conn.ID, "UTC"); !errors.Is(err, ErrUnverified) {
		t.Errorf("another token than the verified one: %v; want ErrUnverified", err)
	}
	if s.spanCount() != 0 {
		t.Errorf("the refused reads stored %v", s.spansSince(0))
	}
}

// Stopping the service in the middle of a run waits for the read it is
// in, then the run ends: nothing more is read, no failure is logged, and
// no goroutine of it is left once Stop returns.
func TestStoppingTheServiceMidRunJoinsItsReads(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	var mu sync.Mutex
	var logged []string
	s.svc.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let := func() { once.Do(func() { close(release) }) }
	t.Cleanup(let)
	s.gate.mu.Lock()
	s.gate.gate = func() {
		close(arrived)
		<-release
	}
	s.gate.mu.Unlock()
	s.shown(t, today)
	select {
	case <-arrived:
	case <-time.After(gateWait):
		t.Fatal("the first read never reached its store")
	}
	stopped := make(chan struct{})
	go func() {
		s.svc.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the run's read was in flight")
	case <-time.After(time.Second):
	}
	let()
	select {
	case <-stopped:
	case <-time.After(gateWait):
		t.Fatal("Stop never returned")
	}
	if got := s.spanCount(); got != 1 {
		t.Fatalf("the run read %v after Stop", s.spansSince(0))
	}
	buf := make([]byte, 1<<20)
	if stacks := string(buf[:runtime.Stack(buf, true)]); strings.Contains(stacks, fmt.Sprintf("(*Service).runWorklogs(%p", s.svc)) {
		t.Fatal("a run of the reads outlived Stop")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logged {
		if strings.Contains(line, "logged time") {
			t.Errorf("Stop logged %q", line)
		}
	}
}

// An agent's widget of logged time (TryWidget, dashboard_widget) starts
// the reads as a shown dashboard does.
func TestAnAgentsWidgetOfLoggedTimeStartsTheReads(t *testing.T) {
	t.Parallel()
	s, _ := dashTime(t)
	scope := Scope{Connection: s.conn.ID, Board: "1"}
	s.syncBoard(t, scope)
	w := KeptWidget{ID: "w_1", Form: "number", Width: 3, Metric: trackerdash.Metric{Name: "w_1", Title: "Your hours", Words: []string{"Your logged time."},
		SQL: `SELECT ticket, seconds AS value FROM worklogs`, Reduce: trackerdash.ReduceSum, Unit: trackerdash.UnitTime}}
	if _, err := s.svc.TryWidget(t.Context(), "boards-"+s.conn.ID, scope, w, TileMarks{}, "thread-1", false); err != nil {
		t.Fatal(err)
	}
	s.told(t, 1)
	if s.spanCount() == 0 {
		t.Fatal("the agent's widget read nothing")
	}
}
