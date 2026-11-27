package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/activity"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// activityClock is a settable clock for an App's recorder.
type activityClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *activityClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// activityApp is an App with a store holding a Jira connection, so the
// recorder writes, and a recorder on clock.
func activityApp(t *testing.T, clock *activityClock) *App {
	t.Helper()
	a := newTestApp(t)
	if _, err := a.store.CreateTrackerConnection(store.NewTrackerConnection{Kind: store.TrackerKindJira, Site: "https://example.atlassian.net",
		Account: "user1@example.com", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	cfg := activity.Config{}
	if clock != nil {
		cfg.Now = clock.now
	}
	a.activity = activity.New(a.store, cfg)
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
		open: func(ctx context.Context) (string, error) { return "", a.RecordActivity(ctx, "t1", false) },
		held: a.activity.OpenSpans,
	}
}

// Only a screen of this computer's person is recorded: a paired browser
// that is not one of the person's devices records nothing, and an id no
// thread could have is refused.
func TestRecordActivityIsThePersonsScreensOnly(t *testing.T) {
	t.Parallel()
	b := newPairedBackend(t)
	b.app.activity = activity.New(b.app.store, activity.Config{})
	t.Cleanup(func() { _ = b.app.activity.Close() })
	grants := []identity.Scope{identity.ScopeThreadsRead}
	session := pairSessionWithScopes(t, b.app, "another-browser", grants)
	if err := b.app.RecordActivity(callFrom(session.ID, false), "t1", false); err != nil {
		t.Fatal(err)
	}
	if n := b.app.activity.OpenSpans(); n != 0 {
		t.Fatalf("another's screen opened %d spans", n)
	}
	window, _ := newTestConnection(t, transport.ConnPrincipal{})
	if err := b.app.RecordActivity(window, "t1", true); err != nil {
		t.Fatal(err)
	}
	if n := b.app.activity.OpenSpans(); n != 1 {
		t.Fatalf("the host's window opened %d spans; want 1", n)
	}
	if err := b.app.RecordActivity(window, strings.Repeat("x", maxFocusedThreadBytes+1), false); err == nil {
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
		if err := a.activity.Report(conn, thread, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	advance := func(d time.Duration) {
		clock.mu.Lock()
		clock.t = clock.t.Add(d)
		clock.mu.Unlock()
	}
	report("c1", "t1")
	advance(2 * time.Minute)
	report("c1", "t2")
	advance(time.Minute)

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
