package browser

import (
	"context"
	"errors"
	"strings"
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
	mount   string
	fake    *fakePage
	events  chan CompanionEvent
	keys    chan keybindings.Accelerator
	calls   chan string
}

func newFindFixture(t *testing.T, address string) *findFixture {
	t.Helper()
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	f := &findFixture{
		manager: manager,
		access:  Access{ThreadID: "thread", Workspace: t.TempDir()},
		events:  make(chan CompanionEvent, 64),
		keys:    make(chan keybindings.Accelerator, 64),
		calls:   make(chan string, 64),
	}
	manager.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "find" {
			f.events <- event
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
	if _, err := manager.Visibility(t.Context(), f.access, &show, info.ID); err != nil {
		t.Fatalf("show: %v", err)
	}
	f.attach(t)
	return f
}

// attach mounts a pane for the thread, as the browser pane does.
func (f *findFixture) attach(t *testing.T) {
	t.Helper()
	mount, err := f.manager.AttachPane(f.access)
	if err != nil {
		t.Fatalf("attach pane: %v", err)
	}
	f.mount = mount.ID
}

func (f *findFixture) page(t *testing.T, pageID string) *fakePage {
	t.Helper()
	p, _, err := f.manager.lookupOwnedPage(f.access, pageID)
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
	state, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, query)
	if err != nil {
		t.Fatalf("find %s %q: %v", kind, query, err)
	}
	return state
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
	select {
	case event := <-f.events:
		if event.Find == nil || event.Find.Active || event.Find.PageID != f.pageID || event.ThreadID != f.access.ThreadID {
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

func TestFindStartsStepsWrapsAndStops(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha-beta-alpha")
	if mode := f.manager.CompanionState(f.access).FindMode; mode != FindModeBar {
		t.Fatalf("state find mode = %q, want %q", mode, FindModeBar)
	}

	started := f.find(t, "start", "ALPHA")
	f.wantCall(t, "start ALPHA")
	if !started.Active || !started.Found || started.Matches != 2 || started.Current != 0 || started.Query != "ALPHA" || started.PageID != f.pageID {
		t.Fatalf("start = %+v", started)
	}
	next := f.find(t, "next", "")
	f.wantCall(t, "step")
	if next.Current != 1 || next.Session != started.Session {
		t.Fatalf("next = %+v", next)
	}
	if wrapped := f.find(t, "next", ""); wrapped.Current != 0 {
		t.Fatalf("next past the last match = %+v, want a wrap to the first", wrapped)
	}
	f.wantCall(t, "step")
	if back := f.find(t, "previous", ""); back.Current != 1 {
		t.Fatalf("previous before the first match = %+v, want a wrap to the last", back)
	}
	f.wantCall(t, "back")

	stopped := f.find(t, "stop", "")
	f.wantCall(t, "stop")
	if stopped.Active || stopped.Session <= started.Session {
		t.Fatalf("stop = %+v", stopped)
	}
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", ""); err == nil || !strings.Contains(err.Error(), "no find") {
		t.Fatalf("next after stop error = %v", err)
	}
	// The caller ended it; nothing is announced.
	f.noEvent(t)

	missing := f.find(t, "start", "gamma")
	if missing.Found || missing.Matches != 0 || missing.Current != -1 || !missing.Active {
		t.Fatalf("start without matches = %+v", missing)
	}
	// An empty query ends the session instead of searching for nothing.
	if empty := f.find(t, "start", ""); empty.Active {
		t.Fatalf("empty start = %+v", empty)
	}
}

func TestFindRefusesBadRequests(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	ctx := t.Context()
	for _, tc := range []struct {
		name, pageID, kind, query, want string
	}{
		{"unknown kind", f.pageID, "replace", "a", "find action"},
		{"no page", "", "start", "a", "page id is required"},
		{"query too long", f.pageID, "start", strings.Repeat("a", maxFindQueryBytes+1), "query exceeds"},
	} {
		if _, err := f.manager.Find(ctx, f.access, tc.pageID, tc.kind, tc.query); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
	intruder := Access{ThreadID: "intruder", Workspace: f.access.Workspace}
	if _, err := f.manager.Find(ctx, intruder, f.pageID, "start", "alpha"); !errors.Is(err, errPageNotFound) {
		t.Fatalf("another thread's find error = %v, want page not found", err)
	}
	select {
	case call := <-f.calls:
		t.Fatalf("a refused find reached the engine: %q", call)
	default:
	}
}

// Find runs on what the user is looking at: the active page of a shown
// companion.
func TestFindRunsOnlyOnTheShownActivePage(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	// A new tab becomes the active one.
	if _, err := f.manager.NewCompanionPage(t.Context(), f.access); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha"); err == nil || !strings.Contains(err.Error(), "shows") {
		t.Fatalf("find on a background tab error = %v", err)
	}
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	hide := false
	if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha"); err == nil || !strings.Contains(err.Error(), "shows") {
		t.Fatalf("find with the companion hidden error = %v", err)
	}
}

func TestFindEndsWhenThePageNavigates(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")

	// A fragment move keeps the document.
	if _, err := f.manager.Open(t.Context(), f.access, "https://example.test/alpha#part", OpenOptions{PageID: f.pageID}); err != nil {
		t.Fatal(err)
	}
	f.noEvent(t)

	if _, err := f.manager.Open(t.Context(), f.access, "https://example.test/beta", OpenOptions{PageID: f.pageID}); err != nil {
		t.Fatal(err)
	}
	ended := f.wantEnded(t, started.Session)
	f.wantCall(t, "stop")
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", ""); err == nil {
		t.Fatalf("next after the navigation ended the find succeeded (session %d)", ended.Session)
	}

	// A navigation the engine reports on its own ends it the same way.
	restarted := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	f.manager.updatePageInfo(f.fake.Handle(), "https://example.test/gamma", "gamma")
	f.wantEnded(t, restarted.Session)
	f.wantCall(t, "stop")
	// A title change is not a navigation.
	again := f.find(t, "start", "gamma")
	f.wantCall(t, "start gamma")
	f.manager.updatePageInfo(f.fake.Handle(), "https://example.test/gamma", "renamed")
	f.noEvent(t)
	if state := f.find(t, "next", ""); state.Session != again.Session {
		t.Fatalf("a title change ended the find: %+v", state)
	}
}

// The engine stop that follows an end never cancels a find started after it.
func TestFindEndDoesNotStopANewerSession(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
	if err != nil {
		t.Fatal(err)
	}
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	// Hold the page so the engine stop queues behind the next start.
	p.mu.Lock()
	f.manager.updatePageInfo(f.fake.Handle(), "https://example.test/beta", "beta")
	f.wantEnded(t, 0)
	p.mu.Unlock()
	restarted := f.find(t, "start", "beta")
	// Whichever ran first, the engine's last word is the new search.
	first := <-f.calls
	if first == "stop" {
		f.wantCall(t, "start beta")
	} else if first != "start beta" {
		t.Fatalf("engine call = %q", first)
	}
	if state := f.find(t, "next", ""); !state.Active || state.Session != restarted.Session {
		t.Fatalf("the late stop ended the new find: %+v", state)
	}
	f.wantCall(t, "step")
	select {
	case call := <-f.calls:
		t.Fatalf("unexpected engine call %q after the new find", call)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestFindEndsWhenThePageIsHiddenOrReplaced(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	other, err := f.manager.NewCompanionPage(t.Context(), f.access)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, other.ID); err != nil {
		t.Fatal(err)
	}
	ended := f.wantEnded(t, started.Session)
	f.wantCall(t, "stop")

	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	restarted := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if restarted.Session <= ended.Session {
		t.Fatalf("restart session %d is not newer than the end %d", restarted.Session, ended.Session)
	}
	hide := false
	if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
		t.Fatal(err)
	}
	f.wantEnded(t, restarted.Session)
	f.wantCall(t, "stop")
}

func TestFindEndsWhenThePageClosesOrSuspends(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID); err != nil {
		t.Fatal(err)
	}
	f.wantEnded(t, started.Session)
	// The page is gone; there is no engine find to stop.
	select {
	case call := <-f.calls:
		t.Fatalf("a suspended page was asked to %q", call)
	case <-time.After(50 * time.Millisecond):
	}

	// Presenting it restores it; then close it mid-find.
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	f.fake = f.page(t, f.pageID)
	gate := make(chan struct{})
	entered := make(chan struct{})
	f.fake.mu.Lock()
	f.fake.findHook = func(call string) {
		if strings.HasPrefix(call, "start") {
			close(entered)
			<-gate
		}
	}
	f.fake.mu.Unlock()
	type answer struct {
		state FindState
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		state, err := f.manager.Find(context.Background(), f.access, f.pageID, "start", "alpha")
		answered <- answer{state, err}
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- f.manager.ClosePage(context.Background(), f.access, f.pageID) }()
	close(gate)
	got := <-answered
	if got.err != nil || !got.state.Active {
		t.Fatalf("find racing the close = %+v, %v", got.state, got.err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	// The close's end is newer than the answer it overtook.
	f.wantEnded(t, got.state.Session)
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", ""); !errors.Is(err, errPageNotFound) {
		t.Fatalf("find on a closed page error = %v", err)
	}
}

// The find shows in a mounted pane: with none it cannot start, and the last
// pane unmounting ends it, each time.
func TestFindEndsWhenItsPaneUnmounts(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	for round := range 2 {
		started := f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		f.manager.DetachPane(f.mount)
		f.wantEnded(t, started.Session)
		f.wantCall(t, "stop")
		if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha"); err == nil {
			t.Fatalf("round %d: a find started with no pane mounted", round)
		}
		f.attach(t)
	}
}

// Each transition that takes the find's page away ends the find and, where
// the page lives on, stops the engine's find on it; a find started after
// coming back works and ends the same way.
func TestFindEndsOnEachTransitionTwice(t *testing.T) {
	for _, tc := range []struct {
		name string
		// away ends the find; back restores the shown page.
		away, back func(*testing.T, *findFixture)
		// stops is whether the page lives on for an engine stop.
		stops bool
	}{
		{
			name: "navigate",
			away: func(t *testing.T, f *findFixture) {
				if _, err := f.manager.Open(t.Context(), f.access, "https://example.test/beta", OpenOptions{PageID: f.pageID}); err != nil {
					t.Fatal(err)
				}
			},
			back: func(t *testing.T, f *findFixture) {
				if _, err := f.manager.Open(t.Context(), f.access, "https://example.test/alpha", OpenOptions{PageID: f.pageID}); err != nil {
					t.Fatal(err)
				}
			},
			stops: true,
		},
		{
			name: "switch page",
			away: func(t *testing.T, f *findFixture) {
				other, err := f.manager.NewCompanionPage(t.Context(), f.access)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.manager.ActivateCompanionPage(t.Context(), f.access, other.ID); err != nil {
					t.Fatal(err)
				}
			},
			back: func(t *testing.T, f *findFixture) {
				if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
					t.Fatal(err)
				}
			},
			stops: true,
		},
		{
			name: "close pane",
			away: func(t *testing.T, f *findFixture) {
				hide := false
				if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
					t.Fatal(err)
				}
			},
			back: func(t *testing.T, f *findFixture) {
				show := true
				if _, err := f.manager.Visibility(t.Context(), f.access, &show, f.pageID); err != nil {
					t.Fatal(err)
				}
			},
			stops: true,
		},
		{
			name: "suspend",
			away: func(t *testing.T, f *findFixture) {
				if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID); err != nil {
					t.Fatal(err)
				}
			},
			back: func(t *testing.T, f *findFixture) {
				if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
					t.Fatal(err)
				}
				f.fake = f.page(t, f.pageID)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			for round := range 2 {
				started := f.find(t, "start", "alpha")
				f.wantCall(t, "start alpha")
				tc.away(t, f)
				f.wantEnded(t, started.Session)
				if tc.stops {
					f.wantCall(t, "stop")
				}
				f.noCall(t)
				tc.back(t, f)
				if round == 0 {
					continue
				}
				if state := f.find(t, "start", "alpha"); !state.Active || state.Matches != 1 {
					t.Fatalf("find after coming back = %+v", state)
				}
				f.wantCall(t, "start alpha")
			}
		})
	}
}

// Closing every page at once (disabling the browser, clearing site data)
// ends each find the same way one page's close does.
func TestFindEndsWhenTheBrowserCloses(t *testing.T) {
	for name, close := range map[string]func(*findFixture) error{
		"disable": func(f *findFixture) error { return f.manager.Reconfigure(Config{Enabled: false}) },
		"clear":   func(f *findFixture) error { return f.manager.ClearSiteData(t.Context()) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			started := f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			if err := close(f); err != nil {
				t.Fatal(err)
			}
			f.wantEnded(t, started.Session)
		})
	}
}

// A bare Escape pressed in the page is the page's, except while the page has
// a find: then it is claimed and routed to the thread so `find.close` ends
// the find with keyboard focus still in the page. Every end of the find
// releases it, and a modified Escape is never claimed by the find.
func TestFindClaimsThePagesEscapeOnlyWhileFinding(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	handle := f.fake.Handle()
	escape := keybindings.Accelerator{Key: "escape"}
	claims := func() bool { return f.manager.keyChord(handle, escape) }
	if claims() {
		t.Fatal("Escape was claimed with no find")
	}
	for round := range 2 {
		f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		if f.manager.keyChord(handle, keybindings.Accelerator{Key: "escape", Ctrl: true}) {
			t.Fatal("Ctrl+Escape was claimed by the find")
		}
		if !claims() {
			t.Fatalf("round %d: Escape was not claimed during a find", round)
		}
		select {
		case key := <-f.keys:
			if key != escape {
				t.Fatalf("routed chord = %+v", key)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the claimed Escape never reached the thread")
		}
		f.find(t, "stop", "")
		f.wantCall(t, "stop")
		if claims() {
			t.Fatalf("round %d: Escape was claimed after the find stopped", round)
		}
	}
	// The Manager's own ends release it too.
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID); err != nil {
		t.Fatal(err)
	}
	f.wantEnded(t, started.Session)
	if claims() {
		t.Fatal("Escape was claimed from a suspended page")
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

func TestFindIsRefusedWhereTheEngineHasNone(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	manager.engine = findlessEngine{manager.engine.(*fakeEngine)}
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	info, err := manager.Open(t.Context(), access, "https://example.test/alpha", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, info.ID); err != nil {
		t.Fatal(err)
	}
	if mode := manager.CompanionState(access).FindMode; mode != "" {
		t.Fatalf("state find mode = %q on an engine without find", mode)
	}
	if _, err := manager.Find(t.Context(), access, info.ID, "start", "alpha"); !errors.Is(err, errFindUnavailable) {
		t.Fatalf("find error = %v, want unavailable", err)
	}

	// A page whose engine runtime lacks the API says so the same way.
	f := newFindFixture(t, "https://example.test/alpha")
	f.fake.mu.Lock()
	f.fake.findMode = ""
	f.fake.mu.Unlock()
	if mode := f.manager.CompanionState(f.access).FindMode; mode != "" {
		t.Fatalf("state find mode = %q for a page without find", mode)
	}
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha"); !errors.Is(err, errFindUnavailable) {
		t.Fatalf("find error = %v, want unavailable", err)
	}
}

// An engine that only says whether there is a match leaves the count and
// position unknown rather than inventing them.
func TestFindKeepsAnUncountedAnswerUncounted(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.fake.mu.Lock()
	f.fake.findAnswer = func(findResult) findResult { return findResult{Found: true, Matches: -1} }
	f.fake.mu.Unlock()
	for _, kind := range []string{"start", "next", "previous"} {
		state := f.find(t, kind, "alpha")
		if !state.Found || state.Matches != -1 || state.Current != -1 {
			t.Fatalf("%s = %+v", kind, state)
		}
	}
}

// Past a capped count the position is not known, and stepping does not
// pretend to wrap.
func TestFindStepsPastACappedCountAsUnknown(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.fake.mu.Lock()
	f.fake.findAnswer = func(findResult) findResult { return findResult{Found: true, Matches: 2, Capped: true} }
	f.fake.mu.Unlock()
	if state := f.find(t, "start", "a"); state.Current != 0 || !state.Capped {
		t.Fatalf("start = %+v", state)
	}
	if state := f.find(t, "next", ""); state.Current != 1 {
		t.Fatalf("next = %+v", state)
	}
	if state := f.find(t, "next", ""); state.Current != -1 {
		t.Fatalf("next past the cap = %+v, want unknown", state)
	}
	if state := f.find(t, "previous", ""); state.Current != -1 {
		t.Fatalf("previous from unknown = %+v", state)
	}
	if state := f.find(t, "start", "a"); state.Current != 0 {
		t.Fatalf("a new start = %+v", state)
	}
	if state := f.find(t, "previous", ""); state.Current != -1 {
		t.Fatalf("previous before the first of a capped count = %+v, want unknown", state)
	}
}

// The engine's own find bar is opened, never stepped, and an empty open
// seeds it with the page's selection.
func TestFindInTheEnginesOwnBar(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.fake.mu.Lock()
	f.fake.findMode = FindModeEngine
	f.fake.selection = "alpha\nbeta"
	f.fake.mu.Unlock()
	if mode := f.manager.CompanionState(f.access).FindMode; mode != FindModeEngine {
		t.Fatalf("state find mode = %q", mode)
	}
	if state := f.find(t, "start", ""); !state.Active {
		t.Fatalf("open = %+v", state)
	}
	// A selection over two lines is not a search term.
	f.wantCall(t, "start ")
	f.fake.mu.Lock()
	f.fake.selection = "  alpha beta "
	f.fake.mu.Unlock()
	f.find(t, "start", "")
	f.wantCall(t, "start alpha beta")
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", ""); err == nil || !strings.Contains(err.Error(), "own find bar") {
		t.Fatalf("stepping the engine's bar error = %v", err)
	}
	f.find(t, "stop", "")
	f.wantCall(t, "stop")
}

func TestFindEngineFailureEndsTheSession(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	f.fake.mu.Lock()
	f.fake.findErr = errors.New("engine went away")
	f.fake.mu.Unlock()
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", ""); err == nil || !strings.Contains(err.Error(), "engine went away") {
		t.Fatalf("failed step error = %v", err)
	}
	f.wantCall(t, "step")
	f.fake.mu.Lock()
	f.fake.findErr = nil
	f.fake.mu.Unlock()
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", ""); err == nil {
		t.Fatal("a failed step left the session running")
	}
	if state := f.find(t, "start", "alpha"); state.Session <= started.Session+1 {
		t.Fatalf("restart after a failure = %+v", state)
	}
}
