package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/transport"
)

// The pane's "focus-page" and "focus-page-withdraw" arm and withdraw a page
// focus intent, and only the host's own window may ask: the native view
// lives there. With nothing presented (the fake engine has no views) each is
// a no-op answering the thread's state.
func TestBrowserCompanionFocusPageIsTheHostWindowsAlone(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-focus")
	opened, err := app.BrowserCompanionDo(t.Context(), thread.ID, BrowserCompanionAction{Kind: "new"})
	if err != nil {
		t.Fatal(err)
	}
	// A request names its mount, page and intent.
	hostCtx, _ := transport.WithConnState(t.Context(), transport.ConnPrincipal{HostWindow: true})
	if _, err := app.BrowserCompanionDo(hostCtx, thread.ID, BrowserCompanionAction{Kind: "focus-page"}); err == nil {
		t.Fatal("a focus-page naming no intent was accepted")
	}
	for _, tc := range []struct {
		name  string
		ctx   func() context.Context
		allow bool
	}{
		{"the host window", func() context.Context {
			ctx, _ := transport.WithConnState(t.Context(), transport.ConnPrincipal{HostWindow: true})
			return ctx
		}, true},
		{"another client with the launch credential", func() context.Context {
			ctx, _ := transport.WithConnState(t.Context(), transport.ConnPrincipal{})
			return ctx
		}, false},
		{"an in-process caller", func() context.Context { return t.Context() }, true},
	} {
		for _, kind := range []string{"focus-page", "focus-page-withdraw"} {
			t.Run(tc.name+" "+kind, func(t *testing.T) {
				action := BrowserCompanionAction{Kind: kind, PageID: opened.PageID, Mount: "mount-1", Intent: 1}
				state, err := app.BrowserCompanionDo(tc.ctx(), thread.ID, action)
				if !tc.allow {
					if !errors.Is(err, errBrowserPaneNotHostWindow) {
						t.Fatalf("%s = %v; want the host-window refusal", kind, err)
					}
					return
				}
				if err != nil || len(state.Pages) != 1 {
					t.Fatalf("%s = %+v, %v; want the thread's state", kind, state, err)
				}
			})
		}
	}
}

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

// The tab menu's actions reach the Manager: duplicate, close to the right,
// close others, reopen, and reload.
func TestBrowserCompanionTabMenuActions(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-menu")
	ctx := t.Context()
	do := func(action BrowserCompanionAction) appbrowser.CompanionEvent {
		t.Helper()
		state, err := app.BrowserCompanionDo(ctx, thread.ID, action)
		if err != nil {
			t.Fatalf("%s: %v", action.Kind, err)
		}
		return state
	}
	urls := func(state appbrowser.CompanionEvent) string {
		var out []string
		for _, page := range state.Pages {
			out = append(out, strings.TrimPrefix(page.URL, "https://example.test/"))
		}
		return strings.Join(out, " ")
	}

	first := do(BrowserCompanionAction{Kind: "new", Address: "https://example.test/a"}).PageID
	do(BrowserCompanionAction{Kind: "new", Address: "https://example.test/b"})
	if got := urls(do(BrowserCompanionAction{Kind: "duplicate", PageID: first})); got != "a a b" {
		t.Fatalf("after duplicate: %s", got)
	}
	if got := urls(do(BrowserCompanionAction{Kind: "close-right", PageID: first})); got != "a" {
		t.Fatalf("after close to the right: %s", got)
	}
	state := do(BrowserCompanionAction{Kind: "reopen"})
	if got := urls(state); got != "a b" || state.ClosedTabs != 1 {
		t.Fatalf("after reopen: %s, %d closed", got, state.ClosedTabs)
	}
	if got := urls(do(BrowserCompanionAction{Kind: "close-others", PageID: first})); got != "a" {
		t.Fatalf("after close others: %s", got)
	}
	if got := urls(do(BrowserCompanionAction{Kind: "reload", PageID: first})); got != "a" {
		t.Fatalf("after reload: %s", got)
	}
}

// Every kind that selects a tab or acts on the active one carries its mount
// and number to the Manager, which admits each number once: a number
// already admitted does nothing.
func TestBrowserCompanionSelectionsCarryTheirOrder(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-order")
	ctx := t.Context()
	do := func(action BrowserCompanionAction) appbrowser.CompanionEvent {
		t.Helper()
		state, err := app.BrowserCompanionDo(ctx, thread.ID, action)
		if err != nil {
			t.Fatalf("%s: %v", action.Kind, err)
		}
		return state
	}
	first := do(BrowserCompanionAction{Kind: "new", Address: "https://example.test/a"}).PageID
	second := do(BrowserCompanionAction{Kind: "new", Address: "https://example.test/b"}).PageID
	do(BrowserCompanionAction{Kind: "close", PageID: do(BrowserCompanionAction{Kind: "new"}).PageID})
	mount, err := app.BrowserCompanionPaneAttach(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := do(BrowserCompanionAction{Kind: "activate", PageID: first, Mount: mount.ID, Select: 1})
	// Number 1 was admitted: each kind sent again as number 1 is passed and
	// does nothing.
	for _, late := range []BrowserCompanionAction{
		{Kind: "activate", PageID: second},
		{Kind: "step", Index: 1},
		{Kind: "new"},
		{Kind: "duplicate", PageID: second},
		{Kind: "reopen"},
		{Kind: "download-open", DownloadID: "none"},
		{Kind: "close-active"},
	} {
		late.Mount, late.Select = mount.ID, 1
		state := do(late)
		if state.ActivePageID != first || len(state.Pages) != len(before.Pages) || state.ClosedTabs != before.ClosedTabs {
			t.Fatalf("a passed %s changed the tabs: %+v", late.Kind, state)
		}
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
