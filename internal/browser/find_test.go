package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/keybindings"
)

func TestFindStartsStepsWrapsAndStops(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha-beta-alpha")
	if mode := f.manager.CompanionState(f.access.Caller()).FindMode; mode != FindModeBar {
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
	if state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "next", "", f.mount, f.seq); err != nil || state.Active || !state.Foreign {
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
		if _, err := f.manager.Find(ctx, f.access, PageID(tc.pageID), tc.kind, tc.query, f.mount, tc.seq); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.want)
		}
	}
	intruder := f.manager.Bind(Caller{ThreadID: "intruder", Workspace: f.access.Workspace()})
	if _, err := f.manager.Find(ctx, intruder, PageID(f.pageID), "start", "alpha", f.mount, f.nextSeq()); !errors.Is(err, errPageNotFound) {
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
	if _, err := f.manager.NewCompanionPage(t.Context(), f.access, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "alpha", f.mount, f.nextSeq()); err == nil || !strings.Contains(err.Error(), "shows") {
		t.Fatalf("find on a background tab error = %v", err)
	}
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(f.pageID)); err != nil {
		t.Fatal(err)
	}
	hide := false
	if _, err := f.manager.Visibility(t.Context(), f.access, &hide, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "alpha", f.mount, f.nextSeq()); err == nil || !strings.Contains(err.Error(), "shows") {
		t.Fatalf("find with the companion hidden error = %v", err)
	}
}

func TestFindEndsWhenThePageNavigates(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")

	// A fragment move keeps the document.
	if _, err := f.manager.Open(t.Context(), f.access, "https://example.test/alpha#part", OpenOptions{PageID: PageID(f.pageID)}); err != nil {
		t.Fatal(err)
	}
	f.noEvent(t)

	if _, err := f.manager.Open(t.Context(), f.access, "https://example.test/beta", OpenOptions{PageID: PageID(f.pageID)}); err != nil {
		t.Fatal(err)
	}
	ended := f.wantEnded(t, started.Session)
	f.wantCall(t, "stop")
	if state, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "next", "", f.mount, f.seq); err != nil || !state.Foreign {
		t.Fatalf("next after the navigation ended the find (session %d) = %+v, %v", ended.Session, state, err)
	}

	// A reload at the same address is a new document: the old count is
	// stale.
	restarted := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	if _, err := f.manager.History(t.Context(), f.access, PageID(f.pageID), "reload"); err != nil {
		t.Fatal(err)
	}
	f.wantEnded(t, restarted.Session)
	f.wantCall(t, "stop")

	// A same-document navigation (history.pushState), and the address and
	// title change the engine reports for it, keep the find.
	again := f.find(t, "start", "beta")
	f.wantCall(t, "start beta")
	f.fake.sameDocumentNavigation("https://example.test/beta/next")
	f.manager.updatePageInfo(f.fake.Handle(), "https://example.test/beta/next", "renamed", 1)
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

func TestFindEndsWhenThePageIsHiddenOrReplaced(t *testing.T) {
	f := newFindFixture(t, "https://example.test/alpha")
	started := f.find(t, "start", "alpha")
	f.wantCall(t, "start alpha")
	other, err := f.manager.NewCompanionPage(t.Context(), f.access, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(other.ID)); err != nil {
		t.Fatal(err)
	}
	ended := f.wantEnded(t, started.Session)
	f.wantCall(t, "stop")

	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(f.pageID)); err != nil {
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
	if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID()); err != nil {
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
	if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(f.pageID)); err != nil {
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
		state, err := f.manager.Find(context.Background(), f.access, PageID(f.pageID), "start", "alpha", f.mount, f.nextSeq())
		answered <- answer{state, err}
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- f.manager.ClosePage(context.Background(), f.access, PageID(f.pageID)) }()
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
	if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "next", "", f.mount, f.seq); !errors.Is(err, errPageNotFound) {
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
		if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "alpha", f.mount, f.nextSeq()); err == nil {
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
				if _, err := f.manager.Open(t.Context(), f.access, "https://example.test/beta", OpenOptions{PageID: PageID(f.pageID)}); err != nil {
					t.Fatal(err)
				}
			},
			back: func(t *testing.T, f *findFixture) {
				if _, err := f.manager.Open(t.Context(), f.access, "https://example.test/alpha", OpenOptions{PageID: PageID(f.pageID)}); err != nil {
					t.Fatal(err)
				}
			},
			stops: true,
		},
		{
			name: "switch page",
			away: func(t *testing.T, f *findFixture) {
				other, err := f.manager.NewCompanionPage(t.Context(), f.access, "")
				if err != nil {
					t.Fatal(err)
				}
				if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(other.ID)); err != nil {
					t.Fatal(err)
				}
			},
			back: func(t *testing.T, f *findFixture) {
				if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(f.pageID)); err != nil {
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
				if _, err := f.manager.Visibility(t.Context(), f.access, &show, PageID(f.pageID)); err != nil {
					t.Fatal(err)
				}
			},
			stops: true,
		},
		{
			name: "suspend",
			away: func(t *testing.T, f *findFixture) {
				if err := f.manager.SuspendThread(t.Context(), f.access.ThreadID()); err != nil {
					t.Fatal(err)
				}
			},
			back: func(t *testing.T, f *findFixture) {
				if err := f.manager.ActivateCompanionPage(t.Context(), f.access, PageID(f.pageID)); err != nil {
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
		if f.manager.keyChord(handle, escape, keyPress{}) {
			t.Fatalf("%s: AO took the page's Escape", state)
		}
		if f.manager.keyChord(handle, escape, keyPress{Held: true}) {
			t.Fatalf("%s: AO took the page's held Escape", state)
		}
		select {
		case event := <-f.escapes:
			if !announced {
				t.Fatalf("%s: an Escape was announced: %+v", state, event)
			}
			if ThreadID(event.ThreadID) != f.access.ThreadID() || event.PageID != f.pageID || event.Find != nil || event.Accelerator != nil {
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
	if f.manager.keyChord(handle, ctrl, keyPress{}) {
		t.Fatal("an unbound Ctrl+Escape was taken")
	}
	f.manager.accelerators = func() keybindings.AcceleratorSet { return keybindings.AcceleratorSet{ctrl: {}} }
	// A chord is routed to the pane showing its page.
	show := true
	if _, err := f.manager.Visibility(t.Context(), f.access, &show, PageID(f.pageID)); err != nil {
		t.Fatal(err)
	}
	f.place(t)
	if !f.manager.keyChord(handle, ctrl, keyPress{}) {
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

func TestFindIsRefusedWhereTheEngineHasNone(t *testing.T) {
	manager := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close manager: %v", err)
		}
	})
	manager.engine = findlessEngine{manager.engine.(*fakeEngine)}
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	info, err := manager.Open(t.Context(), access, "https://example.test/alpha", OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	show := true
	if _, err := manager.Visibility(t.Context(), access, &show, PageID(info.ID)); err != nil {
		t.Fatal(err)
	}
	if mode := manager.CompanionState(access.Caller()).FindMode; mode != "" {
		t.Fatalf("state find mode = %q on an engine without find", mode)
	}
	pane, err := manager.AttachPane(access.Caller())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Find(t.Context(), access, PageID(info.ID), "start", "alpha", MountID(pane.ID), 1); !errors.Is(err, errFindUnavailable) {
		t.Fatalf("find error = %v, want unavailable", err)
	}

	// A page whose engine runtime lacks the API says so the same way.
	f := newFindFixture(t, "https://example.test/alpha")
	f.fake.mu.Lock()
	f.fake.findMode = ""
	f.fake.mu.Unlock()
	if mode := f.manager.CompanionState(f.access.Caller()).FindMode; mode != "" {
		t.Fatalf("state find mode = %q for a page without find", mode)
	}
	if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "start", "alpha", f.mount, f.nextSeq()); !errors.Is(err, errFindUnavailable) {
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
	if mode := f.manager.CompanionState(f.access.Caller()).FindMode; mode != FindModeEngine {
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
	if _, err := f.manager.Find(t.Context(), f.access, PageID(f.pageID), "next", "", f.mount, f.seq); err == nil || !strings.Contains(err.Error(), "own find bar") {
		t.Fatalf("stepping the engine's bar error = %v", err)
	}
	f.find(t, "stop", "")
	f.wantCall(t, "stop")
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
									press := keyPress{Repeat: repeat, Held: held, Composing: composing}
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
