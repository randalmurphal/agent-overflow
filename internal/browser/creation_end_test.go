package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/browser/ids"
)

// reportEnd reports handle's close (crash empty) or crash as its engine
// does.
func reportEnd(m *Manager, handle ids.PageHandle, crash string) {
	if crash == "" {
		m.pageClosed(handle)
		return
	}
	m.pageCrashedReport(handle, crash)
}

// A page its engine reports closed or crashed before NewPage returns, so
// before the Manager knows its handle, is never listed as a live tab: a
// close fails the creation and leaves no tab, a crash leaves the crashed
// tab a listed page's crash does. Both for the user's tab and an agent's.
func TestAPageThatEndsAsItIsCreatedIsNotListedLive(t *testing.T) {
	for _, crash := range []string{"", "oom"} {
		for _, user := range []bool{true, false} {
			name := map[bool]string{true: "user", false: "agent"}[user] + " " + map[string]string{"": "close", "oom": "crash"}[crash]
			t.Run(name, func(t *testing.T) {
				m, engine := newLifecycleManager(t)
				access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
				engine.mu.Lock()
				engine.onNewFakePage = func(page *fakePage) { reportEnd(m, page.Handle(), crash) }
				engine.mu.Unlock()
				var err error
				if user {
					_, err = m.NewCompanionPage(t.Context(), access, "")
				} else {
					_, err = m.NewPage(t.Context(), access)
				}
				if livePagesAt(m, "about:blank") != 0 {
					t.Fatal("a page its engine ended was listed live")
				}
				tabs := m.CompanionState(access.Caller()).Pages
				m.mu.Lock()
				pending := len(m.pending)
				m.mu.Unlock()
				if pending != 0 {
					t.Fatalf("%d creations are left", pending)
				}
				if crash == "" {
					if !errors.Is(err, errClosedBeforeListing) {
						t.Fatalf("the creation answered %v, want errClosedBeforeListing", err)
					}
					if len(tabs) != 0 {
						t.Fatalf("a closed page left tabs %+v", tabs)
					}
					return
				}
				if user && err != nil {
					t.Fatalf("the user's crashed tab answered %v; it shows its failure", err)
				}
				if !user && (err == nil || !strings.Contains(err.Error(), "oom")) {
					t.Fatalf("the agent's creation answered %v, want the crash", err)
				}
				if len(tabs) != 1 || !tabs[0].Suspended || !strings.Contains(tabs[0].Error, "oom") {
					t.Fatalf("a crashed page left tabs %+v, want one crashed tab", tabs)
				}
			})
		}
	}
}

// A restored page its engine reports closed or crashed while it loads,
// after its attach and before its listing, is not listed live: the tab
// stays suspended, with the crash as its failure.
func TestARestoreWhosePageEndsBeforeItsListingStaysSuspended(t *testing.T) {
	for _, crash := range []string{"", "oom"} {
		t.Run(map[string]string{"": "close", "oom": "crash"}[crash], func(t *testing.T) {
			m, engine := newLifecycleManager(t)
			access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			page := mustOpen(t, m, access, "https://example.test/a")
			live := livePage(t, m, access, page.ID)
			if moved, err := m.suspendPage(t.Context(), live); err != nil || !moved {
				t.Fatalf("suspend: %v", err)
			}
			release := engine.hold(&engine.navGate)
			defer release()
			restored := restore(m, access, page.ID)
			receive(t, engine.navigating, "the restore's load")
			m.mu.Lock()
			var handle ids.PageHandle
			for _, scope := range m.scopes {
				for p := range scope.creating {
					if p.id == PageID(page.ID) {
						handle = p.driver.Handle()
					}
				}
			}
			m.mu.Unlock()
			if handle == "" {
				t.Fatal("the restore's page is not attached")
			}
			reportEnd(m, handle, crash)
			release()
			err := <-restored
			if crash == "" && !errors.Is(err, errClosedBeforeListing) {
				t.Fatalf("the restore answered %v, want errClosedBeforeListing", err)
			}
			if crash != "" && (err == nil || !strings.Contains(err.Error(), "oom")) {
				t.Fatalf("the restore answered %v, want the crash", err)
			}
			if n := livePagesAt(m, "https://example.test/a"); n != 0 {
				t.Fatal("a restored page its engine ended was listed live")
			}
			tabs := m.CompanionState(access.Caller()).Pages
			if len(tabs) != 1 || !tabs[0].Suspended {
				t.Fatalf("tabs %+v, want the one suspended tab", tabs)
			}
			if failed := strings.Contains(tabs[0].Error, "oom"); failed != (crash != "") {
				t.Fatalf("the tab shows %q", tabs[0].Error)
			}
		})
	}
}

// setupStep is a step of a page's setup after its attach, which fail makes
// fail for the page its engine is creating.
type setupStep struct {
	name string
	fail func(e *lifecycleEngine, page *fakePage)
}

var (
	sizingStep = setupStep{"sizing", func(_ *lifecycleEngine, page *fakePage) {
		page.mu.Lock()
		page.viewportErr = errors.New("sizing refused")
		page.mu.Unlock()
	}}
	loadingStep = setupStep{"loading", func(_ *lifecycleEngine, page *fakePage) {
		page.mu.Lock()
		page.navigateHook = func(context.Context, string) error { return errors.New("load refused") }
		page.mu.Unlock()
	}}
	readingStep = setupStep{"reading", func(e *lifecycleEngine, _ *fakePage) {
		e.mu.Lock()
		e.infoErr = errors.New("read refused")
		e.mu.Unlock()
	}}
)

// endThenFail has the next page e creates report its close (crash empty)
// or crash before NewPage returns, and then fail step.
func endThenFail(m *Manager, e *lifecycleEngine, crash string, step setupStep) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onNewFakePage = func(page *fakePage) {
		reportEnd(m, page.Handle(), crash)
		step.fail(e, page)
	}
}

// noneReserved fails when a page is still reserved in a jar.
func noneReserved(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, scope := range m.scopes {
		if len(scope.creating) != 0 {
			t.Fatalf("%d pages are still reserved in a jar", len(scope.creating))
		}
	}
}

// A setup step that fails after the engine reported the page's end does
// not decide the page: the end does. A close fails the creation with
// errClosedBeforeListing and leaves no tab, a crash leaves the crashed tab
// with its reason, for the user's tab and an agent's; a restore keeps its
// tab suspended, carrying the crash when there was one.
func TestAFailedSetupStepAfterAPagesEndAnswersTheEnd(t *testing.T) {
	ends := map[string]string{"close": "", "crash": "oom"}
	for endName, crash := range ends {
		for _, user := range []bool{true, false} {
			name := fmt.Sprintf("create %s %s %s", map[bool]string{true: "user", false: "agent"}[user], endName, sizingStep.name)
			t.Run(name, func(t *testing.T) {
				m, engine := newLifecycleManager(t)
				access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
				endThenFail(m, engine, crash, sizingStep)
				var err error
				if user {
					_, err = m.NewCompanionPage(t.Context(), access, "")
				} else {
					_, err = m.NewPage(t.Context(), access)
				}
				noneReserved(t, m)
				tabs := m.CompanionState(access.Caller()).Pages
				if crash == "" {
					if !errors.Is(err, errClosedBeforeListing) || len(tabs) != 0 {
						t.Fatalf("answered %v with tabs %+v, want errClosedBeforeListing and none", err, tabs)
					}
					return
				}
				if user != (err == nil) || (!user && !strings.Contains(err.Error(), "oom")) {
					t.Fatalf("answered %v, want the crash (shown on the user's tab)", err)
				}
				if len(tabs) != 1 || !tabs[0].Suspended || !strings.Contains(tabs[0].Error, "oom") {
					t.Fatalf("tabs %+v, want one crashed tab", tabs)
				}
			})
		}
		for _, step := range []setupStep{sizingStep, loadingStep, readingStep} {
			t.Run(fmt.Sprintf("restore %s %s", endName, step.name), func(t *testing.T) {
				m, engine := newLifecycleManager(t)
				access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
				page := mustOpen(t, m, access, "https://example.test/a")
				suspendTab(t, m, access, page.ID)
				endThenFail(m, engine, crash, step)
				err := <-restore(m, access, page.ID)
				engine.mu.Lock()
				engine.infoErr = nil
				engine.mu.Unlock()
				noneReserved(t, m)
				if crash == "" && !errors.Is(err, errClosedBeforeListing) {
					t.Fatalf("the restore answered %v, want errClosedBeforeListing", err)
				}
				if crash != "" && (err == nil || !strings.Contains(err.Error(), "oom")) {
					t.Fatalf("the restore answered %v, want the crash", err)
				}
				tabs := m.CompanionState(access.Caller()).Pages
				if len(tabs) != 1 || !tabs[0].Suspended {
					t.Fatalf("tabs %+v, want the one suspended tab", tabs)
				}
				if failed := strings.Contains(tabs[0].Error, "oom"); failed != (crash != "") {
					t.Fatalf("the tab shows %q", tabs[0].Error)
				}
			})
		}
	}
}

// A creation whose page crashes while site data is cleared, after the
// clear dropped the suspended tabs and before it closes the browser, lists
// no crashed tab: the clear answers success and leaves no tab at all.
func TestClearSiteDataLeavesNoTabOfACreationThatCrashedDuringIt(t *testing.T) {
	for _, user := range []bool{true, false} {
		t.Run(map[bool]string{true: "user", false: "agent"}[user], func(t *testing.T) {
			m, engine := newLifecycleManager(t)
			access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			old := mustOpen(t, m, access, "https://example.test/old")
			suspendTab(t, m, access, old.ID)
			release := engine.hold(&engine.newPageGate)
			defer release()
			engine.mu.Lock()
			engine.onNewFakePage = func(page *fakePage) { reportEnd(m, page.Handle(), "oom") }
			engine.mu.Unlock()
			created := make(chan error, 1)
			go func() {
				var err error
				if user {
					_, err = m.NewCompanionPage(context.Background(), access, "")
				} else {
					_, err = m.NewPage(context.Background(), access)
				}
				created <- err
			}()
			receive(t, engine.creating, "the new page's creation")
			cleared := make(chan error, 1)
			go func() { cleared <- m.ClearSiteData(context.Background()) }()
			eventually(t, "the clear to drop the suspended tab", func() bool {
				m.mu.Lock()
				defer m.mu.Unlock()
				return m.suspended[PageID(old.ID)] == nil
			})
			release()
			err := <-created
			if user && err != nil || !user && !errors.Is(err, errClosedWhileCreating) {
				t.Fatalf("the creation answered %v", err)
			}
			if err := <-cleared; err != nil {
				t.Fatalf("clear site data: %v", err)
			}
			if tabs := m.CompanionState(access.Caller()).Pages; len(tabs) != 0 {
				t.Fatalf("the clear left tabs %+v", tabs)
			}
			noneReserved(t, m)
		})
	}
}

// The crashed tab of a creation, closed while the creation still closes
// its engine page, leaves no selection behind: the thread selects no tab
// once the creation returns, since it has none.
func TestClosingACrashedCreationsTabLeavesNoSelectionOfIt(t *testing.T) {
	for _, user := range []bool{true, false} {
		t.Run(map[bool]string{true: "user", false: "agent"}[user], func(t *testing.T) {
			m, engine := newLifecycleManager(t)
			access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			release := engine.hold(&engine.closeGate)
			defer release()
			engine.mu.Lock()
			engine.onNewFakePage = func(page *fakePage) { reportEnd(m, page.Handle(), "oom") }
			engine.mu.Unlock()
			created := make(chan error, 1)
			go func() {
				var err error
				if user {
					_, err = m.NewCompanionPage(context.Background(), access, "")
				} else {
					_, err = m.NewPage(context.Background(), access)
				}
				created <- err
			}()
			receive(t, engine.closing, "the crashed page's engine close")
			tabs := m.CompanionState(access.Caller()).Pages
			if len(tabs) != 1 || !tabs[0].Suspended {
				t.Fatalf("tabs %+v, want the crashed tab", tabs)
			}
			if err := m.ClosePage(t.Context(), access, PageID(tabs[0].ID)); err != nil {
				t.Fatalf("close the crashed tab: %v", err)
			}
			release()
			<-created
			state := m.CompanionState(access.Caller())
			if len(state.Pages) != 0 || state.ActivePageID != "" {
				t.Fatalf("tabs %+v selecting %q, want no tab and no selection", state.Pages, state.ActivePageID)
			}
		})
	}
}
