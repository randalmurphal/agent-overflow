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

// The pane's next/previous tab is a relative "step" the Manager applies to
// its own selection, so two steps sent before either answer move two tabs.
func TestBrowserCompanionStepMovesFromTheManagersSelection(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-step")
	ctx := t.Context()

	var ids []string
	for range 3 {
		state, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, state.PageID)
	}
	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "activate", PageID: ids[0]}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "step", Index: 1})
			done <- err
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("step = %v", err)
		}
	}
	state, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "step", Index: -1})
	if err != nil {
		t.Fatal(err)
	}
	if state.ActivePageID != ids[1] {
		t.Fatalf("after two steps forward and one back the active tab is %s, want %s", state.ActivePageID, ids[1])
	}
}
