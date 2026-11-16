package app

import (
	"testing"

	appbrowser "agent-overflow/internal/browser"
)

// The pane's "new" from no tab shows the companion on the new tab and names
// it in the answer, navigating an address it carries (a mod-clicked link);
// its "close" answers with the tab gone.
func TestBrowserCompanionNewNamesAShownTabAndCloseRemovesIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-new")
	ctx := t.Context()

	state, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if state.PageID == "" || state.ActivePageID != state.PageID || state.Visible == nil || !*state.Visible || len(state.Pages) != 1 {
		t.Fatalf("new from no tab = %+v, want one shown tab named in the answer", state)
	}
	first := state.PageID
	state, err = app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new", Address: "https://example.test/link"})
	if err != nil {
		t.Fatal(err)
	}
	if state.PageID == "" || state.PageID == first || state.ActivePageID != state.PageID || len(state.Pages) != 2 {
		t.Fatalf("new with an address = %+v", state)
	}
	for _, page := range state.Pages {
		if page.ID == state.PageID && page.URL != "https://example.test/link" {
			t.Fatalf("the new tab is at %q, want the link", page.URL)
		}
	}
	state, err = app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "close", PageID: first})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pages) != 1 || state.Pages[0].ID == first || state.PageID != "" {
		t.Fatalf("after close = %+v", state)
	}
}

// The pane's "jar" sets the thread's cookie jar; the project jar is keyed by
// the thread's project, which the app passes in with its workspace.
func TestBrowserCompanionJarUsesTheThreadsProject(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-jar")
	ctx := t.Context()

	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new"}); err != nil {
		t.Fatal(err)
	}
	state, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "jar", Jar: appbrowser.JarProject})
	if err != nil {
		t.Fatalf("choose the project jar: %v", err)
	}
	if state.Jar != appbrowser.JarProject || len(state.Pages) != 1 {
		t.Fatalf("after choosing the project jar: %+v", state)
	}
	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "jar", Jar: "shared"}); err == nil {
		t.Fatal("an unknown jar was accepted")
	}
}
