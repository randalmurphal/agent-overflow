package app

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"

	"github.com/google/uuid"
)

// TestConcurrent_CreateThreadsUnderLoad: 50 goroutines concurrently creating
// threads against the same SQLite store. All inserts must succeed with
// distinct IDs and the store must report 50 active threads afterwards.
func TestConcurrent_CreateThreadsUnderLoad(t *testing.T) {
	app, _ := setupE2EApp(t)

	const n = 50
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			thread, err := createTestThread(
				t,
				app,
				string(provider.Claude),
				"/tmp/ws-"+fmt.Sprint(i),
				"claude-opus-4-7",
				"chat",
			)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = thread.ID
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if id == "" {
			t.Fatalf("goroutine %d: empty thread ID", i)
		}
		if seen[id] {
			t.Fatalf("duplicate thread ID %q from goroutine %d", id, i)
		}
		seen[id] = true
	}

	threads, err := app.store.ListThreads()
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != n {
		t.Fatalf("ListThreads = %d threads, want %d", len(threads), n)
	}
}

// TestConcurrent_SameThreadUpdates: 10 goroutines racing UpdateThreadModel on
// the same inactive thread. All writes must succeed; the final stored value
// must be one of the requested models (no garbled state, no torn writes).
func TestConcurrent_SameThreadUpdates(t *testing.T) {
	app, _ := setupE2EApp(t)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	models := []string{
		"claude-opus-4-7", "claude-opus-4-6", "claude-sonnet", "claude-haiku",
		"claude-opus-4-7[1m]", "claude-sonnet-4-5", "claude-haiku-4-5",
		"claude-opus-4-5", "claude-sonnet-4", "claude-haiku-4",
	}

	const n = 10
	var wg sync.WaitGroup
	var successes atomic.Int32
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := app.UpdateThreadModel(thread.ID, models[i%len(models)]); err != nil {
				t.Errorf("UpdateThreadModel[%d]: %v", i, err)
				return
			}
			successes.Add(1)
		}(i)
	}
	wg.Wait()

	if successes.Load() != n {
		t.Fatalf("successful updates = %d, want %d", successes.Load(), n)
	}

	final, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	ok := false
	for _, m := range models {
		if final.Model == m {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatalf("final model = %q, not in update set %v", final.Model, models)
	}
}

// TestConcurrent_ListItemsDuringActiveSession: many concurrent ListItems calls
// while a session is actively streaming into the store. No race, no partial
// rows returned.
func TestConcurrent_ListItemsDuringActiveSession(t *testing.T) {
	app, bus := setupE2EApp(t)

	workspace := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Claude), workspace, "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	resp := []string{
		`{"type":"system","subtype":"init","session_id":"sess-conc","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
	}
	for i := 0; i < 10; i++ {
		resp = append(resp, testutil.MockClaudeStreamedText(
			fmt.Sprintf("m%d", i),
			fmt.Sprintf("chunk-%d", i),
		)...)
	}
	resp = append(resp, `{"type":"result","subtype":"success","is_error":false}`)

	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{resp})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := app.StartSession(thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := app.SendMessage(thread.ID, "go", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	done := make(chan struct{})
	go func() {
		bus.nextProviderEventOfKind(t, provider.EventTurnComplete, 10*time.Second)
		close(done)
	}()

	const readers = 20
	var wg sync.WaitGroup
	wg.Add(readers)
	stop := make(chan struct{})
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				items, err := app.ListItems(thread.ID, true)
				if err != nil {
					t.Errorf("ListItems: %v", err)
					return
				}
				for _, it := range items {
					if it.ID == "" || it.ThreadID != thread.ID {
						t.Errorf("corrupt item: %+v", it)
						return
					}
				}
			}
		}()
	}

	<-done
	close(stop)
	wg.Wait()

	_ = app.StopSession(thread.ID)
}

// TestConcurrent_SettingsUpdateDuringStartup: concurrent UpdateSettings calls
// alongside a session-start. No panic; the settings file stays consistent.
func TestConcurrent_SettingsUpdateDuringStartup(t *testing.T) {
	app, _ := setupE2EApp(t)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{}})
	if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
		t.Fatalf("initial Update: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := app.StartSession(thread.ID); err != nil {
			t.Errorf("StartSession: %v", err)
		}
	}()

	themes := []string{"system", "light", "dark"}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := app.UpdateSettings(context.Background(), map[string]any{
				"theme": themes[i%len(themes)],
			}); err != nil {
				t.Errorf("UpdateSettings[%d]: %v", i, err)
			}
		}(i)
	}

	wg.Wait()

	got, err := app.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.ClaudeBinaryPath == "" {
		t.Fatalf("claudeBinaryPath lost: %+v", got)
	}
	_ = app.StopSession(thread.ID)
}

// TestConcurrent_ThreadArchiveAndListRace: flip archive state while listers
// poll ListThreads. No duplicates in a single list result; no lost threads.
func TestConcurrent_ThreadArchiveAndListRace(t *testing.T) {
	app, _ := setupE2EApp(t)

	const seed = 20
	var ids []string
	for i := 0; i < seed; i++ {
		th, err := createTestThread(
			t,
			app,
			string(provider.Claude),
			"/tmp/ws-"+fmt.Sprint(i),
			"claude-opus-4-7",
			"chat",
		)
		if err != nil {
			t.Fatalf("CreateThread %d: %v", i, err)
		}
		ids = append(ids, th.ID)
	}

	stopFlipper := make(chan struct{})
	var flipperWG sync.WaitGroup
	flipperWG.Add(1)
	go func() {
		defer flipperWG.Done()
		for i := 0; ; i++ {
			select {
			case <-stopFlipper:
				return
			default:
			}
			id := ids[i%len(ids)]
			if i%2 == 0 {
				_ = app.ArchiveThread(id)
			} else {
				_, _ = app.UnarchiveThread(id)
			}
		}
	}()

	const listers = 10
	const iterationsPerLister = 50
	var listerWG sync.WaitGroup
	listerWG.Add(listers)
	for r := 0; r < listers; r++ {
		go func() {
			defer listerWG.Done()
			for i := 0; i < iterationsPerLister; i++ {
				seen := make(map[string]bool)
				threads, err := app.ListThreads()
				if err != nil {
					t.Errorf("ListThreads: %v", err)
					return
				}
				for _, tt := range threads {
					if seen[tt.ID] {
						t.Errorf("duplicate %q in single List result", tt.ID)
						return
					}
					seen[tt.ID] = true
				}
			}
		}()
	}
	listerWG.Wait()
	close(stopFlipper)
	flipperWG.Wait()

	sort.Strings(ids)
	for _, id := range ids {
		if _, err := app.store.GetThread(id); err != nil {
			t.Fatalf("thread %s lost during race: %v", id, err)
		}
	}
}

// TestConcurrent_ItemInsertDuringSameThread: racing InsertItem calls into
// different turns of the same thread. All inserts must succeed (no panic,
// no lost rows); the store survives.
func TestConcurrent_ItemInsertDuringSameThread(t *testing.T) {
	app, _ := setupE2EApp(t)

	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	const turns = 10
	const perTurn = 5
	var wg sync.WaitGroup
	wg.Add(turns * perTurn)
	for turn := 1; turn <= turns; turn++ {
		for idx := 0; idx < perTurn; idx++ {
			go func(turn, idx int) {
				defer wg.Done()
				it := store.Item{
					ID:        uuid.NewString(),
					ThreadID:  thread.ID,
					TurnIndex: turn,
					ItemIndex: idx,
					Kind:      "user_text",
					Role:      "user",
					Summary:   fmt.Sprintf("t%d-i%d", turn, idx),
					CreatedAt: time.Now().UnixMilli(),
				}
				if err := app.store.InsertItem(it); err != nil {
					t.Errorf("InsertItem turn=%d idx=%d: %v", turn, idx, err)
				}
			}(turn, idx)
		}
	}
	wg.Wait()

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) != turns*perTurn {
		t.Fatalf("items = %d, want %d", len(items), turns*perTurn)
	}
}

// TestConcurrent_StartSessionCoalesces verifies that while one startSession
// call is in flight, any concurrent call for the same thread is coalesced
// onto the inflight entry rather than spawning another provider runtime.
//
// Plan (deterministic):
//  1. Spawn ONE goroutine ("leader") whose startSessionFn blocks until we
//     release it. Wait for the inflight entry to appear.
//  2. THEN spawn 9 more ("followers"). Each will hit beginSessionStart,
//     see the leader's entry, and park on done.
//  3. Release the leader. Leader finishes, followers unblock (all err=nil).
//  4. Assert startSessionFn ran exactly once.
//
// This avoids the "who arrives first" race by ensuring the leader is
// unambiguously in flight before any follower is created.
func TestConcurrent_StartSessionCoalesces(t *testing.T) {
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", "chat")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	var spawnCount atomic.Int32
	release := make(chan struct{})
	app.startSessionFn = func(threadID string) error {
		spawnCount.Add(1)
		<-release
		return nil
	}

	// Launch the leader first.
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- app.startSession(context.Background(), thread.ID)
	}()

	// Wait until the leader has registered its inflight entry. This is
	// guaranteed to happen before startSessionFn starts blocking.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, inflight := app.sessionManager().startState(thread.ID)
		if inflight {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, inflight := app.sessionManager().startState(thread.ID)
	if !inflight {
		t.Fatal("leader never registered an inflight entry")
	}

	// Now launch 9 followers. Each hits beginSessionStart, finds the
	// leader's entry, and parks on done.
	const followers = 9
	var wg sync.WaitGroup
	wg.Add(followers)
	errs := make([]error, followers)
	for i := 0; i < followers; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = app.startSession(context.Background(), thread.ID)
		}(i)
	}

	// Spin briefly to give followers time to reach beginSessionStart. Even
	// if some followers haven't arrived yet, as long as the leader's
	// startSessionFn hasn't returned, the inflight entry is still present
	// — newcomers will become followers too.
	time.Sleep(20 * time.Millisecond)

	close(release)
	<-leaderDone
	wg.Wait()

	if got := spawnCount.Load(); got != 1 {
		t.Fatalf("startSessionFn invocations = %d, want 1 (leader only; followers coalesced)", got)
	}
	for i, e := range errs {
		if e != nil {
			t.Fatalf("follower %d: %v", i, e)
		}
	}
}
