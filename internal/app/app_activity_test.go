package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/activity"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"
)

// activityClock is a settable clock for an App's recorder.
type activityClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *activityClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *activityClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

func (c *activityClock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

// activityApp is an App with a store holding a Jira connection, so the
// recorder records, and a recorder on clock as Start builds it.
func activityApp(t *testing.T, clock *activityClock) *App {
	t.Helper()
	a := newTestApp(t)
	if _, err := a.store.CreateTrackerConnection(store.NewTrackerConnection{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net",
		Account: "user1@example.com", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if clock != nil {
		a.activityTiming.Now = clock.now
	}
	a.initActivity()
	t.Cleanup(func() {
		if err := a.activity.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

// RecordActivity's span is the connection's: it ends when the connection
// does, and a connection already closing opens none.
func activityConnectionTie(t *testing.T) connectionTie {
	a := activityApp(t, nil)
	return connectionTie{
		open: func(ctx context.Context) (string, error) { return "", a.RecordActivity(ctx, "t1", false, 0) },
		held: a.activity.OpenSpans,
	}
}

// Only a screen of this computer's person is recorded, with its layout:
// a paired browser that is not one of the person's devices records
// nothing, the host's window in the compact layout records phone time,
// and an id no thread could have is refused.
func TestRecordActivityIsThePersonsScreensOnly(t *testing.T) {
	t.Parallel()
	b := newPairedBackend(t)
	clock := &activityClock{t: time.Date(2026, 11, 24, 9, 0, 0, 0, time.Local)}
	b.app.activity = activity.New(b.app.store, activity.Config{Now: clock.now})
	b.app.activity.SetOn(true)
	t.Cleanup(func() { _ = b.app.activity.Close() })
	grants := []identity.Scope{identity.ScopeThreadsRead}
	session := pairSessionWithScopes(t, b.app, "another-browser", grants)
	if err := b.app.RecordActivity(callFrom(session.ID, false), "t1", false, 0); err != nil {
		t.Fatal(err)
	}
	if n := b.app.activity.OpenSpans(); n != 0 {
		t.Fatalf("another's screen opened %d spans", n)
	}
	window, _ := newTestConnection(t, transport.ConnPrincipal{})
	if err := b.app.RecordActivity(window, "t1", true, 0); err != nil {
		t.Fatal(err)
	}
	if n := b.app.activity.OpenSpans(); n != 1 {
		t.Fatalf("the host's window opened %d spans; want 1", n)
	}
	clock.advance(time.Minute)
	days, err := b.app.activity.Days("2026-11-24", "2026-11-24")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Phone != time.Minute || days[0].Threads["t1"] != time.Minute {
		t.Fatalf("the compact window's minute is %+v; want a minute on t1, on the phone", days)
	}
	if err := b.app.RecordActivity(window, strings.Repeat("x", maxFocusedThreadBytes+1), false, 0); err == nil {
		t.Fatal("a thread id longer than any thread's was taken")
	}
}

// The activity time_read reads is the recorder's days with the commits
// the person authored in the repositories of those days' threads, a
// commit on a day with no input among them, and each repository whose
// commits cannot be read named.
func TestTrackerToolsActivityReadsTheDaysAndTheirCommits(t *testing.T) {
	t.Parallel()
	monday := time.Date(2026, 11, 23, 9, 0, 0, 0, time.Local)
	clock := &activityClock{t: monday}
	a := activityApp(t, clock)
	repo := t.TempDir()
	git := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(nil, "init", "-q", "-b", "main")
	git(nil, "config", "user.email", "me@example.com")
	git(nil, "config", "user.name", "Me")
	commit := func(at time.Time, subject string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, subject), []byte(subject), 0o644); err != nil {
			t.Fatal(err)
		}
		git(nil, "add", "-A")
		date := strconv.FormatInt(at.Unix(), 10) + " +0000"
		git([]string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, "commit", "-q", "-m", subject)
	}
	commit(monday.Add(time.Hour), "monday")
	commit(monday.AddDate(0, 0, 1).Add(time.Hour), "tuesday")
	now := time.Now().UnixMilli()
	for _, p := range []store.Project{
		{ID: "p1", Path: repo, Name: "Repo", CreatedAt: now, UpdatedAt: now},
		{ID: "p2", Path: filepath.Join(repo, "gone"), Name: "Gone", CreatedAt: now, UpdatedAt: now},
	} {
		if _, err := a.store.CreateProject(p); err != nil {
			t.Fatal(err)
		}
	}
	for id, project := range map[string]string{"t1": "p1", "t2": "p2"} {
		if err := a.store.CreateThread(store.Thread{ID: id, ProjectID: project, Title: id, Provider: "claude", WorkspacePath: repo,
			Mode: "chat", RuntimeMode: "approval-required", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	report := func(conn, thread string) {
		t.Helper()
		if err := a.activity.Report(conn, thread, false, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	report("c1", "t1")
	clock.advance(2 * time.Minute)
	report("c1", "t2")
	clock.advance(time.Minute)

	got, err := trackerToolsHost{a}.Activity(context.Background(), "2026-11-23", "2026-11-24")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Days) != 2 || got.Days[0].Day.Day != "2026-11-23" || got.Days[1].Day.Day != "2026-11-24" {
		t.Fatalf("the days are %+v; want Monday and Tuesday", got.Days)
	}
	mon, tue := got.Days[0], got.Days[1]
	if mon.Threads["t1"] != 2*time.Minute || len(mon.Commits) != 1 || mon.Commits[0].Subject != "monday" {
		t.Fatalf("Monday is %+v; want t1's two minutes and its commit", mon)
	}
	if tue.Active != 0 || len(tue.Commits) != 1 || tue.Commits[0].Subject != "tuesday" {
		t.Fatalf("Tuesday is %+v; want its commit alone", tue)
	}
	if len(got.CommitsUnread) != 1 || !strings.HasPrefix(got.CommitsUnread[0], "Gone: ") {
		t.Fatalf("the unread repositories are %q; want Gone's", got.CommitsUnread)
	}
}

// A turn start the app emits names its thread's day for the time draft,
// and another frame names none.
func TestATurnStartNamesItsThreadsDay(t *testing.T) {
	t.Parallel()
	a := activityApp(t, nil)
	at := time.Date(2026, 11, 24, 9, 0, 0, 0, time.Local).UnixMilli()
	a.emit(eventchan.ProviderTurnStarted, triage.TurnStartedEvent{ThreadID: "t1", TurnIndex: 0, StartedAt: at})
	a.emit(eventchan.ProviderTurnCompleted, triage.TurnCompletedEvent{ThreadID: "t2", TurnIndex: 0})
	days, err := a.activity.Days("2026-11-24", "2026-11-24")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || !slices.Equal(days[0].Ran, []string{"t1"}) {
		t.Fatalf("the days are %+v; want t1's turn on Tuesday", days)
	}
}

// activitySpanMinutes is what the store holds of 08:00 to 10:00 on Tue
// Nov 24, each span as its start and end in minutes after nine.
func activitySpanMinutes(t *testing.T, a *App, nine time.Time) [][2]int64 {
	t.Helper()
	spans, err := a.store.ActivitySpans(nine.Add(-time.Hour).UnixMilli(), nine.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]int64
	for _, sp := range spans {
		out = append(out, [2]int64{(sp.StartMs - nine.UnixMilli()) / 60_000, (sp.EndMs - nine.UnixMilli()) / 60_000})
	}
	return out
}

// The recorder records while a Jira connection is on, from the moment one
// is: turning the last one off writes what came before it and records
// nothing after, between two flushes or across one, and so does removing
// it; turning one on records from then, not what came while it was off.
func TestRecordingFollowsTheJiraConnection(t *testing.T) {
	t.Parallel()
	nine := time.Date(2026, 11, 24, 9, 0, 0, 0, time.Local)
	type step func(t *testing.T, f *jiraActivity)
	report := func(f *jiraActivity) {
		if err := f.a.RecordActivity(f.window, "t1", false, 0); err != nil {
			f.t.Fatal(err)
		}
	}
	at := func(minutes int) step {
		return func(t *testing.T, f *jiraActivity) { f.clock.set(nine.Add(time.Duration(minutes) * time.Minute)) }
	}
	on := func(enabled bool) step {
		return func(t *testing.T, f *jiraActivity) {
			if _, err := f.a.trackers.SetEnabled(f.id, enabled); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep := func(t *testing.T, f *jiraActivity) { report(f) }
	ran := func(t *testing.T, f *jiraActivity) { f.a.activity.Ran("t1", f.clock.now().UnixMilli()) }
	flush := func(t *testing.T, f *jiraActivity) { f.a.activity.Flush() }
	remove := func(t *testing.T, f *jiraActivity) {
		if err := f.a.trackers.Remove(f.id); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		steps []step
		spans [][2]int64
		ran   bool
	}{
		{"on to off between two flushes", []step{rep, ran, at(2), on(false), at(3), rep, at(4), flush}, [][2]int64{{0, 2}}, true},
		{"on to off across a flush", []step{rep, at(1), flush, at(2), on(false), at(3), rep, ran, at(4), rep, at(5), flush}, [][2]int64{{0, 2}}, false},
		{"off to on between two flushes", []step{on(false), rep, ran, at(1), on(true), at(2), flush, rep, at(3), flush}, [][2]int64{{2, 3}}, false},
		{"off to on across a flush", []step{on(false), rep, ran, at(1), flush, at(2), on(true), rep, at(3), flush}, [][2]int64{{2, 3}}, false},
		{"the last Jira connection removed", []step{rep, at(1), flush, at(2), remove, at(3), rep, ran, at(4), flush}, [][2]int64{{0, 2}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &activityClock{t: nine}
			a := newTrackerTestApp(t, func(a *App) { a.activityTiming.Now = clock.now })
			conn, err := a.trackers.Add(trackerapp.NewConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com"})
			if err != nil {
				t.Fatal(err)
			}
			window, _ := newTestConnection(t, transport.ConnPrincipal{})
			f := &jiraActivity{t: t, a: a, clock: clock, id: conn.ID, window: window}
			for _, s := range tc.steps {
				s(t, f)
			}
			if got := activitySpanMinutes(t, a, nine); !slices.Equal(got, tc.spans) {
				t.Errorf("stored spans %v; want %v", got, tc.spans)
			}
			days, err := a.store.ThreadDays("2026-11-24", "2026-11-24")
			if err != nil {
				t.Fatal(err)
			}
			if (len(days) == 1) != tc.ran || len(days) > 1 {
				t.Errorf("stored thread days %v; want t1's day %v", days, tc.ran)
			}
		})
	}
}

type jiraActivity struct {
	t      *testing.T
	a      *App
	clock  *activityClock
	id     string
	window context.Context
}

// A span that ends idle at a flush unties its connection: the connection
// keeps no release for a span that is gone.
func TestAnIdleSpanUntiesItsConnection(t *testing.T) {
	t.Parallel()
	clock := &activityClock{t: time.Date(2026, 11, 24, 9, 0, 0, 0, time.Local)}
	a := activityApp(t, clock)
	window, conn := newTestConnection(t, transport.ConnPrincipal{})
	if err := a.RecordActivity(window, "t1", false, 0); err != nil {
		t.Fatal(err)
	}
	if !conn.Bound("activity") {
		t.Fatal("the span is not tied to its connection")
	}
	clock.advance(10 * time.Minute)
	a.activity.Flush()
	if a.activity.OpenSpans() != 0 || conn.Bound("activity") {
		t.Fatalf("after the span ended idle: %d open, still tied %v", a.activity.OpenSpans(), conn.Bound("activity"))
	}
}

// The recording state is one computer's: the call goes to the selected
// computer, so no merge of several can join one's error to another's 0.
func TestTheRecordingStateIsTheSelectedComputers(t *testing.T) {
	t.Parallel()
	i := slices.IndexFunc(transport.GeneratedMethods, func(m transport.MethodMeta) bool { return m.Name == "GetActivityRecording" })
	if i < 0 || transport.GeneratedMethods[i].Route != transport.RouteSelected {
		t.Fatalf("GetActivityRecording is %+v; want the selected route", transport.GeneratedMethods[i])
	}
}

// The harness's reset of the tracker connections leaves no activity: the
// next test reads none of the last one's minutes or turn days.
func TestTheHarnessResetLeavesNoActivity(t *testing.T) {
	t.Parallel()
	nine := time.Date(2026, 11, 24, 9, 0, 0, 0, time.Local)
	clock := &activityClock{t: nine}
	a := newTrackerTestApp(t, func(a *App) { a.activityTiming.Now = clock.now })
	if _, err := a.trackers.Add(trackerapp.NewConnection{Kind: store.TrackerKindJira, Site: "example.atlassian.net", Account: "user1@example.com"}); err != nil {
		t.Fatal(err)
	}
	window, _ := newTestConnection(t, transport.ConnPrincipal{})
	if err := a.RecordActivity(window, "t1", false, 0); err != nil {
		t.Fatal(err)
	}
	a.activity.Ran("t1", nine.UnixMilli())
	clock.advance(time.Minute)
	a.activity.Flush()
	if got := activitySpanMinutes(t, a, nine); len(got) != 1 {
		t.Fatalf("before the reset the store holds %v", got)
	}
	if err := (&harnessHost{app: a}).ResetTrackerConnections(); err != nil {
		t.Fatal(err)
	}
	days, err := a.activity.Days("2026-11-24", "2026-11-24")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := a.store.ThreadDays("2026-11-24", "2026-11-24")
	if got := activitySpanMinutes(t, a, nine); len(got) != 0 || len(days) != 0 || len(stored) != 0 {
		t.Fatalf("after the reset: spans %v, days %+v, thread days %v; want none", got, days, stored)
	}
}
