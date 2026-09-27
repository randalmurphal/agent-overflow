package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// restoreEngine is the fake engine with the two observation points the
// suspend tests need: how many engine pages were created, and a gate that
// holds a page's navigation until the test releases it.
type restoreEngine struct {
	*fakeEngine
	pagesCreated atomic.Int64
	mu           sync.Mutex
	// gate, when set, holds every navigation until it closes; navigating
	// receives one value per navigation that reached the gate.
	gate       chan struct{}
	navigating chan string
	// infoGate, when set, holds every address read the same way; reading
	// receives one value per read that reached it.
	infoGate chan struct{}
	reading  chan struct{}
}

func newRestoreManager(t *testing.T, dir string, opts ManagerOptions) (*Manager, *restoreEngine) {
	t.Helper()
	opts.FakeEngine = true
	manager := NewManager(dir, Config{Enabled: true}, opts)
	t.Cleanup(func() { _ = manager.Close() })
	engine := &restoreEngine{fakeEngine: newFakeEngine(), navigating: make(chan string, 16), reading: make(chan struct{}, 16)}
	manager.engine = engine
	return manager, engine
}

func (e *restoreEngine) NewProfile(ctx context.Context, opts profileOptions) (engineProfile, error) {
	profile, err := e.fakeEngine.NewProfile(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &restoreProfile{engineProfile: profile, engine: e}, nil
}

// hold makes every later navigation wait for release.
func (e *restoreEngine) hold() (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.gate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// holdInfo makes every later address read wait for release.
func (e *restoreEngine) holdInfo() (release func()) {
	gate := make(chan struct{})
	e.mu.Lock()
	e.infoGate = gate
	e.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

type restoreProfile struct {
	engineProfile
	engine *restoreEngine
}

func (p *restoreProfile) NewPage(ctx context.Context, hooks pageHooks) (pageDriver, error) {
	driver, err := p.engineProfile.NewPage(ctx, hooks)
	if err != nil {
		return nil, err
	}
	p.engine.pagesCreated.Add(1)
	return &gatedPage{pageDriver: driver, engine: p.engine}, nil
}

type gatedPage struct {
	pageDriver
	engine *restoreEngine
}

func (p *gatedPage) Navigate(ctx context.Context, rawURL string) error {
	p.engine.mu.Lock()
	gate := p.engine.gate
	p.engine.mu.Unlock()
	if gate != nil {
		p.engine.navigating <- rawURL
		<-gate
	}
	return p.pageDriver.Navigate(ctx, rawURL)
}

func (p *gatedPage) Info(ctx context.Context) (string, string, error) {
	p.engine.mu.Lock()
	gate := p.engine.infoGate
	p.engine.mu.Unlock()
	if gate != nil {
		p.engine.reading <- struct{}{}
		<-gate
	}
	return p.pageDriver.Info(ctx)
}

func (p *gatedPage) fake() *fakePage { return p.pageDriver.(*fakePage) }

// liveFakePage answers the fake page behind a live page.
func liveFakePage(t *testing.T, m *Manager, access Access, pageID string) *fakePage {
	t.Helper()
	p, _, err := m.lookupOwnedPage(access, pageID)
	if err != nil {
		t.Fatalf("page %s is not live: %v", pageID, err)
	}
	return p.driver.(*gatedPage).fake()
}

func suspendedIDs(m *Manager, threadID string) []string {
	var ids []string
	for _, page := range m.CompanionState(Access{ThreadID: threadID}).Pages {
		if page.Suspended {
			ids = append(ids, page.ID)
		}
	}
	return ids
}

func mustOpen(t *testing.T, m *Manager, access Access, rawURL string) PageInfo {
	t.Helper()
	info, err := m.Open(t.Context(), access, rawURL, OpenOptions{})
	if err != nil {
		t.Fatalf("open %s: %v", rawURL, err)
	}
	return info
}

// The reaper's suspension unloads the engine page and keeps the page in its
// thread's tab set with its id, address, title, label, position and the
// thread's selection. Another thread's pages are untouched, and a profile the
// suspension emptied is disposed: a suspended page holds no engine resource.
func TestSuspendThreadUnloadsPagesAndKeepsThemAsTabs(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	shared, alone := t.TempDir(), t.TempDir()
	idle := Access{ThreadID: "idle", Workspace: shared}
	busy := Access{ThreadID: "busy", Workspace: shared}
	first := mustOpen(t, manager, idle, "https://example.test/one")
	second := mustOpen(t, manager, idle, "https://example.test/two")
	if _, err := manager.LabelPage(t.Context(), idle, second.ID, "docs"); err != nil {
		t.Fatal(err)
	}
	// The shown page is not the most recently used one, so the selection
	// that survives is the thread's own, not a fallback.
	if _, err := manager.Visibility(t.Context(), idle, boolPtr(true), first.ID); err != nil {
		t.Fatal(err)
	}
	other := mustOpen(t, manager, busy, "https://example.test/busy")
	lonely := Access{ThreadID: "lonely", Workspace: alone}
	mustOpen(t, manager, lonely, "https://example.test/lonely")
	unloaded := []*fakePage{liveFakePage(t, manager, idle, first.ID), liveFakePage(t, manager, idle, second.ID)}
	before := manager.CompanionState(idle)

	if err := manager.SuspendThread(t.Context(), idle.ThreadID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := manager.SuspendThread(t.Context(), lonely.ThreadID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	for _, page := range unloaded {
		if page.ctx.Err() == nil {
			t.Fatal("a suspended page's engine page is still open")
		}
	}
	after := manager.CompanionState(idle)
	if len(after.Pages) != 2 {
		t.Fatalf("suspended tabs = %#v", after.Pages)
	}
	for i, page := range after.Pages {
		want := before.Pages[i]
		want.Suspended = true
		want.CanGoBack, want.CanGoForward = false, false
		if page != want {
			t.Fatalf("tab %d = %#v, want %#v", i, page, want)
		}
	}
	if after.ActivePageID != first.ID || after.Visible == nil || !*after.Visible {
		t.Fatalf("selection after suspension = %q visible %v, want %q shown", after.ActivePageID, after.Visible, first.ID)
	}
	if _, _, err := manager.lookupOwnedPage(busy, other.ID); err != nil {
		t.Fatalf("another thread's page was suspended: %v", err)
	}
	if got := registeredScopes(manager); got != 1 {
		t.Fatalf("registered profiles = %d, want only the busy thread's", got)
	}
	listed, err := manager.Pages(t.Context(), idle)
	if err != nil || len(listed) != 2 || !listed[0].Suspended || !listed[1].Suspended {
		t.Fatalf("browser_pages = %#v, %v", listed, err)
	}
	if _, err := os.Stat(manager.recordPath(idle.ThreadID)); err != nil {
		t.Fatalf("suspended pages were not saved: %v", err)
	}
}

func boolPtr(value bool) *bool { return &value }

// Any page-scoped call on a suspended page restores it: a new engine page
// with the same id loads the saved address. Listing is not a touch.
func TestATouchRestoresASuspendedPageAtItsAddress(t *testing.T) {
	manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	workspace := t.TempDir()
	file := filepath.Join(workspace, "report.html")
	if err := os.WriteFile(file, []byte("<p>report</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	access := Access{ThreadID: "thread", Workspace: workspace}
	site := mustOpen(t, manager, access, "https://example.test/site")
	local, err := manager.OpenFile(t.Context(), access, file, OpenOptions{})
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	created := engine.pagesCreated.Load()
	if _, err := manager.Pages(t.Context(), access); err != nil {
		t.Fatal(err)
	}
	if got := engine.pagesCreated.Load(); got != created {
		t.Fatal("listing pages restored a suspended page")
	}

	restored, err := manager.History(t.Context(), access, site.ID, "reload")
	if err != nil {
		t.Fatalf("touch a suspended page: %v", err)
	}
	if restored.ID != site.ID || restored.URL != site.URL || restored.Suspended {
		t.Fatalf("restored page = %#v, want %s live at %s", restored, site.ID, site.URL)
	}
	if page := liveFakePage(t, manager, access, site.ID); page.url != site.URL {
		t.Fatalf("restored engine page is at %q, want %q", page.url, site.URL)
	}
	if _, err := manager.SelectPage(t.Context(), access, local.ID); err != nil {
		t.Fatalf("touch the suspended file page: %v", err)
	}
	if page := liveFakePage(t, manager, access, local.ID); page.url != local.URL {
		t.Fatalf("restored file page is at %q, want %q", page.url, local.URL)
	}
	if got := engine.pagesCreated.Load(); got != created+2 {
		t.Fatalf("engine pages created by two restores = %d", got-created)
	}
	if ids := suspendedIDs(manager, access.ThreadID); len(ids) != 0 {
		t.Fatalf("pages still suspended after their restore: %v", ids)
	}
	if _, err := os.Stat(manager.recordPath(access.ThreadID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the saved copy outlived the last suspended page: %v", err)
	}
}

// A restore passes the navigation policy before it creates an engine page. A
// refused restore leaves the page suspended and tells the caller why, through
// the tool result too; navigating the page elsewhere restores it without
// ever loading the refused address.
func TestARestoreThatFailsTheNavigationPolicyStaysSuspended(t *testing.T) {
	manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	workspace := t.TempDir()
	file := filepath.Join(workspace, "report.html")
	if err := os.WriteFile(file, []byte("<p>report</p>"), 0o600); err != nil {
		t.Fatal(err)
	}
	access := Access{ThreadID: "thread", Workspace: workspace}
	local, err := manager.OpenFile(t.Context(), access, file, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	created := engine.pagesCreated.Load()

	_, err = manager.History(t.Context(), access, local.ID, "reload")
	if err == nil || !strings.Contains(err.Error(), "cannot be restored") {
		t.Fatalf("restore of a page the policy refuses = %v", err)
	}
	server := NewMCPServer(manager, true)
	t.Cleanup(func() { _ = server.Close() })
	config, err := server.RegisterThread(access, "session")
	if err != nil {
		t.Fatal(err)
	}
	response := postRPC(t, config[ServerName].(map[string]any)["url"].(string), map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "browser_snapshot", "arguments": map[string]any{"page_id": local.ID}},
	})
	if encoded, _ := json.Marshal(response); !strings.Contains(string(encoded), "cannot be restored") {
		t.Fatalf("tool result for a refused restore = %s", encoded)
	}
	if got := engine.pagesCreated.Load(); got != created {
		t.Fatalf("a refused restore created %d engine pages", got-created)
	}
	if ids := suspendedIDs(manager, access.ThreadID); len(ids) != 1 || ids[0] != local.ID {
		t.Fatalf("suspended pages after a refused restore = %v", ids)
	}
	// The companion still opens on it, where its pane shows the refusal and
	// the page can be navigated elsewhere or closed.
	if info, err := manager.Visibility(t.Context(), access, boolPtr(true), local.ID); err != nil || !info.Visible {
		t.Fatalf("show a page whose restore is refused = %+v, %v", info, err)
	}
	if err := manager.ActivateCompanionPage(t.Context(), access, local.ID); err == nil || !strings.Contains(err.Error(), "cannot be restored") {
		t.Fatalf("select a page whose restore is refused = %v", err)
	}

	moved, err := manager.Open(t.Context(), access, "https://example.test/elsewhere", OpenOptions{PageID: local.ID})
	if err != nil {
		t.Fatalf("navigate a suspended page elsewhere: %v", err)
	}
	if moved.ID != local.ID || moved.URL != "https://example.test/elsewhere" {
		t.Fatalf("navigated page = %#v", moved)
	}
	if history := liveFakePage(t, manager, access, local.ID).history; len(history) != 1 {
		t.Fatalf("navigating a suspended page loaded %v first", history)
	}
}

// Concurrent touches of one suspended page wait for one restore. A caller
// that gives up does not fail the restore for the rest.
func TestConcurrentTouchesRestoreASuspendedPageOnce(t *testing.T) {
	manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/page")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	created := engine.pagesCreated.Load()
	release := engine.hold()
	defer release()

	const touches = 6
	results := make(chan error, touches)
	for range touches {
		go func() {
			info, err := manager.SelectPage(context.Background(), access, page.ID)
			if err == nil && (info.ID != page.ID || info.Suspended) {
				err = fmt.Errorf("touch answered %#v", info)
			}
			results <- err
		}()
	}
	<-engine.navigating
	impatient, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := manager.SelectPage(impatient, access, page.ID)
		gaveUp <- err
	}()
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("a touch that gave up = %v", err)
	}
	release()
	for range touches {
		if err := <-results; err != nil {
			t.Fatalf("concurrent touch: %v", err)
		}
	}
	if got := engine.pagesCreated.Load(); got != created+1 {
		t.Fatalf("concurrent touches created %d engine pages, want 1", got-created)
	}
	if state := manager.CompanionState(access); len(state.Pages) != 1 || state.Pages[0].Suspended {
		t.Fatalf("tabs after concurrent touches = %#v", state.Pages)
	}
}

// A suspended page closed while its restore loads stays closed: the restore
// abandons its engine page instead of registering it.
func TestClosingASuspendedPageDuringItsRestoreAbandonsTheRestore(t *testing.T) {
	manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/page")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	release := engine.hold()
	defer release()
	touched := make(chan error, 1)
	go func() {
		_, err := manager.SelectPage(context.Background(), access, page.ID)
		touched <- err
	}()
	<-engine.navigating
	if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
		t.Fatalf("close a suspended page: %v", err)
	}
	release()
	if err := <-touched; !errors.Is(err, errPageNotFound) {
		t.Fatalf("touch of a page closed during its restore = %v", err)
	}
	if state := manager.CompanionState(access); len(state.Pages) != 0 {
		t.Fatalf("tabs after the close = %#v", state.Pages)
	}
	if got := registeredScopes(manager); got != 0 {
		t.Fatalf("the abandoned restore left %d profiles", got)
	}
}

// A close that found a page live and then waited while the page was
// suspended reports it gone, so ClosePage forgets the record instead of
// leaving it.
func TestAClosePageRacingASuspensionClosesTheRecord(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/page")
	p, scope, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if err := manager.closeFoundPage(t.Context(), access, p, scope); !errors.Is(err, errPageNotFound) {
		t.Fatalf("close of a page suspended after its lookup = %v", err)
	}
	if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	if state := manager.CompanionState(access); len(state.Pages) != 0 {
		t.Fatalf("tabs after the close = %#v", state.Pages)
	}
}

// A suspension that reaches a page closed after the reaper listed it leaves
// it closed.
func TestSuspendingAClosedPageLeavesItClosed(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/page")
	p, _, err := manager.lookupOwnedPage(access, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ClosePage(t.Context(), access, page.ID); err != nil {
		t.Fatal(err)
	}
	if moved, err := manager.suspendPage(t.Context(), p); moved || err != nil {
		t.Fatalf("suspendPage of a closed page = %v, %v", moved, err)
	}
	if state := manager.CompanionState(access); len(state.Pages) != 0 {
		t.Fatalf("a closed page came back suspended: %#v", state.Pages)
	}
}

// Closing a thread (delete, archive) forgets its suspended pages and their
// saved copy along with its live pages.
func TestCloseThreadForgetsSuspendedPagesAndTheirSavedCopy(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	suspended := mustOpen(t, manager, access, "https://example.test/suspended")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, manager, access, "https://example.test/live")

	if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
		t.Fatalf("close thread: %v", err)
	}
	if state := manager.CompanionState(access); len(state.Pages) != 0 {
		t.Fatalf("tabs after closing the thread = %#v", state.Pages)
	}
	if _, err := os.Stat(manager.recordPath(access.ThreadID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the closed thread's saved pages remain: %v", err)
	}
	if _, err := manager.SelectPage(t.Context(), access, suspended.ID); !errors.Is(err, errPageNotFound) {
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
	live := Access{ThreadID: "live", Workspace: workspace}
	idle := Access{ThreadID: "idle", Workspace: workspace}
	gone := Access{ThreadID: "gone", Workspace: workspace}
	unsure := Access{ThreadID: "unsure", Workspace: workspace}
	a := mustOpen(t, first, live, "https://example.test/a")
	b := mustOpen(t, first, live, "https://example.test/b")
	if _, err := first.LabelPage(t.Context(), live, a.ID, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := first.MoveCompanionPage(live, b.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := first.SelectPage(t.Context(), live, a.ID); err != nil {
		t.Fatal(err)
	}
	c := mustOpen(t, first, idle, "https://example.test/c")
	if err := first.SuspendThread(t.Context(), idle.ThreadID); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, first, gone, "https://example.test/gone")
	mustOpen(t, first, unsure, "https://example.test/unsure")
	if err := first.Close(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	second, engine := newRestoreManager(t, dir, ManagerOptions{KeepThread: func(threadID string) (bool, error) {
		switch threadID {
		case gone.ThreadID:
			return false, nil
		case unsure.ThreadID:
			return false, errors.New("store unavailable")
		}
		return true, nil
	}})
	state := second.CompanionState(live)
	if len(state.Pages) != 2 || state.Pages[0].ID != b.ID || state.Pages[1].ID != a.ID {
		t.Fatalf("restarted tabs = %#v, want b then a", state.Pages)
	}
	if !state.Pages[0].Suspended || !state.Pages[1].Suspended || state.Pages[1].Label != "alpha" || state.Pages[1].URL != a.URL {
		t.Fatalf("restarted tabs = %#v", state.Pages)
	}
	if state.ActivePageID != a.ID {
		t.Fatalf("restarted selection = %q, want %q", state.ActivePageID, a.ID)
	}
	if ids := suspendedIDs(second, idle.ThreadID); len(ids) != 1 || ids[0] != c.ID {
		t.Fatalf("a page suspended before shutdown came back as %v", ids)
	}
	if pages := second.CompanionState(gone).Pages; len(pages) != 0 {
		t.Fatalf("a rejected thread's pages came back: %#v", pages)
	}
	if _, err := os.Stat(second.recordPath(gone.ThreadID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected thread's saved pages remain: %v", err)
	}
	if pages := second.CompanionState(unsure).Pages; len(pages) != 0 {
		t.Fatalf("pages of a thread that could not be checked were loaded: %#v", pages)
	}
	if _, err := os.Stat(second.recordPath(unsure.ThreadID)); err != nil {
		t.Fatalf("pages of a thread that could not be checked were removed: %v", err)
	}

	restored, err := second.History(t.Context(), live, a.ID, "reload")
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

// Omitting page_id counts suspended pages: the only page is restored, and
// with several the ambiguity error names the suspended ones too.
func TestImplicitPageResolutionCountsSuspendedPages(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	only := mustOpen(t, manager, access, "https://example.test/only")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	info, err := manager.History(t.Context(), access, "", "reload")
	if err != nil || info.ID != only.ID || info.Suspended {
		t.Fatalf("implicit touch of the only page = %#v, %v", info, err)
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, manager, access, "https://example.test/live")
	_, err = manager.History(t.Context(), access, "", "reload")
	if err == nil || !strings.Contains(err.Error(), only.ID) {
		t.Fatalf("implicit touch with a suspended and a live page = %v", err)
	}
}

// A suspended page keeps its slot in its thread's page cap, which bounds the
// pages a thread saves; it holds none of the workspace or process caps,
// which bound engine pages. A restore needs an engine slot like any page.
func TestSuspendedPagesHoldTheirThreadsSlotsButNoEngineSlot(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	workspace := t.TempDir()
	threads := make([]Access, 4)
	for i := range threads {
		threads[i] = Access{ThreadID: fmt.Sprintf("thread-%d", i), Workspace: workspace}
	}
	var first PageInfo
	for i := range threads[:3] {
		for j := range maxPagesPerThread {
			info := mustOpen(t, manager, threads[i], fmt.Sprintf("https://example.test/%d/%d", i, j))
			if i == 0 && j == 0 {
				first = info
			}
		}
	}
	if err := manager.SuspendThread(t.Context(), threads[0].ThreadID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Open(t.Context(), threads[0], "https://example.test/ninth", OpenOptions{}); err == nil || !strings.Contains(err.Error(), "page limit reached for thread") {
		t.Fatalf("a ninth page beside eight suspended ones = %v", err)
	}
	for j := range maxPagesPerThread {
		mustOpen(t, manager, threads[3], fmt.Sprintf("https://example.test/3/%d", j))
	}
	_, err := manager.SelectPage(t.Context(), threads[0], first.ID)
	if err == nil || !strings.Contains(err.Error(), "page limit reached for workspace") {
		t.Fatalf("restore into a full workspace = %v", err)
	}
	if ids := suspendedIDs(manager, threads[0].ThreadID); len(ids) != maxPagesPerThread {
		t.Fatalf("suspended pages after a refused restore = %d", len(ids))
	}
	if err := manager.ClosePage(t.Context(), threads[3], manager.CompanionState(threads[3]).Pages[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SelectPage(t.Context(), threads[0], first.ID); err != nil {
		t.Fatalf("restore once the workspace has room: %v", err)
	}
}

// A page being restored holds its engine slot while it loads, so pages
// opened meanwhile cannot take the workspace past its cap.
func TestARestoreInFlightHoldsItsEngineSlot(t *testing.T) {
	manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	workspace := t.TempDir()
	restoring := Access{ThreadID: "restoring", Workspace: workspace}
	page := mustOpen(t, manager, restoring, "https://example.test/restoring")
	if err := manager.SuspendThread(t.Context(), restoring.ThreadID); err != nil {
		t.Fatal(err)
	}
	for i := range maxPagesPerWorkspace - 1 {
		filler := Access{ThreadID: fmt.Sprintf("filler-%d", i/maxPagesPerThread), Workspace: workspace}
		mustOpen(t, manager, filler, fmt.Sprintf("https://example.test/filler/%d", i))
	}
	release := engine.hold()
	defer release()
	touched := make(chan error, 1)
	go func() {
		_, err := manager.SelectPage(context.Background(), restoring, page.ID)
		touched <- err
	}()
	<-engine.navigating
	late := Access{ThreadID: "late", Workspace: workspace}
	if _, err := manager.NewPage(t.Context(), late); err == nil || !strings.Contains(err.Error(), "page limit reached for workspace") {
		t.Fatalf("a page opened while a restore holds the last slot = %v", err)
	}
	release()
	if err := <-touched; err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// Labels stay unique across a thread's live and suspended pages, and moving
// a suspended tab is saved with it.
func TestLabelsAndTabMovesIncludeSuspendedPages(t *testing.T) {
	dir := t.TempDir()
	manager, _ := newRestoreManager(t, dir, ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	a := mustOpen(t, manager, access, "https://example.test/a")
	b := mustOpen(t, manager, access, "https://example.test/b")
	if _, err := manager.LabelPage(t.Context(), access, a.ID, "docs"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	live := mustOpen(t, manager, access, "https://example.test/live")
	if _, err := manager.LabelPage(t.Context(), access, live.ID, "DOCS"); err == nil {
		t.Fatal("a live page took a suspended page's label")
	}
	if err := manager.MoveCompanionPage(access, b.ID, 0); err != nil {
		t.Fatalf("move a suspended tab: %v", err)
	}
	if ids := suspendedIDs(manager, access.ThreadID); len(ids) != 2 || ids[0] != b.ID {
		t.Fatalf("tab order after moving a suspended tab = %v", ids)
	}
	var saved pageRecordFile
	data, err := os.ReadFile(manager.recordPath(access.ThreadID))
	if err == nil {
		err = json.Unmarshal(data, &saved)
	}
	if err != nil || len(saved.Pages) != 2 || saved.Pages[0].ID != b.ID {
		t.Fatalf("saved order after the move = %#v, %v", saved.Pages, err)
	}
}

// Presenting a suspended page restores it: the agent's browser_visibility
// and the user's tab click.
func TestPresentingASuspendedPageRestoresIt(t *testing.T) {
	manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	a := mustOpen(t, manager, access, "https://example.test/a")
	b := mustOpen(t, manager, access, "https://example.test/b")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	// Showing the companion opens it on the page; the pane that presents it
	// restores it by selecting it.
	created := engine.pagesCreated.Load()
	info, err := manager.Visibility(t.Context(), access, boolPtr(true), a.ID)
	if err != nil || !info.Visible || info.ActivePageID != a.ID {
		t.Fatalf("show a suspended page = %+v, %v", info, err)
	}
	if got := engine.pagesCreated.Load(); got != created {
		t.Fatalf("showing the companion loaded %d pages", got-created)
	}
	for _, id := range []string{a.ID, b.ID} {
		if err := manager.ActivateCompanionPage(t.Context(), access, id); err != nil {
			t.Fatalf("select suspended tab %s: %v", id, err)
		}
	}
	if ids := suspendedIDs(manager, access.ThreadID); len(ids) != 0 {
		t.Fatalf("pages still suspended after being presented: %v", ids)
	}
	if _, err := manager.Visibility(t.Context(), access, boolPtr(true), "missing"); !errors.Is(err, errPageNotFound) {
		t.Fatalf("show a page the thread does not have = %v", err)
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
			access := Access{ThreadID: "thread", Workspace: t.TempDir()}
			mustOpen(t, manager, access, "https://example.test/a")
			if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
				t.Fatal(err)
			}
			if err := tc.clear(manager); err != nil {
				t.Fatal(err)
			}
			if pages := manager.CompanionState(access).Pages; len(pages) != 0 {
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
		mustOpen(t, first, Access{ThreadID: "thread", Workspace: t.TempDir()}, "https://example.test/a")
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
	if pages := manager.CompanionState(Access{ThreadID: "many"}).Pages; len(pages) != maxPagesPerThread {
		t.Fatalf("loaded %d pages of a file holding %d", len(pages), len(many.Pages))
	}
	if pages := manager.CompanionState(Access{ThreadID: "someone-else"}).Pages; len(pages) != 0 {
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

// The reaper leaves live the page a mounted pane is showing: unloading it
// would only make the pane reload it. The thread's other pages are
// suspended, and a pane that cannot show a page protects nothing.
func TestTheReaperKeepsThePageAPaneShows(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	shown := mustOpen(t, manager, access, "https://example.test/shown")
	other := mustOpen(t, manager, access, "https://example.test/other")
	if _, err := manager.Visibility(t.Context(), access, boolPtr(true), shown.ID); err != nil {
		t.Fatal(err)
	}
	mount, err := manager.AttachPane(access)
	if err != nil {
		t.Fatal(err)
	}
	rect := PaneRect{X: 10, Y: 20, Width: 800, Height: 600, ViewportWidth: 1920, ViewportHeight: 1080, Visible: true}
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatal(err)
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if ids := suspendedIDs(manager, access.ThreadID); len(ids) != 1 || ids[0] != other.ID {
		t.Fatalf("suspended with the pane showing %s = %v", shown.ID, ids)
	}

	rect.Visible = false
	if err := manager.SetPaneRect(mount.ID, rect); err != nil {
		t.Fatal(err)
	}
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if ids := suspendedIDs(manager, access.ThreadID); len(ids) != 2 {
		t.Fatalf("suspended with the pane hidden = %v", ids)
	}
}

// A reap of a thread with no page to suspend leaves its browser state alone:
// no session entry, no event, and no write of a saved copy it never loaded.
func TestSuspendingAThreadWithNoPagesTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	writer, _ := newRestoreManager(t, dir, ManagerOptions{})
	access := Access{ThreadID: "kept", Workspace: t.TempDir()}
	mustOpen(t, writer, access, "https://example.test/kept")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// A boot that could not decide whether the thread keeps its pages
	// leaves the saved copy on disk without loading it.
	manager, _ := newRestoreManager(t, dir, ManagerOptions{KeepThread: func(string) (bool, error) {
		return false, errors.New("store busy")
	}})
	var events atomic.Int64
	manager.SetEventSink(func(CompanionEvent) { events.Add(1) })
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.recordPath(access.ThreadID)); err != nil {
		t.Fatalf("the saved copy of a thread with no loaded pages: %v", err)
	}
	manager.mu.Lock()
	_, hasSession := manager.sessions[access.ThreadID]
	manager.mu.Unlock()
	if hasSession || events.Load() != 0 {
		t.Fatalf("session entry %v, %d events for a thread with no pages", hasSession, events.Load())
	}
}

// A restore that finishes after Close saved its record leaves the record
// saved: the page comes back suspended after the restart.
func TestARestoreFinishingAfterCloseKeepsThePageSaved(t *testing.T) {
	dir := t.TempDir()
	manager, engine := newRestoreManager(t, dir, ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	page := mustOpen(t, manager, access, "https://example.test/page")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	release := engine.hold()
	defer release()
	touched := make(chan error, 1)
	go func() {
		_, err := manager.SelectPage(context.Background(), access, page.ID)
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
	if ids := suspendedIDs(restarted, access.ThreadID); len(ids) != 1 || ids[0] != page.ID {
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
			if err := m.forgetAllRecords(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, engine := newRestoreManager(t, t.TempDir(), ManagerOptions{})
			access := Access{ThreadID: "thread", Workspace: t.TempDir()}
			page := mustOpen(t, manager, access, "https://example.test/page")
			release := engine.holdInfo()
			defer release()
			done := make(chan error, 1)
			go func() { done <- manager.SuspendThread(context.Background(), access.ThreadID) }()
			<-engine.reading
			tc.interrupt(manager)
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if ids := suspendedIDs(manager, access.ThreadID); len(ids) != 0 {
				t.Fatalf("suspended pages = %v", ids)
			}
			if _, _, err := manager.lookupOwnedPage(access, page.ID); err != nil {
				t.Fatalf("the page is no longer live: %v", err)
			}
			if _, err := os.Stat(manager.recordPath(access.ThreadID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("saved copy after the interrupted suspension: %v", err)
			}
		})
	}
}

// Deleting or archiving a thread closes its pages even when their saved copy
// cannot be removed; the next boot removes it.
func TestCloseThreadSucceedsWhenItsSavedCopyCannotBeRemoved(t *testing.T) {
	manager, _ := newRestoreManager(t, t.TempDir(), ManagerOptions{})
	access := Access{ThreadID: "thread", Workspace: t.TempDir()}
	mustOpen(t, manager, access, "https://example.test/page")
	if err := manager.SuspendThread(t.Context(), access.ThreadID); err != nil {
		t.Fatal(err)
	}
	path := manager.recordPath(access.ThreadID)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseThread(t.Context(), access.ThreadID); err != nil {
		t.Fatalf("close a thread whose saved copy cannot be removed: %v", err)
	}
	if state := manager.CompanionState(access); len(state.Pages) != 0 {
		t.Fatalf("tabs after the close = %#v", state.Pages)
	}
}
