package browser

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lifecycleEngine is the recording pane host with gates on the engine work
// behind a tab's lifecycle: a page's creation, a driver's close, a profile's
// disposal, a navigation, an address read and a favicon read. A gate set
// with hold makes every later call wait for its release; the matching
// channel receives one value per call that reached it. onNewPage, when set,
// runs with the page's hooks before NewPage returns, and onNewFakePage with
// its fake engine page.
type lifecycleEngine struct {
	*recordingPaneHost
	mu                                       sync.Mutex
	newPageGate, closeGate, disposeGate      chan struct{}
	navGate, infoGate, evalGate              chan struct{}
	creating, closing, disposing, navigating chan struct{}
	reading, evaluating                      chan struct{}
	newPageErr, disposeErr                   error
	favicon                                  string
	onNewPage                                func(pageHooks)
	onNewFakePage                            func(*fakePage)
	disposed, closed                         atomic.Int64
	disposingNow, profileDuringDispose       atomic.Bool
}

func newLifecycleManager(t *testing.T) (*Manager, *lifecycleEngine) {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = manager.Close() })
	engine := &lifecycleEngine{
		recordingPaneHost: &recordingPaneHost{fakeEngine: manager.engine.(*fakeEngine)},
		creating:          make(chan struct{}, 16),
		closing:           make(chan struct{}, 16),
		disposing:         make(chan struct{}, 16),
		navigating:        make(chan struct{}, 16),
		reading:           make(chan struct{}, 16),
		evaluating:        make(chan struct{}, 16),
	}
	manager.engine = engine
	return manager, engine
}

// hold sets *gate and answers its release.
func (e *lifecycleEngine) hold(gate *chan struct{}) (release func()) {
	ch := make(chan struct{})
	e.mu.Lock()
	*gate = ch
	e.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			e.mu.Lock()
			*gate = nil
			e.mu.Unlock()
			close(ch)
		})
	}
	return release
}

// pass waits at gate when it is set, signalling reached first. ctx ends the
// wait early with its error.
func (e *lifecycleEngine) pass(ctx context.Context, gate *chan struct{}, reached chan struct{}) error {
	e.mu.Lock()
	ch := *gate
	e.mu.Unlock()
	if ch == nil {
		return nil
	}
	reached <- struct{}{}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *lifecycleEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	if e.disposingNow.Load() {
		e.profileDuringDispose.Store(true)
	}
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &lifecycleProfile{engineProfile: profile, engine: e}, nil
}

type lifecycleProfile struct {
	engineProfile
	engine *lifecycleEngine
}

// NewPage ignores ctx at its gate, so a page closed while it is created still
// gets its engine page.
func (p *lifecycleProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	e := p.engine
	_ = e.pass(context.Background(), &e.newPageGate, e.creating)
	e.mu.Lock()
	err := e.newPageErr
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	driver, err := p.engineProfile.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	hook, fakeHook := e.onNewPage, e.onNewFakePage
	e.onNewPage, e.onNewFakePage = nil, nil
	e.mu.Unlock()
	if hook != nil {
		hook(hooks)
	}
	if fakeHook != nil {
		fakeHook(driver.(*fakePage))
	}
	return &lifecycleDriver{pageDriver: driver, engine: e}, nil
}

func (p *lifecycleProfile) Dispose(ctx context.Context) error {
	e := p.engine
	e.disposed.Add(1)
	e.disposingNow.Store(true)
	defer e.disposingNow.Store(false)
	_ = e.pass(context.Background(), &e.disposeGate, e.disposing)
	e.mu.Lock()
	failure := e.disposeErr
	e.mu.Unlock()
	return errors.Join(p.engineProfile.Dispose(ctx), failure)
}

type lifecycleDriver struct {
	pageDriver
	engine *lifecycleEngine
}

func (d *lifecycleDriver) Close() {
	_ = d.engine.pass(context.Background(), &d.engine.closeGate, d.engine.closing)
	d.engine.closed.Add(1)
	d.pageDriver.Close()
}

func (d *lifecycleDriver) Navigate(ctx context.Context, rawURL string) error {
	if err := d.engine.pass(ctx, &d.engine.navGate, d.engine.navigating); err != nil {
		return err
	}
	return d.pageDriver.Navigate(ctx, rawURL)
}

func (d *lifecycleDriver) Info(ctx context.Context) (string, string, error) {
	if err := d.engine.pass(ctx, &d.engine.infoGate, d.engine.reading); err != nil {
		return "", "", err
	}
	return d.pageDriver.Info(ctx)
}

// Evaluate answers the favicon the engine had when the read began, after the
// read's gate.
func (d *lifecycleDriver) Evaluate(ctx context.Context, source string, readOnly bool) (json.RawMessage, error) {
	d.engine.mu.Lock()
	icon := d.engine.favicon
	d.engine.mu.Unlock()
	if err := d.engine.pass(ctx, &d.engine.evalGate, d.engine.evaluating); err != nil {
		return nil, err
	}
	return json.Marshal(icon)
}

// eventLog records the events a Manager publishes.
type eventLog struct{ ch chan CompanionEvent }

func recordEvents(m *Manager) *eventLog {
	log := &eventLog{ch: make(chan CompanionEvent, 4096)}
	m.SetEventSink(func(event CompanionEvent) {
		select {
		case log.ch <- event:
		default:
		}
	})
	return log
}

// waitFor answers the first event, from now on, that match accepts.
func (l *eventLog) waitFor(t *testing.T, what string, match func(CompanionEvent) bool) CompanionEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-l.ch:
			if match(event) {
				return event
			}
		case <-timeout:
			t.Fatalf("no event: %s", what)
		}
	}
}

func receive(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("never reached: %s", what)
	}
}

func tabIDs(m *Manager, access Access) []string {
	var ids []string
	for _, page := range m.CompanionState(access.Caller()).Pages {
		ids = append(ids, page.ID)
	}
	return ids
}

func findTab(m *Manager, access Access, pageID string) (PageInfo, bool) {
	for _, page := range m.CompanionState(access.Caller()).Pages {
		if page.ID == pageID {
			return page, true
		}
	}
	return PageInfo{}, false
}

func async[T any](fn func() T) <-chan T {
	ch := make(chan T, 1)
	go func() { ch <- fn() }()
	return ch
}

func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("never finished: %s", what)
	}
	panic("unreachable")
}

// closeWaiter is ClosePage of a page whose close is running, begun before it
// returns: the close it joins answers it.
func closeWaiter(t *testing.T, m *Manager, access Access, pageID string) <-chan error {
	t.Helper()
	c, err := beginCloseLive(m, access, pageID)
	if err != nil {
		t.Fatalf("join the close of %s: %v", pageID, err)
	}
	return async(func() error { return c.wait(context.Background()) })
}

// A closed tab leaves the state at once, while a tool operation holds the
// page and before the engine has closed it: the operation is cancelled, and
// the engine close runs after it lets go.
func TestAClosedTabLeavesTheStateBeforeItsTeardown(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	a := mustOpen(t, manager, access, "https://example.test/a")
	b := mustOpen(t, manager, access, "https://example.test/b")

	releaseNav := engine.hold(&engine.navGate)
	defer releaseNav()
	navigation := async(func() error {
		_, err := manager.Open(context.Background(), access, "https://example.test/held", OpenOptions{PageID: PageID(a.ID)})
		return err
	})
	receive(t, engine.navigating, "the tool operation")
	releaseClose := engine.hold(&engine.closeGate)
	defer releaseClose()
	events := recordEvents(manager)

	if err := manager.CloseCompanionPage(t.Context(), access, PageID(a.ID)); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := tabIDs(manager, access); !slices.Equal(got, []string{b.ID}) {
		t.Fatalf("tabs right after the close = %v, want only %s", got, b.ID)
	}
	events.waitFor(t, "the state without the closed tab", func(e CompanionEvent) bool {
		return e.Kind == "state" && len(e.Pages) == 1 && e.Pages[0].ID == b.ID
	})
	if err := await(t, navigation, "the held operation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the operation on the closed page = %v, want it cancelled", err)
	}
	receive(t, engine.closing, "the engine close")
	waited := closeWaiter(t, manager, access, a.ID)
	select {
	case err := <-waited:
		t.Fatalf("a waiting close answered %v before the engine closed the page", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseClose()
	if err := await(t, waited, "the waiting close"); err != nil {
		t.Fatalf("waiting close: %v", err)
	}
	if got := engine.closed.Load(); got != 1 {
		t.Fatalf("the engine closed the page %d times", got)
	}
}

// A page still closing holds its thread's slot, and a profile still disposing
// holds its workspace, until the engine is done: close then new does not pass
// the cap or open a second profile beside the one going away.
func TestAClosingPageHoldsItsCapAndProfileUntilDisposed(t *testing.T) {
	t.Run("thread cap", func(t *testing.T) {
		manager, engine := newLifecycleManager(t)
		access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
		var first PageInfo
		for i := range maxPagesPerThread {
			page := mustOpen(t, manager, access, "https://example.test/")
			if i == 0 {
				first = page
			}
		}
		release := engine.hold(&engine.closeGate)
		defer release()
		if err := manager.CloseCompanionPage(t.Context(), access, PageID(first.ID)); err != nil {
			t.Fatal(err)
		}
		receive(t, engine.closing, "the engine close")
		if _, err := manager.Open(t.Context(), access, "https://example.test/next", OpenOptions{}); err == nil || !strings.Contains(err.Error(), "still shutting down") {
			t.Fatalf("open at the cap while a page closes = %v, want the closing refusal", err)
		}
		waited := closeWaiter(t, manager, access, first.ID)
		release()
		if err := await(t, waited, "the close"); err != nil {
			t.Fatal(err)
		}
		mustOpen(t, manager, access, "https://example.test/next")
	})

	t.Run("workspace profile", func(t *testing.T) {
		manager, engine := newLifecycleManager(t)
		access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
		last := mustOpen(t, manager, access, "https://example.test/")
		release := engine.hold(&engine.disposeGate)
		defer release()
		if err := manager.CloseCompanionPage(t.Context(), access, PageID(last.ID)); err != nil {
			t.Fatal(err)
		}
		receive(t, engine.disposing, "the profile's disposal")
		if got := tabIDs(manager, access); len(got) != 0 {
			t.Fatalf("tabs while the profile disposes = %v", got)
		}
		opened := async(func() error {
			_, err := manager.Open(context.Background(), access, "https://example.test/next", OpenOptions{})
			return err
		})
		select {
		case err := <-opened:
			t.Fatalf("an open answered %v while its workspace's profile was disposing", err)
		case <-time.After(20 * time.Millisecond):
		}
		release()
		if err := await(t, opened, "the open after the disposal"); err != nil {
			t.Fatal(err)
		}
		if engine.profileDuringDispose.Load() {
			t.Fatal("a profile was created while the workspace's last one was disposing")
		}
		if got := engine.disposed.Load(); got != 1 {
			t.Fatalf("%d disposals, want 1", got)
		}
	})
}

// A teardown that fails after the tab left reaches the user as a teardown
// event, and a caller that waits gets the error.
func TestATeardownFailureIsReported(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	engine.disposeErr = errors.New("the profile's browser would not stop")
	events := recordEvents(manager)
	for _, wait := range []bool{false, true} {
		access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
		last := mustOpen(t, manager, access, "https://example.test/")
		var err error
		if wait {
			err = manager.ClosePage(t.Context(), access, PageID(last.ID))
		} else {
			err = manager.CloseCompanionPage(t.Context(), access, PageID(last.ID))
		}
		if wait != errors.Is(err, engine.disposeErr) {
			t.Fatalf("close (waiting %v) = %v", wait, err)
		}
		event := events.waitFor(t, "the teardown failure", func(e CompanionEvent) bool { return e.Kind == "teardown" })
		if ThreadID(event.ThreadID) != access.ThreadID() || event.PageID != last.ID || !strings.Contains(event.Error, "would not stop") {
			t.Fatalf("teardown event = %+v", event)
		}
	}
}

// Closing a tab again while its close runs answers that close: no second
// engine close, one teardown report, and every waiter gets its outcome.
func TestRepeatedClosesShareOneTeardown(t *testing.T) {
	manager, engine := newLifecycleManager(t)
	engine.disposeErr = errors.New("would not stop")
	events := recordEvents(manager)
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/")
	release := engine.hold(&engine.closeGate)
	defer release()
	for range 2 {
		if err := manager.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	receive(t, engine.closing, "the engine close")
	waiters := []<-chan error{
		closeWaiter(t, manager, access, page.ID),
		closeWaiter(t, manager, access, page.ID),
	}
	if err := manager.ClosePage(t.Context(), manager.Bind(Caller{ThreadID: "other", Workspace: access.Workspace()}), PageID(page.ID)); !errors.Is(err, errPageNotFound) {
		t.Fatalf("another thread's close of the closing page = %v", err)
	}
	release()
	for _, waiter := range waiters {
		if err := await(t, waiter, "a waiting close"); !errors.Is(err, engine.disposeErr) {
			t.Fatalf("waiting close = %v, want the teardown failure", err)
		}
	}
	if got := engine.closed.Load(); got != 1 {
		t.Fatalf("the engine closed the page %d times", got)
	}
	events.waitFor(t, "the teardown failure", func(e CompanionEvent) bool { return e.Kind == "teardown" })
	teardowns := 1
	for len(events.ch) > 0 {
		if event := <-events.ch; event.Kind == "teardown" {
			teardowns++
		}
	}
	if teardowns != 1 {
		t.Fatalf("%d teardown reports, want 1", teardowns)
	}
	if err := manager.ClosePage(t.Context(), access, PageID(page.ID)); !errors.Is(err, errPageNotFound) {
		t.Fatalf("close after the teardown = %v", err)
	}
}
