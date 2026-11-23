package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/errorsx"
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
	pane := attachPane(t, app, thread.ID)
	state, err = app.BrowserCompanionDo(ctx, thread.ID, pane.next(BrowserCompanionAction{Kind: "close", PageID: first}))
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
	pane := attachPane(t, app, thread.ID)
	if got := urls(do(BrowserCompanionAction{Kind: "duplicate", PageID: first})); got != "a a b" {
		t.Fatalf("after duplicate: %s", got)
	}
	if got := urls(do(pane.next(BrowserCompanionAction{Kind: "close-right", PageID: first}))); got != "a" {
		t.Fatalf("after close to the right: %s", got)
	}
	state := do(BrowserCompanionAction{Kind: "reopen"})
	if got := urls(state); got != "a b" || state.ClosedTabs != 1 {
		t.Fatalf("after reopen: %s, %d closed", got, state.ClosedTabs)
	}
	if got := urls(do(pane.next(BrowserCompanionAction{Kind: "close-others", PageID: first}))); got != "a" {
		t.Fatalf("after close others: %s", got)
	}
	if got := urls(do(BrowserCompanionAction{Kind: "reload", PageID: first})); got != "a" {
		t.Fatalf("after reload: %s", got)
	}
}

// The decoder accepts an action only with the fields its kind requires: a
// named command names its tab, a close or a move carries its pane's mount,
// epoch and number, a selection all three or none, a jar switch its pane's mount
// alone, and nothing else is numbered.
func TestParseCompanionActionRequiresEachKindsFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action BrowserCompanionAction
		ok     bool
	}{
		{BrowserCompanionAction{Kind: "close", PageID: "p", Mount: "m", Select: 1, Epoch: 1}, true},
		{BrowserCompanionAction{Kind: "close", PageID: "p"}, false},
		{BrowserCompanionAction{Kind: "close", PageID: "p", Mount: "m"}, false},
		{BrowserCompanionAction{Kind: "close", PageID: "p", Mount: "m", Select: 1}, false},
		{BrowserCompanionAction{Kind: "close", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "close", PageID: "p", Mount: "m", Select: 1, Epoch: 1, Intent: 1}, false},
		{BrowserCompanionAction{Kind: "close-others", PageID: "p", Mount: "m", Select: 2, Epoch: 1}, true},
		{BrowserCompanionAction{Kind: "close-right", PageID: "p"}, false},
		{BrowserCompanionAction{Kind: "move", PageID: "p", Beside: "q", Mount: "m", Select: 1, Epoch: 1}, true},
		{BrowserCompanionAction{Kind: "move", PageID: "p", Beside: "q", After: true, Mount: "m", Select: 1, Epoch: 1}, true},
		{BrowserCompanionAction{Kind: "move", PageID: "p", Beside: "q"}, false},
		{BrowserCompanionAction{Kind: "move", Beside: "q", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "move", PageID: "p", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "move", PageID: "p", Beside: "p", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "close", PageID: "p", Beside: "q", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "close", PageID: "p", After: true, Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "reload", PageID: "p"}, true},
		{BrowserCompanionAction{Kind: "reload"}, false},
		{BrowserCompanionAction{Kind: "reload", PageID: "p", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "close-active", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "reload-active", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "navigate", PageID: "p", Address: "https://example.test/"}, true},
		{BrowserCompanionAction{Kind: "navigate", Address: "https://example.test/"}, false},
		{BrowserCompanionAction{Kind: "activate", PageID: "p", Mount: "m", Select: 2}, false},
		{BrowserCompanionAction{Kind: "close", PageID: "p", Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "focus-page", PageID: "p", Mount: "m", Intent: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "activate", PageID: "p"}, true},
		{BrowserCompanionAction{Kind: "activate", PageID: "p", Mount: "m", Select: 2, Epoch: 1}, true},
		{BrowserCompanionAction{Kind: "activate", PageID: "p", Select: 2}, false},
		{BrowserCompanionAction{Kind: "activate", PageID: "p", Mount: "m"}, false},
		{BrowserCompanionAction{Kind: "new"}, true},
		{BrowserCompanionAction{Kind: "new", PageID: "p"}, false},
		{BrowserCompanionAction{Kind: "show"}, true},
		{BrowserCompanionAction{Kind: "show", PageID: "p"}, true},
		{BrowserCompanionAction{Kind: "show-browser"}, true},
		{BrowserCompanionAction{Kind: "show-browser", Mount: "m", Select: 1, Epoch: 1}, true},
		{BrowserCompanionAction{Kind: "show-browser", Mount: "m", Select: 1}, false},
		{BrowserCompanionAction{Kind: "show-browser", PageID: "p"}, false},
		{BrowserCompanionAction{Kind: "show-browser", Mount: "m"}, false},
		{BrowserCompanionAction{Kind: "download-open", DownloadID: "d", Mount: "m", Select: 1, Epoch: 1}, true},
		{BrowserCompanionAction{Kind: "download-cancel"}, false},
		{BrowserCompanionAction{Kind: "jar", Mount: "m"}, false},
		{BrowserCompanionAction{Kind: "jar", Jar: "thread", Mount: "m", Confirmed: true}, true},
		{BrowserCompanionAction{Kind: "jar", Jar: "thread", Confirmed: true}, false},
		{BrowserCompanionAction{Kind: "jar", Jar: "thread", Mount: "m", Select: 1, Epoch: 1}, false},
		{BrowserCompanionAction{Kind: "close", PageID: "p", Mount: "m", Select: 1, Epoch: 1, Confirmed: true}, false},
		{BrowserCompanionAction{Kind: "focus-page", PageID: "p", Mount: "m", Intent: 1}, true},
		{BrowserCompanionAction{Kind: "focus-page-withdraw", Mount: "m", Intent: 1}, true},
		{BrowserCompanionAction{Kind: "focus-page", PageID: "p", Mount: "m"}, false},
		{BrowserCompanionAction{Kind: "Close", PageID: "p", Mount: "m", Select: 1, Epoch: 1}, true},
		{BrowserCompanionAction{Kind: "shut"}, false},
	} {
		_, err := parseCompanionAction(tc.action)
		if (err == nil) != tc.ok {
			t.Errorf("%+v: accepted %v, want %v (%v)", tc.action, err == nil, tc.ok, err)
		}
	}
}

// A jar switch carries the consent the wire's confirmed names, and every
// other kind none.
func TestParseCompanionActionTypesTheJarConsent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action BrowserCompanionAction
		want   appbrowser.JarConsent
	}{
		{BrowserCompanionAction{Kind: "jar", Jar: "thread", Mount: "m"}, appbrowser.JarSwitchIfNoLiveTab},
		{BrowserCompanionAction{Kind: "jar", Jar: "thread", Mount: "m", Confirmed: true}, appbrowser.JarSwitchConfirmed},
		{BrowserCompanionAction{Kind: "new"}, appbrowser.JarConsent(0)},
	} {
		cmd, err := parseCompanionAction(tc.action)
		if err != nil {
			t.Fatalf("%+v: %v", tc.action, err)
		}
		if cmd.consent != tc.want {
			t.Errorf("%+v: consent %d, want %d", tc.action, cmd.consent, tc.want)
		}
	}
}

// Every spec'd kind has a handler: none reaches BrowserCompanionDo's
// default.
func TestEveryCompanionKindHasAHandler(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-kinds")
	for name, spec := range companionSpecs {
		action := BrowserCompanionAction{Kind: name}
		if spec.target == targetNamed || spec.target == targetOptional {
			action.PageID = "none"
		}
		switch spec.place {
		case placeNumbered:
			action.Mount, action.Epoch, action.Select = "none", 1, 1
		case placeMount:
			action.Mount = "none"
		case placeFocus:
			action.Mount, action.Intent = "none", 1
		case placeNone, placeIfFromPane:
		}
		if spec.download {
			action.DownloadID = "none"
		}
		if spec.kind == kindJar {
			action.Jar = "none"
		}
		if spec.kind == kindMove {
			action.Beside = "other"
		}
		if _, err := parseCompanionAction(action); err != nil {
			t.Fatalf("%s: the probe does not parse: %v", name, err)
		}
		_, err := app.BrowserCompanionDo(t.Context(), thread.ID, action)
		if errors.Is(err, errNoCompanionHandler) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A close carries its pane's mount and number and the tab it names: the
// Manager closes that tab, not the active one, and a second close of it
// closes nothing more.
func TestBrowserCompanionCloseClosesTheTabItNames(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-targets")
	ctx := t.Context()
	first, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new", Address: "https://example.test/a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new", Address: "https://example.test/b"})
	if err != nil {
		t.Fatal(err)
	}
	pane := attachPane(t, app, thread.ID)
	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "close", PageID: first.PageID}); err == nil {
		t.Fatal("a close with no pane number was accepted")
	}
	for range 2 {
		state, err := app.BrowserCompanionDo(ctx, thread.ID, pane.next(BrowserCompanionAction{Kind: "close", PageID: first.PageID}))
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Pages) != 1 || state.Pages[0].ID != second.PageID {
			t.Fatalf("after closing a: %+v, want b alone", state.Pages)
		}
	}
}

// paneNumbers numbers one attached pane mount's commands as the window
// does, consecutive from 1 in epoch 1.
type paneNumbers struct {
	mount string
	seq   uint64
}

// attachPane attaches a pane to the thread and answers its numbering.
func attachPane(t *testing.T, app *App, threadID string) *paneNumbers {
	t.Helper()
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{HostWindow: true})
	mount, err := app.BrowserCompanionPaneAttach(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	return &paneNumbers{mount: mount.ID}
}

// pageIDs is the thread's tabs in strip order.
func pageIDs(state appbrowser.CompanionEvent) []string {
	ids := make([]string, 0, len(state.Pages))
	for _, page := range state.Pages {
		ids = append(ids, page.ID)
	}
	return ids
}

// next is action as the pane's next numbered command.
func (p *paneNumbers) next(action BrowserCompanionAction) BrowserCompanionAction {
	p.seq++
	action.Mount, action.Epoch, action.Select = p.mount, 1, p.seq
	return action
}

// Every kind that selects, closes or moves a tab carries its mount and number to
// the Manager, which admits each number once: a number
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
	third := do(BrowserCompanionAction{Kind: "new"}).PageID
	pane := attachPane(t, app, thread.ID)
	do(pane.next(BrowserCompanionAction{Kind: "close", PageID: third}))
	before := do(pane.next(BrowserCompanionAction{Kind: "activate", PageID: first}))
	// Number 2 was admitted: each kind sent again as number 2 is passed and
	// does nothing.
	for _, late := range []BrowserCompanionAction{
		{Kind: "activate", PageID: second},
		{Kind: "step", Index: 1},
		{Kind: "new"},
		{Kind: "duplicate", PageID: second},
		{Kind: "reopen"},
		{Kind: "download-open", DownloadID: "none"},
		{Kind: "close", PageID: second},
		{Kind: "close-others", PageID: second},
		{Kind: "close-right", PageID: first},
		{Kind: "move", PageID: first, Beside: second, After: true},
		{Kind: "show-browser"},
	} {
		late.Mount, late.Epoch, late.Select = pane.mount, 1, 2
		state := do(late)
		if state.ActivePageID != first || !slices.Equal(pageIDs(state), pageIDs(before)) || state.ClosedTabs != before.ClosedTabs {
			t.Fatalf("a passed %s changed the tabs: %+v", late.Kind, state)
		}
	}
}

// The chat header's show names no tab: on a thread with none it opens one,
// shown and named in the answer, and again it shows that tab and names
// none.
func TestBrowserCompanionShowBrowserOpensOneTabAndNamesIt(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-show")
	ctx := t.Context()
	opened, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "show-browser"})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Pages) != 1 || opened.PageID != opened.Pages[0].ID || opened.ActivePageID != opened.PageID || opened.Visible == nil || !*opened.Visible {
		t.Fatalf("the first show answered %+v, want one tab, named, active and shown", opened)
	}
	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "hide"}); err != nil {
		t.Fatal(err)
	}
	again, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "show-browser"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pages) != 1 || again.PageID != "" || again.ActivePageID != opened.PageID || again.Visible == nil || !*again.Visible {
		t.Fatalf("the second show answered %+v, want the same tab shown and none named", again)
	}
}

// An action on a download the thread's list no longer has (a toast's,
// after the list was cleared) answers the code the window shows as a
// notice.
func TestBrowserCompanionDownloadNoLongerListedIsCoded(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-download-gone")
	for _, kind := range []string{"download-reveal", "download-open", "download-cancel"} {
		_, err := app.BrowserCompanionDo(t.Context(), thread.ID, BrowserCompanionAction{Kind: kind, DownloadID: "d-cleared"})
		if code, _, _ := errorsx.PublicDetails(err); code != browserDownloadUnavailableCode {
			t.Errorf("%s of a download no longer listed answered %v, want code %s", kind, err, browserDownloadUnavailableCode)
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
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{HostWindow: true})

	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new"}); err != nil {
		t.Fatal(err)
	}
	mount, err := app.BrowserCompanionPaneAttach(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The tab is live: an unconfirmed switch is refused with the code the
	// pane asks on, and the confirmed one goes ahead.
	_, err = app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "jar", Jar: appbrowser.JarProject, Mount: mount.ID})
	if code, _, _ := errorsx.PublicDetails(err); code != browserJarUnconfirmedCode {
		t.Fatalf("an unconfirmed switch with a live tab answered %v, want code %s", err, browserJarUnconfirmedCode)
	}
	access, err := app.bindBrowser(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.CompanionState(access.Caller()).Jar; got == appbrowser.JarProject {
		t.Fatal("the refused switch changed the jar")
	}
	state, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "jar", Jar: appbrowser.JarProject, Mount: mount.ID, Confirmed: true})
	if err != nil {
		t.Fatalf("choose the project jar: %v", err)
	}
	if state.Jar != appbrowser.JarProject || len(state.Pages) != 1 {
		t.Fatalf("after choosing the project jar: %+v", state)
	}
	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "jar", Jar: "shared", Mount: mount.ID}); err == nil {
		t.Fatal("an unknown jar was accepted")
	}
}

// A confirmed jar switch held on its way while the thread's browser was
// retired and opened again on a fresh tab switches nothing: its consent was
// given in a pane of the retired browser, and the fresh tab stays live in
// the jar it opened in. One from a pane that detached is refused too.
func TestBrowserCompanionJarSwitchFromARetiredPaneChangesNothing(t *testing.T) {
	t.Parallel()
	app := newTestAppWithStore(t)
	manager := appbrowser.NewManager(t.TempDir(), appbrowser.Config{Enabled: true}, appbrowser.ManagerOptions{FakeEngine: true})
	app.browser.manager = manager
	t.Cleanup(func() { _ = manager.Close() })
	thread := makeWorkspaceThread(t, app, "thread-companion-jar-retired")
	ctx, _ := newTestConnection(t, transport.ConnPrincipal{HostWindow: true})
	if _, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new"}); err != nil {
		t.Fatal(err)
	}
	mount, err := app.BrowserCompanionPaneAttach(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseThread(ctx, appbrowser.ThreadID(thread.ID)); err != nil {
		t.Fatal(err)
	}
	fresh, err := app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "new"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "jar", Jar: appbrowser.JarThread, Mount: mount.ID, Confirmed: true})
	if code, _, _ := errorsx.PublicDetails(err); code != browserJarPaneGoneCode {
		t.Fatalf("the retired pane's switch answered %v, want code %s", err, browserJarPaneGoneCode)
	}
	state, err := app.BrowserCompanionThreadState(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Jar == appbrowser.JarThread || len(state.Pages) != 1 || state.Pages[0].ID != fresh.PageID || state.Pages[0].Suspended {
		t.Fatalf("after the retired pane's switch: jar %s, tabs %+v; want the fresh tab live in its jar", state.Jar, state.Pages)
	}

	next, err := app.BrowserCompanionPaneAttach(ctx, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.BrowserCompanionPaneDetach(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	_, err = app.BrowserCompanionDo(ctx, thread.ID, BrowserCompanionAction{Kind: "jar", Jar: appbrowser.JarThread, Mount: next.ID, Confirmed: true})
	if code, _, _ := errorsx.PublicDetails(err); code != browserJarPaneGoneCode {
		t.Fatalf("the detached pane's switch answered %v, want code %s", err, browserJarPaneGoneCode)
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
