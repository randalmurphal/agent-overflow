package trackerapp

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The worklogs of a Jira connection come from Jira, or from its Tempo
// connection while that one is on. These tests read them against the fake
// Jira site and its Tempo account, each holding one worklog of user 1 on
// the same day: Jira's 20001 and Tempo's 7001.

// worklogGateStore runs gate, once, before the first ReplaceMirrorWorklogs
// after it is set: a test holds a refresh between its read and its store
// with it.
type worklogGateStore struct {
	*store.Store
	mu   sync.Mutex
	gate func()
}

func (g *worklogGateStore) ReplaceMirrorWorklogs(root int64, source, author, from, to string, read store.MirrorRead, logs []store.MirrorWorklog) (store.MirrorWorklogChanges, error) {
	g.mu.Lock()
	gate := g.gate
	g.gate = nil
	g.mu.Unlock()
	if gate != nil {
		gate()
	}
	return g.Store.ReplaceMirrorWorklogs(root, source, author, from, to, read, logs)
}

// timeScenario is a fake scenario with a Jira connection whose site and
// Tempo account each hold one worklog of user 1 on day, and the three
// days around it as the range read.
type timeScenario struct {
	*fakeScenario
	conn     Connection
	gate     *worklogGateStore
	from, to string
}

const tempoTokenFake = "fake-tempo-token-1"

func newTimeScenario(t *testing.T) *timeScenario {
	t.Helper()
	var day time.Time
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		site := jiraSeed(at)
		// User 3's profile hides the time zone.
		site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-user3", DisplayName: "User 3", Email: "user3@example.com", HidesTimeZone: true})
		day = at.AddDate(0, 0, -2).Truncate(24 * time.Hour)
		site.Issues[0].Worklogs = []trackerfake.JiraWorklog{{ID: "20001", Author: "acc-user1", Started: day.Add(9 * time.Hour), TimeSpent: 3600}}
		site.Tempo = &trackerfake.TempoAccount{Token: tempoTokenFake, Worklogs: []trackerfake.TempoWorklog{
			{ID: 7001, Issue: "10101", Author: "acc-user1", StartDate: day.Format(time.DateOnly), StartTime: "09:00:00", Spent: 1800}}}
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}, Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
	}, "example.atlassian.net")
	gate := &worklogGateStore{Store: s.st}
	s.svc.store = gate
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	return &timeScenario{fakeScenario: s, conn: conn, gate: gate,
		from: day.AddDate(0, 0, -1).Format(time.DateOnly), to: day.AddDate(0, 0, 1).Format(time.DateOnly)}
}

// mirrored is the mirror's worklog IDs of user 1 over the range, and the
// days of it that no read replaced.
func (s *timeScenario) mirrored(t *testing.T) (ids, unread []string) {
	t.Helper()
	got := must[WorklogDays](t)(s.svc.Worklogs(s.conn.ID, "acc-user1", s.from, s.to))
	for _, w := range got.Worklogs {
		ids = append(ids, w.ID)
	}
	return ids, got.Unread
}

// refreshed refreshes the range and fails unless it read want and left
// no day unread.
func (s *timeScenario) refreshed(t *testing.T, what, want string) {
	t.Helper()
	got := must[WorklogDays](t)(s.svc.RefreshWorklogs(t.Context(), s.conn.ID, "acc-user1", s.from, s.to))
	if got.Failure != nil || len(got.Worklogs) != 1 || got.Worklogs[0].ID != want || len(got.Unread) != 0 {
		t.Fatalf("%s: the refresh answered %+v; want worklog %s with every day read", what, got, want)
	}
	if ids, unread := s.mirrored(t); !slices.Equal(ids, []string{want}) || len(unread) != 0 {
		t.Fatalf("%s: the mirror reads %v unread %v after the refresh", what, ids, unread)
	}
}

// emptied fails unless the mirror holds none of the range's worklogs and
// says every day of it is unread.
func (s *timeScenario) emptied(t *testing.T, what string) {
	t.Helper()
	if ids, unread := s.mirrored(t); len(ids) != 0 || !slices.Equal(unread, s.days()) {
		t.Fatalf("%s: the mirror reads %v unread %v; want nothing, every day unread", what, ids, unread)
	}
}

func (s *timeScenario) days() []string {
	first, _ := time.Parse(time.DateOnly, s.from)
	return []string{s.from, first.AddDate(0, 0, 1).Format(time.DateOnly), s.to}
}

// gateWait bounds each wait of refreshAcross, so a regression fails the
// test instead of hanging it.
const gateWait = 30 * time.Second

// refreshAcross refreshes the range with its store held between the
// tracker's answer and the replace, runs change while it is held, and
// returns what the refresh answered. A refresh that never reaches the
// replace, or never returns, fails the test after gateWait; the hold is
// released when the test ends, whatever happened, before the service
// stops (cleanups run last registered first).
func (s *timeScenario) refreshAcross(t *testing.T, change func()) WorklogDays {
	t.Helper()
	arrived, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	let := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(let)
	s.gate.mu.Lock()
	s.gate.gate = func() {
		close(arrived)
		select {
		case <-release:
		case <-time.After(gateWait):
		}
	}
	s.gate.mu.Unlock()
	done := make(chan WorklogDays, 1)
	go func() {
		got, err := s.svc.RefreshWorklogs(t.Context(), s.conn.ID, "acc-user1", s.from, s.to)
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	select {
	case <-arrived:
	case got := <-done:
		t.Fatalf("the refresh answered %+v before it reached the replace", got)
	case <-time.After(gateWait):
		t.Fatal("the refresh never reached the replace")
	}
	change()
	let()
	select {
	case got := <-done:
		return got
	case <-time.After(gateWait):
		t.Fatal("the refresh never returned after its replace was released")
	}
	return WorklogDays{}
}

func (s *timeScenario) addTempoOn(t *testing.T) Connection {
	t.Helper()
	tempo := s.addTempo(s.conn.ID)
	s.writeToken(tempo, tempoTokenFake)
	return tempo
}

// A mirror that a board read made says every day is unread until a
// refresh reads it. Each change of where the time comes from (Tempo
// added, turned off, turned on, removed) empties the mirror's worklogs
// and their days, so the mirror never shows one source's worklogs as the
// other's; the next refresh reads the new source. Turning the Jira
// connection off and on still detaches its mirror.
func TestWorklogsFollowTheirTimeSource(t *testing.T) {
	t.Parallel()
	s := newTimeScenario(t)
	scope := Scope{Connection: s.conn.ID, Board: "1"}
	s.listBoards(s.conn.ID, scope.Board)
	s.syncBoard(t, scope)
	if _, ok, err := s.st.GetMirrorRoot(s.conn.ID); err != nil || !ok {
		t.Fatalf("the board read made no mirror: %v, %v", ok, err)
	}
	s.emptied(t, "before any refresh")
	s.refreshed(t, "Jira's own worklogs", "20001")
	// Only the days a read replaced are read: the day before the range
	// is unread beside the range's days.
	first, _ := time.Parse(time.DateOnly, s.from)
	before := first.AddDate(0, 0, -1).Format(time.DateOnly)
	if got := must[WorklogDays](t)(s.svc.Worklogs(s.conn.ID, "acc-user1", before, s.to)); !slices.Equal(got.Unread, []string{before}) || len(got.Worklogs) != 1 {
		t.Fatalf("a read of the day before and the range answered %+v; want only %s unread", got, before)
	}

	tempo := s.addTempoOn(t)
	s.emptied(t, "Tempo added")
	s.refreshed(t, "with Tempo", "7001")
	must[Connection](t)(s.svc.SetEnabled(tempo.ID, false))
	s.emptied(t, "Tempo turned off")
	s.refreshed(t, "with Tempo off", "20001")
	must[Connection](t)(s.svc.SetEnabled(tempo.ID, true))
	s.emptied(t, "Tempo turned on")
	s.refreshed(t, "with Tempo on", "7001")
	must[Connection](t)(s.svc.SetEnabled(tempo.ID, false))
	must[Connection](t)(s.svc.SetEnabled(tempo.ID, true))
	s.emptied(t, "Tempo turned off and on")
	s.refreshed(t, "with Tempo on again", "7001")
	if err := s.svc.Remove(tempo.ID); err != nil {
		t.Fatal(err)
	}
	s.emptied(t, "Tempo removed")
	s.refreshed(t, "with Tempo removed", "20001")

	must[Connection](t)(s.svc.SetEnabled(s.conn.ID, false))
	if _, err := s.svc.Worklogs(s.conn.ID, "acc-user1", s.from, s.to); !errors.Is(err, ErrDisabled) {
		t.Fatalf("the mirror read of a connection turned off: %v", err)
	}
	must[Connection](t)(s.svc.SetEnabled(s.conn.ID, true))
	s.emptied(t, "Jira turned off and on")
	s.refreshed(t, "Jira on again", "20001")
}

// A refresh whose read began before the time source changed stores
// nothing: its replace is refused, the refresh answers a failure that
// says to refresh again, the connection's problem is left as it was, and
// the next refresh reads the new source. The other order, the replace
// committed before the change, is TestWorklogsFollowTheirTimeSource's.
func TestALateWorklogReplaceFromAnEarlierSourceIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// before sets the source the read uses, and returns the change.
		before func(t *testing.T, s *timeScenario) func()
		// late is the source read before the change, now the source of
		// the next refresh.
		late, now string
	}{
		{"Tempo added", func(t *testing.T, s *timeScenario) func() {
			return func() { s.addTempoOn(t) }
		}, "20001", "7001"},
		{"Tempo turned off", func(t *testing.T, s *timeScenario) func() {
			tempo := s.addTempoOn(t)
			return func() { must[Connection](t)(s.svc.SetEnabled(tempo.ID, false)) }
		}, "7001", "20001"},
		{"Tempo turned on", func(t *testing.T, s *timeScenario) func() {
			tempo := s.addTempoOn(t)
			must[Connection](t)(s.svc.SetEnabled(tempo.ID, false))
			return func() { must[Connection](t)(s.svc.SetEnabled(tempo.ID, true)) }
		}, "20001", "7001"},
		{"Tempo removed", func(t *testing.T, s *timeScenario) func() {
			tempo := s.addTempoOn(t)
			return func() {
				if err := s.svc.Remove(tempo.ID); err != nil {
					t.Error(err)
				}
			}
		}, "7001", "20001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTimeScenario(t)
			change := tc.before(t, s)
			s.refreshed(t, "before the change", tc.late)
			got := s.refreshAcross(t, change)
			if got.Failure == nil || got.Failure.Kind != string(FailureTransient) || got.Failure.Message != timeSourceChanged || len(got.Worklogs) != 0 {
				t.Fatalf("the late refresh answered %+v", got)
			}
			s.svc.mu.Lock()
			problem := s.svc.problems[s.conn.ID]
			s.svc.mu.Unlock()
			if problem.Kind != "" {
				t.Fatalf("the late refresh left the problem %+v", problem)
			}
			s.emptied(t, "after the late refresh")
			s.refreshed(t, "after the change", tc.now)
		})
	}
}

// The mirror read of worklogs refuses what the refresh refuses, from the
// connection's row in the snapshot it reads the worklogs from: a Linear
// connection, which has no time tracking, before and after a board read
// made its mirror (the mirror read makes no provider, so it decides by
// kind, which this holds to the providers' answer); a Tempo connection;
// one turned off; one removed. A range of more than 366 days is refused
// before the store is read.
func TestTheMirrorReadOfWorklogsRefusesWhatTheRefreshRefuses(t *testing.T) {
	t.Parallel()
	s := newTimeScenario(t)
	linear := s.addLinear()
	s.writeToken(linear, "lin_api_fake00001")
	tempo := s.addTempoOn(t)
	for _, c := range []Connection{s.conn, linear} {
		if has := s.provider(c.ID).Time() != nil; has != hasTimeTracking(c.Kind) {
			t.Fatalf("the %s provider has a time tracker: %v; the mirror read says %v", c.Kind, has, hasTimeTracking(c.Kind))
		}
	}
	unsupported := func(what string) {
		t.Helper()
		if _, err := s.svc.Worklogs(linear.ID, "user-1", s.from, s.to); !errors.Is(err, tracker.ErrUnsupported) {
			t.Fatalf("the mirror read of Linear's worklogs %s: %v", what, err)
		}
	}
	unsupported("before anything read it")
	if _, err := s.svc.RefreshWorklogs(t.Context(), linear.ID, "user-1", s.from, s.to); !errors.Is(err, tracker.ErrUnsupported) {
		t.Fatalf("the refresh of Linear's worklogs: %v", err)
	}
	scope := Scope{Connection: linear.ID, Board: tracker.BoardID(lid(10))}
	s.listBoards(linear.ID, scope.Board)
	s.syncBoard(t, scope)
	unsupported("after a board read made its mirror")
	if _, err := s.svc.Worklogs(tempo.ID, "acc-user1", s.from, s.to); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("the mirror read of a Tempo connection: %v", err)
	}

	cs := newCountingStore(s.st)
	s.svc.store = cs
	if _, err := s.svc.Worklogs(s.conn.ID, "acc-user1", "2026-01-01", "2027-01-02"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a mirror read of 367 days: %v", err)
	}
	if got := cs.take(); len(got) != 0 {
		t.Fatalf("a mirror read of 367 days read the store: %v", got)
	}
	year := must[WorklogDays](t)(s.svc.Worklogs(s.conn.ID, "acc-user1", "2026-01-01", "2027-01-01"))
	if len(year.Unread) != 366 || year.Unread[0] != "2026-01-01" || year.Unread[365] != "2027-01-01" {
		t.Fatalf("a mirror read of 366 days answered %d unread days", len(year.Unread))
	}
	if got := cs.take(); got["ReadMirrorWorklogDays"] != 1 || len(got) != 1 {
		t.Fatalf("a mirror read of 366 days read the store %v", got)
	}
	s.svc.store = s.gate

	must[Connection](t)(s.svc.SetEnabled(s.conn.ID, false))
	if _, err := s.svc.Worklogs(s.conn.ID, "acc-user1", s.from, s.to); !errors.Is(err, ErrDisabled) {
		t.Fatalf("the mirror read of a connection turned off: %v", err)
	}
	if err := s.svc.Remove(s.conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.Worklogs(s.conn.ID, "acc-user1", s.from, s.to); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the mirror read of a connection removed: %v", err)
	}
}
