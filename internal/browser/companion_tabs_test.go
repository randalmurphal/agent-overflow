package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// tabPushes holds each tabs manager's state pushes (newTabsManager).
var tabPushes sync.Map

// newTabsManager opens one user tab per address, in order, and answers
// their ids.
func newTabsManager(t *testing.T, addresses ...string) (*Manager, Access, []string) {
	t.Helper()
	m := NewManager(t.TempDir(), Config{Enabled: true}, ManagerOptions{FakeEngine: true})
	t.Cleanup(func() { _ = m.Close() })
	checkSelectionAtEnd(t, m)
	tabPushes.Store(m, countStatePushes(m))
	t.Cleanup(func() { tabPushes.Delete(m) })
	access := m.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir(), ProjectID: "project"})
	var ids []string
	for _, address := range addresses {
		page, err := m.NewCompanionPage(t.Context(), access, address)
		if err != nil {
			t.Fatalf("open %s: %v", address, err)
		}
		ids = append(ids, page.ID)
	}
	return m, access, ids
}

// openTab opens a user tab, waiting out the slot a closed tab's teardown
// holds until it ends, which runs after its close returned.
func openTab(t *testing.T, m *Manager, access Access, address string) (PageInfo, error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, err := m.NewCompanionPage(t.Context(), access, address)
		if err == nil || !strings.Contains(err.Error(), "still shutting down") || time.Now().After(deadline) {
			return page, err
		}
		time.Sleep(time.Millisecond)
	}
}

// reopenTab is ReopenClosedTab, waiting out closed tabs' teardowns as
// openTab does.
func reopenTab(t *testing.T, m *Manager, access Access) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := m.ReopenClosedTab(t.Context(), access)
		if err == nil || !strings.Contains(err.Error(), "still shutting down") || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

// newestPush is the newest state the manager pushed, as a client keeps it.
func newestPush(t *testing.T, m *Manager) CompanionEvent {
	t.Helper()
	pushes, ok := tabPushes.Load(m)
	if !ok {
		t.Fatal("the manager is not a tabs manager")
	}
	c := pushes.(*statePushes)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.newest
}

// closedCount is the thread's reopenable tab count, which the newest push
// must already show.
func closedCount(t *testing.T, m *Manager, access Access) int {
	t.Helper()
	read := m.CompanionState(access.Caller()).ClosedTabs
	if pushed := newestPush(t, m).ClosedTabs; pushed != read {
		t.Fatalf("the newest push shows %d closed tabs, the state %d", pushed, read)
	}
	return read
}

func addr(name string) string { return "https://example.test/" + name }

// A tab the user closed reopens where it was, with its address and label,
// selected; the state counts what can be reopened.
func TestReopenClosedTabPutsItBackWhereItWas(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"), addr("c"))
	if _, err := m.LabelPage(t.Context(), access, PageID(ids[1]), "docs"); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[1])); err != nil {
		t.Fatal(err)
	}
	if got := closedCount(t, m, access); got != 1 {
		t.Fatalf("ClosedTabs = %d, want 1", got)
	}
	if err := reopenTab(t, m, access); err != nil {
		t.Fatal(err)
	}
	state := m.CompanionState(access.Caller())
	if got := tabURLs(m, access); !slices.Equal(got, []string{addr("a"), addr("b"), addr("c")}) {
		t.Fatalf("tabs = %v, want b back in the middle", got)
	}
	reopened := state.Pages[1]
	if reopened.ID == ids[1] || reopened.Label != "docs" || state.ActivePageID != reopened.ID {
		t.Fatalf("reopened tab = %+v, active %s; want a new, labelled, selected tab", reopened, state.ActivePageID)
	}
	if state.ClosedTabs != 0 {
		t.Fatalf("ClosedTabs = %d after the reopen", state.ClosedTabs)
	}
	// Nothing left: a no-op.
	if err := m.ReopenClosedTab(t.Context(), access); err != nil {
		t.Fatal(err)
	}
	if got := len(tabIDs(m, access)); got != 3 {
		t.Fatalf("an empty reopen opened a tab: %d tabs", got)
	}
}

// A reopened tab whose label another tab took meanwhile comes back without
// it, so the thread's labels stay unique.
func TestReopenedTabLeavesATakenLabel(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"))
	if _, err := m.LabelPage(t.Context(), access, PageID(ids[0]), "docs"); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LabelPage(t.Context(), access, PageID(ids[1]), "DOCS"); err != nil {
		t.Fatal(err)
	}
	if err := reopenTab(t, m, access); err != nil {
		t.Fatal(err)
	}
	pages := m.CompanionState(access.Caller()).Pages
	if len(pages) != 2 || pages[0].Label != "" || pages[1].Label != "DOCS" {
		t.Fatalf("tabs = %+v, want the reopened tab unlabelled and the other keeping DOCS", pages)
	}
}

// A reopened tab's label reaches the clients even when its address no
// longer opens.
func TestReopenedTabsLabelIsPushedWhenItsAddressFails(t *testing.T) {
	m, access, _ := newTabsManager(t)
	file := filepath.Join(access.Workspace(), "notes.html")
	if err := os.WriteFile(file, []byte("<title>notes</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := openTab(t, m, access, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LabelPage(t.Context(), access, PageID(page.ID), "notes"); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := reopenTab(t, m, access); err == nil {
		t.Fatal("the reopen of a removed file succeeded")
	}
	pushed := newestPush(t, m).Pages
	if len(pushed) != 1 || pushed[0].Label != "notes" {
		t.Fatalf("the newest push shows %+v, want the reopened tab with its label", pushed)
	}
}

// Close others and close to the right close in strip order, so reopening
// one by one restores the strip.
func TestCloseOthersAndToTheRightReopenInOrder(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"), addr("c"), addr("d"))
	if err := m.CloseCompanionTabs(t.Context(), access, PageID(ids[1]), CloseTabsRight); err != nil {
		t.Fatal(err)
	}
	if got := tabURLs(m, access); !slices.Equal(got, []string{addr("a"), addr("b")}) {
		t.Fatalf("after close to the right: %v", got)
	}
	if err := m.CloseCompanionTabs(t.Context(), access, PageID(ids[1]), CloseOtherTabs); err != nil {
		t.Fatal(err)
	}
	if got := tabURLs(m, access); !slices.Equal(got, []string{addr("b")}) {
		t.Fatalf("after close others: %v", got)
	}
	for range 3 {
		if err := reopenTab(t, m, access); err != nil {
			t.Fatal(err)
		}
	}
	if got := tabURLs(m, access); !slices.Equal(got, []string{addr("a"), addr("b"), addr("c"), addr("d")}) {
		t.Fatalf("after reopening all: %v", got)
	}
	if err := m.CloseCompanionTabs(t.Context(), access, PageID(ids[1]), "everything"); err == nil {
		t.Fatal("an unknown set of tabs was accepted")
	}
	// Around a tab gone by its turn it closes nothing.
	if err := m.CloseCompanionTabs(t.Context(), access, "nope", CloseOtherTabs); err != nil {
		t.Fatalf("closing around an unknown tab answered %v", err)
	}
	if got := tabURLs(m, access); len(got) != 4 {
		t.Fatalf("closing around an unknown tab closed tabs: %v", got)
	}
}

func TestDuplicateOpensBesideTheTabAtItsAddress(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"))
	if err := m.DuplicateCompanionPage(t.Context(), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}
	state := m.CompanionState(access.Caller())
	if got := tabURLs(m, access); !slices.Equal(got, []string{addr("a"), addr("a"), addr("b")}) {
		t.Fatalf("tabs = %v, want the copy right after its source", got)
	}
	if state.Pages[1].ID == ids[0] || state.ActivePageID != state.Pages[1].ID {
		t.Fatalf("the copy is not a new, selected tab: %+v", state)
	}
	if err := m.DuplicateCompanionPage(t.Context(), access, "nope"); err == nil {
		t.Fatal("duplicating an unknown tab succeeded")
	}
}

// Only the user's closes of tabs with an address are kept, at most
// maxClosedTabs of them, and never in a private jar.
func TestClosedTabsKeepOnlyWhatCanBeReopened(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), "")
	// An agent's close is not the user's.
	if err := m.ClosePage(t.Context(), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}
	// A blank tab has nothing to reopen.
	if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[1])); err != nil {
		t.Fatal(err)
	}
	// A close of no tab closes and keeps nothing.
	if err := m.CloseCompanionPage(t.Context(), access, "nope"); err != nil {
		t.Fatalf("closing an unknown tab answered %v", err)
	}
	if got := closedCount(t, m, access); got != 0 {
		t.Fatalf("ClosedTabs = %d, want 0", got)
	}

	for i := range maxClosedTabs + 2 {
		page, err := openTab(t, m, access, addr(fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if got := closedCount(t, m, access); got != maxClosedTabs {
		t.Fatalf("ClosedTabs = %d, want the bound %d", got, maxClosedTabs)
	}
	if err := reopenTab(t, m, access); err != nil {
		t.Fatal(err)
	}
	if got := tabURLs(m, access); !slices.Equal(got, []string{addr(fmt.Sprint(maxClosedTabs + 1))}) {
		t.Fatalf("reopened %v, want the most recent", got)
	}

	if err := m.SetJar(context.Background(), access, JarPrivate, JarSwitchConfirmed); err != nil {
		t.Fatal(err)
	}
	if got := closedCount(t, m, access); got != 0 {
		t.Fatalf("ClosedTabs = %d after the jar switch, want 0", got)
	}
	page, err := openTab(t, m, access, addr("private"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
		t.Fatal(err)
	}
	if got := closedCount(t, m, access); got != 0 {
		t.Fatalf("a private tab was kept: ClosedTabs = %d", got)
	}
}

// Only the close that takes a tab out of the tab set records it: the user's
// closes racing on one tab record it once, and a user's close of a tab an
// agent's close took, while that close still runs, records nothing.
func TestOnlyTheCloseThatTakesATabRecordsIt(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"))
	// Holding a page's lock keeps its close's teardown waiting, so the page
	// stays closing until the test ends, before the manager closes.
	hold := func(id string) {
		p := livePage(t, m, access, id)
		p.mu.Lock()
		t.Cleanup(p.mu.Unlock)
	}

	hold(ids[0])
	if err := m.closePage(t.Context(), access, PageID(ids[0]), false, false); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[0])); err != nil {
		t.Fatalf("the user's close of a closing tab: %v", err)
	}
	if got := closedCount(t, m, access); got != 0 {
		t.Fatalf("ClosedTabs = %d, want nothing for the agent's close", got)
	}

	hold(ids[1])
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			<-start
			if err := m.CloseCompanionPage(context.Background(), access, PageID(ids[1])); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := closedCount(t, m, access); got != 1 {
		t.Fatalf("ClosedTabs = %d after 16 simultaneous closes of one tab, want 1", got)
	}
}

// A tab whose close failed after the tab left is still reopenable.
func TestClosedTabRecordFollowsWhatTheCloseDid(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"))
	if err := m.ClosePage(t.Context(), access, PageID(ids[0])); err != nil {
		t.Fatal(err)
	}

	// A suspended tab whose saved copy cannot be updated: its close fails
	// after it left the tab set.
	if err := m.SuspendThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(m.recordDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.recordDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[1])); err == nil {
		t.Fatal("the close's save failure was not reported")
	}
	if _, ok := findTab(m, access, ids[1]); ok {
		t.Fatal("the tab is still there")
	}
	if got := closedCount(t, m, access); got != 1 {
		t.Fatalf("ClosedTabs = %d, want the closed tab reopenable", got)
	}
}

// A reopen whose closed tab a clear, a jar switch or the thread's close
// forgets after the reopen read it opens nothing: the forgotten address does
// not load in the new profile, however far the reopen's tab got by then.
func TestAReopenedTabForgottenAfterItsPopOpensNothing(t *testing.T) {
	forgets := []struct {
		name string
		run  func(t *testing.T, m *Manager, access Access)
	}{
		{"clear site data", func(t *testing.T, m *Manager, _ Access) {
			if err := m.ClearSiteData(context.Background()); err != nil {
				t.Error(err)
			}
		}},
		{"its jar switch", func(t *testing.T, m *Manager, access Access) {
			if err := m.SetJar(context.Background(), access, JarProject, JarSwitchConfirmed); err != nil {
				t.Error(err)
			}
		}},
		{"its close", func(t *testing.T, m *Manager, access Access) {
			if err := m.CloseThread(context.Background(), access.ThreadID()); err != nil {
				t.Error(err)
			}
		}},
		// The forget lands while the reopen's tab waits for its page: a
		// clear forgets in the engine-start hold the creation waits for.
		{"a forget while its page waits", func(t *testing.T, m *Manager, access Access) {
			m.startMu.Lock()
			go func() {
				defer m.startMu.Unlock()
				for {
					m.mu.Lock()
					waiting := len(m.pending) > 0
					if waiting {
						m.forgetBrowsingLocked(access.tb)
					}
					m.mu.Unlock()
					if waiting {
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
		}},
	}
	for _, forget := range forgets {
		t.Run(forget.name, func(t *testing.T) {
			m, access, ids := newTabsManager(t, addr("keep"), addr("x"))
			if err := m.CloseCompanionPage(t.Context(), access, PageID(ids[1])); err != nil {
				t.Fatal(err)
			}
			m.reopenRead = func() { forget.run(t, m, access) }
			// A close of the thread answers the action bound before it.
			var want error
			if forget.name == "its close" {
				want = ErrThreadClosed
			}
			if err := m.ReopenClosedTab(t.Context(), access); !errors.Is(err, want) {
				t.Fatalf("reopen: %v, want %v", err, want)
			}
			for _, tab := range m.CompanionState(access.Caller()).Pages {
				if tab.URL == addr("x") {
					t.Fatalf("the forgotten tab was reopened: %+v", tab)
				}
			}
			m.mu.Lock()
			pending := len(m.pending)
			m.mu.Unlock()
			if pending != 0 {
				t.Fatalf("%d creations are left", pending)
			}
			if got := closedCount(t, m, access); got != 0 {
				t.Fatalf("ClosedTabs = %d", got)
			}
		})
	}
}

// Clearing site data and deleting the thread forget the closed tabs; a
// reopen that cannot open a tab keeps its entry.
func TestClosedTabsAreForgottenWithTheBrowsingData(t *testing.T) {
	closeOne := func(t *testing.T, m *Manager, access Access) {
		t.Helper()
		page, err := openTab(t, m, access, addr("x"))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.CloseCompanionPage(t.Context(), access, PageID(page.ID)); err != nil {
			t.Fatal(err)
		}
		if closedCount(t, m, access) == 0 {
			t.Fatal("the close was not kept")
		}
	}
	t.Run("clear site data", func(t *testing.T) {
		m, access, _ := newTabsManager(t)
		closeOne(t, m, access)
		if err := m.ClearSiteData(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := closedCount(t, m, access); got != 0 {
			t.Fatalf("ClosedTabs = %d", got)
		}
	})
	t.Run("thread delete", func(t *testing.T) {
		m, access, _ := newTabsManager(t)
		closeOne(t, m, access)
		if err := m.CloseThread(t.Context(), access.ThreadID()); err != nil {
			t.Fatal(err)
		}
		if got := closedCount(t, m, access); got != 0 {
			t.Fatalf("ClosedTabs = %d", got)
		}
	})
	t.Run("browser off", func(t *testing.T) {
		m, access, _ := newTabsManager(t)
		closeOne(t, m, access)
		if err := m.Reconfigure(Config{Enabled: false}); err != nil {
			t.Fatal(err)
		}
		if got := closedCount(t, m, access); got != 0 {
			t.Fatalf("ClosedTabs = %d", got)
		}
	})
	// fill opens blank tabs up to the thread's page limit. The closed tab's
	// teardown runs after its close returned and holds its slot until it ends.
	fill := func(t *testing.T, m *Manager, access Access) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for opened := 0; opened < maxPagesPerThread; {
			if _, err := m.NewCompanionPage(context.Background(), access, ""); err == nil {
				opened++
			} else if time.Now().After(deadline) {
				t.Fatal(err)
			}
		}
	}
	t.Run("reopen at the page limit", func(t *testing.T) {
		m, access, _ := newTabsManager(t)
		closeOne(t, m, access)
		fill(t, m, access)
		if err := m.ReopenClosedTab(t.Context(), access); err == nil {
			t.Fatal("a reopen past the page limit succeeded")
		}
		if got := closedCount(t, m, access); got != 1 {
			t.Fatalf("ClosedTabs = %d, want the entry kept", got)
		}
	})
}
