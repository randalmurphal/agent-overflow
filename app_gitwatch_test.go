package main

import (
	"context"
	"errors"
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
	// Status passes per cwd. A cwd with no watcher yet is only reached by
	// a Subscribe, so its count is a deterministic "did this call acquire
	// anything" probe.
	passes map[string]int
}

func (s *stubGitWatch) fn() gitwatch.StatusFn {
	return func(cwd string) (gitops.GitStatus, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.passes == nil {
			s.passes = make(map[string]int)
		}
		s.passes[cwd]++
		return s.current, nil
	}
}

func (s *stubGitWatch) passesFor(cwd string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.passes[cwd]
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

// gitStatusEventsFor collects every git:status emission recorded so far
// for cwd.
func gitStatusEventsFor(events *[]recordedEmission, mu *sync.Mutex, cwd string) []GitStatusEvent {
	mu.Lock()
	defer mu.Unlock()
	var matches []GitStatusEvent
	for _, ev := range *events {
		if ev.name != "git:status" {
			continue
		}
		payload, ok := ev.data.(GitStatusEvent)
		if !ok || payload.Cwd != cwd {
			continue
		}
		matches = append(matches, payload)
	}
	return matches
}

// waitForGitStatusEvent blocks until at least one git:status emission for
// cwd arrives, or the timeout fires. Returns the first matching event (or
// t.Fatal-s on timeout). Uses 50ms polling to avoid time.Sleep-style
// synchronization while keeping the test fast on the happy path.
func waitForGitStatusEvent(t *testing.T, events *[]recordedEmission, mu *sync.Mutex, cwd string, timeout time.Duration) GitStatusEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if matches := gitStatusEventsFor(events, mu, cwd); len(matches) > 0 {
			return matches[0]
		}
		select {
		case <-deadline:
			t.Fatalf("no git:status event received for cwd %q within %s", cwd, timeout)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// gitWatchPumpRefs reports the App-side refcount on cwd's pump, and
// whether a pump exists at all.
func gitWatchPumpRefs(app *App, cwd string) (int, bool) {
	app.gitStatus.mu.Lock()
	defer app.gitStatus.mu.Unlock()
	pump, ok := app.gitStatus.pumps[cwd]
	if !ok {
		return 0, false
	}
	return pump.refs, true
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
	if res.Cwd == "" {
		t.Fatalf("expected the canonical cwd on the subscribe result")
	}
	if res.Status.Branch != "main" {
		t.Fatalf("initial Branch = %q, want main", res.Status.Branch)
	}

	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", HasChanges: true})
	if err := os.WriteFile(filepath.Join(thread.WorkspacePath, "trigger.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write trigger file: %v", err)
	}

	got := waitForGitStatusEvent(t, events, mu, res.Cwd, 5*time.Second)
	if !got.Status.HasChanges {
		t.Fatalf("event Status.HasChanges = false, want true")
	}

	if err := app.GitStatusUnsubscribe(res.ID); err != nil {
		t.Fatalf("GitStatusUnsubscribe: %v", err)
	}
	if _, tracked := gitWatchPumpRefs(app, res.Cwd); tracked {
		t.Fatalf("pump for %q still tracked after the last Unsubscribe", res.Cwd)
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

	if _, present := gitWatchPumpRefs(app, res.Cwd); !present {
		t.Fatalf("pump should be tracked after Subscribe")
	}

	state.RunCleanups()

	if _, stillTracked := gitWatchPumpRefs(app, res.Cwd); stillTracked {
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

// TestGitStatusSubscribeSharesOnePumpPerCwd pins the refcount: git status
// is workspace state, so N callers on one workspace share ONE gitwatch
// subscription and ONE wire event per change — not N copies that can drift
// apart. Releasing one caller must leave the stream running for the rest.
func TestGitStatusSubscribeSharesOnePumpPerCwd(t *testing.T) {
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

	if resA.ID == resB.ID {
		t.Fatalf("expected distinct subscription handles")
	}
	if resA.Cwd != resB.Cwd {
		t.Fatalf("same workspace resolved to different cwds: %q vs %q", resA.Cwd, resB.Cwd)
	}
	if refs, ok := gitWatchPumpRefs(app, resA.Cwd); !ok || refs != 2 {
		t.Fatalf("pump refs = %d (present=%v), want 2", refs, ok)
	}
	app.gitStatus.mu.Lock()
	pumpCount := len(app.gitStatus.pumps)
	app.gitStatus.mu.Unlock()
	if pumpCount != 1 {
		t.Fatalf("pump count = %d, want 1 for one workspace", pumpCount)
	}

	events, mu := captureGitStatusEmissions(app)
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", HasChanges: true})
	if err := os.WriteFile(filepath.Join(threadA.WorkspacePath, "shared-trigger.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := waitForGitStatusEvent(t, events, mu, resA.Cwd, 5*time.Second)
	if !got.Status.HasChanges {
		t.Fatalf("event Status.HasChanges = false, want true")
	}
	// One change, one event — the pre-refactor per-subscriber emit produced
	// one copy per caller on the same channel.
	if n := len(gitStatusEventsFor(events, mu, resA.Cwd)); n != 1 {
		t.Fatalf("got %d git:status events for one change, want exactly 1", n)
	}

	// Releasing one caller leaves the pump running for the other.
	if err := app.GitStatusUnsubscribe(resA.ID); err != nil {
		t.Fatalf("unsubscribe A: %v", err)
	}
	if refs, ok := gitWatchPumpRefs(app, resA.Cwd); !ok || refs != 1 {
		t.Fatalf("after releasing A: refs = %d (present=%v), want 1", refs, ok)
	}

	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", HasChanges: true, AheadCount: 3})
	if err := os.WriteFile(filepath.Join(threadA.WorkspacePath, "shared-trigger-2.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		matches := gitStatusEventsFor(events, mu, resA.Cwd)
		if len(matches) > 0 && matches[len(matches)-1].Status.AheadCount == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("pump stopped emitting after one of two callers unsubscribed")
		case <-time.After(50 * time.Millisecond):
		}
	}

	if err := app.GitStatusUnsubscribe(resB.ID); err != nil {
		t.Fatalf("unsubscribe B: %v", err)
	}
	if _, ok := gitWatchPumpRefs(app, resA.Cwd); ok {
		t.Fatalf("pump survived the last unsubscribe")
	}
}

// TestGetGitStatusPushesTheRefreshToSubscribers pins the healing half of
// item 2: a post-action GetGitStatus is not the acting client's private
// answer. Every other client watching the workspace observes the same
// refresh through the shared stream.
func TestGetGitStatusPushesTheRefreshToSubscribers(t *testing.T) {
	app := newTestAppWithStore(t)
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	installGitWatchForTest(t, app, stub)
	thread := makeWorkspaceThread(t, app, "thread-refresh-fanout")

	res, err := app.GitStatusSubscribe(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer app.GitStatusUnsubscribe(res.ID)

	events, mu := captureGitStatusEmissions(app)
	// The watcher's StatusFn now reports a change no filesystem event
	// announced — exactly the shape of "another client just committed".
	stub.setStatus(gitops.GitStatus{IsRepo: true, Branch: "main", AheadCount: 1})

	if _, err := app.GetGitStatus(thread.ID); err != nil {
		t.Fatalf("GetGitStatus: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		matches := gitStatusEventsFor(events, mu, res.Cwd)
		if len(matches) > 0 && matches[len(matches)-1].Status.AheadCount == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("GetGitStatus did not push the refresh to the workspace's subscribers")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestGitStatusSubscribeRefusesADyingPump covers the window between a pump
// goroutine exiting (its Updates() channel closed under it) and its deferred
// drop taking the lock. A Subscribe that lands in that window used to
// increment the doomed pump's refcount and hand back a handle that would
// receive no events and, once the drop deleted every handle for that cwd,
// release nothing either.
func TestGitStatusSubscribeRefusesADyingPump(t *testing.T) {
	app := newTestAppWithStore(t)
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	installGitWatchForTest(t, app, stub)
	thread := makeWorkspaceThread(t, app, "thread-dying-pump")

	first, err := app.GitStatusSubscribe(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Stand in for the goroutine's teardown having begun: the pump is
	// stamped dead but its drop has not removed it from the map yet.
	app.gitStatus.mu.Lock()
	dying := app.gitStatus.pumps[first.Cwd]
	if dying == nil {
		app.gitStatus.mu.Unlock()
		t.Fatalf("no pump tracked for %q after Subscribe", first.Cwd)
	}
	dying.dead = true
	app.gitStatus.mu.Unlock()

	second, err := app.GitStatusSubscribe(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("subscribe onto a dying pump: %v", err)
	}

	app.gitStatus.mu.Lock()
	fresh := app.gitStatus.pumps[first.Cwd]
	held := app.gitStatus.handles[second.ID]
	app.gitStatus.mu.Unlock()
	if fresh == dying {
		t.Fatalf("Subscribe shared the dying pump instead of minting a fresh one")
	}
	if held != fresh {
		t.Fatalf("the new handle references %p, want the fresh pump %p", held, fresh)
	}

	// The dying pump's own drop must take only what belonged to IT.
	app.dropGitWatchPump(dying)

	app.gitStatus.mu.Lock()
	stillMapped := app.gitStatus.pumps[first.Cwd]
	stillHeld, ok := app.gitStatus.handles[second.ID]
	_, staleHeld := app.gitStatus.handles[first.ID]
	app.gitStatus.mu.Unlock()
	if stillMapped != fresh {
		t.Fatalf("the dying pump's drop evicted its successor")
	}
	if !ok || stillHeld != fresh {
		t.Fatalf("the dying pump's drop released a handle it did not own")
	}
	if staleHeld {
		t.Fatalf("the dying pump's own handle survived its drop")
	}

	if err := app.GitStatusUnsubscribe(second.ID); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if _, tracked := gitWatchPumpRefs(app, first.Cwd); tracked {
		t.Fatalf("fresh pump survived its last unsubscribe")
	}
}

// TestGitStatusSubscribeCapsOutstandingHandles: every distinct cwd behind a
// handle costs a recursive fs watch and a status cadence, so the handle map
// is bounded rather than trusting callers to unsubscribe. The refusal is
// typed — retrying the same call never fixes it.
func TestGitStatusSubscribeCapsOutstandingHandles(t *testing.T) {
	app := newTestAppWithStore(t)
	stub := &stubGitWatch{current: gitops.GitStatus{IsRepo: true, Branch: "main"}}
	installGitWatchForTest(t, app, stub)
	thread := makeWorkspaceThread(t, app, "thread-handle-cap")

	ids := make([]string, 0, maxGitWatchHandles)
	cwd := ""
	for i := 0; i < maxGitWatchHandles; i++ {
		res, err := app.GitStatusSubscribe(context.Background(), thread.ID)
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		ids = append(ids, res.ID)
		cwd = res.Cwd
	}

	if _, err := app.GitStatusSubscribe(context.Background(), thread.ID); !errors.Is(err, ErrTooManyGitStatusSubscriptions) {
		t.Fatalf("subscribe past the cap: err = %v, want ErrTooManyGitStatusSubscriptions", err)
	}
	// The refusal must not have leaked a reference into the pump.
	if refs, ok := gitWatchPumpRefs(app, cwd); !ok || refs != maxGitWatchHandles {
		t.Fatalf("pump refs = %d (present=%v), want %d", refs, ok, maxGitWatchHandles)
	}

	// …and it must be refused BEFORE it acquires anything. A capped caller
	// naming an unwatched workspace used to drop that workspace's PR cache
	// and stand up a recursive fs watch — a full status pass — for a
	// subscription it was about to refuse.
	other := makeWorkspaceThread(t, app, "thread-handle-cap-other")
	if _, err := app.GitStatusSubscribe(context.Background(), other.ID); !errors.Is(err, ErrTooManyGitStatusSubscriptions) {
		t.Fatalf("subscribe past the cap (unwatched cwd): err = %v, want ErrTooManyGitStatusSubscriptions", err)
	}
	if passes := stub.passesFor(other.WorkspacePath); passes != 0 {
		t.Fatalf("a refused subscribe ran %d status passes on %s, want 0", passes, other.WorkspacePath)
	}
	if _, tracked := gitWatchPumpRefs(app, other.WorkspacePath); tracked {
		t.Fatalf("a refused subscribe left a pump behind for %s", other.WorkspacePath)
	}

	// Releasing one makes room again.
	if err := app.GitStatusUnsubscribe(ids[0]); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	res, err := app.GitStatusSubscribe(context.Background(), thread.ID)
	if err != nil {
		t.Fatalf("subscribe after releasing one handle: %v", err)
	}
	ids = append(ids[1:], res.ID)
	for _, id := range ids {
		if err := app.GitStatusUnsubscribe(id); err != nil {
			t.Fatalf("teardown unsubscribe: %v", err)
		}
	}
}
