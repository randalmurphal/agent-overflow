package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Closing a thread (delete, archive) forgets its suspended pages and their
// saved copy along with its live pages.
func TestCloseThreadForgetsSuspendedPagesAndTheirSavedCopy(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	suspended := mustOpen(t, manager, access, "https://example.test/suspended")
	if err := manager.SuspendThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, manager, access, "https://example.test/live")

	if err := manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatalf("close thread: %v", err)
	}
	if state := manager.CompanionState(access.Caller()); len(state.Pages) != 0 {
		t.Fatalf("tabs after closing the thread = %#v", state.Pages)
	}
	if _, err := os.Stat(manager.recordPath(access.ThreadID())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the closed thread's saved pages remain: %v", err)
	}
	if _, err := manager.SelectPage(t.Context(), access, PageID(suspended.ID)); !errors.Is(err, ErrThreadClosed) {
		t.Fatalf("touch through an action bound before the close = %v, want ErrThreadClosed", err)
	}
	if _, err := manager.SelectPage(t.Context(), manager.Bind(access.Caller()), PageID(suspended.ID)); !errors.Is(err, errPageNotFound) {
		t.Fatalf("touch of a forgotten page = %v", err)
	}
}

// Pages open at shutdown, live or suspended, come back after a restart as
// suspended pages of their thread, in order, with labels and the selection,
// and restore on their next touch. A thread KeepThread rejects loses its
// saved pages; one it cannot answer for keeps them for the next boot.
func TestSavedPagesComeBackSuspendedAfterARestart(t *testing.T) {
	dir := t.TempDir()
	first, _ := newRestoreManager(t, dir, ManagerOptions{})
	workspace := t.TempDir()
	live := first.Bind(Caller{ThreadID: "live", Workspace: workspace})
	idle := first.Bind(Caller{ThreadID: "idle", Workspace: workspace})
	gone := first.Bind(Caller{ThreadID: "gone", Workspace: workspace})
	unsure := first.Bind(Caller{ThreadID: "unsure", Workspace: workspace})
	a := mustOpen(t, first, live, "https://example.test/a")
	b := mustOpen(t, first, live, "https://example.test/b")
	if _, err := first.LabelPage(t.Context(), live, PageID(a.ID), "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := first.MoveCompanionPage(live, PageID(b.ID), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := first.SelectPage(t.Context(), live, PageID(a.ID)); err != nil {
		t.Fatal(err)
	}
	c := mustOpen(t, first, idle, "https://example.test/c")
	if err := first.SuspendThread(t.Context(), idle.ThreadID()); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, first, gone, "https://example.test/gone")
	mustOpen(t, first, unsure, "https://example.test/unsure")
	if err := first.Close(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	second, engine := newRestoreManager(t, dir, ManagerOptions{KeepThread: func(threadID ThreadID) (bool, error) {
		switch threadID {
		case gone.ThreadID():
			return false, nil
		case unsure.ThreadID():
			return false, errors.New("store unavailable")
		}
		return true, nil
	}})
	state := second.CompanionState(live.Caller())
	if len(state.Pages) != 2 || state.Pages[0].ID != b.ID || state.Pages[1].ID != a.ID {
		t.Fatalf("restarted tabs = %#v, want b then a", state.Pages)
	}
	if !state.Pages[0].Suspended || !state.Pages[1].Suspended || state.Pages[1].Label != "alpha" || state.Pages[1].URL != a.URL {
		t.Fatalf("restarted tabs = %#v", state.Pages)
	}
	if state.ActivePageID != a.ID {
		t.Fatalf("restarted selection = %q, want %q", state.ActivePageID, a.ID)
	}
	if ids := suspendedIDs(second, string(idle.ThreadID())); len(ids) != 1 || ids[0] != c.ID {
		t.Fatalf("a page suspended before shutdown came back as %v", ids)
	}
	if pages := second.CompanionState(gone.Caller()).Pages; len(pages) != 0 {
		t.Fatalf("a rejected thread's pages came back: %#v", pages)
	}
	if _, err := os.Stat(second.recordPath(gone.ThreadID())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected thread's saved pages remain: %v", err)
	}
	if pages := second.CompanionState(unsure.Caller()).Pages; len(pages) != 0 {
		t.Fatalf("pages of a thread that could not be checked were loaded: %#v", pages)
	}
	if _, err := os.Stat(second.recordPath(unsure.ThreadID())); err != nil {
		t.Fatalf("pages of a thread that could not be checked were removed: %v", err)
	}

	live = second.Bind(live.Caller())
	restored, err := second.History(t.Context(), live, PageID(a.ID), "reload")
	if err != nil {
		t.Fatalf("touch after restart: %v", err)
	}
	if restored.URL != a.URL || restored.Label != "alpha" || restored.Suspended {
		t.Fatalf("restored after restart = %#v", restored)
	}
	if got := engine.pagesCreated.Load(); got != 1 {
		t.Fatalf("a restart created %d engine pages before a touch", got)
	}
}

// Clearing site data and turning the browser off forget the suspended pages
// with the live ones, and a boot with the browser off removes saved pages.
func TestClearingSiteDataAndDisablingForgetSuspendedPages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clear func(*Manager) error
	}{
		{"clear site data", func(m *Manager) error { return m.ClearSiteData(context.Background()) }},
		{"disable", func(m *Manager) error { return m.Reconfigure(Config{Enabled: false}) }},
		{"toggle site-data persistence", func(m *Manager) error { return m.Reconfigure(Config{Enabled: true, PersistSiteData: true}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			mustOpen(t, manager, access, "https://example.test/a")
			if err := manager.SuspendThread(t.Context(), access.ThreadID()); err != nil {
				t.Fatal(err)
			}
			if err := tc.clear(manager); err != nil {
				t.Fatal(err)
			}
			if pages := manager.CompanionState(access.Caller()).Pages; len(pages) != 0 {
				t.Fatalf("suspended pages survived: %#v", pages)
			}
			if _, err := os.Stat(manager.recordDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("saved pages survived: %v", err)
			}
		})
	}
	t.Run("boot disabled", func(t *testing.T) {
		dir := t.TempDir()
		first, _ := newRestoreManager(t, dir, ManagerOptions{})
		mustOpen(t, first, first.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()}), "https://example.test/a")
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		disabled := NewManager(dir, Config{}, ManagerOptions{FakeEngine: true})
		defer disabled.Close()
		if _, err := os.Stat(disabled.recordDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a disabled boot kept saved pages: %v", err)
		}
	})
}

// A saved file is bounded: pages past the thread cap are dropped, and a file
// that is oversized, malformed, a leftover temp file, or not its thread's own
// is removed rather than retried at every boot.
func TestSavedPagesAreBoundedAtLoad(t *testing.T) {
	dir := t.TempDir()
	probe := &Manager{recordDir: filepath.Join(dir, browserPageRecordDir)}
	if err := os.MkdirAll(probe.recordDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, file pageRecordFile) {
		data, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	many := pageRecordFile{Version: pageRecordVersion, ThreadID: "many"}
	for i := range maxPagesPerThread + 3 {
		many.Pages = append(many.Pages, pageRecord{ID: fmt.Sprintf("page-%d", i), URL: "https://example.test/", Order: int64(i)})
	}
	write(probe.recordPath("many"), many)
	write(probe.recordPath("impostor"), pageRecordFile{Version: pageRecordVersion, ThreadID: "someone-else", Pages: many.Pages[:1]})
	junk := map[string][]byte{
		"malformed.json":     []byte("{"),
		"oversized.json":     make([]byte, maxPageRecordFileBytes+1),
		"x.json.tmp-1234567": []byte("{}"),
	}
	for name, data := range junk {
		if err := os.WriteFile(filepath.Join(probe.recordDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	manager, _ := newRestoreManager(t, dir, ManagerOptions{})
	if pages := manager.CompanionState(Caller{ThreadID: "many"}).Pages; len(pages) != maxPagesPerThread {
		t.Fatalf("loaded %d pages of a file holding %d", len(pages), len(many.Pages))
	}
	if pages := manager.CompanionState(Caller{ThreadID: "someone-else"}).Pages; len(pages) != 0 {
		t.Fatalf("a file not named for its thread was loaded: %#v", pages)
	}
	entries, err := os.ReadDir(manager.recordDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(manager.recordPath("many")) {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("saved pages left after load = %v", names)
	}
}

// A restore that finishes after Close saved its record leaves the record
// saved: the page comes back suspended after the restart.
func TestARestoreFinishingAfterCloseKeepsThePageSaved(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newRestoreManager(t, dir, ManagerOptions{})
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	page := mustOpen(t, manager, access, "https://example.test/page")
	if err := manager.SuspendThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	release := engine.hold()
	defer release()
	touched := make(chan error, 1)
	go func() {
		_, err := manager.SelectPage(context.Background(), access, PageID(page.ID))
		touched <- err
	}()
	<-engine.navigating
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-touched; err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("a restore finishing after Close = %v", err)
	}
	restarted, _ := newRestoreManager(t, dir, ManagerOptions{})
	if ids := suspendedIDs(restarted, string(access.ThreadID())); len(ids) != 1 || ids[0] != page.ID {
		t.Fatalf("saved pages after the restart = %v", ids)
	}
}

// A suspension caught mid-way by a shutdown leaves its page live for Close
// to save, and one caught by a clear of the saved pages (the browser turned
// off, or its site data cleared) records nothing.
func TestASuspensionCaughtByShutdownOrAClearRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt func(*Manager)
	}{
		{"shutdown", func(m *Manager) {
			m.mu.Lock()
			m.closed = true
			m.mu.Unlock()
		}},
		{"clear", func(m *Manager) {
			m.beginForget()
			if err := m.forgetAllRecords(true); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
			access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
			page := mustOpen(t, manager, access, "https://example.test/page")
			release := engine.holdInfo()
			defer release()
			done := make(chan error, 1)
			go func() { done <- manager.SuspendThread(context.Background(), access.ThreadID()) }()
			<-engine.reading
			tc.interrupt(manager)
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if ids := suspendedIDs(manager, string(access.ThreadID())); len(ids) != 0 {
				t.Fatalf("suspended pages = %v", ids)
			}
			if _, _, err := manager.lookupOwnedPage(access, PageID(page.ID)); err != nil {
				t.Fatalf("the page is no longer live: %v", err)
			}
			if _, err := os.Stat(manager.recordPath(access.ThreadID())); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("saved copy after the interrupted suspension: %v", err)
			}
		})
	}
}

// Deleting or archiving a thread closes its pages even when their saved copy
// cannot be removed; the next boot removes it.
func TestCloseThreadSucceedsWhenItsSavedCopyCannotBeRemoved(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := manager.Bind(Caller{ThreadID: "thread", Workspace: t.TempDir()})
	mustOpen(t, manager, access, "https://example.test/page")
	if err := manager.SuspendThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatal(err)
	}
	path := manager.recordPath(access.ThreadID())
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseThread(t.Context(), access.ThreadID()); err != nil {
		t.Fatalf("close a thread whose saved copy cannot be removed: %v", err)
	}
	if state := manager.CompanionState(access.Caller()); len(state.Pages) != 0 {
		t.Fatalf("tabs after the close = %#v", state.Pages)
	}
}
