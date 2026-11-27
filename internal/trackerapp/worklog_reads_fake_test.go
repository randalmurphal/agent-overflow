package trackerapp

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
)

// The reads a dashboard makes of its viewer's logged time
// (ReadWorklogDays), against the fake Jira site through the real
// provider, with the service's steady clock on the fake's clock.

// dashTime is a time scenario whose connection is verified as user 1,
// whose steady clock is the fake's, and whose day is the service's.
func dashTime(t *testing.T) (*timeScenario, string) {
	t.Helper()
	s := newTimeScenario(t)
	s.conn = must[Connection](t)(s.svc.Verify(t.Context(), s.conn.ID))
	s.svc.useClock(s.svc.now, s.clock.Now)
	return s, s.svc.now().UTC().Format(time.DateOnly)
}

// dashRead is one ReadWorklogDays of the scenario's connection, failing
// the test on an error.
func (s *timeScenario) dashRead(t *testing.T, today, reason string) WorklogRead {
	t.Helper()
	return must[WorklogRead](t)(s.svc.ReadWorklogDays(t.Context(), s.conn.ID, today, reason))
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

// windowDays is every day the dashboard's reads cover: from the day
// before the history window's start to today.
func (s *timeScenario) windowDays(today string) (first string, n int) {
	start := s.svc.window().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour)
	end, _ := time.Parse(time.DateOnly, today)
	return start.Format(time.DateOnly), int(end.Sub(start)/(24*time.Hour)) + 1
}

// backfill calls ReadWorklogDays with reason, then again as shown while
// it answers More, and returns the answers.
func (s *timeScenario) backfill(t *testing.T, today, reason string) []WorklogRead {
	t.Helper()
	var out []WorklogRead
	for got := s.dashRead(t, today, reason); ; got = s.dashRead(t, today, WorklogReadShown) {
		out = append(out, got)
		if !got.More {
			return out
		}
		if len(out) > 20 {
			t.Fatalf("the reads never end: %+v", out)
		}
	}
}

// A first show reads every day of the window, newest first, 31 days a
// call, each day once: 6 calls for the 182 days, one tracker read each,
// and nothing more on the next show. The read days then cover the window.
func TestADashboardReadsItsWindowOfLoggedTimeOnce(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	first, n := s.windowDays(today)
	var reads []WorklogRead
	routes := s.requestsOf(t, func() { reads = s.backfill(t, today, WorklogReadShown) })
	spans := s.spansSince(0)
	if want := (n + worklogSpan - 1) / worklogSpan; len(reads) != want || len(spans) != want {
		t.Fatalf("the first show made %d calls and %d reads (%v) for %d days; want %d", len(reads), len(spans), spans, n, want)
	}
	for i, r := range reads {
		if !r.Read || r.Failure != nil || r.RereadIn != 0 {
			t.Fatalf("call %d answered %+v", i, r)
		}
	}
	if spans[0] != time.Now().UTC().AddDate(0, 0, 1-worklogSpan).Format(time.DateOnly)+".."+today || !slices.IsSortedFunc(spans, func(a, b string) int { return -cmpString(a, b) }) {
		t.Fatalf("the reads were %v; want the newest 31 days first, then older", spans)
	}
	t.Logf("first show: %d calls, %d requests (%s)", len(reads), total(routes), describe(routes))
	read, err := s.st.ReadMirrorWorklogReadDays(s.conn.ID, "acc-user1", first, today)
	if err != nil || len(read) != n {
		t.Fatalf("the read days are %d of %d, %v", len(read), n, err)
	}
	again := s.requestsOf(t, func() {
		if got := s.dashRead(t, today, WorklogReadShown); got.Read || got.More {
			t.Fatalf("the next show answered %+v; want nothing read", got)
		}
	})
	if total(again) != 0 {
		t.Fatalf("the next show made requests: %s", describe(again))
	}
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// A day whose read failed is not read again on its own: the failure is
// answered with nothing more to read now, a later show reads the older
// days but not the failed ones, and a person's ask reads them again.
func TestADashboardReadThatFailedWaitsForAPerson(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	first, n := s.windowDays(today)
	calls := (n + worklogSpan - 1) / worklogSpan
	jira := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
	if err := s.e.AddFault(trackerfake.Fault{Origin: jira, Route: "POST rest/api/3/search/jql", Kind: trackerfake.FaultRefuse, Times: 1, Status: 400,
		Body: json.RawMessage(`{"errorMessages":["refused"],"errors":{}}`)}); err != nil {
		t.Fatal(err)
	}
	if got := s.dashRead(t, today, WorklogReadShown); !got.Read || got.More || got.Failure == nil {
		t.Fatalf("the failed read answered %+v; want its failure and nothing more now", got)
	}
	s.backfill(t, today, WorklogReadShown)
	spans := s.spansSince(0)
	if len(spans) != calls-1 || slices.ContainsFunc(spans, func(span string) bool { return span[len(span)-10:] == today }) {
		t.Fatalf("the later show read %v; want the %d older spans and not the failed one", spans, calls-1)
	}
	if read, err := s.st.ReadMirrorWorklogReadDays(s.conn.ID, "acc-user1", today, today); err != nil || len(read) != 0 {
		t.Fatalf("today reads as read %v, %v", read, err)
	}
	if got := s.dashRead(t, today, WorklogReadShown); got.Read || got.More {
		t.Fatalf("a show after the backfill answered %+v; want the failed days left to a person", got)
	}
	s.backfill(t, today, WorklogReadPerson)
	if again := s.spansSince(calls - 1); len(again) != calls || again[0][len(again[0])-10:] != today {
		t.Fatalf("a person's ask read %v; want the whole window again, newest first", again)
	}
	if read, err := s.st.ReadMirrorWorklogReadDays(s.conn.ID, "acc-user1", first, today); err != nil || len(read) != n {
		t.Fatalf("after the person's ask the read days are %d of %d, %v", len(read), n, err)
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

// Time logged outside the app on a day of the newest 31 is read when the
// board changes, at most once per worklogRereadEvery: a change sooner is
// answered with how long until it may read, and a show reads nothing
// again. A person's ask reads the whole window again now.
func TestABoardsChangeReadsTheNewestLoggedTimeAgain(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	_, n := s.windowDays(today)
	s.backfill(t, today, WorklogReadShown)
	mark := s.spanCount()
	newest := time.Now().UTC().AddDate(0, 0, 1-worklogSpan).Format(time.DateOnly) + ".." + today

	s.logOutside(t, "20009", 3600)
	if got := s.dashRead(t, today, WorklogReadShown); got.Read || got.More || !s.holdsNot(t, today, "20009") {
		t.Fatalf("a show answered %+v; want no read of days already read", got)
	}
	if got := s.dashRead(t, today, WorklogReadChanged); !got.Read || got.More || got.Failure != nil || !s.holds(t, today, "20009") {
		t.Fatalf("the board's change answered %+v; want the newest days read and worklog 20009 held", got)
	}
	if spans := s.spansSince(mark); !slices.Equal(spans, []string{newest}) {
		t.Fatalf("the board's change read %v; want %s", spans, newest)
	}

	s.logOutside(t, "20010", 1800)
	s.clock.advance(worklogRereadEvery - time.Minute)
	routes := s.requestsOf(t, func() {
		got := s.dashRead(t, today, WorklogReadChanged)
		if got.Read || got.More || got.RereadIn != time.Minute.Milliseconds() {
			t.Fatalf("a change 4 minutes later answered %+v; want no read and 60000 ms to wait", got)
		}
	})
	if total(routes) != 0 || s.holds(t, today, "20010") {
		t.Fatalf("a change 4 minutes later made requests %s", describe(routes))
	}
	s.clock.advance(time.Minute)
	if got := s.dashRead(t, today, WorklogReadChanged); !got.Read || !s.holds(t, today, "20010") {
		t.Fatalf("a change 5 minutes later answered %+v; want worklog 20010 read", got)
	}

	s.logOutside(t, "20011", 900)
	mark = s.spanCount()
	s.backfill(t, today, WorklogReadPerson)
	if spans := s.spansSince(mark); len(spans) != (n+worklogSpan-1)/worklogSpan || spans[0] != newest || !s.holds(t, today, "20011") {
		t.Fatalf("a person's ask right after a change read %v; want the whole window, newest first", spans)
	}
}

func (s *timeScenario) holdsNot(t *testing.T, today, id string) bool {
	t.Helper()
	return !s.holds(t, today, id)
}

// The dashboard's reads and RefreshWorklogs of one connection take turns:
// a read that starts while another is between its tracker answer and its
// store waits for it, so a second dashboard read reads the next days
// rather than the same ones, and a refresh lands after it.
func TestTheReadsOfOnesLoggedTimeTakeTurns(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	arrived, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s.gate.mu.Lock()
	s.gate.gate = func() {
		close(arrived)
		<-release
	}
	s.gate.mu.Unlock()
	type answer struct {
		what string
		err  error
	}
	done := make(chan answer, 3)
	go func() {
		_, err := s.svc.ReadWorklogDays(t.Context(), s.conn.ID, today, WorklogReadShown)
		done <- answer{"the first read", err}
	}()
	select {
	case <-arrived:
	case <-time.After(gateWait):
		t.Fatal("the first read never reached its store")
	}
	go func() {
		_, err := s.svc.ReadWorklogDays(t.Context(), s.conn.ID, today, WorklogReadShown)
		done <- answer{"the second read", err}
	}()
	go func() {
		_, err := s.svc.RefreshWorklogs(t.Context(), s.conn.ID, "acc-user1", s.from, s.to)
		done <- answer{"the refresh", err}
	}()
	// Neither answers while the first holds the turn; a second that did
	// not wait would answer at once.
	select {
	case a := <-done:
		t.Fatalf("%s answered (%v) while the first read held its turn", a.what, a.err)
	case <-time.After(time.Second):
	}
	close(release)
	for range 3 {
		select {
		case a := <-done:
			if a.err != nil {
				t.Fatalf("%s: %v", a.what, a.err)
			}
		case <-time.After(gateWait):
			t.Fatal("a read never answered after the first was released")
		}
	}
	spans := s.spansSince(0)
	newest := time.Now().UTC().AddDate(0, 0, 1-worklogSpan).Format(time.DateOnly) + ".." + today
	if len(spans) != 3 || spans[0] != newest || !slices.Contains(spans, s.from+".."+s.to) || slices.Index(spans[1:], newest) >= 0 {
		t.Fatalf("the reads stored %v; want %s once, the next older days and the refresh", spans, newest)
	}
}

// What the reads tried and owe goes with the connection: turned off and
// on, its days are read again on their own, the failed ones included.
func TestTurningAConnectionOffForgetsItsDashboardReads(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	_, n := s.windowDays(today)
	jira := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
	if err := s.e.AddFault(trackerfake.Fault{Origin: jira, Route: "POST rest/api/3/search/jql", Kind: trackerfake.FaultRefuse, Times: 1, Status: 400,
		Body: json.RawMessage(`{"errorMessages":["refused"],"errors":{}}`)}); err != nil {
		t.Fatal(err)
	}
	if got := s.dashRead(t, today, WorklogReadShown); got.Failure == nil {
		t.Fatalf("the read answered %+v; want its failure", got)
	}
	must[Connection](t)(s.svc.SetEnabled(s.conn.ID, false))
	s.svc.worklogs.mu.Lock()
	_, kept := s.svc.worklogs.byID[s.conn.ID]
	s.svc.worklogs.mu.Unlock()
	if kept {
		t.Fatal("the connection turned off kept its reads' record")
	}
	must[Connection](t)(s.svc.SetEnabled(s.conn.ID, true))
	mark := s.spanCount()
	s.backfill(t, today, WorklogReadShown)
	if spans := s.spansSince(mark); len(spans) != (n+worklogSpan-1)/worklogSpan || spans[0][len(spans[0])-10:] != today {
		t.Fatalf("after off and on the show read %v; want the whole window, the failed days first", spans)
	}
}

// A read is refused before anything is read for a reason it does not
// know, a day that is today in no time zone, and an account the
// connection has not verified.
func TestADashboardReadIsRefusedWhatItCannotRead(t *testing.T) {
	t.Parallel()
	s, today := dashTime(t)
	for what, call := range map[string]func() error{
		"an unknown reason": func() error { _, err := s.svc.ReadWorklogDays(t.Context(), s.conn.ID, today, "later"); return err },
		"two days ahead": func() error {
			_, err := s.svc.ReadWorklogDays(t.Context(), s.conn.ID, time.Now().UTC().AddDate(0, 0, 2).Format(time.DateOnly), WorklogReadShown)
			return err
		},
		"two days behind": func() error {
			_, err := s.svc.ReadWorklogDays(t.Context(), s.conn.ID, time.Now().UTC().AddDate(0, 0, -2).Format(time.DateOnly), WorklogReadShown)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v; want ErrInvalidRequest", what, err)
		}
	}
	for _, d := range []int{-1, 1} {
		if _, err := s.svc.ReadWorklogDays(t.Context(), s.conn.ID, time.Now().UTC().AddDate(0, 0, d).Format(time.DateOnly), WorklogReadChanged); err != nil {
			t.Errorf("a day %d from the service's: %v", d, err)
		}
	}
	s.writeToken(s.conn, "another-token")
	if _, err := s.svc.ReadWorklogDays(t.Context(), s.conn.ID, today, WorklogReadShown); !errors.Is(err, ErrUnverified) {
		t.Errorf("another token than the verified one: %v; want ErrUnverified", err)
	}
	if s.spanCount() != 2 {
		t.Errorf("the refused reads stored %v", s.spansSince(0))
	}
}
