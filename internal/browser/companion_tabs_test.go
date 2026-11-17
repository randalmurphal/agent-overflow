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
	tabPushes.Store(m, countStatePushes(m))
	t.Cleanup(func() { tabPushes.Delete(m) })
	access := Access{ThreadID: "thread", Workspace: t.TempDir(), ProjectID: "project"}
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
	read := m.CompanionState(access).ClosedTabs
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
	if _, err := m.LabelPage(t.Context(), access, ids[1], "docs"); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, ids[1]); err != nil {
		t.Fatal(err)
	}
	if got := closedCount(t, m, access); got != 1 {
		t.Fatalf("ClosedTabs = %d, want 1", got)
	}
	if err := reopenTab(t, m, access); err != nil {
		t.Fatal(err)
	}
	state := m.CompanionState(access)
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

// A reopened tab's label reaches the clients even when its address no
// longer opens.
func TestReopenedTabsLabelIsPushedWhenItsAddressFails(t *testing.T) {
	m, access, _ := newTabsManager(t)
	file := filepath.Join(access.Workspace, "notes.html")
	if err := os.WriteFile(file, []byte("<title>notes</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := openTab(t, m, access, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LabelPage(t.Context(), access, page.ID, "notes"); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, page.ID); err != nil {
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
	if err := m.CloseCompanionTabs(t.Context(), access, ids[1], CloseTabsRight); err != nil {
		t.Fatal(err)
	}
	if got := tabURLs(m, access); !slices.Equal(got, []string{addr("a"), addr("b")}) {
		t.Fatalf("after close to the right: %v", got)
	}
	if err := m.CloseCompanionTabs(t.Context(), access, ids[1], CloseOtherTabs); err != nil {
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
	if err := m.CloseCompanionTabs(t.Context(), access, ids[1], "everything"); err == nil {
		t.Fatal("an unknown set of tabs was accepted")
	}
	if err := m.CloseCompanionTabs(t.Context(), access, "nope", CloseOtherTabs); err == nil {
		t.Fatal("closing around an unknown tab succeeded")
	}
}

func TestDuplicateOpensBesideTheTabAtItsAddress(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"))
	if err := m.DuplicateCompanionPage(t.Context(), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	state := m.CompanionState(access)
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
	if err := m.ClosePage(t.Context(), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	// A blank tab has nothing to reopen.
	if err := m.CloseCompanionPage(t.Context(), access, ids[1]); err != nil {
		t.Fatal(err)
	}
	// A close that fails keeps nothing.
	if err := m.CloseCompanionPage(t.Context(), access, "nope"); err == nil {
		t.Fatal("closing an unknown tab succeeded")
	}
	if got := closedCount(t, m, access); got != 0 {
		t.Fatalf("ClosedTabs = %d, want 0", got)
	}

	for i := range maxClosedTabs + 2 {
		page, err := openTab(t, m, access, addr(fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := m.CloseCompanionPage(t.Context(), access, page.ID); err != nil {
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

	if err := m.SetJar(access, JarPrivate); err != nil {
		t.Fatal(err)
	}
	if got := closedCount(t, m, access); got != 0 {
		t.Fatalf("ClosedTabs = %d after the jar switch, want 0", got)
	}
	page, err := openTab(t, m, access, addr("private"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	if got := closedCount(t, m, access); got != 0 {
		t.Fatalf("a private tab was kept: ClosedTabs = %d", got)
	}
}

// A tab another close took first is that close's to keep; one whose close
// failed after the tab left is still reopenable.
func TestClosedTabRecordFollowsWhatTheCloseDid(t *testing.T) {
	m, access, ids := newTabsManager(t, addr("a"), addr("b"))
	m.mu.Lock()
	kept := m.keepClosedLocked(access.ThreadID, ids[0])
	m.mu.Unlock()
	// The agent closes it between the user's record and close.
	if err := m.ClosePage(t.Context(), access, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := m.closeKept(t.Context(), access, ids[0], kept); !errors.Is(err, errPageNotFound) {
		t.Fatalf("close of a gone tab = %v", err)
	}
	if got := closedCount(t, m, access); got != 0 {
		t.Fatalf("ClosedTabs = %d, want the record of a tab the user did not close forgotten", got)
	}

	// A suspended tab whose saved copy cannot be updated: its close fails
	// after it left the tab set.
	if err := m.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(m.recordDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.recordDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseCompanionPage(t.Context(), access, ids[1]); err == nil {
		t.Fatal("the close's save failure was not reported")
	}
	if _, ok := findTab(m, access, ids[1]); ok {
		t.Fatal("the tab is still there")
	}
	if got := closedCount(t, m, access); got != 1 {
		t.Fatalf("ClosedTabs = %d, want the closed tab reopenable", got)
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
		if err := m.CloseCompanionPage(t.Context(), access, page.ID); err != nil {
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
		if err := m.CloseThread(t.Context(), access.ThreadID); err != nil {
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
	t.Run("reopen at the page limit", func(t *testing.T) {
		m, access, _ := newTabsManager(t)
		closeOne(t, m, access)
		// The closed tab's teardown runs after its close returned and holds
		// its slot until it ends.
		deadline := time.Now().Add(5 * time.Second)
		for opened := 0; opened < maxPagesPerThread; {
			if _, err := m.NewCompanionPage(context.Background(), access, ""); err == nil {
				opened++
			} else if time.Now().After(deadline) {
				t.Fatal(err)
			}
		}
		if err := m.ReopenClosedTab(t.Context(), access); err == nil {
			t.Fatal("a reopen past the page limit succeeded")
		}
		if got := closedCount(t, m, access); got != 1 {
			t.Fatalf("ClosedTabs = %d, want the entry kept", got)
		}
	})
}
