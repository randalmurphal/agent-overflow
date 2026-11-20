package browser

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/keybindings"
)

// findFixture is a fake-engine Manager with one page shown in the thread's
// companion. The fake page's text is its title, which its address names.
type findFixture struct {
	manager *Manager
	access  Access
	pageID  string
	mount   MountID
	// seq is the sequence of the fixture's newest start: each start takes
	// the next one, as the pane's provider numbers its starts.
	seq     uint64
	fake    *fakePage
	events  chan CompanionEvent
	escapes chan CompanionEvent
	keys    chan keybindings.Accelerator
	calls   chan string
}

func newFindFixture(t *testing.T, address string) *findFixture {
	t.Helper()
	return newFindFixtureWith(t, address, nil)
}

// newFindFixtureWith is newFindFixture with configure run on the Manager
// before anything else uses it.
func newFindFixtureWith(t *testing.T, address string, configure func(*Manager)) *findFixture {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	if configure != nil {
		configure(manager)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	checkSelectionAtEnd(t, manager)
	f := &findFixture{
		manager: manager,
		access:  manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()}),
		events:  make(chan CompanionEvent, 64),
		escapes: make(chan CompanionEvent, 64),
		keys:    make(chan keybindings.Accelerator, 64),
		calls:   make(chan string, 64),
	}
	manager.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "find" {
			f.events <- event
		}
		if event.Kind == "escape" {
			f.escapes <- event
		}
		if event.Kind == "accelerator" && event.Accelerator != nil {
			f.keys <- *event.Accelerator
		}
	})
	info, err := manager.Open(t.Context(), f.access, address, OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f.pageID = info.ID
	f.fake = f.page(t, info.ID)
	show := true
	if _, err := manager.Visibility(t.Context(), f.access, &show, PageID(info.ID)); err != nil {
		t.Fatalf("show: %v", err)
	}
	f.attach(t)
	return f
}

// place reports the fixture's pane on screen, so it presents the thread's
// active page while the companion is shown.
func (f *findFixture) place(t *testing.T) {
	t.Helper()
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	if err := f.manager.SetPaneRect(f.mount, rect); err != nil {
		t.Fatalf("place the pane: %v", err)
	}
}

// attach mounts a pane for the thread, as the browser pane does.
func (f *findFixture) attach(t *testing.T) {
	t.Helper()
	mount, err := f.manager.AttachPane(f.access.Caller())
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	f.mount = MountID(mount.ID)
}

func (f *findFixture) page(t *testing.T, pageID string) *fakePage {
	t.Helper()
	p, _, err := f.manager.lookupOwnedPage(f.access, PageID(pageID))
	if err != nil {
		t.Fatal(err)
	}
	fake := p.driver.(*fakePage)
	fake.mu.Lock()
	fake.findHook = func(call string) { f.calls <- call }
	fake.mu.Unlock()
	return fake
}

func (f *findFixture) find(t *testing.T, kind, query string) FindState {
	t.Helper()
	seq := f.seq
	if kind == FindStart {
		seq = f.nextSeq()
	}
	state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), kind, query, f.mount, seq)
	if err != nil {
		t.Fatalf("find %s %q: %v", kind, query, err)
	}
	return state
}

// seqFor is the sequence an action of kind names: a new one for a start,
// the newest start's otherwise.
func (f *findFixture) seqFor(kind string) uint64 {
	if kind == FindStart {
		return f.nextSeq()
	}
	return f.seq
}

// nextSeq is the sequence of a new start.
func (f *findFixture) nextSeq() uint64 {
	f.seq++
	return f.seq
}

// session reads the page's find state.
func (f *findFixture) session(t *testing.T) findSession {
	t.Helper()
	p, _, err := f.manager.lookupOwnedPage(f.access, PageID(f.pageID))
	if err != nil {
		t.Fatal(err)
	}
	p.findMu.Lock()
	defer p.findMu.Unlock()
	return p.find
}

func (f *findFixture) wantCall(t *testing.T, want string) {
	t.Helper()
	select {
	case call := <-f.calls:
		if call != want {
			t.Fatalf("engine find call = %q, want %q", call, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("engine find call %q never arrived", want)
	}
}

// noCall asserts the engine was asked nothing more.
func (f *findFixture) noCall(t *testing.T) {
	t.Helper()
	select {
	case call := <-f.calls:
		t.Fatalf("unexpected engine find call %q", call)
	case <-time.After(50 * time.Millisecond):
	}
}

// wantEnded waits for the Manager's own end of the page's find, which a bar
// showing it closes on.
func (f *findFixture) wantEnded(t *testing.T, after uint64) FindState {
	t.Helper()
	return f.wantEndedFor(t, after, string(f.mount))
}

// wantEndedFor is wantEnded for the find mount owned.
func (f *findFixture) wantEndedFor(t *testing.T, after uint64, mount string) FindState {
	t.Helper()
	select {
	case event := <-f.events:
		if event.Find == nil || event.Find.Active || event.Find.PageID != f.pageID || event.Find.Mount != mount || ThreadID(event.ThreadID) != f.access.ThreadID() {
			t.Fatalf("find end event = %+v (%+v)", event, event.Find)
		}
		if event.Find.Session <= after {
			t.Fatalf("find end session %d is not newer than %d", event.Find.Session, after)
		}
		return *event.Find
	case <-time.After(5 * time.Second):
		t.Fatal("the Manager never announced the end of the find")
	}
	return FindState{}
}

func (f *findFixture) noEvent(t *testing.T) {
	t.Helper()
	select {
	case event := <-f.events:
		t.Fatalf("unexpected find event %+v", event.Find)
	default:
	}
}

// findlessPage hides the fake's find: a driver of an engine without one.
type findlessPage struct{ pageDriver }

type findlessEngine struct{ *fakeEngine }

func (e findlessEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	return findlessProfile{profile}, err
}

type findlessProfile struct{ engineProfile }

func (p findlessProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	driver, err := p.engineProfile.NewPage(ctx, hooks)
	return findlessPage{driver}, err
}

// findWaiting reports each find action's context once the action waits for
// its page's lock.
func findWaiting(m *Manager) <-chan context.Context {
	waiting := make(chan context.Context, 16)
	hook := func(ctx context.Context) { waiting <- ctx }
	m.findWaitHook.Store(&hook)
	return waiting
}
