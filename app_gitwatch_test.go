package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitwatch"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
)

// stubGitWatch is a controllable StatusFn for the App-level subscribe
// tests. It returns whatever was last set via setStatus, so we can
// drive the watcher's debounce loop into emitting on demand.
type stubGitWatch struct {
	mu      sync.Mutex
	current gitops.GitStatus
}

func (s *stubGitWatch) fn() gitwatch.StatusFn {
	return func(string) (gitops.GitStatus, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.current, nil
	}
}

func (s *stubGitWatch) setStatus(next gitops.GitStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = next
}

func installGitWatchForTest(t *testing.T, app *App, stub *stubGitWatch) {
	t.Helper()
	app.gitWatch = gitwatch.NewManager(gitwatch.ManagerConfig{StatusFn: stub.fn()})
	t.Cleanup(app.gitWatch.Close)
}

func makeWorkspaceThread(t *testing.T, app *App, threadID string) store.Thread {
	t.Helper()
	repo := testutil.InitGitRepo(t)
	thread := testThread(threadID)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	thread.ProjectID = project.ID
	thread.WorkspacePath = repo
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return thread
}

type recordedEmission struct {
	name string
	data any
}

func captureGitStatusEmissions(app *App) (*[]recordedEmission, *sync.Mutex) {
	var (
		mu     sync.Mutex
		events []recordedEmission
	)
	app.testEmitHook = func(name string, data any) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, recordedEmission{name: name, data: data})
	}
	return &events, &mu
}

// waitForGitStatusEvent blocks until at least one git:status emission
// matching subID arrives, or the timeout fires. Returns the matching
// event (or t.Fatal-s on timeout). Uses 50ms polling to avoid
// time.Sleep-style synchronization while keeping the test fast on the
// happy path.
func waitForGitStatusEvent(t *testing.T, events *[]recordedEmission, mu *sync.Mutex, subID string, timeout time.Duration) GitStatusEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		mu.Lock()
		for _, ev := range *events {
			if ev.name != "git:status" {
				continue
			}
			payload, ok := ev.data.(GitStatusEvent)
			if !ok {
				continue
			}
			if payload.SubscriptionID == subID {
				mu.Unlock()
				return payload
			}
		}
		mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("no git:status event received for sub %q within %s", subID, timeout)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestGitStatusSubscribeReturnsInitialAndStreamsUpdates(t *testing.T) {
	app := newTestAppWithStore(t)
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	installGitWatchForTest(t, app, stub)
	thread := makeWorkspaceThread(t, app, "thread-sub-1")

	events, mu := captureGitStatusEmissions(app)

	res, err := app.GitStatusSubscribe(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("GitStatusSubscribe: %v", err)
	}
	if res.ID == "" {
		t.Fatalf("expected non-empty subscription ID")
	}
	if res.Status.Branch != "main" {
		t.Fatalf("initial Branch = %q, want main", res.Status.Branch)
	}

	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", HasChanges: true})
	if err := os.WriteFile(filepath.Join(thread.WorkspacePath, "trigger.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write trigger file: %v", err)
	}

	got := waitForGitStatusEvent(t, events, mu, res.ID, 5*time.Second)
	if !got.Status.HasChanges {
		t.Fatalf("event Status.HasChanges = false, want true")
	}

	if err := app.GitStatusUnsubscribe(res.ID); err != nil {
		t.Fatalf("GitStatusUnsubscribe: %v", err)
	}
	app.gitWatchPumpsMu.Lock()
	_, stillTracked := app.gitWatchPumps[res.ID]
	app.gitWatchPumpsMu.Unlock()
	if stillTracked {
		t.Fatalf("subscription %q still tracked after Unsubscribe", res.ID)
	}
}

func TestGitStatusUnsubscribeIsIdempotent(t *testing.T) {
	app := newTestAppWithStore(t)
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	installGitWatchForTest(t, app, stub)
	thread := makeWorkspaceThread(t, app, "thread-sub-idem")

	res, err := app.GitStatusSubscribe(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := app.GitStatusUnsubscribe(res.ID); err != nil {
		t.Fatalf("first Unsubscribe: %v", err)
	}
	if err := app.GitStatusUnsubscribe(res.ID); err != nil {
		t.Fatalf("second Unsubscribe must be a no-op, got %v", err)
	}
	if err := app.GitStatusUnsubscribe("does-not-exist"); err != nil {
		t.Fatalf("Unsubscribe with unknown id must be a no-op, got %v", err)
	}
}

func TestGitStatusSubscribeReleasesOnConnectionClose(t *testing.T) {
	app := newTestAppWithStore(t)
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	installGitWatchForTest(t, app, stub)
	thread := makeWorkspaceThread(t, app, "thread-sub-conn")

	// Mimic the per-connection ctx the transport layer installs. When
	// the WS connection ends, RunCleanups fires and our registered
	// callback releases the subscription. This is the safety net for
	// unclean disconnects (network drop, kill -9 client).
	ctx, state := transport.WithConnState(context.Background())
	res, err := app.GitStatusSubscribe(ctx, thread.ID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	app.gitWatchPumpsMu.Lock()
	_, present := app.gitWatchPumps[res.ID]
	app.gitWatchPumpsMu.Unlock()
	if !present {
		t.Fatalf("subscription should be tracked after Subscribe")
	}

	state.RunCleanups()

	app.gitWatchPumpsMu.Lock()
	_, stillTracked := app.gitWatchPumps[res.ID]
	app.gitWatchPumpsMu.Unlock()
	if stillTracked {
		t.Fatalf("connection cleanup did not release subscription %q", res.ID)
	}

	// Cleanup is idempotent: a follow-up explicit Unsubscribe is a
	// no-op rather than an error or panic.
	if err := app.GitStatusUnsubscribe(res.ID); err != nil {
		t.Fatalf("Unsubscribe after connection cleanup: %v", err)
	}
}

func TestGitStatusSubscribeFailsOnUnknownThread(t *testing.T) {
	app := newTestAppWithStore(t)
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	installGitWatchForTest(t, app, stub)

	if _, err := app.GitStatusSubscribe(context.Background(), "no-such-thread"); err == nil {
		t.Fatalf("expected error for unknown thread")
	}
}

func TestGitStatusSubscribeReusesWatcherForSameWorkspace(t *testing.T) {
	app := newTestAppWithStore(t)
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	installGitWatchForTest(t, app, stub)

	threadA := makeWorkspaceThread(t, app, "share-a")
	threadB := testThread("share-b")
	threadB.ProjectID = threadA.ProjectID
	threadB.WorkspacePath = threadA.WorkspacePath
	if err := app.store.CreateThread(threadB); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	resA, err := app.GitStatusSubscribe(context.Background(), threadA.ID)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	resB, err := app.GitStatusSubscribe(context.Background(), threadB.ID)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	defer app.GitStatusUnsubscribe(resA.ID)
	defer app.GitStatusUnsubscribe(resB.ID)

	if resA.ID == resB.ID {
		t.Fatalf("expected distinct subscription IDs")
	}

	events, mu := captureGitStatusEmissions(app)
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", HasChanges: true})
	if err := os.WriteFile(filepath.Join(threadA.WorkspacePath, "shared-trigger.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	waitForGitStatusEvent(t, events, mu, resA.ID, 5*time.Second)
	waitForGitStatusEvent(t, events, mu, resB.ID, 5*time.Second)
}
