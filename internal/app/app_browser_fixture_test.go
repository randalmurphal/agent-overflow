package app

import (
	"context"
	"testing"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/store"
)

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

func newBrowserTestApp(t *testing.T) (*App, *appbrowser.Manager) {
	t.Helper()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	app.browser.mcp = appbrowser.NewMCPServer(manager, true)
	t.Cleanup(func() { _ = app.browser.mcp.Close(); _ = manager.Close() })
	return app, manager
}

// browserTestThread creates a thread with a workspace and binds an action
// of it on the app's browser manager.
func browserTestThread(t *testing.T, app *App, id string) (store.Thread, appbrowser.Access) {
	t.Helper()
	thread, _ := browserTestCaller(t, app, id)
	access, err := app.bindBrowser(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	return thread, access
}

// browserTestCaller creates a thread with a workspace and answers its
// browser Caller, for a manager the test binds on itself.
func browserTestCaller(t *testing.T, app *App, id string) (store.Thread, appbrowser.Caller) {
	t.Helper()
	thread := testThread(id)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	caller, err := app.browserCaller(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	return thread, caller
}

func browserPageStates(manager *appbrowser.Manager, caller appbrowser.Caller) []appbrowser.PageInfo {
	return manager.CompanionState(caller).Pages
}
