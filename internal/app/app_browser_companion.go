package app

import (
	"context"
	"fmt"
	"strings"

	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/transport"
)

type BrowserCompanionAction struct {
	Kind    string `json:"kind"`
	PageID  string `json:"pageId,omitempty"`
	Address string `json:"address,omitempty"`
}

func (a *App) browserAccess(threadID string) (appbrowser.Access, error) {
	thread, err := a.store.GetThread(strings.TrimSpace(threadID))
	if err != nil {
		return appbrowser.Access{}, err
	}
	return appbrowser.Access{
		ThreadID: thread.ID, Workspace: thread.WorkspacePath, ProjectRoot: thread.ProjectPath,
	}, nil
}

// BrowserCompanionThreadState answers the thread's current page/session
// snapshot without acquiring anything. The `browser:companion-state` channel
// is ephemeral (no replay), so a freshly loaded UI has no way to know a
// thread already has live pages — this is the hydration read behind the chat
// header's browser chip and the pane-reopen reconcile.
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
func (a *App) BrowserCompanionPaneAttach(ctx context.Context, threadID string) (appbrowser.CompanionSubscription, error) {
	if a.browser.manager == nil {
		return appbrowser.CompanionSubscription{}, fmt.Errorf("browser manager unavailable")
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
		if !state.RegisterCleanup(func() { a.browser.manager.DetachPane(result.ID) }) {
			a.browser.manager.DetachPane(result.ID)
			return appbrowser.CompanionSubscription{}, fmt.Errorf("browser: connection closing")
		}
	}
	return result, nil
}

func (a *App) BrowserCompanionPaneDetach(paneID string) error {
	if a.browser.manager != nil {
		a.browser.manager.DetachPane(paneID)
	}
	return nil
}

// BrowserCompanionPaneRect reports where the mounted pane's host rect sits,
// coalesced to one call per changed frame by the frontend.
func (a *App) BrowserCompanionPaneRect(paneID string, rect appbrowser.PaneRect) error {
	if a.browser.manager == nil {
		return fmt.Errorf("browser manager unavailable")
	}
	return a.browser.manager.SetPaneRect(paneID, rect)
}

// BrowserCompanionCopyPageFile puts the local file a companion page is
// displaying onto the OS clipboard as a file object, so pasting it into a chat
// or mail client attaches the file itself. Remote pages have no file to copy.
func (a *App) BrowserCompanionCopyPageFile(ctx context.Context, threadID, pageID string) error {
	if a.browser.manager == nil {
		return fmt.Errorf("browser manager unavailable")
	}
	access, err := a.browserAccess(threadID)
	if err != nil {
		return err
	}
	return a.browser.manager.CopyPageFileToClipboard(ctx, access, pageID)
}

func (a *App) BrowserCompanionDo(ctx context.Context, threadID string, action BrowserCompanionAction) (appbrowser.CompanionEvent, error) {
	if a.browser.manager == nil {
		return appbrowser.CompanionEvent{}, fmt.Errorf("browser manager unavailable")
	}
	access, err := a.browserAccess(threadID)
	if err != nil {
		return appbrowser.CompanionEvent{}, err
	}
	switch strings.ToLower(strings.TrimSpace(action.Kind)) {
	case "navigate":
		_, err = a.browser.manager.NavigateCompanion(ctx, access, action.PageID, action.Address)
	case "back", "forward", "reload", "stop":
		_, err = a.browser.manager.History(ctx, access, action.PageID, action.Kind)
	case "activate":
		err = a.browser.manager.ActivateCompanionPage(access, action.PageID)
	case "show":
		visible := true
		_, err = a.browser.manager.Visibility(ctx, access, &visible, action.PageID)
	case "hide":
		visible := false
		_, err = a.browser.manager.Visibility(ctx, access, &visible, "")
	case "new":
		_, err = a.browser.manager.NewCompanionPage(ctx, access)
	case "close":
		err = a.browser.manager.ClosePage(ctx, access, action.PageID)
	case "devtools":
		err = a.browser.manager.OpenPaneDevTools(ctx, access, action.PageID)
	default:
		err = fmt.Errorf("browser: unsupported companion action %q", action.Kind)
	}
	if err != nil {
		return appbrowser.CompanionEvent{}, err
	}
	return a.browser.manager.CompanionState(access), nil
}
