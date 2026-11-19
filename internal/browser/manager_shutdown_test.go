package browser

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// A page asked for while site data is being cleared opens only after both
// clears end: a profile started in between would run on the old cookies and
// have its store deleted, or rewritten, under it.
func TestClearSiteDataHoldsNewPagesUntilBothClearsEnd(t *testing.T) {
	manager, engine := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	access := Access{ThreadID: "t", Workspace: t.TempDir()}
	openPage(t, manager, access, "https://example.test/")

	gate := make(chan struct{})
	engine.mu.Lock()
	engine.clearGate, engine.clearing = gate, make(chan struct{}, 1)
	engine.mu.Unlock()
	cleared := make(chan error, 1)
	go func() { cleared <- manager.ClearSiteData(context.Background()) }()
	<-engine.clearing

	opened := make(chan error, 1)
	go func() {
		_, err := manager.Open(context.Background(), access, "https://example.test/next", OpenOptions{})
		opened <- err
	}()
	select {
	case err := <-opened:
		t.Fatalf("an open finished while the clear ran (err %v); it must wait for the clear", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	if err := <-cleared; err != nil {
		t.Fatalf("clear site data: %v", err)
	}
	if err := <-opened; err != nil {
		t.Fatalf("open after the clear: %v", err)
	}
	if engine.profileInClear.Load() {
		t.Fatal("a profile was created while the site data was being cleared")
	}
}

// A tab a jar switch, or any other path, turns into a suspended page while
// Clear Site Data waits to close the browser is forgotten with the rest: the
// clear leaves no tab, in memory or saved, at an address it cleared.
func TestClearSiteDataForgetsATabSuspendedWhileItWaits(t *testing.T) {
	manager, _ := newJarManager(t, t.TempDir(), Config{Enabled: true, PersistSiteData: true})
	access := Access{ThreadID: "t", Workspace: t.TempDir()}
	openPage(t, manager, access, "https://example.test/old")

	// An engine start in flight holds startMu, which the clear waits for
	// after it forgets the records it finds.
	manager.startMu.Lock()
	cleared := make(chan error, 1)
	go func() { cleared <- manager.ClearSiteData(context.Background()) }()
	eventually(t, "the clear begins", func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.forgettingLocked()
	})
	// The switch keeps its tabs live for the clear to close, or is refused.
	_ = manager.SetJar(context.Background(), access, JarThread, JarSwitchConfirmed)
	// The reaper's suspension and a crash record nothing either.
	if err := manager.SuspendThread(context.Background(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	manager.startMu.Unlock()
	if err := <-cleared; err != nil {
		t.Fatalf("clear site data: %v", err)
	}
	if tabs := manager.CompanionState(access).Pages; len(tabs) != 0 {
		t.Fatalf("after the clear the thread lists tabs %+v", tabs)
	}
	saved, err := os.ReadFile(manager.recordPath(access.ThreadID))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if bytes.Contains(saved, []byte("example.test/old")) {
		t.Fatalf("the saved pages still hold the cleared address: %s", saved)
	}
}

// uiThreadEngine is the fake engine claiming the caller is the UI thread, the
// way the WebKit engines do when Wails runs ServiceShutdown on it. It records
// which goroutine each profile's Dispose ran on.
type uiThreadEngine struct {
	*fakeEngine
	mu         sync.Mutex
	disposedOn []string
}

func (e *uiThreadEngine) OnUIThread() bool { return true }

func (e *uiThreadEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &recordingProfile{engineProfile: profile, owner: e}, nil
}

type recordingProfile struct {
	engineProfile
	owner *uiThreadEngine
}

func (p *recordingProfile) Dispose(ctx context.Context) error {
	p.owner.mu.Lock()
	p.owner.disposedOn = append(p.owner.disposedOn, goroutineID())
	p.owner.mu.Unlock()
	return p.engineProfile.Dispose(ctx)
}

func goroutineID() string {
	buf := make([]byte, 64)
	n := runtime.Stack(buf, false)
	// "goroutine 123 [running]:" — the id is the second field.
	return string(bytes.Fields(buf[:n])[1])
}

// Wails runs ServiceShutdown on the UI thread, which every native call of the
// WebKit engines dispatches to. Close must therefore dispose on the caller when
// the caller IS that thread: fanned out to goroutines, each dispatch would park
// for its full timeout behind the blocked thread and quit would beachball.
func TestCloseDisposesInlineOnTheUIThread(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	engine := &uiThreadEngine{fakeEngine: newFakeEngine()}
	manager.engine = engine
	for _, thread := range []string{"a", "b"} {
		access := Access{ThreadID: thread, Workspace: t.TempDir()}
		if _, err := manager.Open(t.Context(), access, "https://example.test/"+thread, OpenOptions{}); err != nil {
			t.Fatalf("open for %s: %v", thread, err)
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	caller := goroutineID()
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.disposedOn) != 2 {
		t.Fatalf("disposed %d profiles, want 2", len(engine.disposedOn))
	}
	for _, id := range engine.disposedOn {
		if id != caller {
			t.Fatalf("Dispose ran on goroutine %s; the UI-thread caller is %s", id, caller)
		}
	}
}
