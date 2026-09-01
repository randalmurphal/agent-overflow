package browser

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"testing"
)

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
