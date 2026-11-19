package browser

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/webview2host"
)

// focusCalls answers the engine's focus requests since the last take.
func focusCalls(e *recordingPaneHost) []string {
	var calls []string
	for _, call := range e.take() {
		if strings.HasPrefix(call, "focus:") {
			calls = append(calls, call)
		}
	}
	return calls
}

// focusEvents collects the Manager's Kind "focus" events.
type focusEvents struct {
	mu     sync.Mutex
	events []CompanionEvent
}

func watchFocusEvents(m *Manager) *focusEvents {
	f := &focusEvents{}
	m.SetEventSink(func(event CompanionEvent) {
		if event.Kind == "focus" {
			f.mu.Lock()
			f.events = append(f.events, event)
			f.mu.Unlock()
		}
	})
	return f
}

func (f *focusEvents) all() []CompanionEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

func (w *focusWorld) focusHolder() string {
	focus, _ := w.e.state()
	return focus
}

// The address bar's Enter and Escape hand the keyboard to the page the
// thread's pane presents, and to no other thread's page, and the pane hears
// that its intent landed.
func TestFocusCompanionPageFocusesThePresentedPage(t *testing.T) {
	w := newFocusWorld(t)
	events := watchFocusEvents(w.m)
	w.e.take()
	if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1); err != nil {
		t.Fatalf("focus thread a's page: %v", err)
	}
	if focus := w.focusHolder(); focus != w.handle(w.a, w.aPage) {
		t.Fatalf("focus is with %q, want thread a's presented page", focus)
	}
	if calls := focusCalls(w.e.recordingPaneHost); !slices.Equal(calls, []string{"focus:" + w.handle(w.a, w.aPage)}) {
		t.Fatalf("focus requests %v, want thread a's presented page alone", calls)
	}
	want := CompanionEvent{Kind: "focus", ThreadID: string(w.a.ThreadID), Mount: string(w.aMount), FocusIntent: 1}
	if got := events.all(); len(got) != 1 || got[0].Kind != want.Kind || got[0].ThreadID != want.ThreadID ||
		got[0].Mount != want.Mount || got[0].FocusIntent != want.FocusIntent || got[0].Error != "" {
		t.Fatalf("focus events %+v, want %+v", got, want)
	}
	if err := w.m.FocusCompanionPage(w.b, w.bMount, PageID(w.bPage), 1); err != nil {
		t.Fatalf("focus thread b's page: %v", err)
	}
	if focus := w.focusHolder(); focus != w.handle(w.b, w.bPage) {
		t.Fatalf("focus is with %q, want thread b's presented page", focus)
	}
	// A mount belongs to its own thread.
	if err := w.m.FocusCompanionPage(w.b, w.aMount, PageID(w.aPage), 2); err == nil {
		t.Fatal("thread b armed a focus on thread a's mount")
	}
}

// A request is withdrawn by the user's next focus choice. A delayed request
// that reaches the engine after it must leave the keyboard where the user
// put it: in the composer, back in the address bar, or with the tab the
// user switched to.
func TestADelayedFocusIsWithdrawnByTheUsersNextFocusChoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choose func(t *testing.T, w *focusWorld)
	}{
		{"the composer focused", func(t *testing.T, w *focusWorld) {
			if err := w.m.WithdrawCompanionPageFocus(w.a, w.aMount, 1); err != nil {
				t.Fatalf("withdraw: %v", err)
			}
		}},
		{"the address bar refocused", func(t *testing.T, w *focusWorld) {
			if err := w.m.WithdrawCompanionPageFocus(w.a, w.aMount, 1); err != nil {
				t.Fatalf("withdraw: %v", err)
			}
		}},
		{"a tab switch", func(t *testing.T, w *focusWorld) {
			if err := w.m.ActivateCompanionPage(t.Context(), w.a, PageID(w.aOther)); err != nil {
				t.Fatalf("switch tabs: %v", err)
			}
			if err := w.m.WithdrawCompanionPageFocus(w.a, w.aMount, 1); err != nil {
				t.Fatalf("withdraw: %v", err)
			}
		}},
		// Another pane of the thread takes over the page, still shown.
		{"the pane unmounting", func(t *testing.T, w *focusWorld) {
			w.m.DetachPane(w.aMount)
		}},
	} {
		t.Run(tc.name+" before the request reaches the UI thread", func(t *testing.T) {
			w := newFocusWorld(t)
			remountPane(t, w.m, w.a)
			events := watchFocusEvents(w.m)
			page := w.handle(w.a, w.aPage)
			var once sync.Once
			w.e.mu.Lock()
			w.e.beforeFocus = func(handle string) {
				if handle == page {
					once.Do(func() { tc.choose(t, w) })
				}
			}
			w.e.mu.Unlock()
			if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1); err != nil {
				t.Fatalf("focus: %v", err)
			}
			if focus := w.focusHolder(); focus != focusHeldByTheApp {
				t.Fatalf("focus is with %q after the user chose elsewhere, want the app", focus)
			}
			if got := events.all(); len(got) != 0 {
				t.Fatalf("a withdrawn focus reported %+v", got)
			}
		})
		t.Run(tc.name+" before the request reaches the Manager", func(t *testing.T) {
			w := newFocusWorld(t)
			remountPane(t, w.m, w.a)
			tc.choose(t, w)
			w.e.take()
			err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1)
			if tc.name == "the pane unmounting" {
				if err == nil {
					t.Fatal("a request for an unmounted pane was accepted")
				}
			} else if err != nil {
				t.Fatalf("focus: %v", err)
			}
			if calls := focusCalls(w.e.recordingPaneHost); len(calls) != 0 {
				t.Fatalf("a withdrawn request reached the engine: %v", calls)
			}
			if focus := w.focusHolder(); focus != focusHeldByTheApp {
				t.Fatalf("focus is with %q, want the app", focus)
			}
		})
	}
}

// A request names its page: when another tab became presented before the
// request arrived, neither tab takes the keyboard.
func TestAFocusRequestNeverFocusesAnotherTab(t *testing.T) {
	w := newFocusWorld(t)
	if err := w.m.ActivateCompanionPage(t.Context(), w.a, PageID(w.aOther)); err != nil {
		t.Fatalf("switch tabs: %v", err)
	}
	// The other tab shows once it has laid out at the pane's size.
	other := w.handle(w.a, w.aOther)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, shown := w.e.state(); shown[other] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other tab was never shown")
		}
	}
	if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1); err != nil {
		t.Fatalf("focus: %v", err)
	}
	if focus := w.focusHolder(); focus != focusHeldByTheApp {
		t.Fatalf("focus is with %q, want the app", focus)
	}
}

// A withdrawal that reaches the Manager after the focus landed changes
// nothing: the page may hold the keyboard by the user's own click since,
// which nothing here can tell apart from the intent's move.
func TestAWithdrawalAfterTheFocusLandedLeavesIt(t *testing.T) {
	w := newFocusWorld(t)
	page := w.handle(w.a, w.aPage)
	if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1); err != nil {
		t.Fatalf("focus: %v", err)
	}
	if focus := w.focusHolder(); focus != page {
		t.Fatalf("focus is with %q, want the page", focus)
	}
	w.e.click(t, page)
	if err := w.m.WithdrawCompanionPageFocus(w.a, w.aMount, 1); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if focus := w.focusHolder(); focus != page {
		t.Fatalf("focus is with %q after a late withdrawal, want the clicked page", focus)
	}
}

// A newer request supersedes an older one on the mount, and an older one
// arriving late changes nothing.
func TestANewerFocusRequestSupersedesTheOlder(t *testing.T) {
	w := newFocusWorld(t)
	hide := false
	if _, err := w.m.Visibility(t.Context(), w.a, &hide, ""); err != nil {
		t.Fatalf("hide: %v", err)
	}
	if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aOther), 1); err != nil {
		t.Fatalf("focus the other tab: %v", err)
	}
	if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 2); err != nil {
		t.Fatalf("focus the page: %v", err)
	}
	if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aOther), 1); err != nil {
		t.Fatalf("a late older request: %v", err)
	}
	show := true
	if _, err := w.m.Visibility(t.Context(), w.a, &show, PageID(w.aOther)); err != nil {
		t.Fatalf("show the other tab: %v", err)
	}
	if focus := w.focusHolder(); focus != focusHeldByTheApp {
		t.Fatalf("focus is with %q, want the app: the request for this tab was superseded", focus)
	}
}

// A request for a page that is not presented yet (the pane hidden or off
// screen, the tab suspended and restoring) waits, and is applied once when
// the page is presented. The show itself never focuses.
func TestAFocusRequestWaitsForItsPageToBePresented(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unshow  func(t *testing.T, w *focusWorld)
		present func(t *testing.T, w *focusWorld)
	}{
		{"the companion hidden",
			func(t *testing.T, w *focusWorld) {
				hide := false
				if _, err := w.m.Visibility(t.Context(), w.a, &hide, ""); err != nil {
					t.Fatalf("hide the companion: %v", err)
				}
			},
			func(t *testing.T, w *focusWorld) {
				show := true
				if _, err := w.m.Visibility(t.Context(), w.a, &show, PageID(w.aPage)); err != nil {
					t.Fatalf("show the companion: %v", err)
				}
			}},
		{"the pane off screen",
			func(t *testing.T, w *focusWorld) {
				rect := visiblePaneRect
				rect.Visible = false
				if err := w.m.SetPaneRect(w.aMount, rect); err != nil {
					t.Fatalf("report the pane off screen: %v", err)
				}
			},
			func(t *testing.T, w *focusWorld) {
				if err := w.m.SetPaneRect(w.aMount, visiblePaneRect); err != nil {
					t.Fatalf("report the pane on screen: %v", err)
				}
			}},
		{"the tab suspended and restored by its navigation",
			// The tab is selected in a visible pane but not loaded: the
			// address takes Enter, which navigates and so restores it.
			func(t *testing.T, w *focusWorld) {
				hide, show := false, true
				if _, err := w.m.Visibility(t.Context(), w.a, &hide, ""); err != nil {
					t.Fatalf("hide the companion: %v", err)
				}
				if err := w.m.SuspendThread(t.Context(), w.a.ThreadID); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				if ids := suspendedIDs(w.m, string(w.a.ThreadID)); !slices.Contains(ids, w.aPage) {
					t.Fatalf("suspended %v, want the page among them", ids)
				}
				if _, err := w.m.Visibility(t.Context(), w.a, &show, PageID(w.aPage)); err != nil {
					t.Fatalf("show the suspended tab: %v", err)
				}
			},
			func(t *testing.T, w *focusWorld) {
				if _, err := w.m.NavigateCompanion(t.Context(), w.a, PageID(w.aPage), "https://example.test/next"); err != nil {
					t.Fatalf("navigate: %v", err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFocusWorld(t)
			tc.unshow(t, w)
			w.e.take()
			if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1); err != nil {
				t.Fatalf("focus with nothing presented: %v", err)
			}
			if calls := focusCalls(w.e.recordingPaneHost); len(calls) != 0 {
				t.Fatalf("focus requests %v with nothing presented", calls)
			}
			tc.present(t, w)
			page := w.handle(w.a, w.aPage)
			if focus := w.focusHolder(); focus != page {
				t.Fatalf("focus is with %q once the page is presented, want the page", focus)
			}
			// Applied once: hidden and shown again, the page does not take
			// the keyboard a second time.
			tc.unshow(t, w)
			if focus := w.focusHolder(); focus == page {
				t.Fatal("the page kept the focus through its hide")
			}
			tc.present(t, w)
			if focus := w.focusHolder(); focus == page {
				t.Fatal("a second presentation focused the page again")
			}
		})
	}
}

// A waiting request ends with its tab closing, with its age, with the
// user's next focus choice, and with its pane unmounting.
func TestAWaitingFocusRequestEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, w *focusWorld)
	}{
		{"its tab closed", func(t *testing.T, w *focusWorld) {
			if err := w.m.CloseCompanionPage(t.Context(), w.a, PageID(w.aPage)); err != nil {
				t.Fatalf("close the page: %v", err)
			}
		}},
		{"its age", func(t *testing.T, w *focusWorld) {
			w.m.mu.Lock()
			w.m.panes[w.aMount].focus.intent.armed = time.Now().Add(-focusIntentMaxAge - time.Second)
			w.m.mu.Unlock()
		}},
		{"a withdrawal", func(t *testing.T, w *focusWorld) {
			if err := w.m.WithdrawCompanionPageFocus(w.a, w.aMount, 1); err != nil {
				t.Fatalf("withdraw: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFocusWorld(t)
			hide := false
			if _, err := w.m.Visibility(t.Context(), w.a, &hide, ""); err != nil {
				t.Fatalf("hide the companion: %v", err)
			}
			if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1); err != nil {
				t.Fatalf("focus: %v", err)
			}
			tc.end(t, w)
			show, target := true, w.aPage
			if !w.m.hasTab(w.a.tb, PageID(w.aPage)) {
				target = w.aOther
			}
			if _, err := w.m.Visibility(t.Context(), w.a, &show, PageID(target)); err != nil {
				t.Fatalf("show the companion: %v", err)
			}
			if focus := w.focusHolder(); focus != focusHeldByTheApp {
				t.Fatalf("focus is with %q, want the app", focus)
			}
			w.m.mu.Lock()
			kept := w.m.panes[w.aMount].focus.intent
			w.m.mu.Unlock()
			if kept != nil {
				t.Fatalf("the ended request is still held: %+v", kept)
			}
		})
	}

	t.Run("its pane unmounted", func(t *testing.T) {
		w := newFocusWorld(t)
		hide := false
		if _, err := w.m.Visibility(t.Context(), w.a, &hide, ""); err != nil {
			t.Fatalf("hide the companion: %v", err)
		}
		if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1); err != nil {
			t.Fatalf("focus: %v", err)
		}
		w.m.DetachPane(w.aMount)
		show := true
		if _, err := w.m.Visibility(t.Context(), w.a, &show, PageID(w.aPage)); err != nil {
			t.Fatalf("show the companion: %v", err)
		}
		mount := remountPane(t, w.m, w.a)
		if focus := w.focusHolder(); focus != focusHeldByTheApp {
			t.Fatalf("focus is with %q under a new mount %s, want the app", focus, mount)
		}
		if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 2); err == nil {
			t.Fatal("a focus armed on an unmounted pane was accepted")
		}
		if err := w.m.WithdrawCompanionPageFocus(w.a, w.aMount, 2); err != nil {
			t.Fatalf("withdrawing on an unmounted pane: %v", err)
		}
	})
}

// An engine with no native views presents nothing to focus.
func TestFocusCompanionPageOnAnEngineWithoutViewsIsANoOp(t *testing.T) {
	m := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.FocusCompanionPage(m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()}), "mount", "page", 1); err != nil {
		t.Fatalf("focus on an engine with no views: %v", err)
	}
}

// A page that is closing is never focused, even while the pass that hides
// it still has it recorded as presented.
func TestAClosingPageIsNeverFocused(t *testing.T) {
	w := newFocusWorld(t)
	closing := w.handle(w.a, w.aPage)
	asked := make(chan error, 1)
	w.e.mu.Lock()
	w.e.onHide = func(handle string) {
		if handle == closing {
			select {
			case asked <- w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 1):
			default:
			}
		}
	}
	w.e.mu.Unlock()
	w.e.take()
	if err := w.m.CloseCompanionPage(t.Context(), w.a, PageID(w.aPage)); err != nil {
		t.Fatalf("close the page: %v", err)
	}
	select {
	case err := <-asked:
		if err != nil {
			t.Fatalf("focus while the page closes: %v", err)
		}
	default:
		t.Fatal("the closing page was never hidden")
	}
	for _, call := range focusCalls(w.e.recordingPaneHost) {
		if call == "focus:"+closing {
			t.Fatal("the closing page was asked to take focus")
		}
	}
	if focus := w.focusHolder(); focus == closing {
		t.Fatal("the closing page holds the focus")
	}
}

// An engine that refuses the focus reaches the pane as a Kind "focus" event
// for the intent, naming the page.
func TestFocusCompanionPageReportsTheEnginesRefusal(t *testing.T) {
	w := newFocusWorld(t)
	events := watchFocusEvents(w.m)
	w.e.mu.Lock()
	w.e.focusErr = errors.New("view refused")
	w.e.mu.Unlock()
	if err := w.m.FocusCompanionPage(w.a, w.aMount, PageID(w.aPage), 3); err != nil {
		t.Fatalf("focus: %v", err)
	}
	got := events.all()
	if len(got) != 1 || MountID(got[0].Mount) != w.aMount || got[0].FocusIntent != 3 ||
		!strings.Contains(got[0].Error, "view refused") || !strings.Contains(got[0].Error, w.aPage) {
		t.Fatalf("refused focus events %+v", got)
	}
	// An engine's UI-thread dispatch that missed its deadline still runs
	// after the error: it finds the request over.
	w.e.mu.Lock()
	late := w.e.lastFocus
	w.e.mu.Unlock()
	if late.Live == nil || late.Live() {
		t.Fatal("a failed request is still live for a dispatch that runs late")
	}
}

// The hosted engine asks the launcher to focus only a page it shows and an
// intent that is live, naming the intent, on the page's own lane.
func TestHostedFocusPageDirective(t *testing.T) {
	engine, sink := newTestHostedEngine(t, stubRelay{}, engineEvents{})
	bindTestPage(t, engine, "page1", "TARGET-1")
	engine.mu.Lock()
	engine.drivers["page1"] = &cdpPage{ctx: t.Context()}
	engine.readPageRatio = func(context.Context, *cdpPage) (float64, error) { return 1, nil }
	engine.mu.Unlock()
	live := func() bool { return true }

	if result, err := engine.FocusPage("page1", focusRequest{Mount: "mount1", Intent: 4, Live: live}); err != nil || result != focusNotPresented {
		t.Fatalf("focus a hidden page = %v, %v", result, err)
	}
	engine.ShowPage("page1")
	if result, err := engine.FocusPage("page1", focusRequest{Mount: "mount1", Intent: 4, Live: func() bool { return false }}); err != nil || result != focusWithdrawn {
		t.Fatalf("focus a withdrawn intent = %v, %v", result, err)
	}
	if ops := sink.ops(); slices.Contains(ops, webview2host.OpFocus) {
		t.Fatalf("a hidden page or a withdrawn intent was sent: %v", ops)
	}
	if result, err := engine.FocusPage("page1", focusRequest{Mount: "mount1", Intent: 4, Live: live}); err != nil || result != focusDispatched {
		t.Fatalf("focus a shown page = %v, %v", result, err)
	}
	var focus *webview2host.Directive
	sink.mu.Lock()
	all := slices.Clone(sink.all)
	sink.mu.Unlock()
	for _, d := range all {
		if d.Op == webview2host.OpFocus {
			focus = &d
		}
	}
	if focus == nil || focus.PageID != "page1" || focus.Mount != "mount1" || focus.Intent != 4 {
		t.Fatalf("directives %+v, want a focus for page1's intent 4 on mount1", all)
	}
	if err := focus.Validate(); err != nil {
		t.Fatalf("the focus directive is invalid: %v", err)
	}
}

// The launcher's answers to a focus travel the report route the launcher
// uses (Manager.ReportPaneHost) and reach the mount that armed the intent
// as Kind "focus" events, a refusal with its error.
func TestTheLaunchersFocusReportsReachTheIntentsPane(t *testing.T) {
	sink := newDirectiveSink()
	m := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{PaneHost: &PaneHostOptions{
		Directive: sink.send,
		Relay:     stubRelay{},
	}})
	t.Cleanup(func() { _ = m.Close() })
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	m.mu.Lock()
	m.panes["mount1"] = paneMount{threadID: access.ThreadID, owner: access.tb, seq: 1}
	m.mu.Unlock()
	page := &managedPage{id: "page1", owner: access.tb, driver: &fakePage{handle: "handle1"}}
	m.mu.Lock()
	m.scopes = map[jarKey]*jarScope{"scope": {pages: map[PageID]*managedPage{"page1": page}}}
	m.mu.Unlock()
	// The stand-in page has no engine page behind it for Close to close.
	t.Cleanup(func() {
		m.mu.Lock()
		m.scopes = map[jarKey]*jarScope{}
		m.mu.Unlock()
	})
	events := watchFocusEvents(m)

	failed := webview2host.FocusReport{Mount: "mount1", Intent: 2, Error: "E_FAIL"}.Encode()
	if err := m.ReportPaneHost("handle1", webview2host.ReportFocusFailed, failed); err != nil {
		t.Fatalf("route a focus failure: %v", err)
	}
	focused := webview2host.FocusReport{Mount: "mount1", Intent: 3}.Encode()
	if err := m.ReportPaneHost("handle1", webview2host.ReportFocused, focused); err != nil {
		t.Fatalf("route a focus: %v", err)
	}
	// A report for a mount of another thread reaches nobody.
	other := webview2host.FocusReport{Mount: "mount-gone", Intent: 1}.Encode()
	if err := m.ReportPaneHost("handle1", webview2host.ReportFocused, other); err != nil {
		t.Fatalf("route a focus for a gone mount: %v", err)
	}
	got := events.all()
	if len(got) != 2 {
		t.Fatalf("focus events %+v, want the failure and the focus", got)
	}
	if ThreadID(got[0].ThreadID) != access.ThreadID || got[0].Mount != "mount1" || got[0].FocusIntent != 2 || !strings.Contains(got[0].Error, "E_FAIL") {
		t.Fatalf("failure event %+v", got[0])
	}
	if got[1].Mount != "mount1" || got[1].FocusIntent != 3 || got[1].Error != "" {
		t.Fatalf("focus event %+v", got[1])
	}
}
