package app

import (
	"context"
	"errors"
	"testing"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/transport"
)

// The pane's "focus-page" hands the keyboard to the presented page, and only
// the host's own window may ask: the native view lives there. With nothing
// presented (the fake engine has no views) it is a no-op answering the
// thread's state.
func TestBrowserCompanionFocusPageIsTheHostWindowsAlone(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-focus")
	if _, err := app.BrowserCompanionDo(t.Context(), thread.ID, BrowserCompanionAction{Kind: "new"}); err != nil {
		t.Fatal(err)
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
		t.Run(tc.name, func(t *testing.T) {
			state, err := app.BrowserCompanionDo(tc.ctx(), thread.ID, BrowserCompanionAction{Kind: "focus-page"})
			if !tc.allow {
				if !errors.Is(err, errBrowserPaneNotHostWindow) {
					t.Fatalf("focus-page = %v; want the host-window refusal", err)
				}
				return
			}
			if err != nil || len(state.Pages) != 1 {
				t.Fatalf("focus-page = %+v, %v; want the thread's state", state, err)
			}
		})
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
