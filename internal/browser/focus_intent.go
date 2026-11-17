package browser

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"agent-overflow/internal/webview2host"
)

// A focus intent is the user's request, by the address bar's Enter or
// Escape, that one page take the keyboard. It is transient: the user's next
// focus choice (another element focused, a press outside the page, a tab
// switch, the pane unmounting) withdraws it, and a withdrawn intent never
// moves focus. Being presented, visible or in the foreground makes a page
// eligible; none of them is permission to override that newer choice.
//
// The pane names an intent by its mount and a number that increases per
// mount. Arm and withdraw are separate calls that can arrive in either
// order, so a mount keeps the highest number it has heard of: an arm at or
// below it is already withdrawn or superseded.
//
// The engine applies an intent only while its page is the one the mount
// presents, and checks Live again on its own UI thread immediately before
// moving focus (paneHost.FocusPage); the launcher-hosted engine checks in the
// launcher, which hears withdrawals straight from the SPA
// (webview2host.FocusIntents). Nothing undoes a focus that landed: a
// withdrawal that reaches the application point after the move changes
// nothing, since by then the page, or a newer focus of it, holds the keyboard
// legitimately as far as the engine can tell.

// focusIntentMaxAge bounds how long an intent waits for its page to be
// presented (a restore, a layout).
const focusIntentMaxAge = webview2host.FocusIntentMaxAge

// focusRequest is one intent as an engine applies it. Live answers whether
// it still stands; an engine asks it on its UI thread, immediately before it
// moves focus, so it must not wait on anything.
type focusRequest struct {
	Mount  string
	Intent uint64
	Live   func() bool
}

// focusResult is what an engine did with a focusRequest.
type focusResult int

const (
	// focusMoved: the page's view took keyboard focus.
	focusMoved focusResult = iota
	// focusDispatched: the request went to the host process, which decides
	// and reports (the launcher-hosted engine).
	focusDispatched
	// focusNotPresented: the host does not show the page; focus is unchanged.
	focusNotPresented
	// focusWithdrawn: Live answered false; focus is unchanged.
	focusWithdrawn
)

// pageFocusIntent is one mount's current intent.
type pageFocusIntent struct {
	thread, mount, pageID string
	id                    uint64
	armed                 time.Time
	withdrawn             atomic.Bool
	// claimed is set, under Manager.mu, once the intent is handed to the
	// engine, so it is applied at most once at a time.
	claimed bool
}

// mountFocus is a mount's focus intent state. Guarded by Manager.mu.
type mountFocus struct {
	// latest is the highest intent number armed or withdrawn on the mount.
	latest uint64
	intent *pageFocusIntent
}

// mountFocusLocked answers a mounted pane's focus state, nil for a mount the
// thread does not hold. The caller holds m.mu.
func (m *Manager) mountFocusLocked(threadID, mount string) *mountFocus {
	pane, ok := m.panes[mount]
	if !ok || pane.threadID != threadID {
		return nil
	}
	if pane.focus == nil {
		pane.focus = &mountFocus{}
		m.panes[mount] = pane
	}
	return pane.focus
}

// FocusCompanionPage arms the pane mount's focus intent number intent for
// pageID and applies it if the page is presented there now; otherwise it
// waits until the page is (presentPanes), up to focusIntentMaxAge. An intent
// already withdrawn or superseded, a page that is gone, or an engine with no
// native views does nothing. The outcome of a focus attempt arrives as a
// Kind "focus" event (emitPageFocus).
func (m *Manager) FocusCompanionPage(access Access, mount, pageID string, intent uint64) error {
	mount, pageID = strings.TrimSpace(mount), strings.TrimSpace(pageID)
	if mount == "" || pageID == "" || intent == 0 {
		return errors.New("browser: a page focus needs its pane mount, page and intent")
	}
	if _, ok := m.engine.(paneHost); !ok {
		return nil
	}
	m.mu.Lock()
	focus := m.mountFocusLocked(access.ThreadID, mount)
	if focus == nil {
		m.mu.Unlock()
		return errors.New("browser: pane mount not found")
	}
	if intent <= focus.latest {
		m.mu.Unlock()
		return nil
	}
	focus.latest = intent
	if focus.intent != nil {
		focus.intent.withdrawn.Store(true)
	}
	focus.intent = &pageFocusIntent{thread: access.ThreadID, mount: mount, pageID: pageID, id: intent, armed: time.Now()}
	m.mu.Unlock()
	m.applyFocusIntent(access.ThreadID)
	return nil
}

// WithdrawCompanionPageFocus withdraws the mount's focus intents numbered
// up to intent: the user made another focus choice. A mount that is gone has
// nothing to withdraw.
func (m *Manager) WithdrawCompanionPageFocus(access Access, mount string, intent uint64) error {
	mount = strings.TrimSpace(mount)
	if mount == "" || intent == 0 {
		return errors.New("browser: a focus withdrawal needs its pane mount and intent")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	focus := m.mountFocusLocked(access.ThreadID, mount)
	if focus == nil {
		return nil
	}
	focus.latest = max(focus.latest, intent)
	if focus.intent != nil && focus.intent.id <= intent {
		focus.intent.withdrawn.Store(true)
		focus.intent = nil
	}
	return nil
}

// dropFocusIntentLocked ends a mount's intent if it is still in. The caller
// holds m.mu.
func (m *Manager) dropFocusIntentLocked(in *pageFocusIntent) {
	if pane, ok := m.panes[in.mount]; ok && pane.focus != nil && pane.focus.intent == in {
		pane.focus.intent = nil
	}
}

// applyFocusIntent applies the focus intent of the mount presenting the
// thread's pages, when its page is the one presented. The presentation runs
// it after every pass, so an intent armed while its page was suspended,
// restoring or held for its layout is applied once the page shows. An intent
// that was withdrawn, outlived focusIntentMaxAge, or whose tab closed is
// dropped.
func (m *Manager) applyFocusIntent(threadID string) {
	host, ok := m.engine.(paneHost)
	if !ok {
		return
	}
	m.mu.Lock()
	mount, _, _ := m.presenterLocked(threadID)
	pane := m.panes[mount]
	if pane.focus == nil || pane.focus.intent == nil || pane.focus.intent.claimed {
		m.mu.Unlock()
		return
	}
	in := pane.focus.intent
	if in.withdrawn.Load() || time.Since(in.armed) > focusIntentMaxAge || !m.hasTabLocked(threadID, in.pageID) {
		pane.focus.intent = nil
		m.mu.Unlock()
		return
	}
	var page *managedPage
	// Presented and still selected, as a pass's show requires: a page shown
	// by a pass that a newer selection overtook is hidden by the next pass,
	// which applies the intent if its page is shown then.
	if m.presented[threadID].id == in.pageID && m.sessions[threadID].ActivePageID == in.pageID {
		// A closing page has left its scope's pages already.
		if p, _ := m.livePageLocked(in.pageID); p != nil && p.owner == threadID {
			page = p
		}
	}
	if page == nil {
		m.mu.Unlock()
		return
	}
	in.claimed = true
	handle := page.driver.Handle()
	m.mu.Unlock()

	result, err := host.FocusPage(handle, focusRequest{
		Mount:  in.mount,
		Intent: in.id,
		Live:   func() bool { return !in.withdrawn.Load() },
	})
	switch {
	case err != nil:
		// A UI-thread dispatch that missed its deadline still runs: it
		// must find the intent over.
		in.withdrawn.Store(true)
		m.mu.Lock()
		m.dropFocusIntentLocked(in)
		m.mu.Unlock()
		m.emitPageFocus(threadID, in.mount, in.id, fmt.Errorf("browser: focus page %s: %w", in.pageID, err))
	case result == focusMoved:
		// The intent stays, claimed, until the pane retires it: a
		// withdrawal sent before this landed hands the focus back.
		m.emitPageFocus(threadID, in.mount, in.id, nil)
	case result == focusNotPresented:
		// A hide raced the request. The intent waits for the page's next
		// presentation, whose pass applies it.
		m.mu.Lock()
		in.claimed = false
		m.mu.Unlock()
	}
}

// hasTabLocked is hasTab for a caller holding m.mu.
func (m *Manager) hasTabLocked(threadID, pageID string) bool {
	for _, tab := range m.companionTabsLocked(threadID) {
		if tab.id == pageID {
			return true
		}
	}
	return false
}

// emitPageFocus tells the mount's pane how its focus intent ended: moved
// into the page (err nil) or refused. The pane shows a refusal only while it
// is the newest thing the user asked of the browser.
func (m *Manager) emitPageFocus(threadID, mount string, intent uint64, err error) {
	event := CompanionEvent{Kind: "focus", ThreadID: threadID, Mount: mount, FocusIntent: intent}
	if err != nil {
		event.Error = err.Error()
	}
	m.emit(event)
}

// pageFocusReported is engineEvents.PageFocusReported: the launcher's answer
// to a focus it was asked for after FocusPage answered. It reaches the pane
// mount that armed the intent, as a Kind "focus" event.
func (m *Manager) pageFocusReported(handle, mount string, intent uint64, err error) {
	m.mu.Lock()
	page := m.pageByHandleLocked(handle)
	owner := ""
	if page != nil && m.panes[mount].threadID == page.owner {
		owner = page.owner
	}
	m.mu.Unlock()
	if owner == "" {
		log.Printf("browser: focus report for a page or mount that is gone (%s, %s): %v", handle, mount, err)
		return
	}
	m.emitPageFocus(owner, mount, intent, err)
}
