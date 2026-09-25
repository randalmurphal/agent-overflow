package worktreewatch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/testutil"
)

// recorder counts OnChange calls per project and lets a test wait for the
// next one.
type recorder struct {
	mu    sync.Mutex
	calls map[string]int
	wake  chan struct{}
}

func newRecorder() *recorder {
	return &recorder{calls: make(map[string]int), wake: make(chan struct{}, 64)}
}

func (r *recorder) onChange(project string) {
	r.mu.Lock()
	r.calls[project]++
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *recorder) count(project string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[project]
}

// waitCount blocks until project has been reported at least n times.
func (r *recorder) waitCount(t *testing.T, project string, n int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for r.count(project) < n {
		select {
		case <-r.wake:
		case <-deadline:
			t.Fatalf("project %s reported %d times, want at least %d within %s", project, r.count(project), n, within)
		}
	}
}

// assertNoMore proves nothing further is reported for project during quiet.
func (r *recorder) assertNoMore(t *testing.T, project string, quiet time.Duration) {
	t.Helper()
	before := r.count(project)
	time.Sleep(quiet)
	if after := r.count(project); after != before {
		t.Fatalf("project %s reported %d more times during a quiet window", project, after-before)
	}
}

func newTestManager(t *testing.T, rec *recorder, cfg Config) *Manager {
	t.Helper()
	cfg.OnChange = rec.onChange
	if cfg.Debounce == 0 {
		cfg.Debounce = 20 * time.Millisecond
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}
	if cfg.LivenessInterval == 0 {
		// Long enough that no test below relies on it unless it says so.
		cfg.LivenessInterval = time.Hour
	}
	m := NewManager(cfg)
	t.Cleanup(m.Close)
	return m
}

func addWorktree(t *testing.T, repo, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	testutil.RunGit(t, repo, "worktree", "add", "-b", name, path)
	return path
}

func TestFirstWatchReportsTheProjectOnce(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	rec := newRecorder()
	m := newTestManager(t, rec, Config{})

	m.SetProjects([]string{repo})

	rec.waitCount(t, repo, 1, 5*time.Second)
	rec.assertNoMore(t, repo, 150*time.Millisecond)
	if got := m.Projects(); len(got) != 1 || got[0] != repo {
		t.Fatalf("Projects() = %v, want [%s]", got, repo)
	}
}

func TestRegistryChangesAreReported(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	rec := newRecorder()
	m := newTestManager(t, rec, Config{})
	m.SetProjects([]string{repo})
	rec.waitCount(t, repo, 1, 5*time.Second)

	// `worktrees/` did not exist when the project was first watched: the
	// common-dir watch is what sees it appear.
	worktree := addWorktree(t, repo, "feature-add")
	rec.waitCount(t, repo, 2, 5*time.Second)
	rec.assertNoMore(t, repo, 150*time.Millisecond)

	// A removal in a terminal: the registration and the directory both go.
	testutil.RunGit(t, repo, "worktree", "remove", "--force", worktree)
	rec.waitCount(t, repo, 3, 5*time.Second)
	rec.assertNoMore(t, repo, 150*time.Millisecond)
	if m.Polling() {
		t.Fatal("manager fell back to polling on a filesystem where watches install")
	}
}

// A registered worktree deleted with rm -rf keeps its registration; the
// change is the directory's presence, seen through the extra directory when
// the checkout lives there.
func TestDeletedCheckoutUnderExtraDirIsReported(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	extra := t.TempDir()
	worktree := filepath.Join(extra, "feature-rm")
	testutil.RunGit(t, repo, "worktree", "add", "-b", "feature-rm", worktree)

	rec := newRecorder()
	m := newTestManager(t, rec, Config{ExtraDir: func(string) string { return extra }})
	m.SetProjects([]string{repo})
	rec.waitCount(t, repo, 1, 5*time.Second)

	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}
	rec.waitCount(t, repo, 2, 5*time.Second)
	// The registration is intact, so the snapshot now holds an absent entry.
	snap, err := snapshot(repo)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap) != 1 || snap[0].Present {
		t.Fatalf("snapshot = %+v, want one absent registration", snap)
	}
}

// The liveness read catches a deletion nothing watches: a checkout at an
// arbitrary path removed with rm -rf.
func TestLivenessReadCatchesUnwatchedDeletion(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	worktree := addWorktree(t, repo, "feature-far")
	rec := newRecorder()
	m := newTestManager(t, rec, Config{LivenessInterval: 60 * time.Millisecond})
	m.SetProjects([]string{repo})
	rec.waitCount(t, repo, 1, 5*time.Second)
	rec.assertNoMore(t, repo, 200*time.Millisecond)

	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}
	rec.waitCount(t, repo, 2, 5*time.Second)
}

func TestPollingFallbackObservesTheSameChanges(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	rec := newRecorder()
	m := newTestManager(t, rec, Config{disableFSWatch: true})
	if !m.Polling() {
		t.Fatal("Polling() = false with fs watch disabled")
	}
	m.SetProjects([]string{repo})
	rec.waitCount(t, repo, 1, 5*time.Second)

	worktree := addWorktree(t, repo, "feature-poll")
	rec.waitCount(t, repo, 2, 5*time.Second)
	testutil.RunGit(t, repo, "worktree", "remove", "--force", worktree)
	rec.waitCount(t, repo, 3, 5*time.Second)
}

func TestRemovedProjectStopsReporting(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	other := testutil.InitGitRepo(t)
	rec := newRecorder()
	m := newTestManager(t, rec, Config{})
	m.SetProjects([]string{repo, other})
	rec.waitCount(t, repo, 1, 5*time.Second)
	rec.waitCount(t, other, 1, 5*time.Second)

	m.SetProjects([]string{other})
	if got := m.Projects(); len(got) != 1 || got[0] != other {
		t.Fatalf("Projects() = %v, want [%s]", got, other)
	}
	addWorktree(t, repo, "feature-gone")
	rec.assertNoMore(t, repo, 300*time.Millisecond)

	// The kept project is still live.
	addWorktree(t, other, "feature-kept")
	rec.waitCount(t, other, 2, 5*time.Second)
}

// A change observed while OnChange runs yields exactly one more call, not
// one per event and never a concurrent one.
func TestChangesDuringACallCoalesceIntoOneMoreCall(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	var mu sync.Mutex
	calls, concurrent := 0, 0
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	m := NewManager(Config{
		Debounce:         20 * time.Millisecond,
		LivenessInterval: time.Hour,
		OnChange: func(string) {
			mu.Lock()
			calls++
			concurrent++
			if concurrent > 1 {
				t.Errorf("OnChange ran concurrently for one project")
			}
			n := calls
			mu.Unlock()
			started <- struct{}{}
			if n == 1 {
				<-release
			}
			mu.Lock()
			concurrent--
			mu.Unlock()
		},
	})
	t.Cleanup(m.Close)

	m.SetProjects([]string{repo})
	<-started // the initial call, now blocked on release

	first := addWorktree(t, repo, "feature-one")
	addWorktree(t, repo, "feature-two")
	testutil.RunGit(t, repo, "worktree", "remove", "--force", first)
	// Give the loop time to read the registry (several times) while the
	// first call is still running.
	time.Sleep(200 * time.Millisecond)
	close(release)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no follow-up call after the blocked call returned")
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("OnChange calls = %d, want 2 (initial plus one coalesced follow-up)", calls)
	}
}

func TestProjectThatIsNotACheckoutIsWatchedOnceItBecomesOne(t *testing.T) {
	dir := t.TempDir()
	rec := newRecorder()
	m := newTestManager(t, rec, Config{LivenessInterval: 60 * time.Millisecond})
	m.SetProjects([]string{dir})
	rec.waitCount(t, dir, 1, 5*time.Second)

	testutil.RunGit(t, dir, "init", "-b", "main")
	// Nothing registered yet, so the snapshot is unchanged and nothing is
	// reported; the liveness read re-derives the registry dir and arms the
	// watches so the first worktree is then seen through them.
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, dir, "add", "f")
	testutil.RunGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-m", "init")
	addWorktree(t, dir, "feature-late")
	rec.waitCount(t, dir, 2, 5*time.Second)
}

func TestCloseWaitsForCallsInFlight(t *testing.T) {
	repo := testutil.InitGitRepo(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	m := NewManager(Config{
		LivenessInterval: time.Hour,
		OnChange: func(string) {
			close(entered)
			<-release
			close(finished)
		},
	})
	m.SetProjects([]string{repo})
	<-entered

	closed := make(chan struct{})
	go func() {
		m.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while OnChange was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after OnChange finished")
	}
	select {
	case <-finished:
	default:
		t.Fatal("OnChange did not finish before Close returned")
	}
	// Idempotent, and a later SetProjects is a no-op rather than a panic.
	m.Close()
	m.SetProjects([]string{repo})
}
