package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	f := &findFixture{
		manager: manager,
		access:  Access{ThreadID: "thread", Workspace: t.TempDir()},
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
	seq := f.seq
	if kind == FindStart {
		seq = f.nextSeq()
	}
	state, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, query, f.mount, seq)
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
	p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
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
	// The search is gone: a step answers so, asking the engine nothing.
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount, f.seq); err != nil || state.Active || !state.Foreign {
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
		name, pageID, kind, query string
		seq                       uint64
		want                      string
	}{
		{"unknown kind", f.pageID, "replace", "a", 1, "find action"},
		{"a bar's open is no action", f.pageID, "open", "", 1, "find action"},
		{"a bar's close is no action", f.pageID, "close", "", 1, "find action"},
		{"no page", "", "start", "a", 1, "page id is required"},
		{"query too long", f.pageID, "start", strings.Repeat("a", maxFindQueryBytes+1), 1, "that search is too long; shorten it to 1024 bytes or fewer"},
		{"no sequence", f.pageID, "start", "a", 0, "names its sequence"},
		{"a stop with no sequence", f.pageID, "stop", "", 0, "names its sequence"},
	} {
		if _, err := f.manager.Find(ctx, f.access, tc.pageID, tc.kind, tc.query, f.mount, tc.seq); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
	intruder := Access{ThreadID: "intruder", Workspace: f.access.Workspace}
	if _, err := f.manager.Find(ctx, intruder, f.pageID, "start", "alpha", f.mount, f.nextSeq()); !errors.Is(err, errPageNotFound) {
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount, f.nextSeq()); err == nil || !strings.Contains(err.Error(), "shows") {
		t.Fatalf("find on a background tab error = %v", err)
	}
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, f.pageID); err != nil {
		t.Fatal(err)
	}
	hide := false
	if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount, f.nextSeq()); err == nil || !strings.Contains(err.Error(), "shows") {
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
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount, f.seq); err != nil || !state.Foreign {
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
		state, err := f.manager.Find(context.Background(), f.access, f.pageID, "start", "alpha", f.mount, f.nextSeq())
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount, f.seq); !errors.Is(err, errPageNotFound) {
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
		if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount, f.nextSeq()); err == nil {
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

// A bare Escape pressed in a page is the page's in every state of its find:
// none, a search running, one ended, the engine's own bar, the page not
// shown. While a pane shows the page it is announced, once per press and
// not for a key held down; a modified Escape goes through the accelerator
// gate like any chord.
func TestFindNeverClaimsAPagesBareEscape(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	handle := f.fake.Handle()
	escape := keybindings.Accelerator{Key: "escape"}
	press := func(t *testing.T, state string, announced bool) {
		t.Helper()
		if f.manager.keyChord(handle, escape, KeyPress{}) {
			t.Fatalf("%s: AO took the page's Escape", state)
		}
		if f.manager.keyChord(handle, escape, KeyPress{Held: true}) {
			t.Fatalf("%s: AO took the page's held Escape", state)
		}
		select {
		case event := <-f.escapes:
			if !announced {
				t.Fatalf("%s: an Escape was announced: %+v", state, event)
			}
			if event.ThreadID != f.access.ThreadID || event.PageID != f.pageID || event.Find != nil || event.Accelerator != nil {
				t.Fatalf("%s: escape event = %+v", state, event)
			}
		case <-time.After(5 * time.Second):
			if announced {
				t.Fatalf("%s: the Escape was never announced", state)
			}
		}
		select {
		case event := <-f.escapes:
			t.Fatalf("%s: a held Escape was announced again: %+v", state, event)
		case <-time.After(50 * time.Millisecond):
		}
		select {
		case key := <-f.keys:
			t.Fatalf("%s: a bare Escape was routed as a chord: %+v", state, key)
		default:
		}
	}
	press(t, "no search", true)
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	press(t, "a search running", true)
	f.fake.commitDocument()
	f.wantEnded(t, started.Session)
	f.wantCall(t, "stop")
	press(t, "the search ended", true)
	f.fake.mu.Lock()
	f.fake.findMode = FindModeEngine
	f.fake.mu.Unlock()
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	press(t, "the engine's own bar", true)
	f.fake.mu.Lock()
	f.fake.findMode = FindModeBar
	f.fake.mu.Unlock()
	hide := false
	if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
		t.Fatal(err)
	}
	press(t, "the page not shown", false)

	// A modified Escape is a chord: AO takes it only when it is bound.
	ctrl := keybindings.Accelerator{Key: "escape", Ctrl: true}
	if f.manager.keyChord(handle, ctrl, KeyPress{}) {
		t.Fatal("an unbound Ctrl+Escape was taken")
	}
	f.manager.accelerators = func() keybindings.AcceleratorSet { return keybindings.AcceleratorSet{ctrl: {}} }
	if !f.manager.keyChord(handle, ctrl, KeyPress{}) {
		t.Fatal("a bound Ctrl+Escape did not go through the accelerator gate")
	}
	select {
	case key := <-f.keys:
		if key != ctrl {
			t.Fatalf("routed chord %+v, want Ctrl+Escape", key)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a bound Ctrl+Escape was never routed")
	}
	select {
	case event := <-f.escapes:
		t.Fatalf("a Ctrl+Escape was announced as the page's Escape: %+v", event)
	case <-time.After(50 * time.Millisecond):
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
	if _, err := manager.Find(t.Context(), access, info.ID, "start", "alpha", pane.ID, 1); !errors.Is(err, errFindUnavailable) {
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount, f.nextSeq()); !errors.Is(err, errFindUnavailable) {
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount, f.seq); err == nil || !strings.Contains(err.Error(), "own find bar") {
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
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount, f.seq); err == nil || !strings.Contains(err.Error(), "engine went away") {
		t.Fatalf("failed step error = %v", err)
	}
	f.wantCall(t, "step")
	f.fake.mu.Lock()
	f.fake.findErr = nil
	f.fake.mu.Unlock()
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "next", "", f.mount, f.seq); err != nil || state.Active {
		t.Fatalf("a failed step left the session running: %+v, %v", state, err)
	}
	if state := f.find(t, "start", "alpha"); !state.Active || state.Session <= started.Session {
		t.Fatalf("restart after a failure = %+v", state)
	}
}

// A find that resolved its page and then waited for the page's lock while
// the page closed never reaches the closed driver: the native view is gone.
func TestFindNeverCallsTheDriverOfAPageClosedWhileItWaited(t *testing.T) {
	for _, kind := range []string{"start", "next"} {
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
			waiting := findWaiting(f.manager)
			p.mu.Lock()
			answered := make(chan error, 1)
			go func() {
				_, err := f.manager.Find(context.Background(), f.access, f.pageID, kind, "alpha", f.mount, f.seqFor(kind))
				answered <- err
			}()
			// Once the find resolved the page and waits for its lock, tear
			// the page down under that lock, as closing does.
			<-waiting
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
// answers its search, and the end that follows is newer and stops the
// search.
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
			var once sync.Once
			setFindPoint(f.manager, func(point string) {
				if point == "engine" {
					once.Do(func() { tc.end(t, f) })
				}
			})
			got, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount, f.nextSeq())
			if err != nil || !got.Active {
				t.Fatalf("start with an end landing during it = %+v, %v", got, err)
			}
			end := f.wantEnded(t, got.Session)
			if end.Session <= got.Session {
				t.Fatalf("the end %d is not newer than the start %d", end.Session, got.Session)
			}
			f.wantCall(t, "start alpha")
			f.wantCall(t, "stop")
			if state := f.find(t, "next", ""); !state.Foreign || state.Active {
				t.Fatalf("step after the end = %+v", state)
			}
		})
	}
}

// A stop ends what is there and asks nothing more: with no search it
// answers so, on a page already closed it answers as for no search, and on
// a suspended page it leaves it suspended rather than restoring it to stop
// a search that already ended.
func TestFindStopNeverRestoresAndIsIdempotent(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	// A stop names a bound even before any start.
	f.seq = 1
	for range 2 {
		if state := f.find(t, "stop", ""); state.Active || !state.Foreign {
			t.Fatalf("stop with no search = %+v", state)
		}
	}
	f.noCall(t)
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID); err != nil {
		t.Fatal(err)
	}
	f.wantEnded(t, 0)
	if state := f.find(t, "stop", ""); state.Active || !state.Foreign || state.Mount != "" {
		t.Fatalf("stop on a suspended page = %+v", state)
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
	if state := f.find(t, "stop", ""); state.Active || state.PageID != f.pageID || !state.Foreign || state.Mount != "" {
		t.Fatalf("stop on a closed page = %+v", state)
	}
}

// A pane mount detached while its start runs, after the start checked the
// mount, ends the search once the start finishes: another mount of the
// thread stays, so the page stays shown, and the search is ended, announced
// and stopped.
func TestFindDetachedDuringAnActionEndsItAfter(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	owner := f.mount
	f.attach(t)
	var once sync.Once
	setFindPoint(f.manager, func(point string) {
		if point == "checked" {
			once.Do(func() { f.manager.DetachPane(owner) })
		}
	})
	got, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", owner, f.nextSeq())
	if err != nil || got.Foreign || got.Mount != owner {
		t.Fatalf("start by a mount detached during it = %+v, %v", got, err)
	}
	f.wantEndedFor(t, got.Session, owner)
	f.wantCall(t, "start alpha")
	f.wantCall(t, "stop")
	f.noCall(t)
}

// The transport can run a start whose connection already closed (its
// mount's cleanup runs after). The cleanup detaches the mount, which ends
// the search and stops the engine's, and nothing of it is left.
func TestFindStartedByAMountThatThenDetachesLeavesNothing(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	f.manager.DetachPane(f.mount)
	if end := f.wantEnded(t, started.Session); end.Seq != f.seq {
		t.Fatalf("end names sequence %d, want %d", end.Seq, f.seq)
	}
	f.wantCall(t, "stop")
	if session := f.session(t); session.active || session.engine || session.owner != "" || session.seq != 0 {
		t.Fatalf("after the detach the page holds %+v", session)
	}
	f.fake.mu.Lock()
	query := f.fake.findQuery
	f.fake.mu.Unlock()
	if query != "" {
		t.Fatalf("the engine still searches for %q", query)
	}
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount, f.nextSeq()); !errors.Is(err, errFindNoMount) {
		t.Fatalf("start from the detached mount = %v", err)
	}
	f.noCall(t)
}

// A restored page is a new incarnation under the same id, and its find's
// generations continue past every one the page answered before, so an end
// of the restored page's find is newer than what a bar last showed.
func TestFindGenerationsOutliveARestore(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
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
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if started.Incarnation == before.Incarnation {
		t.Fatalf("restored page answered incarnation %d, the same as before", started.Incarnation)
	}
	if started.Session <= ended.Session {
		t.Fatalf("restored page's start session %d is not newer than the end %d", started.Session, ended.Session)
	}
	f.fake.commitDocument()
	if end := f.wantEnded(t, started.Session); end.Incarnation != started.Incarnation {
		t.Fatalf("end incarnation %d, want %d", end.Incarnation, started.Incarnation)
	}
}

// A start from another mount replaces the search: the old owner is told its
// search ended, the engine's search is replaced rather than stopped, and the
// old owner's later stop or step leaves the new owner's search alone.
func TestFindTakenOverByAnotherMount(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha-beta")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	old, oldSeq := f.mount, f.seq
	f.attach(t)
	taken := f.find(t, "start", "beta")
	if end := f.wantEndedFor(t, started.Session, old); end.Session >= taken.Session || end.Seq != oldSeq {
		t.Fatalf("takeover end %+v against the new start %d", end, taken.Session)
	}
	f.wantCall(t, "start beta")
	if taken.Mount != f.mount || taken.Seq != f.seq || !taken.Active {
		t.Fatalf("start answered %+v", taken)
	}
	for _, kind := range []string{"stop", "next"} {
		// Not the old owner's search: the answer is marked so and names the
		// search's real owner.
		state, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, "", old, oldSeq+10)
		if err != nil || state.Active || !state.Foreign || state.Mount != f.mount || state.Seq != f.seq {
			t.Fatalf("%s by the old owner = %+v, %v", kind, state, err)
		}
	}
	f.noCall(t)
	f.noEvent(t)
	if state := f.find(t, "next", ""); !state.Active || state.Mount != f.mount {
		t.Fatalf("new owner's step after the old owner's stop = %+v", state)
	}
	f.wantCall(t, "step")
	f.find(t, "stop", "")
	f.wantCall(t, "stop")
}

// A commit is stamped where the engine reports it; the end it causes can run
// later. A late end leaves a find opened on the new document alone and ends
// one opened before the commit.
func TestFindLateCommitEndsOnlyOlderDocuments(t *testing.T) {
	t.Run("newer find", func(t *testing.T) {
		f := newFindFixture(t, "https://example.test/alpha")
		f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		deliver := f.fake.commitDocumentLate()
		started := f.find(t, "start", "beta")
		f.wantCall(t, "start beta")
		deliver()
		f.noEvent(t)
		f.noCall(t)
		if state := f.find(t, "next", ""); !state.Active || state.Document != started.Document {
			t.Fatalf("step after a late commit = %+v", state)
		}
		f.wantCall(t, "step")
	})
	t.Run("older find", func(t *testing.T) {
		f := newFindFixture(t, "https://example.test/alpha")
		started := f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		deliver := f.fake.commitDocumentLate()
		deliver()
		if end := f.wantEnded(t, started.Session); end.Document != started.Document {
			t.Fatalf("end document %d, want %d", end.Document, started.Document)
		}
		f.wantCall(t, "stop")
	})
}

// A search belongs to its mount and sequence: a later start of the same
// mount replaces it without an end (its provider made that start), and the
// earlier search's late steps and stops, a retried stop among them, change
// nothing of the later one's.
func TestFindEarlierSearchOfTheSameMountTouchesNothingOfALaterOne(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha-beta")
	first := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	earlier := f.seq
	later := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	f.noEvent(t)
	if later.Session <= first.Session || later.Seq != f.seq || later.Foreign {
		t.Fatalf("later start = %+v", later)
	}
	for _, kind := range []string{"next", "previous", "stop", "stop"} {
		state, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, "", f.mount, earlier)
		if err != nil || !state.Foreign || state.Seq != f.seq {
			t.Fatalf("%s by the earlier bar = %+v, %v", kind, state, err)
		}
	}
	f.noCall(t)
	f.noEvent(t)
	if state := f.find(t, "next", ""); !state.Active || state.Query != "beta" {
		t.Fatalf("the later bar's search after the earlier bar's actions = %+v", state)
	}
	f.wantCall(t, "step")
}

// A start from a mount that is already gone is refused before it takes
// anything over, and its steps and stops answer foreign: the live owner's
// search stays.
func TestFindFromAGoneMountTakesNothingOver(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	owner := f.mount
	f.attach(t)
	gone := f.mount
	f.mount = owner
	f.manager.DetachPane(gone)
	if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "beta", gone, 99); !errors.Is(err, errFindNoMount) {
		t.Fatalf("start from a gone mount: %v", err)
	}
	for _, kind := range []string{"next", "stop"} {
		if state, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, "", gone, 99); err != nil || !state.Foreign || state.Mount != owner {
			t.Fatalf("%s from a gone mount = %+v, %v", kind, state, err)
		}
	}
	f.noEvent(t)
	f.noCall(t)
	if state := f.find(t, "next", ""); !state.Active {
		t.Fatalf("owner's search after a gone mount's start = %+v", state)
	}
	f.wantCall(t, "step")
}

// A stop that waited for the page lock while a reload ended its find
// changes nothing: the end is applied before the stop, whose answer is
// marked Foreign and names no owner, so it acknowledges nothing the end did
// not.
func TestFindStopOvertakenByAnEndAnswersForeign(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
	if err != nil {
		t.Fatal(err)
	}
	waiting := findWaiting(f.manager)
	p.mu.Lock()
	type answer struct {
		state FindState
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		state, err := f.manager.Find(context.Background(), f.access, f.pageID, "stop", "", f.mount, f.seq)
		answered <- answer{state, err}
	}()
	// The stop resolved the page and waits for its lock; the reload's
	// commit is submitted meanwhile.
	<-waiting
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

// A stop whose engine call fails reports it and ends the search; the engine
// stop stays owed and is made again as its transition ends, and then by
// each next stop from that bar, whose search is gone, which reports each
// failure while the engine keeps refusing.
func TestFindStopRetriesAStopTheEngineRefused(t *testing.T) {
	for _, always := range []bool{false, true} {
		t.Run(fmt.Sprintf("always %v", always), func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			// No end submitted before is still to be applied: every
			// transition that follows is the test's own.
			f.manager.findWork.Wait()
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
			if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "stop", "", f.mount, f.seq); err == nil {
				t.Fatal("a stop whose engine call failed reported nothing")
			}
			// The stop, then the end of its transition making the stop it
			// is still owed.
			f.wantCall(t, "stop")
			f.wantCall(t, "stop")
			if session := f.session(t); session.active || session.engine != always {
				t.Fatalf("after a refused stop the page holds %+v", session)
			}
			if !always {
				// Nothing is owed any more: the next stop asks nothing.
				if state := f.find(t, "stop", ""); !state.Foreign {
					t.Fatalf("stop after the engine recovered = %+v", state)
				}
				f.noCall(t)
				return
			}
			for round := range 2 {
				state, err := f.manager.Find(t.Context(), f.access, f.pageID, "stop", "", f.mount, f.seq)
				f.wantCall(t, "stop")
				f.wantCall(t, "stop")
				f.fake.mu.Lock()
				query := f.fake.findQuery
				f.fake.mu.Unlock()
				if err == nil || !state.Foreign || query == "" {
					t.Fatalf("round %d: stop against a failing engine = %+v, %v; engine %q", round, state, err, query)
				}
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
			if _, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, "beta", f.mount, f.seqFor(kind)); err == nil {
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

// Ends submitted while the Manager closes, from the engine's commit callback
// and from presentation changes, are either joined by Close or never
// started: no find work runs once Close has returned.
func TestFindEndsSubmittedDuringCloseAreJoined(t *testing.T) {
	for round := range 50 {
		f := newFindFixture(t, "https://example.test/alpha")
		f.find(t, "start", "alpha")
		f.wantCall(t, "start alpha")
		p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
		if err != nil {
			t.Fatal(err)
		}
		var closed atomic.Bool
		var late atomic.Int32
		setFindPoint(f.manager, func(point string) {
			if point == "apply" && closed.Load() {
				late.Add(1)
			}
		})
		var submitters sync.WaitGroup
		start := make(chan struct{})
		for i := range 8 {
			submitters.Add(1)
			go func() {
				defer submitters.Done()
				<-start
				for range 20 {
					if i%2 == 0 {
						f.fake.commitDocument()
					} else {
						f.manager.endHiddenFinds([]*managedPage{p})
					}
				}
			}()
		}
		close(start)
		if err := f.manager.Close(); err != nil {
			t.Fatalf("round %d: close: %v", round, err)
		}
		closed.Store(true)
		submitters.Wait()
		// Submitted after Close returned: refused, not started.
		f.fake.commitDocument()
		f.manager.endFind(p)
		// Work wrongly started after Close would be running by now.
		time.Sleep(20 * time.Millisecond)
		if n := late.Load(); n != 0 {
			t.Fatalf("round %d: %d find applies ran after Close returned", round, n)
		}
	}
}

// A find action's deadline runs from before it waits for the page lock: an
// action that waited past it is refused and changes nothing, whatever its
// kind, and so is one whose caller went while it waited.
func TestFindActionThatWaitedPastItsDeadlineChangesNothing(t *testing.T) {
	for _, why := range []string{"deadline", "caller"} {
		for _, kind := range []string{"start", "next", "previous", "stop"} {
			t.Run(why+" "+kind, func(t *testing.T) {
				f := newFindFixtureWith(t, "https://example.test/alpha", func(m *Manager) {
					if why == "deadline" {
						m.findTimeout = 200 * time.Millisecond
					}
				})
				if kind != "start" {
					f.find(t, "start", "alpha")
					f.wantCall(t, "start alpha")
				}
				p, _, err := f.manager.lookupOwnedPage(f.access, f.pageID)
				if err != nil {
					t.Fatal(err)
				}
				p.findMu.Lock()
				before := p.find
				p.findMu.Unlock()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				waiting := findWaiting(f.manager)
				p.mu.Lock()
				answered := make(chan error, 1)
				go func() {
					_, err := f.manager.Find(ctx, f.access, f.pageID, kind, "beta", f.mount, f.seqFor(kind))
					answered <- err
				}()
				// The action waits for the lock with its deadline running;
				// the lock is held until its caller went, or until the
				// deadline passed.
				action := <-waiting
				if why == "caller" {
					cancel()
				}
				<-action.Done()
				p.mu.Unlock()
				if err := <-answered; err == nil {
					t.Fatalf("%s that waited past its deadline was not refused", kind)
				}
				f.noCall(t)
				f.noEvent(t)
				p.findMu.Lock()
				after := p.find
				p.findMu.Unlock()
				if after != before {
					t.Fatalf("%s that waited past its deadline changed the find: %+v, was %+v", kind, after, before)
				}
			})
		}
	}
}

// An engine stop that never answers is bounded by its action's context
// wherever a find action makes one: a stop, and the clearing of a step the
// engine failed. The fake engine says when the stop is waiting, and the
// action's caller then goes: the action answers, the stop stays owed, and
// the next transition makes it. Nothing waits on a clock.
func TestFindStopThatNeverAnswersIsBoundedAndStaysOwed(t *testing.T) {
	for _, kind := range []string{"stop", "next"} {
		t.Run(kind, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			stopping := make(chan struct{})
			var first sync.Once
			f.fake.mu.Lock()
			if kind == "next" {
				// The step fails; the stop that clears it is the one that
				// never answers.
				f.fake.findErr = errors.New("engine failed")
				record := f.fake.findHook
				f.fake.findHook = func(call string) {
					f.fake.mu.Lock()
					f.fake.findErr = nil
					f.fake.mu.Unlock()
					record(call)
				}
			}
			// The first stop never answers before its context ends; every
			// later one answers at once, refused only if its context already
			// ended.
			f.fake.findStopErr = func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				hang := false
				first.Do(func() { hang = true })
				if !hang {
					return nil
				}
				close(stopping)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.Context().Done():
					// The test ended: let its cleanup close the page.
					return errors.New("released by the test's end")
				}
			}
			f.fake.mu.Unlock()
			// No end submitted before is still to be applied: the next
			// transition is the test's own.
			f.manager.findWork.Wait()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			answered := make(chan error, 1)
			go func() {
				_, err := f.manager.Find(ctx, f.access, f.pageID, kind, "", f.mount, f.seqFor(kind))
				answered <- err
			}()
			<-stopping
			cancel()
			// A stop reports its stop's end; a step reports its own failure.
			// The bound is reached only when the engine stop ignores the
			// action's end.
			select {
			case err := <-answered:
				if err == nil || (kind == "stop" && !errors.Is(err, context.Canceled)) {
					t.Fatalf("a %s whose engine stop never answered = %v", kind, err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("a %s's engine stop outlived the action's end", kind)
			}
			if !f.session(t).engine {
				t.Fatal("a stop that never answered was recorded as made")
			}
			// The action's own calls, its end-of-transition retry among them;
			// then the next transition makes the stop it is owed.
			for draining := true; draining; {
				select {
				case <-f.calls:
				default:
					draining = false
				}
			}
			f.find(t, "stop", "")
			f.wantCall(t, "stop")
			if f.session(t).engine {
				t.Fatal("the next transition left the owed stop owed")
			}
		})
	}
}

// findWaiting reports each find action's context once the action waits for
// its page's lock.
func findWaiting(m *Manager) <-chan context.Context {
	waiting := make(chan context.Context, 16)
	hook := func(ctx context.Context) { waiting <- ctx }
	m.findWaitHook.Store(&hook)
	return waiting
}

// A page whose search ended with its engine stop still owed is not settled:
// hiding the companion or unmounting the pane makes that stop, so no
// highlight outlives the pane.
func TestFindOwedStopIsMadeWhenThePaneGoes(t *testing.T) {
	for _, why := range []string{"detach", "hide"} {
		t.Run(why, func(t *testing.T) {
			f := newFindFixture(t, "https://example.test/alpha")
			f.find(t, "start", "alpha")
			f.wantCall(t, "start alpha")
			refuse := true
			f.fake.mu.Lock()
			f.fake.findStopErr = func(context.Context) error {
				f.fake.mu.Lock()
				defer f.fake.mu.Unlock()
				if refuse {
					return errors.New("engine busy")
				}
				return nil
			}
			f.fake.mu.Unlock()
			f.manager.findWork.Wait()
			if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "stop", "", f.mount, f.seq); err == nil {
				t.Fatal("a refused stop reported nothing")
			}
			f.wantCall(t, "stop")
			f.wantCall(t, "stop")
			if session := f.session(t); session.active || !session.engine {
				t.Fatalf("after the refused stop the page holds %+v", session)
			}
			f.fake.mu.Lock()
			refuse = false
			f.fake.mu.Unlock()
			if why == "detach" {
				f.manager.DetachPane(f.mount)
			} else {
				hide := false
				if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
					t.Fatal(err)
				}
			}
			f.manager.findWork.Wait()
			f.wantCall(t, "stop")
			if f.session(t).engine {
				t.Fatalf("the owed stop was not made on %s", why)
			}
			f.fake.mu.Lock()
			query := f.fake.findQuery
			f.fake.mu.Unlock()
			if query != "" {
				t.Fatalf("the engine still searches for %q after %s", query, why)
			}
		})
	}
}

// The announce decision for a key pressed in a page, over every key the
// engines offer the gate for it: only a bare Escape, newly pressed and not
// cancelling a composition, is announced. And the gate never takes a bare
// Escape, in any of those states, even from a bound set that names one.
func TestPageEscapeAnnouncedOnlyForAFreshBareEscape(t *testing.T) {
	bools := []bool{false, true}
	m := &Manager{accelerators: func() keybindings.AcceleratorSet {
		return keybindings.AcceleratorSet{{Key: "escape"}: {}}
	}}
	for _, key := range []string{"escape", "f"} {
		for _, ctrl := range bools {
			for _, meta := range bools {
				for _, alt := range bools {
					for _, shift := range bools {
						pressed := keybindings.Accelerator{Key: key, Ctrl: ctrl, Meta: meta, Alt: alt, Shift: shift}
						bare := key == "escape" && !ctrl && !meta && !alt && !shift
						for _, repeat := range bools {
							for _, held := range bools {
								for _, composing := range bools {
									press := KeyPress{Repeat: repeat, Held: held, Composing: composing}
									want := bare && !repeat && !held && !composing
									if got := pageEscapeAnnounced(pressed, press); got != want {
										t.Errorf("pageEscapeAnnounced(%+v, %+v) = %v, want %v", pressed, press, got, want)
									}
									if bare && m.keyChord("no-such-page", pressed, press) {
										t.Errorf("the gate took a bare Escape (%+v)", press)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// A start's sequence must be above every start its mount made before: an
// older or repeated one is refused and changes nothing. Another mount
// numbers its own starts.
func TestFindRefusesAStartNotAboveTheMountsLast(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.seq = 4
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	for _, seq := range []uint64{5, 4, 1} {
		if _, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "beta", f.mount, seq); !errors.Is(err, errFindStale) {
			t.Fatalf("start %d after %d = %v, want stale", seq, started.Seq, err)
		}
	}
	f.noCall(t)
	if state := f.find(t, "next", ""); !state.Active || state.Seq != 5 || state.Query != "alpha" {
		t.Fatalf("the search after the refused starts = %+v", state)
	}
	f.wantCall(t, "step")
	first := f.mount
	f.attach(t)
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "start", "alpha", f.mount, 1); err != nil || !state.Active {
		t.Fatalf("another mount's first start = %+v, %v", state, err)
	}
	f.wantEndedFor(t, started.Session, first)
	f.wantCall(t, "start alpha")
}

// A stop ends the mount's search exactly when its sequence is at or below
// the stop's bound; a step names its search's exact sequence.
func TestFindStopEndsTheMountsSearchAtOrBelowItsBound(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.seq = 2
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	for _, kind := range []string{"next", "previous"} {
		for _, seq := range []uint64{2, 4} {
			if state, err := f.manager.Find(t.Context(), f.access, f.pageID, kind, "", f.mount, seq); err != nil || !state.Foreign || state.Seq != 3 {
				t.Fatalf("%s naming %d = %+v, %v", kind, seq, state, err)
			}
		}
	}
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "stop", "", f.mount, 2); err != nil || !state.Foreign || state.Seq != 3 {
		t.Fatalf("stop through 2 of search 3 = %+v, %v", state, err)
	}
	f.noCall(t)
	if state := f.session(t); !state.active || state.seq != started.Seq {
		t.Fatalf("after the earlier stop the page holds %+v", state)
	}
	if state, err := f.manager.Find(t.Context(), f.access, f.pageID, "stop", "", f.mount, 9); err != nil || state.Foreign || state.Seq != 3 {
		t.Fatalf("stop through 9 of search 3 = %+v, %v", state, err)
	}
	f.wantCall(t, "stop")
	if state := f.session(t); state.active {
		t.Fatalf("after the stop the page holds %+v", state)
	}
}

// A stop ends the mount's search on whichever page it runs: one naming a
// page already gone still ends the search on the page the mount searched.
func TestFindStopEndsTheMountsSearchOnAnotherPage(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	state, err := f.manager.Find(t.Context(), f.access, "page-already-closed", "stop", "", f.mount, f.seq+1)
	if err != nil || !state.Foreign || state.PageID != "page-already-closed" {
		t.Fatalf("stop naming a gone page = %+v, %v", state, err)
	}
	f.wantCall(t, "stop")
	if session := f.session(t); session.active || session.engine {
		t.Fatalf("after the stop the searched page holds %+v", session)
	}
	// Another mount's search is not the stop's.
	f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	if _, err := f.manager.Find(t.Context(), f.access, "page-already-closed", "stop", "", "another-mount", 99); err != nil {
		t.Fatal(err)
	}
	f.noCall(t)
	if session := f.session(t); !session.active {
		t.Fatalf("another mount's stop ended the search: %+v", session)
	}
}
