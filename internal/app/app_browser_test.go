package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
)

func TestBrowserSettingsDefaultToEnabledPersistent(t *testing.T) {
	config := browserConfigFromSettings(settings.DefaultSettings)
	if !config.Enabled || !config.PersistSiteData || config.AllowOutsideWorkspace {
		t.Fatalf("browser config = %+v", config)
	}
}

// The fake-engine pin crosses the bootstrap boundary as one bool, and
// startup hands it straight to ManagerOptions.FakeEngine — which wins
// engine selection ahead of every other fact. A boot that asks for the
// pin must get it, and one that lifted it (the manual real-engine gate,
// docs/specs/embedded-browser.md §10) must not have it reinstated here.
func TestConfigureIsolationCarriesTheBrowserEnginePin(t *testing.T) {
	pinned := &App{}
	ConfigureIsolation(pinned, IsolationConfig{MockBrowserEngine: true})
	if !pinned.browser.mockEngine {
		t.Fatal("MockBrowserEngine: true did not pin the fake engine")
	}
	lifted := &App{}
	ConfigureIsolation(lifted, IsolationConfig{MockBrowserEngine: false})
	if lifted.browser.mockEngine {
		t.Fatal("MockBrowserEngine: false still pinned the fake engine")
	}
}

// No window getter is the whole windowless story: selection reads only
// whether one EXISTS, so an App that was never handed one has no
// in-process engine at all. The isolated boots rely on the other half of
// that rule — a getter installed before Start whose pointer arrives
// later — so the presence, not the answer, is what must be recorded here.
func TestSetBrowserNativeWindowRecordsThePresenceOfAGetter(t *testing.T) {
	app := &App{}
	if app.browser.nativeWindow != nil {
		t.Fatal("a bare App already carries a window getter")
	}
	app2 := &App{}
	SetBrowserNativeWindow(app2, func() unsafe.Pointer { return nil })
	if app2.browser.nativeWindow == nil {
		t.Fatal("SetBrowserNativeWindow did not record the getter")
	}
	if app2.browser.nativeWindow() != nil {
		t.Fatal("a getter answering nil should still answer nil")
	}
}

func TestBrowserMCPConfigRegistersOnlyHeadlessProviders(t *testing.T) {
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	server := appbrowser.NewMCPServer(manager, true)
	app := &App{}
	app.browser.manager, app.browser.mcp = manager, server
	t.Cleanup(func() { _ = server.Close(); _ = manager.Close() })

	thread := store.Thread{ID: "t", Provider: string(provider.Claude), WorkspacePath: t.TempDir(), ProjectPath: t.TempDir()}
	servers, err := app.browserMCPConfigForThread(thread, "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[appbrowser.ServerName] == nil {
		t.Fatalf("servers = %#v", servers)
	}
	thread.Provider = string(provider.ClaudeTUI)
	servers, err = app.browserMCPConfigForThread(thread, "session")
	if err != nil || len(servers) != 0 {
		t.Fatalf("TUI servers = %#v, %v", servers, err)
	}
}

func TestPatchTouchesBrowserSettings(t *testing.T) {
	if !patchTouchesBrowserSettings(map[string]any{"browserEnabled": false}) {
		t.Fatal("browser setting not detected")
	}
	if patchTouchesBrowserSettings(map[string]any{"streamingEnabled": false}) {
		t.Fatal("unrelated setting detected")
	}
}

func TestRefreshLiveBrowserMCPUsesClaudeToggle(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-claude", claude.Config{
		Binary:  writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put("browser-claude", session{Provider: string(provider.Claude), Token: "browser-toggle", Claude: sess})
	app.refreshLiveBrowserMCP(false)
	envelope := readClaudeMcpToggleCapture(t, captureDir, 3*time.Second)
	if envelope.Request["serverName"] != appbrowser.ServerName || envelope.Request["enabled"] != false {
		t.Fatalf("browser toggle = %#v", envelope.Request)
	}
}

func TestRefreshLiveBrowserMCPPreservesThreadDisable(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	app.browser.mcp = appbrowser.NewMCPServer(nil, true)
	app.browser.mcp.SetThreadEnabled("browser-claude-disabled", false)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-claude-disabled", claude.Config{
		Binary:  writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put("browser-claude-disabled", session{Provider: string(provider.Claude), Token: "browser-toggle-disabled", Claude: sess})

	app.refreshLiveBrowserMCP(true)
	envelope := readClaudeMcpToggleCapture(t, captureDir, 3*time.Second)
	if envelope.Request["enabled"] != false {
		t.Fatalf("thread-disabled browser toggle = %#v", envelope.Request)
	}
}

func TestBrowserSettingsCoalescingKeepsSkippedEnableTransition(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := claude.NewSession(context.Background(), "browser-coalesced", claude.Config{
		Binary:  writeClaudeMcpToggleCaptureBinary(t, captureDir),
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put("browser-coalesced", session{Provider: string(provider.Claude), Token: "browser-coalesced", Claude: sess})
	app.browser.liveEnabled.Store(true)

	// Hold the worker lock so both updates are queued before either can apply.
	// The second update supersedes the first but does not itself change enabled;
	// it must still deliver the true -> false transition to the provider.
	app.browser.applyMu.Lock()
	app.scheduleBrowserSettings(settings.Settings{BrowserEnabled: false})
	app.scheduleBrowserSettings(settings.Settings{BrowserEnabled: false, BrowserPersistSiteData: true})
	app.browser.applyMu.Unlock()
	app.browser.applyWG.Wait()

	envelope := readClaudeMcpToggleCapture(t, captureDir, 3*time.Second)
	if envelope.Request["serverName"] != appbrowser.ServerName || envelope.Request["enabled"] != false {
		t.Fatalf("coalesced browser toggle = %#v", envelope.Request)
	}
}

func TestRefreshLiveBrowserMCPUsesCodexReload(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	captureDir := t.TempDir()
	sess, err := codex.NewSession(context.Background(), "browser-codex", codex.Config{
		Binary:  writeCodexRefreshCaptureBinary(t, captureDir, "browser-codex-provider", ""),
		Model:   "gpt-5",
		WorkDir: t.TempDir(),
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessionManager().put("browser-codex", session{Provider: string(provider.Codex), Token: "browser-reload", Codex: sess})
	app.refreshLiveBrowserMCP(false)
	if method := readCodexReloadCapture(t, captureDir, 3*time.Second); method != "config/mcpServer/reload" {
		t.Fatalf("reload method = %q", method)
	}
}

func TestBrowserMCPRowSeparatesPreferenceFromSettings(t *testing.T) {
	app, _, _ := newMCPTestApp(t)
	app.browser.mcp = appbrowser.NewMCPServer(nil, true)
	t.Cleanup(func() {
		if err := app.browser.mcp.Close(); err != nil {
			t.Error(err)
		}
	})
	thread := store.Thread{ID: "browser-state", Provider: string(provider.Codex)}
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"browserEnabled": false}); err != nil {
		t.Fatal(err)
	}
	for _, rows := range [][]ThreadMCPServer{nil, {{Name: appbrowser.ServerName, Status: "connected", Tools: []string{"read"}}}} {
		row := findServer(app.withBrowserMCPRow(thread, rows, true), appbrowser.ServerName)
		if row.Disabled || row.Status != "disabled" || row.ToggleDisabledReason == "" || len(row.Tools) != 0 {
			t.Fatalf("blocked row = %#v", row)
		}
	}
	if err := app.setBrowserThreadMCPEnabled(thread, true); err == nil {
		t.Fatal("enabled browser while settings blocked it")
	}
	app.browser.mcp.SetThreadEnabled(thread.ID, false)
	if _, err := app.UpdateSettings(context.Background(), map[string]any{"browserEnabled": true}); err != nil {
		t.Fatal(err)
	}
	row := findServer(app.withBrowserMCPRow(thread, nil, false), appbrowser.ServerName)
	if !row.Disabled || row.ToggleDisabledReason != "" {
		t.Fatalf("saved off preference was not preserved: %#v", row)
	}
	app.browser.mcp.SetThreadEnabled(thread.ID, true)
	row = findServer(app.withBrowserMCPRow(thread, []ThreadMCPServer{{Name: appbrowser.ServerName, Status: "connected", Disabled: true, ToggleDisabledReason: "external"}}, true), appbrowser.ServerName)
	if row.Disabled || row.ToggleDisabledReason != "" {
		t.Fatalf("managed owner did not restore control: %#v", row)
	}
}

// TestBrowserCompanionPaneDetachUnbindsItsConnectionTie: see checkByIDTie.
func TestBrowserCompanionPaneDetachUnbindsItsConnectionTie(t *testing.T) {
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-tie")
	access, err := app.browserAccess(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.NewPage(t.Context(), access); err != nil {
		t.Fatalf("open page: %v", err)
	}
	checkByIDTie(t, byIDTie{
		subscribe: func(ctx context.Context) (string, error) {
			pane, err := app.BrowserCompanionPaneAttach(ctx, thread.ID)
			return pane.ID, err
		},
		unsubscribe: app.BrowserCompanionPaneDetach,
		key:         companionPaneCleanupKey,
		live:        func(id string) bool { return manager.SetPaneRect(id, appbrowser.PaneRect{}) == nil },
	})
}

func TestDeleteThreadReleasesItsBrowserPagesAndToolToggles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete func(*App, string) error
	}{
		{"delete thread", (*App).DeleteThread},
		{"delete empty draft", func(app *App, threadID string) error {
			deleted, err := app.DeleteEmptyDraftThread(threadID)
			if err == nil && !deleted {
				err = errors.New("draft was not deleted")
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestAppWithStore(t)
			manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
			app.browser.manager = manager
			app.browser.mcp = appbrowser.NewMCPServer(manager, true)
			t.Cleanup(func() { _ = manager.Close() })
			thread := makeWorkspaceThread(t, app, "thread-delete-browser")
			access, err := app.browserAccess(thread.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.NewPage(t.Context(), access); err != nil {
				t.Fatalf("open page: %v", err)
			}
			app.applyDraftMCPPreferences(thread.ID, draftMCPPreferences{})

			if err := tc.delete(app, thread.ID); err != nil {
				t.Fatalf("delete: %v", err)
			}
			pages, err := manager.Pages(t.Context(), access)
			if err != nil {
				t.Fatalf("pages: %v", err)
			}
			if len(pages) != 0 {
				t.Fatalf("deleted thread still owns %d browser pages", len(pages))
			}
			if got := app.draftMCPPreferences(thread.ID); got != (draftMCPPreferences{Threads: true, Remote: true, Browser: true}) {
				t.Fatalf("deleted thread's tool toggles = %+v, want forgotten", got)
			}
		})
	}
}

// holdingController parks NewPage until released, as a browser tools call
// running when its thread is deleted.
type holdingController struct {
	*appbrowser.Manager
	entered, release chan struct{}
}

func (c holdingController) NewPage(ctx context.Context, access appbrowser.Access) (appbrowser.PageInfo, error) {
	close(c.entered)
	<-c.release
	return c.Manager.NewPage(ctx, access)
}

// A delete waits out a browser tools call already running for the thread,
// then closes the page that call opened.
func TestDeleteThreadWaitsForARunningBrowserToolCall(t *testing.T) {
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	held := holdingController{Manager: manager, entered: make(chan struct{}), release: make(chan struct{})}
	app.browser.manager, app.browser.mcp = manager, appbrowser.NewMCPServer(held, true)
	t.Cleanup(func() { _ = app.browser.mcp.Close(); _ = manager.Close() })
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	t.Cleanup(release)
	thread := makeWorkspaceThread(t, app, "thread-delete-running-call")
	config, err := app.browserMCPConfigForThread(thread, "session")
	if err != nil {
		t.Fatal(err)
	}
	url := config[appbrowser.ServerName].(map[string]any)["url"].(string)
	call := make(chan error, 1)
	go func() {
		resp, err := http.Post(url, "application/json", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"browser_new_page","arguments":{}}}`))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = errors.New(resp.Status)
			}
		}
		call <- err
	}()
	<-held.entered
	deleted := make(chan error, 1)
	go func() { deleted <- app.DeleteThread(thread.ID) }()
	select {
	case err := <-deleted:
		t.Fatalf("delete returned (%v) while a browser tools call it must outlast was running", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-call; err != nil {
		t.Fatalf("browser tools call: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if pages := manager.CompanionState(appbrowser.Access{ThreadID: thread.ID}).Pages; len(pages) != 0 {
		t.Fatalf("the deleted thread owns %d browser pages", len(pages))
	}
}

// A companion tab that finishes opening after a delete closed the thread's
// pages, but before its row dropped, is closed once the row is gone.
func TestCompanionTabOpenedDuringADeleteIsClosed(t *testing.T) {
	deletes := map[string]func(app *App, threadID string) error{
		"thread": func(app *App, threadID string) error { return app.DeleteThread(threadID) },
		"draft": func(app *App, threadID string) error {
			deleted, err := app.DeleteEmptyDraftThread(threadID)
			if err == nil && !deleted {
				err = fmt.Errorf("the empty draft %s was not deleted", threadID)
			}
			return err
		},
	}
	for _, action := range []BrowserCompanionAction{
		{Kind: "new"},
		{Kind: "navigate", Address: "https://example.test/"},
	} {
		for deleteName, deleteThread := range deletes {
			t.Run(action.Kind+"/"+deleteName, func(t *testing.T) {
				app := newTestAppWithStore(t)
				manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
				app.browser.manager = manager
				t.Cleanup(func() { _ = manager.Close() })
				thread := makeWorkspaceThread(t, app, "thread-companion-"+action.Kind+"-"+deleteName)
				access, err := app.browserAccess(thread.ID)
				if err != nil {
					t.Fatal(err)
				}
				// Holding the mutation lock parks the delete after it closed the
				// thread's pages and before its row drops. Closing them forgets
				// the thread's named browser session.
				unlockMutation, err := app.threadApplication().LockMutable(t.Context(), thread.ID)
				if err != nil {
					t.Fatal(err)
				}
				var unlockOnce sync.Once
				unlock := func() { unlockOnce.Do(unlockMutation) }
				t.Cleanup(unlock)
				if _, err := manager.NameSession(t.Context(), access, "before the delete"); err != nil {
					t.Fatal(err)
				}
				deleted := make(chan error, 1)
				go func() { deleted <- deleteThread(app, thread.ID) }()
				waitFor(t, "the delete to close the thread's pages", func() bool {
					return manager.CompanionState(access).SessionName == ""
				})
				opened := make(chan error, 1)
				go func() {
					_, err := app.BrowserCompanionDo(context.Background(), thread.ID, action)
					opened <- err
				}()
				// The row is still present, so the action leaves the page to
				// the delete.
				if err := <-opened; err != nil {
					t.Fatalf("opening a tab while the delete runs: %v", err)
				}
				if pages := manager.CompanionState(access).Pages; len(pages) != 1 {
					t.Fatalf("the thread owns %d browser pages before the delete finishes, want 1", len(pages))
				}
				unlock()
				if err := <-deleted; err != nil {
					t.Fatalf("delete: %v", err)
				}
				if pages := manager.CompanionState(access).Pages; len(pages) != 0 {
					t.Fatalf("the deleted thread owns %d browser pages", len(pages))
				}
			})
		}
	}
}

// deleteOnFirstErr runs onErr the first time its Err is read. A page open
// checks its context before it registers the page, after the companion
// action read the thread's row.
type deleteOnFirstErr struct {
	context.Context
	fired atomic.Bool
	onErr func()
}

func (c *deleteOnFirstErr) Err() error {
	if c.fired.CompareAndSwap(false, true) {
		c.onErr()
	}
	return c.Context.Err()
}

// Every companion action opens a page for a thread that has none. A thread
// deleted between the action's row read and the page's registration gets
// that page closed.
func TestCompanionActionOpeningAPageForADeletedThreadClosesIt(t *testing.T) {
	calls := map[string]func(ctx context.Context, app *App, threadID string) error{
		"reveal": func(ctx context.Context, app *App, threadID string) error {
			return app.BrowserCompanionRevealPageFile(ctx, threadID, "")
		},
	}
	for _, action := range []BrowserCompanionAction{
		{Kind: "navigate", Address: "https://example.test/"},
		{Kind: "new"},
		{Kind: "back"},
		{Kind: "forward"},
		{Kind: "reload"},
		{Kind: "stop"},
	} {
		calls[action.Kind] = func(ctx context.Context, app *App, threadID string) error {
			_, err := app.BrowserCompanionDo(ctx, threadID, action)
			return err
		}
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			app := newTestAppWithStore(t)
			manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
			app.browser.manager = manager
			t.Cleanup(func() { _ = manager.Close() })
			thread := makeWorkspaceThread(t, app, "thread-companion-deleted-"+name)
			access, err := app.browserAccess(thread.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx := &deleteOnFirstErr{Context: context.Background(), onErr: func() {
				if err := app.DeleteThread(thread.ID); err != nil {
					t.Errorf("delete: %v", err)
				}
			}}
			err = call(ctx, app, thread.ID)
			if !ctx.fired.Load() {
				t.Fatalf("the action opened no page (%v)", err)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("the action returned %v, want the deleted thread's missing row", err)
			}
			if pages := manager.CompanionState(access).Pages; len(pages) != 0 {
				t.Fatalf("the deleted thread owns %d browser pages", len(pages))
			}
		})
	}
}

// A page that opens after the delete's last close finds the row gone, or
// kept as a holder, and closes itself.
func TestCompanionPageOpenedAfterADeleteClosesItself(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("holder=%v", held), func(t *testing.T) {
			app := newTestAppWithStore(t)
			manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
			app.browser.manager = manager
			t.Cleanup(func() { _ = manager.Close() })
			var threadID string
			if held {
				threadID, _ = seedHeldSource(t, app, 1)
			} else {
				thread := makeWorkspaceThread(t, app, "thread-companion-late")
				threadID = thread.ID
				if err := app.DeleteThread(threadID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := app.BrowserCompanionDo(context.Background(), threadID, BrowserCompanionAction{Kind: "new"}); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("a companion action on the deleted thread returned %v, want its missing row", err)
			}
			// An action that read the row before the delete opens its page
			// after the delete's last close.
			access := appbrowser.Access{ThreadID: threadID, Workspace: t.TempDir()}
			if _, err := manager.NewCompanionPage(context.Background(), access); err != nil {
				t.Fatal(err)
			}
			if err := app.closeBrowserPagesIfThreadDeleted(threadID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("the recheck returned %v, want the missing row", err)
			}
			if pages := manager.CompanionState(access).Pages; len(pages) != 0 {
				t.Fatalf("the deleted thread owns %d browser pages", len(pages))
			}
		})
	}
}

func newBrowserTestApp(t *testing.T) (*App, *appbrowser.Manager) {
	t.Helper()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	app.browser.mcp = appbrowser.NewMCPServer(manager, true)
	t.Cleanup(func() { _ = app.browser.mcp.Close(); _ = manager.Close() })
	return app, manager
}

func browserTestThread(t *testing.T, app *App, id string) (store.Thread, appbrowser.Access) {
	t.Helper()
	thread := testThread(id)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	access, err := app.browserAccess(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	return thread, access
}

func browserPageStates(manager *appbrowser.Manager, access appbrowser.Access) []appbrowser.PageInfo {
	return manager.CompanionState(access).Pages
}

// The reaper ending an idle session suspends the thread's browser pages; a
// user stopping the session leaves them live. Only the idle end suspends.
func TestOnlyTheIdleReaperSuspendsBrowserPages(t *testing.T) {
	app, manager := newBrowserTestApp(t)
	reaped, reapedAccess := browserTestThread(t, app, "thread-browser-reaped")
	stopped, stoppedAccess := browserTestThread(t, app, "thread-browser-stopped")
	for _, access := range []appbrowser.Access{reapedAccess, stoppedAccess} {
		if _, err := manager.Open(t.Context(), access, "https://example.test/", appbrowser.OpenOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	app.sessionManager().put(reaped.ID, session{
		Provider: string(provider.Codex), Token: "reaped",
		Liveness: newSessionLiveness(now.Add(-idleReapThreshold - time.Minute)),
	})
	app.sessionManager().put(stopped.ID, session{
		Provider: string(provider.Codex), Token: "stopped", Liveness: newSessionLiveness(now),
	})

	app.reapIdleSessions(now)
	if err := app.StopSession(stopped.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	if pages := browserPageStates(manager, reapedAccess); len(pages) != 1 || !pages[0].Suspended {
		t.Fatalf("pages of the reaped thread = %#v, want one suspended", pages)
	}
	if pages := browserPageStates(manager, stoppedAccess); len(pages) != 1 || pages[0].Suspended {
		t.Fatalf("pages of the stopped thread = %#v, want one live", pages)
	}

	// The pane hydrates the suspended tab and presenting it restores it.
	hydrated, err := app.BrowserCompanionThreadState(reaped.ID)
	if err != nil || len(hydrated.Pages) != 1 || !hydrated.Pages[0].Suspended {
		t.Fatalf("BrowserCompanionThreadState = %#v, %v; want one suspended page", hydrated.Pages, err)
	}
	pageID := hydrated.Pages[0].ID
	if _, err := app.BrowserCompanionDo(t.Context(), reaped.ID, BrowserCompanionAction{Kind: "activate", PageID: pageID}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if pages := browserPageStates(manager, reapedAccess); len(pages) != 1 || pages[0].ID != pageID || pages[0].Suspended {
		t.Fatalf("pages after presenting = %#v, want %s live", pages, pageID)
	}
}

// Archiving a thread closes its browser pages, live and suspended, through
// both doors that archive; a session kept because a turn started after the
// request keeps its pages too.
func TestArchiveThreadClosesItsBrowserPages(t *testing.T) {
	archived := true
	for _, tc := range []struct {
		name    string
		archive func(*App, string) error
	}{
		{"archive binding", (*App).ArchiveThread},
		{"thread_update", func(app *App, threadID string) error {
			_, err := app.applyThreadOrganizePatch(context.Background(), threadID, threadapp.OrganizePatch{Archived: &archived})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, manager := newBrowserTestApp(t)
			thread, access := browserTestThread(t, app, "thread-archive-browser")
			if _, err := manager.Open(t.Context(), access, "https://example.test/suspended", appbrowser.OpenOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := manager.SuspendThread(t.Context(), thread.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Open(t.Context(), access, "https://example.test/live", appbrowser.OpenOptions{}); err != nil {
				t.Fatal(err)
			}

			if err := tc.archive(app, thread.ID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if pages := browserPageStates(manager, access); len(pages) != 0 {
				t.Fatalf("archived thread still has browser pages: %#v", pages)
			}
		})
	}
	t.Run("session kept", func(t *testing.T) {
		app, manager := newBrowserTestApp(t)
		thread, access := browserTestThread(t, app, "thread-archive-browser-reengaged")
		if _, err := manager.Open(t.Context(), access, "https://example.test/", appbrowser.OpenOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := app.store.InsertTurn(store.Turn{
			TurnID: "turn-after", ThreadID: thread.ID, StartedAt: time.Now().Add(time.Minute).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
		app.sessionManager().put(thread.ID, session{
			Provider: string(provider.Codex), Token: "tok", Liveness: newSessionLiveness(time.Now()),
		})
		if err := app.ArchiveThread(thread.ID); err != nil {
			t.Fatalf("ArchiveThread: %v", err)
		}
		if pages := browserPageStates(manager, access); len(pages) != 1 || pages[0].Suspended {
			t.Fatalf("pages of a thread whose session the archive kept = %#v", pages)
		}
	})
}

// At boot, saved browser pages belong to a thread that exists and is not
// archived.
func TestBrowserThreadKeepsPagesOnlyWhileListed(t *testing.T) {
	app := newTestAppWithStore(t)
	listed := testThread("thread-listed")
	archivedThread := testThread("thread-archived")
	for _, thread := range []store.Thread{listed, archivedThread} {
		if err := app.store.CreateThread(thread); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := app.store.ArchiveThread(archivedThread.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{{listed.ID, true}, {archivedThread.ID, false}, {"thread-missing", false}} {
		got, err := app.browserThreadKeepsPages(tc.id)
		if err != nil || got != tc.want {
			t.Fatalf("browserThreadKeepsPages(%s) = %v, %v; want %v", tc.id, got, err, tc.want)
		}
	}
}

// The boot's browser manager brings back the saved pages of a listed thread
// and drops those of a thread archived while the app was down.
func TestBootKeepsSavedBrowserPagesOnlyForListedThreads(t *testing.T) {
	app := newTestAppWithStore(t)
	app.browser.mockEngine = true
	dir := t.TempDir()
	listed, listedAccess := browserTestThread(t, app, "thread-boot-listed")
	archived, archivedAccess := browserTestThread(t, app, "thread-boot-archived")
	current := app.currentSettings()
	current.BrowserEnabled = true
	first := app.newBrowserManager(dir, current)
	for _, access := range []appbrowser.Access{listedAccess, archivedAccess} {
		if _, err := first.Open(t.Context(), access, "https://example.test/", appbrowser.OpenOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.store.ArchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}

	restarted := app.newBrowserManager(dir, current)
	t.Cleanup(func() { _ = restarted.Close() })
	if pages := browserPageStates(restarted, listedAccess); len(pages) != 1 || !pages[0].Suspended {
		t.Fatalf("pages of %s after the restart = %#v, want one suspended", listed.ID, pages)
	}
	if pages := browserPageStates(restarted, archivedAccess); len(pages) != 0 {
		t.Fatalf("pages of archived %s after the restart = %#v", archived.ID, pages)
	}
}

// Shutdown cancels the reaper's suspension before its next page: a desktop
// shutdown holds the UI thread every engine call needs, and Close saves the
// pages still live.
func TestShutdownStopsTheReapersBrowserSuspension(t *testing.T) {
	app, manager := newBrowserTestApp(t)
	thread, access := browserTestThread(t, app, "thread-browser-shutdown")
	if _, err := manager.Open(t.Context(), access, "https://example.test/", appbrowser.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app.appCtx = ctx
	cancel()
	if err := app.suspendThreadBrowserPages(thread.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("suspension after the shutdown began = %v", err)
	}
	if pages := browserPageStates(manager, access); len(pages) != 1 || pages[0].Suspended {
		t.Fatalf("pages = %#v, want one live", pages)
	}
}
