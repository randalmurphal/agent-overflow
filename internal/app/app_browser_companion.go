package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/transport"
)

type BrowserCompanionAction struct {
	Kind    string `json:"kind"`
	PageID  string `json:"pageId,omitempty"`
	Address string `json:"address,omitempty"`
	// Index is the target tab position for "move" and the relative step for
	// "step" (1 next, -1 previous, wrapping). Not omitempty: index 0 (move to
	// the front) is a real value.
	Index int `json:"index"`
	// Jar is the cookie jar for "jar": worktree, project, thread or private.
	Jar string `json:"jar,omitempty"`
	// Confirmed says the user agreed, for "jar", to lose the live tabs' page
	// state. Unconfirmed, a switch with a live tab is refused with code
	// browser_jar_unconfirmed and changes nothing; the pane asks and sends
	// it again confirmed.
	Confirmed bool `json:"confirmed,omitempty"`
	// Mount and Intent name a page focus intent for "focus-page" and
	// "focus-page-withdraw": the pane mount and its number for the request.
	Mount  string `json:"mount,omitempty"`
	Intent uint64 `json:"intent,omitempty"`
	// Select numbers a user's tab selection made in Mount ("activate",
	// "step", and the "new", "reopen", "duplicate" and "download-open" that
	// select the tab they open) and a keyboard command on the active tab
	// ("close-active", "reload-active"), consecutive from 1 in each Epoch of
	// a mount in the order the user acted. The Manager admits a mount's
	// commands in that order (internal/browser/selection.go). A keyboard
	// command on the active tab requires Mount, Epoch and Select; a
	// selection carries all three or none, and with none applies in arrival
	// order. Validated by parseCompanionAction.
	Select uint64 `json:"select,omitempty"`
	// Epoch is the client's run of numbered commands Select counts in, from
	// 1: the client starts the next one only when every command it sent in
	// this one has answered or failed, and the Manager then refuses what
	// arrives of an older one.
	Epoch uint64 `json:"epoch,omitempty"`
	// DownloadID names one of the thread's downloads (ThreadDownload.ID)
	// for "download-cancel", "download-reveal" and "download-open".
	DownloadID string `json:"downloadId,omitempty"`
}

func (a *App) browserAccess(threadID string) (appbrowser.Access, error) {
	thread, err := a.browserThread(strings.TrimSpace(threadID))
	if err != nil {
		return appbrowser.Access{}, err
	}
	return appbrowser.Access{
		ThreadID: thread.ID, Workspace: thread.WorkspacePath, ProjectRoot: thread.ProjectPath, ProjectID: thread.ProjectID,
	}, nil
}

// BrowserCompanionThreadState answers the thread's current page/session
// snapshot without acquiring anything. The `browser:companion-state` channel
// is ephemeral (no replay), so a freshly loaded UI has no way to know a
// thread already has live pages — this is the hydration read behind the chat
// header's browser chip and the pane-reopen reconcile.
//
//ao:scope host
//ao:route home
func (a *App) BrowserCompanionThreadState(threadID string) (appbrowser.CompanionEvent, error) {
	if a.browser.manager == nil {
		return appbrowser.CompanionEvent{}, fmt.Errorf("browser manager unavailable")
	}
	access, err := a.browserAccess(threadID)
	if err != nil {
		return appbrowser.CompanionEvent{}, err
	}
	return a.browser.manager.CompanionState(access), nil
}

// BrowserCompanionPaneAttach registers the calling connection's mounted pane
// surface for a thread. The native view is presented only while a mount with
// a paintable rect exists, so connection cleanup guarantees a dead UI can
// never leave a browser view painted over a window that no longer renders
// the pane under it.
//
// Only the host's own window may mount one: the native view lives in that
// window, so a pane in another client holding the launch credential, such
// as a browser tab on this machine, would be a mount nothing can be shown
// in. That client shows what a connected browser shows
// (transport.ConnState.HostWindow; the SPA's canUseNativeBrowser).
//
//ao:scope host
//ao:route home
func (a *App) BrowserCompanionPaneAttach(ctx context.Context, threadID string) (appbrowser.CompanionSubscription, error) {
	if a.browser.manager == nil {
		return appbrowser.CompanionSubscription{}, fmt.Errorf("browser manager unavailable")
	}
	if !transport.ConnStateFromContext(ctx).HostWindow() {
		return appbrowser.CompanionSubscription{}, errBrowserPaneNotHostWindow
	}
	access, err := a.browserAccess(threadID)
	if err != nil {
		return appbrowser.CompanionSubscription{}, err
	}
	result, err := a.browser.manager.AttachPane(access)
	if err != nil {
		return appbrowser.CompanionSubscription{}, err
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		if !state.BindCleanup(companionPaneCleanupKey(result.ID), func() { a.browser.manager.DetachPane(result.ID) }) {
			a.browser.manager.DetachPane(result.ID)
			return appbrowser.CompanionSubscription{}, fmt.Errorf("browser: connection closing")
		}
	}
	return result, nil
}

//ao:scope host
//ao:route home
func (a *App) BrowserCompanionPaneDetach(ctx context.Context, paneID string) error {
	transport.ConnStateFromContext(ctx).UnbindCleanup(companionPaneCleanupKey(paneID))
	if a.browser.manager != nil {
		a.browser.manager.DetachPane(paneID)
	}
	return nil
}

func companionPaneCleanupKey(id string) string { return "companion-pane:" + id }

var errBrowserPaneNotHostWindow = errors.New("browser: the browser pane shows only in this computer's app window")

// BrowserCompanionPaneRect reports where the mounted pane's host rect sits,
// coalesced to one call per changed frame by the frontend.
//
//ao:scope host
//ao:route home
func (a *App) BrowserCompanionPaneRect(paneID string, rect appbrowser.PaneRect) error {
	if a.browser.manager == nil {
		return fmt.Errorf("browser manager unavailable")
	}
	return a.browser.manager.SetPaneRect(paneID, rect)
}

// BrowserCompanionRevealPageFile opens the OS file manager with the local
// file a companion page is displaying selected, so it can be dragged into
// chat and mail apps — which accept file drops but not a pasted file object.
// Remote pages have no file to reveal.
//
//ao:scope host
//ao:route home
func (a *App) BrowserCompanionRevealPageFile(ctx context.Context, threadID, pageID string) error {
	if a.browser.manager == nil {
		return fmt.Errorf("browser manager unavailable")
	}
	ctx, end := a.browser.manager.BindThread(ctx, strings.TrimSpace(threadID))
	access, err := a.browserAccess(threadID)
	if err == nil {
		err = a.browser.manager.RevealPageFile(ctx, access, pageID)
	}
	return errors.Join(err, end())
}

// BrowserCompanionRecentAddresses answers the addresses the thread's pages
// recently opened, newest first, for the address bar's suggestions. Read when
// the address field takes focus rather than carried by every state push.
//
//ao:scope host
//ao:route home
func (a *App) BrowserCompanionRecentAddresses(threadID string) ([]appbrowser.RecentAddress, error) {
	if a.browser.manager == nil {
		return nil, fmt.Errorf("browser manager unavailable")
	}
	access, err := a.browserAccess(threadID)
	if err != nil {
		return nil, err
	}
	return a.browser.manager.RecentAddresses(access), nil
}

//ao:scope host
//ao:route home
func (a *App) BrowserCompanionDo(ctx context.Context, threadID string, action BrowserCompanionAction) (appbrowser.CompanionEvent, error) {
	if a.browser.manager == nil {
		return appbrowser.CompanionEvent{}, fmt.Errorf("browser manager unavailable")
	}
	cmd, err := parseCompanionAction(action)
	if err != nil {
		return appbrowser.CompanionEvent{}, err
	}
	// Bound before the row is read: a close of the thread's pages after the
	// read leaves this action nothing to open (BindThread).
	ctx, end := a.browser.manager.BindThread(ctx, strings.TrimSpace(threadID))
	access, err := a.browserAccess(threadID)
	if err != nil {
		return appbrowser.CompanionEvent{}, errors.Join(err, end())
	}
	// newPage is the page a "new" opened, named in the answer so the client
	// that asked can tell its own tab from one an agent opened meanwhile.
	newPage := ""
	if cmd.numbered && cmd.kind != kindCloseActive && cmd.kind != kindReloadActive {
		ctx = appbrowser.WithSelection(ctx, cmd.place)
	}
	m := a.browser.manager
	switch cmd.kind {
	case kindNavigate:
		_, err = m.NavigateCompanion(ctx, access, cmd.page, cmd.address)
	case kindBack:
		_, err = m.History(ctx, access, cmd.page, "back")
	case kindForward:
		_, err = m.History(ctx, access, cmd.page, "forward")
	case kindReload:
		// A suspended or crashed tab is restored at its address.
		err = m.ReloadCompanionPage(ctx, access, cmd.page)
	case kindReloadActive:
		// The keyboard's reload: the tab active at its turn in the user's
		// order, which the Manager resolves.
		var outcome appbrowser.ActiveOutcome
		outcome, err = m.ReloadActiveCompanionPage(ctx, access, cmd.place)
		err = errors.Join(err, activeOutcomeError(outcome))
	case kindStop:
		err = m.StopCompanionLoad(ctx, access, cmd.page)
	case kindActivate:
		err = m.ActivateCompanionPage(ctx, access, cmd.page)
	case kindRestore:
		// The pane's own request for the suspended tab it shows.
		err = m.RestoreActiveCompanionPage(ctx, access, cmd.page)
	case kindStep:
		err = m.StepCompanionPage(ctx, access, cmd.index)
	case kindMove:
		err = m.MoveCompanionPage(access, cmd.page, cmd.index)
	case kindShow:
		visible := true
		_, err = m.Visibility(ctx, access, &visible, cmd.page)
	case kindHide:
		visible := false
		_, err = m.Visibility(ctx, access, &visible, "")
	case kindNew:
		// Address, when set, is navigated in the new tab (a mod-clicked link).
		var page appbrowser.PageInfo
		page, err = m.NewCompanionPage(ctx, access, cmd.address)
		newPage = page.ID
	case kindClose:
		// Answers once the tab is gone; a teardown failure after it is a
		// "teardown" event.
		err = m.CloseCompanionPage(ctx, access, cmd.page)
	case kindCloseActive:
		// The keyboard's close: the tab active at its turn in the user's
		// order, which the Manager resolves.
		var outcome appbrowser.ActiveOutcome
		outcome, err = m.CloseActiveCompanionPage(ctx, access, cmd.place)
		err = errors.Join(err, activeOutcomeError(outcome))
	case kindCloseOthers:
		err = m.CloseCompanionTabs(ctx, access, cmd.page, appbrowser.CloseOtherTabs)
	case kindCloseRight:
		err = m.CloseCompanionTabs(ctx, access, cmd.page, appbrowser.CloseTabsRight)
	case kindReopen:
		// The most recently closed tab, at its place; nothing with none.
		err = m.ReopenClosedTab(ctx, access)
	case kindDuplicate:
		err = m.DuplicateCompanionPage(ctx, access, cmd.page)
	case kindDevtools:
		err = m.OpenPaneDevTools(ctx, access, cmd.page)
	case kindJar:
		// Closes the live tabs and reopens their addresses in the new jar;
		// unconfirmed, it is refused while a tab is live.
		err = jarSwitchError(m.SetJar(ctx, access, cmd.jar, cmd.consent))
	case kindDownloadCancel:
		err = m.CancelThreadDownload(access, cmd.download)
	case kindDownloadReveal:
		err = m.RevealThreadDownload(ctx, access, cmd.download)
	case kindDownloadOpen:
		// A new tab showing the finished file.
		var page appbrowser.PageInfo
		page, err = m.OpenThreadDownload(ctx, access, cmd.download)
		newPage = page.ID
	case kindDownloadsClear:
		m.ClearThreadDownloads(access)
	case kindFocusPage, kindFocusPageWithdraw:
		// Keyboard focus moves within the window the native view lives in,
		// so only that window may ask for it or take the request back.
		switch {
		case !transport.ConnStateFromContext(ctx).HostWindow():
			err = errBrowserPaneNotHostWindow
		case cmd.kind == kindFocusPage:
			err = m.FocusCompanionPage(access, cmd.mount, cmd.page, cmd.intent)
		default:
			err = m.WithdrawCompanionPageFocus(access, cmd.mount, cmd.intent)
		}
	default:
		err = fmt.Errorf("%w: kind %d", errNoCompanionHandler, cmd.kind)
	}
	if endErr := end(); endErr != nil {
		return appbrowser.CompanionEvent{}, errors.Join(err, endErr)
	}
	if err != nil {
		return appbrowser.CompanionEvent{}, err
	}
	state := a.browser.manager.CompanionState(access)
	state.PageID = newPage
	return state, nil
}

// BrowserFindAction is one request of the browser pane's find: start (the
// query; empty ends the search, or opens an engine's own find bar empty),
// next, previous or stop. Mount is the pane mount the find belongs to and
// Seq the provider's sequence: a start's own, increasing with every start
// the provider makes; the one a step steps; for a stop, the highest the
// provider made, and the stop ends the mount's search at or below it. A step
// or stop for a search another owner holds changes nothing.
type BrowserFindAction struct {
	Kind   string `json:"kind"`
	PageID string `json:"pageId"`
	Query  string `json:"query,omitempty"`
	Mount  string `json:"mount"`
	Seq    uint64 `json:"seq"`
}

// BrowserCompanionFind runs one find action on the page the thread's browser
// pane shows, through the engine's own find-in-page. The answer is the
// session it leaves; an end the backend decides on its own (navigation, a
// tab switch, the page closing) arrives as a `find` companion event.
//
//ao:scope host
//ao:route home
func (a *App) BrowserCompanionFind(ctx context.Context, threadID string, action BrowserFindAction) (appbrowser.FindState, error) {
	if a.browser.manager == nil {
		return appbrowser.FindState{}, fmt.Errorf("browser manager unavailable")
	}
	ctx, end := a.browser.manager.BindThread(ctx, strings.TrimSpace(threadID))
	access, err := a.browserAccess(threadID)
	if err != nil {
		return appbrowser.FindState{}, errors.Join(err, end())
	}
	state, err := a.browser.manager.Find(ctx, access, action.PageID, action.Kind, action.Query, action.Mount, action.Seq)
	if endErr := end(); endErr != nil {
		return appbrowser.FindState{}, errors.Join(err, endErr)
	}
	return state, err
}

// browserThread reads a thread that can hold browser pages. A deleted thread
// kept as the holder its forks read reads as missing.
func (a *App) browserThread(threadID string) (store.Thread, error) {
	thread, err := a.store.GetThread(threadID)
	if err == nil && thread.Mode == threadmode.ModeHolder {
		return store.Thread{}, fmt.Errorf("browser: thread %s was deleted: %w", threadID, sql.ErrNoRows)
	}
	return thread, err
}
