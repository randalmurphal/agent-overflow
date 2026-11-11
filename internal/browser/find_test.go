package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	// A search in AO's bar runs in the mount's open bar: the pane opens it
	// before its first query.
	if kind == FindStart && query != "" && f.fake.FindMode() == FindModeBar && !f.barOpen(t) {
		if _, err := f.manager.Find(t.Context(), f.access, f.pageID, FindOpen, "", f.mount); err != nil {
			t.Fatalf("open the bar for start %q: %v", query, err)
		}
	}
	state, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, query, f.mount)
	if err != nil {
		t.Fatalf("find %s %q: %v", kind, query, err)
	}
	return state
}

// barOpen reports whether the fixture's mount holds the page's bar.
func (f *findFixture) barOpen(t *testing.T) bool {
	t.Helper()
	p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
	if err != nil {
		return false
	}
	p.findMu.Lock()
	defer p.findMu.Unlock()
	return p.find.bar && p.find.owner == f.mount
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
	return f.wantEndedFor(t, after, f.mount)
}

// wantEndedFor is wantEnded for the find mount owned.
func (f *findFixture) wantEndedFor(t *testing.T, after uint64, mount string) FindState {
	t.Helper()
	select {
	case event := <-f.events:
		if event.Find == nil || event.Find.Active || event.Find.PageID != f.pageID || event.Find.Mount != mount || event.ThreadID != f.access.ThreadID {
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
	// The bar stays open with no search: a step answers that, asking the
	// engine nothing.
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount); err != nil || state.Active || state.Foreign {
		t.Fatalf("next after stop = %+v, %v", state, err)
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
		if _, err := f.manager.Find(ctx, f.access, tc.pageID, tc.kind, tc.query, f.mount); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
	intruder := Access{ThreadID: "intruder", Workspace: f.access.Workspace}
	if _, err := f.manager.Find(ctx, intruder, f.pageID, "start", "alpha", f.mount); !errors.Is(err, errPageNotFound) {
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "open", "", f.mount); err == nil || !strings.Contains(err.Error(), "shows") {
		t.Fatalf("find on a background tab error = %v", err)
	}
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	hide := false
	if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "open", "", f.mount); err == nil || !strings.Contains(err.Error(), "shows") {
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
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount); err != nil || !state.Foreign {
		t.Fatalf("next after the navigation ended the find (session %d) = %+v, %v", ended.Session, state, err)
	}

	// A reload at the same address is a new document: the old count is
	// stale.
	restarted := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	if _, err := f.manager.History(t.Context(), f.access, f.pageID, "reload"); err != nil {
		t.Fatal(err)
	}
	f.wantEnded(t, restarted.Session)
	f.wantCall(t, "stop")

	// A same-document navigation (history.pushState), and the address and
	// title change the engine reports for it, keep the find.
	again := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	f.fake.SameDocumentNavigation("https://example.test/beta/next")
	f.manager.updatePageInfo(f.fake.Handle(), "https://example.test/beta/next", "renamed")
	f.noEvent(t)
	if state := f.find(t, "next", ""); !state.Active || state.Session != again.Session {
		t.Fatalf("a same-document navigation ended the find: %+v", state)
	}
	f.wantCall(t, "step")

	// A commit the engine reports on its own ends it the same way.
	f.fake.commitDocument()
	f.wantEnded(t, again.Session)
	f.wantCall(t, "stop")
}

// An end submitted while the page is busy is applied, with the engine stop
// it owes, before any action that comes after it: the stop never lands on
// a search started later.
func TestFindEndDoesNotStopANewerSession(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
	if err != nil {
		t.Fatal(err)
	}
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	// Hold the page so the end waits behind the page's operation.
	p.mu.Lock()
	f.fake.commitDocument()
	f.noEvent(t)
	p.mu.Unlock()
	f.wantEnded(t, 0)
	f.wantCall(t, "stop")
	restarted := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
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
	f.find(t, "open", "")
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
		state, err := f.manager.Find(context.Background(), f.access, f.pageID, "start", "alpha", f.mount)
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount); !errors.Is(err, errPageNotFound) {
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
		if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "open", "", f.mount); err == nil {
			t.Fatalf("round %d: a find opened with no pane mounted", round)
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

// A bare Escape pressed in the page is the page's, except while AO's find bar
// is open on it: then it is claimed and routed to the thread so `find.close`
// closes the bar with keyboard focus still in the page. An open bar claims
// it with no search (an empty or cleared query), a closed bar releases it,
// and a modified Escape is never claimed.
func TestFindBarClaimsThePagesEscapeWhileOpen(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	handle := f.fake.Handle()
	escape := keybindings.Accelerator{Key: "escape"}
	claims := func() bool { return f.manager.keyChord(handle, escape) }
	routed := func(t *testing.T) {
		t.Helper()
		select {
		case key := <-f.keys:
			if key != escape {
				t.Fatalf("routed chord = %+v", key)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the claimed Escape never reached the thread")
		}
	}
	if claims() {
		t.Fatal("Escape was claimed with no bar")
	}
	for round := range 2 {
		f.find(t, "open", "")
		f.noCall(t)
		if !claims() {
			t.Fatalf("round %d: Escape was not claimed by an empty open bar", round)
		}
		routed(t)
		if f.manager.keyChord(handle, keybindings.Accelerator{Key: "escape", Ctrl: true}) {
			t.Fatal("Ctrl+Escape was claimed by the bar")
		}
		f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		// The query cleared: the search ends, the bar stays open.
		if state := f.find(t, "start", ""); state.Active {
			t.Fatalf("round %d: empty query = %+v", round, state)
		}
		f.wantCall(t, "stop")
		if !claims() {
			t.Fatalf("round %d: Escape was not claimed after the query cleared", round)
		}
		routed(t)
		f.find(t, "close", "")
		f.noCall(t)
		if claims() {
			t.Fatalf("round %d: Escape was claimed after the bar closed", round)
		}
	}
	// Opening is for a mount of the thread's own.
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "open", "", "no-such-mount"); err == nil {
		t.Fatal("a bar opened for an unknown pane mount")
	}
	if claims() {
		t.Fatal("a refused open claimed Escape")
	}
}

// Every end of the find releases the bar's claim, so a page's Escape is
// never swallowed after the bar is gone: a new document, a page switch, a
// hidden companion, a suspend, a page close, and the bar's pane unmounting
// (which connection teardown does too), each twice. The end is announced so
// the bar closes.
func TestFindBarClaimEndsWithEveryEnding(t *testing.T) {
	for _, tc := range []struct {
		name string
		away func(*testing.T, *findFixture)
		back func(*testing.T, *findFixture)
	}{
		{
			name: "new document",
			away: func(t *testing.T, f *findFixture) { f.fake.commitDocument() },
			back: func(*testing.T, *findFixture) {},
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
		},
		{
			name: "hide companion",
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
		{
			name: "close page",
			away: func(t *testing.T, f *findFixture) {
				if err := f.manager.ClosePage(t.Context(), f.access, f.pageID); err != nil {
					t.Fatal(err)
				}
			},
			back: func(t *testing.T, f *findFixture) {
				info, err := f.manager.Open(t.Context(), f.access, "https://example.test/alpha", OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				f.pageID = info.ID
				f.fake = f.page(t, info.ID)
				show := true
				if _, err := f.manager.Visibility(t.Context(), f.access, &show, info.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// A second mount (another client) stays, so only the bar's own
			// mount going away ends it.
			name: "bar's pane unmounts",
			away: func(t *testing.T, f *findFixture) {
				bar := f.mount
				f.attach(t)
				f.manager.DetachPane(bar)
			},
			back: func(*testing.T, *findFixture) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			for round := range 2 {
				handle := f.fake.Handle()
				opened := f.find(t, "open", "")
				started := f.find(t, "start", "alpha")
				f.wantCall(t, "start alpha")
				owner := f.mount
				tc.away(t, f)
				f.wantEndedFor(t, max(opened.Session, started.Session), owner)
				if f.manager.keyChord(handle, findEscape) {
					t.Fatalf("round %d: the page's Escape was claimed after the find ended", round)
				}
				tc.back(t, f)
				if f.manager.keyChord(f.fake.Handle(), findEscape) {
					t.Fatalf("round %d: the page's Escape was claimed after coming back", round)
				}
				// Drain the engine stop an end left on a page that lives on.
				for drained := false; !drained; {
					select {
					case <-f.calls:
					case <-time.After(100 * time.Millisecond):
						drained = true
					}
				}
			}
		})
	}
}

// The engine's own bar closes without telling AO, so it is never AO's open
// bar: an open is refused, a start in it never claims the page's Escape, and
// one the user typed after closing that bar always reaches the page.
func TestFindInTheEnginesBarNeverClaimsThePagesEscape(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	handle := f.fake.Handle()
	f.fake.mu.Lock()
	f.fake.findMode = FindModeEngine
	f.fake.mu.Unlock()
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "open", "", f.mount); err == nil {
		t.Fatal("AO's bar opened on a page whose engine shows its own")
	}
	for round := range 2 {
		if state := f.find(t, "start", "alpha"); !state.Active {
			t.Fatalf("round %d: open = %+v", round, state)
		}
		f.wantCall(t, "start alpha")
		if f.manager.keyChord(handle, findEscape) {
			t.Fatalf("round %d: Escape was claimed while the engine's bar may be open", round)
		}
	}
	f.find(t, "close", "")
	f.wantCall(t, "stop")
	if f.manager.keyChord(handle, findEscape) {
		t.Fatal("Escape was claimed after the engine's find stopped")
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
	pane, err := manager.AttachPane(access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Find(t.Context(), access, info.ID, "start", "alpha", pane.ID); !errors.Is(err, errFindUnavailable) {
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount); !errors.Is(err, errFindUnavailable) {
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount); err == nil || !strings.Contains(err.Error(), "own find bar") {
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount); err == nil || !strings.Contains(err.Error(), "engine went away") {
		t.Fatalf("failed step error = %v", err)
	}
	f.wantCall(t, "step")
	f.fake.mu.Lock()
	f.fake.findErr = nil
	f.fake.mu.Unlock()
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount); err != nil || state.Active {
		t.Fatalf("a failed step left the session running: %+v, %v", state, err)
	}
	if state := f.find(t, "start", "alpha"); !state.Active || state.Session <= started.Session {
		t.Fatalf("restart after a failure = %+v", state)
	}
}

// A find that resolved its page and then waited for the page's lock while
// the page closed never reaches the closed driver: the native view is gone.
func TestFindNeverCallsTheDriverOfAPageClosedWhileItWaited(t *testing.T) {
	for _, kind := range []string{"open", "start", "next"} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			if kind == "next" {
				f.find(t, "start", "alpha")
				f.wantCall(t, "start alpha")
			}
			p, scope, err := f.manager.lookupOwnedPage(f.access, f.pageID)
			if err != nil {
				t.Fatal(err)
			}
			p.mu.Lock()
			answered := make(chan error, 1)
			go func() {
				_, err := f.manager.Find(context.Background(), f.access, f.pageID, kind, "alpha", f.mount)
				answered <- err
			}()
			// Let the find resolve the page and block on its lock, then tear
			// the page down under that lock, as closing does.
			time.Sleep(100 * time.Millisecond)
			p.driver.Close()
			f.manager.mu.Lock()
			delete(scope.pages, p.id)
			f.manager.mu.Unlock()
			p.mu.Unlock()
			if err := <-answered; !errors.Is(err, errPageNotFound) {
				t.Fatalf("find on a page closed while it waited: %v", err)
			}
			f.noCall(t)
		})
	}
}

// An end that lands while a start's engine call runs (the page is switched
// away, or commits another document) is applied after the start: the start
// answers its search, and the end that follows is newer, closes the bar,
// releases the page's Escape and stops the search.
func TestFindEndDuringAStartLandsAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*testing.T, *findFixture)
	}{
		{"switch page", func(t *testing.T, f *findFixture) {
			other, err := f.manager.NewCompanionPage(t.Context(), f.access)
			if err != nil {
				t.Error(err)
				return
			}
			if err := f.manager.ActivateCompanionPage(t.Context(), f.access, other.ID); err != nil {
				t.Error(err)
			}
		}},
		{"new document", func(t *testing.T, f *findFixture) { f.fake.commitDocument() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			handle := f.fake.Handle()
			f.find(t, "open", "")
			var once sync.Once
			setFindPoint(f.manager, func(point string) {
				if point == "engine" {
					once.Do(func() { tc.end(t, f) })
				}
			})
			got, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount)
			if err != nil || !got.Active {
				t.Fatalf("start with an end landing during it = %+v, %v", got, err)
			}
			end := f.wantEnded(t, got.Session)
			if end.Session <= got.Session {
				t.Fatalf("the end %d is not newer than the start %d", end.Session, got.Session)
			}
			f.wantCall(t, "start alpha")
			f.wantCall(t, "stop")
			if f.manager.keyChord(handle, findEscape) {
				t.Fatal("the page's Escape was claimed after the find ended")
			}
			if state := f.find(t, "next", ""); !state.Foreign || state.Active {
				t.Fatalf("step after the end = %+v", state)
			}
		})
	}
}

// Stop and close end what is there and ask nothing more: on a page already
// closed they answer as for no find, and on a suspended page they leave it
// suspended rather than restoring it to stop a find that already ended.
func TestFindStopNeverRestoresAndIsIdempotent(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	for _, kind := range []string{"stop", "close", "stop"} {
		if state := f.find(t, kind, ""); state.Active {
			t.Fatalf("%s with no find = %+v", kind, state)
		}
	}
	f.noCall(t)
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID); err != nil {
		t.Fatal(err)
	}
	f.wantEnded(t, 0)
	for _, kind := range []string{"stop", "close"} {
		if state := f.find(t, kind, ""); state.Active || !state.Foreign || state.Mount != "" {
			t.Fatalf("%s on a suspended page = %+v", kind, state)
		}
	}
	f.manager.mu.Lock()
	_, suspended := f.manager.suspended[f.pageID]
	live, _ := f.manager.livePageLocked(f.pageID)
	f.manager.mu.Unlock()
	if !suspended || live != nil {
		t.Fatalf("a stop restored the suspended page (suspended %v, live %v)", suspended, live != nil)
	}
	if err := f.manager.ClosePage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"stop", "close"} {
		if state := f.find(t, kind, ""); state.Active || state.PageID != f.pageID || !state.Foreign || state.Mount != "" {
			t.Fatalf("%s on a closed page = %+v", kind, state)
		}
	}
}

// A pane mount detached while its find action runs, after the action
// checked the mount, ends what the action made once the action finishes:
// another mount of the thread stays, so the page stays shown, and the bar,
// its claim and the search all end.
func TestFindDetachedDuringAnActionEndsItAfter(t *testing.T) {
	for _, kind := range []string{"open", "start"} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			handle := f.fake.Handle()
			owner := f.mount
			if kind == "start" {
				f.find(t, "open", "")
			}
			f.attach(t)
			var once sync.Once
			setFindPoint(f.manager, func(point string) {
				if point == "checked" {
					once.Do(func() { f.manager.DetachPane(owner) })
				}
			})
			got, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, "alpha", owner)
			if err != nil || got.Foreign || got.Mount != owner {
				t.Fatalf("%s by a mount detached during it = %+v, %v", kind, got, err)
			}
			f.wantEndedFor(t, got.Session, owner)
			if kind == "start" {
				f.wantCall(t, "start alpha")
				f.wantCall(t, "stop")
			}
			if f.manager.keyChord(handle, findEscape) {
				t.Fatal("the page's Escape stayed claimed for a detached mount")
			}
			f.noCall(t)
		})
	}
}

// A restored page is a new incarnation under the same id, and its find's
// generations continue past every one the page answered before, so an end
// of the restored page's find is newer than what a bar last showed.
func TestFindGenerationsOutliveARestore(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.find(t, "open", "")
	for _, query := range []string{"a", "al", "alp", "alpha"} {
		f.find(t, "start", query)
		f.wantCall(t, "start "+query)
	}
	before := f.find(t, "next", "")
	f.wantCall(t, "step")
	if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID); err != nil {
		t.Fatal(err)
	}
	ended := f.wantEnded(t, before.Session)
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	f.fake = f.page(t, f.pageID)
	opened := f.find(t, "open", "")
	if opened.Incarnation == before.Incarnation {
		t.Fatalf("restored page answered incarnation %d, the same as before", opened.Incarnation)
	}
	if opened.Session <= ended.Session {
		t.Fatalf("restored page's open session %d is not newer than the end %d", opened.Session, ended.Session)
	}
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	f.fake.commitDocument()
	if end := f.wantEnded(t, started.Session); end.Incarnation != opened.Incarnation {
		t.Fatalf("end incarnation %d, want %d", end.Incarnation, opened.Incarnation)
	}
}

// A mount opening a find on a page another mount's find holds takes it
// over: the old owner is told its find ended and its search is stopped, and
// the old owner's later stop, close or step leaves the new owner's find
// alone.
func TestFindTakenOverByAnotherMount(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	handle := f.fake.Handle()
	f.find(t, "open", "")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	old := f.mount
	f.attach(t)
	opened := f.find(t, "open", "")
	if end := f.wantEndedFor(t, started.Session, old); end.Session >= opened.Session {
		t.Fatalf("takeover end %d is not older than the new open %d", end.Session, opened.Session)
	}
	f.wantCall(t, "stop")
	if opened.Mount != f.mount {
		t.Fatalf("open answered mount %q, want %q", opened.Mount, f.mount)
	}
	f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	for _, kind := range []string{"stop", "close"} {
		// Not the old owner's find: the answer is marked so and names the
		// find's real owner.
		state, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, "", old)
		if err != nil || state.Active || !state.Foreign || state.Mount != f.mount {
			t.Fatalf("%s by the old owner = %+v, %v", kind, state, err)
		}
	}
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", old); err != nil || !state.Foreign || state.Mount != f.mount {
		t.Fatalf("the old owner stepped the new owner's search: %+v, %v", state, err)
	}
	f.noCall(t)
	f.noEvent(t)
	if !f.manager.keyChord(handle, findEscape) {
		t.Fatal("the old owner's close released the new owner's claim")
	}
	if state := f.find(t, "next", ""); !state.Active || state.Mount != f.mount {
		t.Fatalf("new owner's step after the old owner's close = %+v", state)
	}
	f.wantCall(t, "step")
	f.find(t, "close", "")
	f.wantCall(t, "stop")
	if f.manager.keyChord(handle, findEscape) {
		t.Fatal("the owner's close left the page's Escape claimed")
	}
}

// A commit is stamped where the engine reports it; the end it causes can run
// later. A late end leaves a find opened on the new document alone and ends
// one opened before the commit.
func TestFindLateCommitEndsOnlyOlderDocuments(t *testing.T) {
	t.Run("newer find", func(t *testing.T) {
		f := newFindFixture(t, "https://example.test/alpha")
		handle := f.fake.Handle()
		f.find(t, "open", "")
		f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		deliver := f.fake.commitDocumentLate()
		started := f.find(t, "start", "beta")
		f.wantCall(t, "start beta")
		deliver()
		f.noEvent(t)
		f.noCall(t)
		if !f.manager.keyChord(handle, findEscape) {
			t.Fatal("a late commit released the claim of a find on its document")
		}
		if state := f.find(t, "next", ""); !state.Active || state.Document != started.Document {
			t.Fatalf("step after a late commit = %+v", state)
		}
		f.wantCall(t, "step")
	})
	t.Run("older find", func(t *testing.T) {
		f := newFindFixture(t, "https://example.test/alpha")
		handle := f.fake.Handle()
		f.find(t, "open", "")
		started := f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		deliver := f.fake.commitDocumentLate()
		deliver()
		if end := f.wantEnded(t, started.Session); end.Document != started.Document {
			t.Fatalf("end document %d, want %d", end.Document, started.Document)
		}
		f.wantCall(t, "stop")
		if f.manager.keyChord(handle, findEscape) {
			t.Fatal("a late commit left an older find's claim")
		}
	})
}

// An action from a mount that is already gone is refused before it takes
// anything over: the live owner's find stays.
func TestFindFromAGoneMountTakesNothingOver(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	handle := f.fake.Handle()
	f.find(t, "open", "")
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	owner := f.mount
	f.attach(t)
	gone := f.mount
	f.mount = owner
	f.manager.DetachPane(gone)
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "open", "", gone); !errors.Is(err, errFindNoMount) {
		t.Fatalf("open from a gone mount: %v", err)
	}
	// A start searches only in its own mount's bar.
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "beta", gone); err != nil || !state.Foreign || state.Mount != owner {
		t.Fatalf("start from a gone mount = %+v, %v", state, err)
	}
	f.noEvent(t)
	f.noCall(t)
	if !f.manager.keyChord(handle, findEscape) {
		t.Fatal("a gone mount's open released the owner's claim")
	}
	if state := f.find(t, "next", ""); !state.Active {
		t.Fatalf("owner's search after a gone mount's open = %+v", state)
	}
	f.wantCall(t, "step")
}

// A stop that waited for the page lock while a reload ended its find
// changes nothing: the end is applied before the stop, whose answer is
// marked Foreign and names no owner, so it acknowledges nothing the end did
// not.
func TestFindStopOvertakenByAnEndAnswersForeign(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.find(t, "open", "")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	type answer struct {
		state FindState
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		state, err := f.manager.Find(context.Background(), f.access, f.pageID, "stop", "", f.mount)
		answered <- answer{state, err}
	}()
	// Let the stop resolve the page and wait for its lock; the reload's
	// commit is submitted meanwhile.
	time.Sleep(50 * time.Millisecond)
	f.fake.commitDocument()
	p.mu.Unlock()
	got := <-answered
	end := f.wantEnded(t, started.Session)
	if got.err != nil || !got.state.Foreign || got.state.Mount != "" || got.state.Active {
		t.Fatalf("stop overtaken by an end = %+v, %v", got.state, got.err)
	}
	if got.state.Session != end.Session {
		t.Fatalf("overtaken stop answered generation %d, the end was %d", got.state.Session, end.Session)
	}
	f.wantCall(t, "stop")
	f.noCall(t)
}

// A close whose engine stop fails reports it and closes the bar; the stop
// stays owed, so the next close from that mount, whose find is gone, makes
// it, and reports each failure while the engine keeps refusing.
func TestFindCloseRetriesAStopTheEngineRefused(t *testing.T) {
	for _, always := range []bool{false, true} {
		t.Run(fmt.Sprintf("always %v", always), func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			handle := f.fake.Handle()
			f.find(t, "open", "")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			failures := 0
			f.fake.mu.Lock()
			f.fake.findStopErr = func(context.Context) error {
				if always || failures == 0 {
					failures++
					return errors.New("engine busy")
				}
				return nil
			}
			f.fake.mu.Unlock()
			if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "close", "", f.mount); err == nil {
				t.Fatal("a close whose stop failed reported nothing")
			}
			f.wantCall(t, "stop")
			if f.manager.keyChord(handle, findEscape) {
				t.Fatal("a close whose stop failed left the page's Escape claimed")
			}
			for round := range 2 {
				state, err := f.manager.Find(t.Context(), f.access, f.pageID, "close", "", f.mount)
				f.wantCall(t, "stop")
				f.fake.mu.Lock()
				query := f.fake.findQuery
				f.fake.mu.Unlock()
				if always {
					if err == nil || query == "" {
						t.Fatalf("round %d: close against a failing engine = %+v, %v; engine %q", round, state, err, query)
					}
					continue
				}
				if err != nil || !state.Foreign || query != "" {
					t.Fatalf("round %d: close after the engine recovered = %+v, %v; engine %q", round, state, err, query)
				}
				// Nothing is owed any more: the next close asks nothing.
				f.noCall(t)
				break
			}
		})
	}
}

// A start or step the engine failed is cleared with it: the engine keeps no
// search the Manager does not hold.
func TestFindFailedEngineCallIsCleared(t *testing.T) {
	for _, kind := range []string{"start", "next"} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			f.fake.mu.Lock()
			f.fake.findErr = errors.New("engine failed")
			// The failing call alone fails; the stop after it succeeds.
			f.fake.findHook = func(call string) {
				f.fake.mu.Lock()
				f.fake.findErr = nil
				f.fake.mu.Unlock()
				f.calls <- call
			}
			f.fake.mu.Unlock()
			if _, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, "beta", f.mount); err == nil {
				t.Fatalf("%s did not fail", kind)
			}
			f.fake.mu.Lock()
			query := f.fake.findQuery
			f.fake.mu.Unlock()
			if query != "" {
				t.Fatalf("engine search after a failed %s = %q", kind, query)
			}
		})
	}
}

// An engine stop that never answers is bounded by the action's deadline
// wherever a find action makes one: the takeover of another mount's find
// by an open, and a close. The action answers, the stop stays owed, and the
// next transition makes it.
func TestFindStopThatNeverAnswersIsBoundedAndStaysOwed(t *testing.T) {
	for _, kind := range []string{"open", "close"} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "open", "")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			hang := true
			f.fake.mu.Lock()
			f.fake.findStopErr = func(ctx context.Context) error {
				f.fake.mu.Lock()
				hanging := hang
				f.fake.mu.Unlock()
				if !hanging {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return errors.New("released by the test")
				}
			}
			f.fake.mu.Unlock()
			if kind == "open" {
				f.attach(t)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			answered := make(chan error, 1)
			go func() {
				_, err := f.manager.Find(ctx, f.access, f.pageID, kind, "", f.mount)
				answered <- err
			}()
			select {
			case err := <-answered:
				if kind == "close" && err == nil {
					t.Fatal("a close whose stop timed out reported nothing")
				}
				if kind == "open" && err != nil {
					t.Fatalf("open over a find whose stop timed out: %v", err)
				}
			case <-time.After(5 * time.Second):
				unblock()
				t.Fatalf("%s waited on an engine stop past its deadline", kind)
			}
			f.wantCall(t, "stop")
			p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
			if err != nil {
				t.Fatal(err)
			}
			p.findMu.Lock()
			owed := p.find.engine
			p.findMu.Unlock()
			if !owed {
				t.Fatal("a stop that timed out was recorded as made")
			}
			f.fake.mu.Lock()
			hang = false
			f.fake.mu.Unlock()
			// The action's own retry after it timed out too; then the next
			// transition makes the stop it is owed.
			for draining := true; draining; {
				select {
				case <-f.calls:
				default:
					draining = false
				}
			}
			f.find(t, "stop", "")
			f.wantCall(t, "stop")
			p.findMu.Lock()
			owed = p.find.engine
			p.findMu.Unlock()
			if owed {
				t.Fatal("the next transition left the owed stop owed")
			}
		})
	}
}
