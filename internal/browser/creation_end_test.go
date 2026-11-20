package browser

import (
	"errors"
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
