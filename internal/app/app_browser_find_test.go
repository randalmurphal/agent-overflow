package app

import (
	"context"
	"testing"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/transport"
)

// The pane's find reaches the thread's shown page through the binding, with
// the caller's thread as the owner every page operation checks.
func TestBrowserCompanionFindRunsOnTheThreadsShownPage(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-find")
	other := makeWorkspaceThread(t, app, "thread-companion-find-other")
	ctx := t.Context()
	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new", Address: "https://example.test/alpha-beta-alpha"}); err != nil {
		t.Fatal(err)
	}
	state, err := app.BrowserCompanionThreadState(thread.ID)
	if err != nil || len(state.Pages) != 1 {
		t.Fatalf("state = %+v, %v", state, err)
	}
	pageID := state.Pages[0].ID
	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "show", PageID: pageID}); err != nil {
		t.Fatal(err)
	}
	if state, err := app.BrowserCompanionThreadState(thread.ID); err != nil || state.FindMode != appbrowser.FindModeBar {
		t.Fatalf("state find mode = %+v, %v", state, err)
	}
	// The find shows in the mounted pane; with none there is nothing to show it.
	if _, err := app.BrowserCompanionFind(ctx, thread.ID, BrowserFindAction{Kind: "start", PageID: pageID, Query: "alpha"}); err == nil {
		t.Fatal("a find started with no pane mounted")
	}
	mount, err := app.BrowserCompanionPaneAttach(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.BrowserCompanionPaneDetach(context.Background(), mount.ID) })
	found, err := app.BrowserCompanionFind(ctx, thread.ID, BrowserFindAction{Kind: "start", PageID: pageID, Query: "alpha", Mount: mount.ID, Seq: 1})
	if err != nil || found.Matches != 2 || found.Current != 0 || !found.Active || found.Mount != mount.ID {
		t.Fatalf("start = %+v, %v", found, err)
	}
	if next, err := app.BrowserCompanionFind(ctx, thread.ID, BrowserFindAction{Kind: "next", PageID: pageID, Mount: mount.ID, Seq: 1}); err != nil || next.Current != 1 {
		t.Fatalf("next = %+v, %v", next, err)
	}
	if _, err := app.BrowserCompanionFind(ctx, other.ID, BrowserFindAction{Kind: "next", PageID: pageID, Mount: mount.ID, Seq: 1}); err == nil {
		t.Fatal("another thread stepped this thread's find")
	}
	if stopped, err := app.BrowserCompanionFind(ctx, thread.ID, BrowserFindAction{Kind: "stop", PageID: pageID, Mount: mount.ID, Seq: 1}); err != nil || stopped.Active {
		t.Fatalf("stop = %+v, %v", stopped, err)
	}
}

// A find still running when its connection closes is not cancelled
// (BrowserCompanionFind is not caller-bound) and returns before the
// connection's cleanup runs, which the transport starts only after joining
// its handlers. The pane mount it names is the connection's, so that
// cleanup detaches it and ends the search: a step from it then finds
// nothing, and a start from it is refused.
func TestBrowserFindRunAfterItsConnectionClosedIsEndedByTheCleanup(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-find-closed")
	if _, err := app.BrowserCompanionDo(t.Context(), thread.ID, BrowserCompanionAction{Kind: "new", Address: "https://example.test/alpha"}); err != nil {
		t.Fatal(err)
	}
	state, err := app.BrowserCompanionThreadState(thread.ID)
	if err != nil || len(state.Pages) != 1 {
		t.Fatalf("state = %+v, %v", state, err)
	}
	pageID := state.Pages[0].ID
	if _, err := app.BrowserCompanionDo(t.Context(), thread.ID, BrowserCompanionAction{Kind: "show", PageID: pageID}); err != nil {
		t.Fatal(err)
	}
	ctx, conn := transport.WithConnState(context.Background(), transport.ConnPrincipal{HostWindow: true})
	mount, err := app.BrowserCompanionPaneAttach(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The socket closes while the start runs: the start finishes, then the
	// cleanup runs.
	action := BrowserFindAction{Kind: "start", PageID: pageID, Query: "alpha", Mount: mount.ID, Seq: 1}
	if started, err := app.BrowserCompanionFind(ctx, thread.ID, action); err != nil || !started.Active {
		t.Fatalf("start after the socket closed = %+v, %v", started, err)
	}
	conn.RunCleanups()
	action.Kind = "next"
	if step, err := app.BrowserCompanionFind(context.Background(), thread.ID, action); err != nil || !step.Foreign || step.Active || step.Mount != "" {
		t.Fatalf("step after the connection's cleanup = %+v, %v", step, err)
	}
	action.Kind = "start"
	if _, err := app.BrowserCompanionFind(context.Background(), thread.ID, action); err == nil {
		t.Fatal("a start from the closed connection's mount ran after its cleanup")
	}
}
