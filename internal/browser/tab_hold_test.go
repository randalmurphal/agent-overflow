package browser

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// awaitStructureWaiters waits until n goroutines wait out a jar switch at
// the structural gate (lockStructure).
func awaitStructureWaiters(t *testing.T, n int) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		size := runtime.Stack(buf, true)
		if size == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		waiting := 0
		for _, stack := range strings.Split(string(buf[:size]), "\n\n") {
			header, _, _ := strings.Cut(stack, "\n")
			if strings.Contains(header, "[select") && strings.Contains(stack, "browser.(*Manager).lockStructure(") {
				waiting++
			}
		}
		if waiting >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d goroutines never reached the structural gate", n)
}

// livePagesAt counts the live pages, in every jar, whose address is url.
func livePagesAt(m *Manager, url string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, scope := range m.scopes {
		for _, p := range scope.pages {
			if p.cachedInfo().URL == url {
				n++
			}
		}
	}
	return n
}

// A reopen or a duplicate whose tab's first live push meets a jar switch,
// after its creation took the claim and before its navigation, loads the
// claimed address nowhere: the switch forgot the browsing the address came
// from and suspended the tab, and the navigation acts only on the page the
// creation made. The tab stays, blank, in the new jar.
func TestAClaimedAddressDoesNotLoadAcrossAJarSwitch(t *testing.T) {
	for _, op := range []string{"reopen", "duplicate"} {
		for _, kind := range []string{JarProject, JarPrivate} {
			t.Run(fmt.Sprintf("%s into the %s jar", op, kind), func(t *testing.T) {
				m, access, ids := newTabsManager(t, addr("keep"), addr("x"))
				if op == "reopen" {
					if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[1])); err != nil {
						t.Fatal(err)
					}
				}
				var switched atomic.Bool
				switchErr := make(chan error, 1)
				m.SetEventSink(func(event CompanionEvent) {
					if event.Kind != "state" || switched.Load() {
						return
					}
					for _, page := range event.Pages {
						if !slices.Contains(ids, page.ID) && !page.Creating && page.Error == "" && switched.CompareAndSwap(false, true) {
							switchErr <- m.SetJar(context.Background(), access, kind, JarSwitchConfirmed)
						}
					}
				})
				var err error
				// A reopen's tab takes the closed one's place.
				wantTabs, wantTabsAtX, wantErr := 2, 0, error(nil)
				if op == "reopen" {
					err = m.ReopenClosedTab(t.Context(), access)
				} else {
					err = m.DuplicateCompanionPage(t.Context(), access, PageID(ids[1]))
					// The source tab keeps its address, and the duplicate
					// has nothing to duplicate.
					wantTabs, wantTabsAtX, wantErr = 3, 1, errPageNotFound
				}
				if !switched.Load() {
					t.Fatal("the new tab was never pushed live")
				}
				if serr := <-switchErr; serr != nil {
					t.Fatal(serr)
				}
				if !errors.Is(err, wantErr) || (wantErr == nil && err != nil) {
					t.Fatalf("%s across the switch answered %v, want %v", op, err, wantErr)
				}
				if n := livePagesAt(m, addr("x")); n != 0 {
					t.Fatalf("%d live pages loaded the forgotten address", n)
				}
				atX := 0
				for _, page := range m.CompanionState(access.Caller()).Pages {
					if page.URL == addr("x") {
						atX++
					}
				}
				if atX != wantTabsAtX {
					t.Fatalf("%d tabs are at the forgotten address, want %d", atX, wantTabsAtX)
				}
				// The sink above replaced the one closedCount reads.
				if got := m.CompanionState(access.Caller()).ClosedTabs; got != 0 {
					t.Fatalf("ClosedTabs = %d after the switch", got)
				}
				if got := len(m.CompanionState(access.Caller()).Pages); got != wantTabs {
					t.Fatalf("%d tabs, want %d with the new blank one", got, wantTabs)
				}
			})
		}
	}
}

// A label set on a live tab while a jar switch writes its saved copy lands
// on the tab as the switch left it, not on the page the switch suspended:
// the call answers what the tab shows.
func TestALabelDuringASwitchLandsOnTheTab(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"))
	type labeled struct {
		info PageInfo
		err  error
	}
	done := make(chan labeled, 1)
	m.switchWritten = func() {
		m.switchWritten = nil
		go func() {
			info, err := m.LabelPage(context.Background(), access, PageID(ids[0]), "Docs")
			done <- labeled{info, err}
		}()
		// The label resolved the live page and waits at the gate.
		awaitStructureWaiters(t, 1)
	}
	if err := m.SetJar(t.Context(), access, JarThread, JarSwitchConfirmed); err != nil {
		t.Fatal(err)
	}
	got := await(t, done, "the label")
	if got.err != nil {
		t.Fatal(got.err)
	}
	pages := m.CompanionState(access.Caller()).Pages
	if len(pages) != 1 || pages[0].Label != "Docs" || got.info.Label != "Docs" {
		t.Fatalf("the label answered %+v and the tab shows %+v", got.info, pages)
	}
}

// A show of a tab and the tab's close, both waiting out a jar switch's
// write, never leave the companion showing a tab the thread no longer has:
// the show checks the tab in the hold that shows it. Either may pass the
// gate first, so the pair runs several times.
func TestAShowNeverSelectsATabClosedDuringASwitch(t *testing.T) {
	for i := range 12 {
		m, access, ids := newTabsManager(t, addr("a"), addr("b"))
		shown, closed := make(chan error, 1), make(chan error, 1)
		m.switchWritten = func() {
			m.switchWritten = nil
			visible := true
			go func() {
				_, err := m.Visibility(context.Background(), access, &visible, PageID(ids[1]))
				shown <- err
			}()
			go func() { closed <- m.ClosePage(context.Background(), access, PageID(ids[1])) }()
			awaitStructureWaiters(t, 2)
		}
		if err := m.SetJar(t.Context(), access, JarThread, JarSwitchConfirmed); err != nil {
			t.Fatal(err)
		}
		if err := await(t, closed, "the close"); err != nil {
			t.Fatal(err)
		}
		if err := await(t, shown, "the show"); err != nil && !errors.Is(err, errPageNotFound) {
			t.Fatal(err)
		}
		state := m.CompanionState(access.Caller())
		if state.ActivePageID != "" && !slices.ContainsFunc(state.Pages, func(p PageInfo) bool { return p.ID == state.ActivePageID }) {
			t.Fatalf("run %d: the companion shows tab %s, which the thread no longer has", i, state.ActivePageID)
		}
	}
}
