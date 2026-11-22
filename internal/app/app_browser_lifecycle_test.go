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

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
)

func TestDeleteThreadReleasesItsBrowserPagesAndToolToggles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		delete func(*App, string) error
	}{
		{"delete thread", (*App).deleteThread},
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
			access, err := app.bindBrowser(thread.ID)
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

// A delete waits out a browser tools call already running for the thread,
// then closes the page that call opened.
func TestDeleteThreadWaitsForARunningBrowserToolCall(t *testing.T) {
	t.Parallel()
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
			err = resp.Body.Close()
			if err == nil && resp.StatusCode != http.StatusOK {
				err = errors.New(resp.Status)
			}
		}
		call <- err
	}()
	<-held.entered
	deleted := make(chan error, 1)
	go func() { deleted <- app.deleteThread(thread.ID) }()
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
	if pages := manager.CompanionState(appbrowser.Caller{ThreadID: appbrowser.ThreadID(thread.ID)}).Pages; len(pages) != 0 {
		t.Fatalf("the deleted thread owns %d browser pages", len(pages))
	}
}

// A companion tab that finishes opening after a delete closed the thread's
// pages, but before its row dropped, is closed once the row is gone.
func TestCompanionTabOpenedDuringADeleteIsClosed(t *testing.T) {
	t.Parallel()
	deletes := map[string]func(app *App, threadID string) error{
		"thread": func(app *App, threadID string) error { return app.deleteThread(threadID) },
		"draft": func(app *App, threadID string) error {
			deleted, err := app.DeleteEmptyDraftThread(threadID)
			if err == nil && !deleted {
				err = fmt.Errorf("the empty draft %s was not deleted", threadID)
			}
			return err
		},
	}
	for name, action := range map[string]BrowserCompanionAction{
		"new":         {Kind: "new"},
		"new-address": {Kind: "new", Address: "https://example.test/"},
	} {
		for deleteName, deleteThread := range deletes {
			t.Run(name+"/"+deleteName, func(t *testing.T) {
				app := newTestAppWithStore(t)
				manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
				app.browser.manager = manager
				t.Cleanup(func() { _ = manager.Close() })
				thread := makeWorkspaceThread(t, app, "thread-companion-"+name+"-"+deleteName)
				access, err := app.bindBrowser(thread.ID)
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
					return manager.CompanionState(access.Caller()).SessionName == ""
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
				if pages := manager.CompanionState(access.Caller()).Pages; len(pages) != 1 {
					t.Fatalf("the thread owns %d browser pages before the delete finishes, want 1", len(pages))
				}
				unlock()
				if err := <-deleted; err != nil {
					t.Fatalf("delete: %v", err)
				}
				if pages := manager.CompanionState(access.Caller()).Pages; len(pages) != 0 {
					t.Fatalf("the deleted thread owns %d browser pages", len(pages))
				}
			})
		}
	}
}

// A companion action that opens a page for a thread that has none (a new
// tab, with an address or not, the pane's reveal), with a thread
// deleted between the action's row read and the page's registration, gets
// that page closed by the delete, and the action answers the close.
// Navigate, back, forward and reload name their tab (parseCompanionAction),
// so they open none.
func TestCompanionActionOpeningAPageForADeletedThreadClosesIt(t *testing.T) {
	t.Parallel()
	calls := map[string]func(ctx context.Context, app *App, threadID string) error{
		"reveal": func(ctx context.Context, app *App, threadID string) error {
			return app.BrowserCompanionRevealPageFile(ctx, threadID, "")
		},
	}
	for name, action := range map[string]BrowserCompanionAction{
		"new-address": {Kind: "new", Address: "https://example.test/"},
		"new":         {Kind: "new"},
	} {
		calls[name] = func(ctx context.Context, app *App, threadID string) error {
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
			access, err := app.bindBrowser(thread.ID)
			if err != nil {
				t.Fatal(err)
			}
			// A page open pushes its creating tab after the action read the
			// thread's row and before the page is registered.
			var fired atomic.Bool
			manager.SetEventSink(func(event appbrowser.CompanionEvent) {
				if event.ThreadID == thread.ID && len(event.Pages) > 0 && fired.CompareAndSwap(false, true) {
					if err := app.deleteThread(thread.ID); err != nil {
						t.Errorf("delete: %v", err)
					}
				}
			})
			err = call(context.Background(), app, thread.ID)
			if !fired.Load() {
				t.Fatalf("the action opened no page (%v)", err)
			}
			if !errors.Is(err, appbrowser.ErrThreadClosed) {
				t.Fatalf("the action returned %v, want the thread's close", err)
			}
			if pages := manager.CompanionState(access.Caller()).Pages; len(pages) != 0 {
				t.Fatalf("the deleted thread owns %d browser pages", len(pages))
			}
		})
	}
}

// A companion action on a deleted thread, or one kept as a holder, answers
// its missing row; one bound before the delete opens nothing after it.
func TestCompanionPageOpenedAfterADeleteClosesItself(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("holder=%v", held), func(t *testing.T) {
			app := newTestAppWithStore(t)
			manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
			app.browser.manager = manager
			t.Cleanup(func() { _ = manager.Close() })
			var threadID string
			var access appbrowser.Access
			if held {
				threadID, _ = seedHeldSource(t, app, 1)
			} else {
				thread := makeWorkspaceThread(t, app, "thread-companion-late")
				threadID = thread.ID
				// An action that read the row before the delete.
				var err error
				access, err = manager.BindThread(appbrowser.ThreadID(threadID), func() (appbrowser.Caller, error) {
					caller := appbrowser.Caller{ThreadID: appbrowser.ThreadID(threadID), Workspace: t.TempDir()}
					return caller, app.deleteThread(threadID)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := app.BrowserCompanionDo(context.Background(), threadID, BrowserCompanionAction{Kind: "new"}); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("a companion action on the deleted thread returned %v, want its missing row", err)
			}
			if held {
				return
			}
			// The action that read the row before the delete opens its page
			// after the delete's last close.
			if _, err := manager.NewCompanionPage(context.Background(), access, ""); !errors.Is(err, appbrowser.ErrThreadClosed) {
				t.Fatalf("the action bound before the delete opened a tab: %v", err)
			}
			if pages := manager.CompanionState(access.Caller()).Pages; len(pages) != 0 {
				t.Fatalf("the deleted thread owns %d browser pages", len(pages))
			}
			if err := access.End(); !errors.Is(err, appbrowser.ErrThreadClosed) {
				t.Fatalf("the action ended with %v, want the thread's close", err)
			}
		})
	}
}

// A companion action bound before an archive or a handoff closed the
// thread's pages opens no page after that close, though the thread's row is
// still there, and its end leaves alone the tab an action bound after the
// close opened.
func TestACompanionActionOverlappingAPageCloseLeavesNoPage(t *testing.T) {
	t.Parallel()
	app, manager := newBrowserTestApp(t)
	thread, access := browserTestThread(t, app, "thread-companion-retired")

	caller := access.Caller()
	lookup := func() (appbrowser.Caller, error) { return caller, nil }
	stale, err := manager.BindThread(appbrowser.ThreadID(thread.ID), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.closeThreadBrowserPages(thread.ID); err != nil {
		t.Fatal(err)
	}
	later, err := manager.BindThread(appbrowser.ThreadID(thread.ID), lookup)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := manager.NewCompanionPage(t.Context(), later, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.NewCompanionPage(t.Context(), stale, ""); !errors.Is(err, appbrowser.ErrThreadClosed) {
		t.Fatalf("the stale action's new tab answered %v, want ErrThreadClosed", err)
	}
	if err := stale.End(); !errors.Is(err, appbrowser.ErrThreadClosed) {
		t.Fatalf("the stale action ended with %v, want ErrThreadClosed", err)
	}
	if err := later.End(); err != nil {
		t.Fatalf("an action bound after the close ended with %v", err)
	}
	pages := manager.CompanionState(caller).Pages
	if len(pages) != 1 || pages[0].ID != fresh.ID {
		t.Fatalf("the thread has tabs %+v, want only the one the later action opened", pages)
	}
}

// The reaper ending an idle session suspends the thread's browser pages; a
// user stopping the session leaves them live. Only the idle end suspends.
func TestOnlyTheIdleReaperSuspendsBrowserPages(t *testing.T) {
	t.Parallel()
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

	if pages := browserPageStates(manager, reapedAccess.Caller()); len(pages) != 1 || !pages[0].Suspended {
		t.Fatalf("pages of the reaped thread = %#v, want one suspended", pages)
	}
	if pages := browserPageStates(manager, stoppedAccess.Caller()); len(pages) != 1 || pages[0].Suspended {
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
	if pages := browserPageStates(manager, reapedAccess.Caller()); len(pages) != 1 || pages[0].ID != pageID || pages[0].Suspended {
		t.Fatalf("pages after presenting = %#v, want %s live", pages, pageID)
	}
}

// A Stop that brings a spawned thread to rest leaves its browser pages open,
// for the person to look at what it had open.
func TestAStopKeepsTheStoppedThreadsBrowserPages(t *testing.T) {
	t.Parallel()
	app, manager := newBrowserTestApp(t)
	caller, _ := browserTestThread(t, app, "thread-browser-spawner")
	worker, access := browserTestThread(t, app, "thread-browser-worker")
	if _, err := manager.Open(t.Context(), access, "https://example.test/", appbrowser.OpenOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "spawn-browser-worker", CallerThreadID: caller.ID, Kind: store.ThreadRequestSpawn,
		TargetThreadID: worker.ID, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	app.sessionManager().put(worker.ID, session{
		Provider: string(provider.Codex), Token: "worker", Liveness: newSessionLiveness(time.Now()),
	})

	stop, err := app.StopThreadAgentRow(t.Context(), caller.ID, "spawn-browser-worker")
	if err != nil || len(stop.Failures) != 0 {
		t.Fatalf("StopThreadAgentRow = %+v, %v", stop, err)
	}
	if _, live := app.sessionManager().get(worker.ID); live {
		t.Fatal("the stopped worker still has a session")
	}
	if pages := browserPageStates(manager, access.Caller()); len(pages) != 1 || pages[0].Suspended {
		t.Fatalf("pages of the stopped worker = %#v, want its one page left open", pages)
	}
}

// Archiving a thread closes its browser pages, live and suspended, through
// both doors that archive; a session kept because a turn started after the
// request keeps its pages too.
func TestArchiveThreadClosesItsBrowserPages(t *testing.T) {
	t.Parallel()
	archived := true
	for _, tc := range []struct {
		name    string
		archive func(*App, string) error
	}{
		{"archive binding", archiveThread},
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
			if err := manager.SuspendThread(t.Context(), appbrowser.ThreadID(thread.ID)); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Open(t.Context(), access, "https://example.test/live", appbrowser.OpenOptions{}); err != nil {
				t.Fatal(err)
			}

			if err := tc.archive(app, thread.ID); err != nil {
				t.Fatalf("archive: %v", err)
			}
			if pages := browserPageStates(manager, access.Caller()); len(pages) != 0 {
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
		if err := archiveThread(app, thread.ID); err != nil {
			t.Fatalf("ArchiveThread: %v", err)
		}
		if pages := browserPageStates(manager, access.Caller()); len(pages) != 1 || pages[0].Suspended {
			t.Fatalf("pages of a thread whose session the archive kept = %#v", pages)
		}
	})
}

// At boot, saved browser pages belong to a thread that exists and is not
// archived.
func TestBrowserThreadKeepsPagesOnlyWhileListed(t *testing.T) {
	t.Parallel()
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
		got, err := app.browserThreadKeepsPages(appbrowser.ThreadID(tc.id))
		if err != nil || got != tc.want {
			t.Fatalf("browserThreadKeepsPages(%s) = %v, %v; want %v", tc.id, got, err, tc.want)
		}
	}
}

// The boot's browser manager brings back the saved pages of a listed thread
// and drops those of a thread archived while the app was down.
func TestBootKeepsSavedBrowserPagesOnlyForListedThreads(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	app.browser.mockEngine = true
	dir := t.TempDir()
	listed, listedAccess := browserTestCaller(t, app, "thread-boot-listed")
	archived, archivedAccess := browserTestCaller(t, app, "thread-boot-archived")
	current := app.currentSettings()
	current.BrowserEnabled = true
	first := app.newBrowserManager(dir, current)
	for _, caller := range []appbrowser.Caller{listedAccess, archivedAccess} {
		if _, err := first.Open(t.Context(), first.Bind(caller), "https://example.test/", appbrowser.OpenOptions{}); err != nil {
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
	t.Parallel()
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
	if pages := browserPageStates(manager, access.Caller()); len(pages) != 1 || pages[0].Suspended {
		t.Fatalf("pages = %#v, want one live", pages)
	}
}
