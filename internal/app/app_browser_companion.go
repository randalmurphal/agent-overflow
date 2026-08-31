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

// BrowserCompanionSubscribe attaches the calling connection to the live frame
// stream for a thread. Chrome only screencasts while at least one companion is
// mounted; connection cleanup is the leak-proof fallback for an unclean UI
// disconnect.
func (a *App) BrowserCompanionSubscribe(ctx context.Context, threadID string, width, height int) (appbrowser.CompanionSubscription, error) {
	if a.browser.manager == nil {
		return appbrowser.CompanionSubscription{}, fmt.Errorf("browser manager unavailable")
	}
	access, err := a.browserAccess(threadID)
	if err != nil {
		return appbrowser.CompanionSubscription{}, err
	}
	result, err := a.browser.manager.SubscribeCompanion(access, width, height)
	if err != nil {
		return appbrowser.CompanionSubscription{}, err
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		if !state.RegisterCleanup(func() { a.browser.manager.UnsubscribeCompanion(result.ID) }) {
			a.browser.manager.UnsubscribeCompanion(result.ID)
			return appbrowser.CompanionSubscription{}, fmt.Errorf("browser: connection closing")
		}
	}
	return result, nil
}

func (a *App) BrowserCompanionUnsubscribe(subscriptionID string) error {
	if a.browser.manager != nil {
		a.browser.manager.UnsubscribeCompanion(subscriptionID)
	}
	return nil
}

func (a *App) BrowserCompanionNextFrame(ctx context.Context, subscriptionID string) (appbrowser.CompanionEvent, error) {
	if a.browser.manager == nil {
		return appbrowser.CompanionEvent{}, fmt.Errorf("browser manager unavailable")
	}
	return a.browser.manager.NextCompanionFrame(ctx, subscriptionID)
}

func (a *App) BrowserCompanionResize(subscriptionID string, width, height int) error {
	if a.browser.manager == nil {
		return fmt.Errorf("browser manager unavailable")
	}
	return a.browser.manager.ResizeCompanion(subscriptionID, width, height)
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
	default:
		err = fmt.Errorf("browser: unsupported companion action %q", action.Kind)
	}
	if err != nil {
		return appbrowser.CompanionEvent{}, err
	}
	return a.browser.manager.CompanionState(access), nil
}

func (a *App) BrowserCompanionInput(ctx context.Context, threadID, pageID string, event appbrowser.CompanionInput) error {
	if a.browser.manager == nil {
		return fmt.Errorf("browser manager unavailable")
	}
	access, err := a.browserAccess(threadID)
	if err != nil {
		return err
	}
	return a.browser.manager.CompanionInput(ctx, access, pageID, event)
}
