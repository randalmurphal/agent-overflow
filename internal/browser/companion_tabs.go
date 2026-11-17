package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The tab strip's actions on whole tabs: close one or several, reopen the
// last closed, duplicate, reload.
//
// The Manager keeps, per thread and in memory only, the last maxClosedTabs
// tabs the user closed (address, label, position). A tab an agent closes is
// not kept, nor one with no address to reopen, nor any tab of a private jar.
// Clearing site data, switching the thread's jar, turning the browser off,
// and deleting or archiving the thread forget them.

// maxClosedTabs bounds a thread's reopenable tabs.
const maxClosedTabs = 10

// closedTab is a tab the user closed, as it can be reopened.
type closedTab struct {
	url, label string
	// index is the tab's position when it closed.
	index int
}

// reopenable reports whether a closed tab at rawURL is worth keeping: an
// address a new tab can load through the address bar.
func reopenable(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "http", "https", "file":
		return true
	}
	return false
}

// CloseCompanionPage is the user closing a tab: it returns once the tab is
// gone from the state, and a failure of the teardown after it arrives as a
// "teardown" event. The tab can be reopened (ReopenClosedTab).
func (m *Manager) CloseCompanionPage(ctx context.Context, access Access, pageID string) error {
	return m.closePage(ctx, access, pageID, false, true)
}

// recordUserCloseLocked keeps the thread's tab pageID as the user's most
// recent close. The close that takes the tab out of the tab set calls it in
// that same m.mu hold, so only the close that removed the tab records it,
// and the state that close pushes counts it. The caller holds m.mu.
func (m *Manager) recordUserCloseLocked(threadID, pageID string) {
	if jarKindOf(m.sessions[threadID]) == JarPrivate {
		return
	}
	for i, tab := range m.companionTabsLocked(threadID) {
		if tab.id != pageID {
			continue
		}
		if tab.pending == nil && reopenable(tab.info.URL) {
			m.pushClosedLocked(threadID, closedTab{url: tab.info.URL, label: tab.info.Label, index: i})
		}
		return
	}
}

// pushClosedLocked adds tab as the thread's most recently closed, dropping
// the oldest past maxClosedTabs. The caller holds m.mu.
func (m *Manager) pushClosedLocked(threadID string, tab closedTab) {
	stack := append(m.closedTabs[threadID], tab)
	if len(stack) > maxClosedTabs {
		stack = append([]closedTab(nil), stack[len(stack)-maxClosedTabs:]...)
	}
	m.closedTabs[threadID] = stack
}

// setClosedLocked stores a thread's closed tabs, removing an empty entry.
// The caller holds m.mu.
func (m *Manager) setClosedLocked(threadID string, stack []closedTab) {
	if len(stack) == 0 {
		delete(m.closedTabs, threadID)
		return
	}
	m.closedTabs[threadID] = stack
}

// forgetClosedLocked forgets threadID's closed tabs, answering whether it
// had any; the caller pushes its state when it did. The caller holds m.mu.
// The tabs are forgotten where the recent addresses are (forgetRecentLocked).
func (m *Manager) forgetClosedLocked(threadID string) bool {
	_, had := m.closedTabs[threadID]
	delete(m.closedTabs, threadID)
	return had
}

// forgetAllClosedLocked forgets every thread's closed tabs, answering the
// threads that had some. The caller holds m.mu.
func (m *Manager) forgetAllClosedLocked() []string {
	threads := make([]string, 0, len(m.closedTabs))
	for threadID := range m.closedTabs {
		threads = append(threads, threadID)
	}
	clear(m.closedTabs)
	return threads
}

// ReopenClosedTab reopens the thread's most recently closed tab at its
// position, selected, and loads its address as the address bar would. With
// none to reopen it does nothing.
func (m *Manager) ReopenClosedTab(ctx context.Context, access Access) error {
	m.mu.Lock()
	stack := m.closedTabs[access.ThreadID]
	if len(stack) == 0 {
		m.mu.Unlock()
		return nil
	}
	tab := stack[len(stack)-1]
	m.setClosedLocked(access.ThreadID, stack[:len(stack)-1])
	m.mu.Unlock()
	info, err := m.openUserTab(ctx, access, tab.url, tab.label, tab.index)
	if err != nil && info.ID == "" {
		// No tab opened (the thread's page limit, say): it can be reopened
		// once the reason is gone.
		m.mu.Lock()
		if jarKindOf(m.sessions[access.ThreadID]) != JarPrivate {
			m.pushClosedLocked(access.ThreadID, tab)
		}
		m.mu.Unlock()
		m.emitThreadState(access.ThreadID)
	}
	return err
}

// DuplicateCompanionPage opens a new tab right after pageID, selected, at
// pageID's address. A tab with no address yet opens a blank one.
func (m *Manager) DuplicateCompanionPage(ctx context.Context, access Access, pageID string) error {
	pageID = strings.TrimSpace(pageID)
	m.mu.Lock()
	index, address := -1, ""
	for i, tab := range m.companionTabsLocked(access.ThreadID) {
		if tab.id == pageID {
			index = i + 1
			if tab.pending == nil && reopenable(tab.info.URL) {
				address = tab.info.URL
			}
		}
	}
	m.mu.Unlock()
	if index < 0 {
		return errPageNotFound
	}
	_, err := m.openUserTab(ctx, access, address, "", index)
	return err
}

// Which tabs CloseCompanionTabs closes beside the one it names.
const (
	CloseOtherTabs = "others"
	CloseTabsRight = "right"
)

// CloseCompanionTabs closes, as the user, every tab of the thread but
// pageID (CloseOtherTabs) or every tab after it (CloseTabsRight), left to
// right, so reopening them one by one puts each back where it was.
func (m *Manager) CloseCompanionTabs(ctx context.Context, access Access, pageID, which string) error {
	pageID = strings.TrimSpace(pageID)
	if which != CloseOtherTabs && which != CloseTabsRight {
		return fmt.Errorf("browser: unknown set of tabs to close %q", truncateUTF8(which, 32))
	}
	m.mu.Lock()
	tabs := m.companionTabsLocked(access.ThreadID)
	m.mu.Unlock()
	at := -1
	for i := range tabs {
		if tabs[i].id == pageID {
			at = i
		}
	}
	if at < 0 {
		return errPageNotFound
	}
	var errs []error
	for i, tab := range tabs {
		if i == at || (which == CloseTabsRight && i < at) {
			continue
		}
		// A tab gone meanwhile was closed by someone else.
		if err := m.CloseCompanionPage(ctx, access, tab.id); err != nil && !errors.Is(err, errPageNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ReloadCompanionPage is the user's reload. A suspended tab, a crashed one
// included, is restored at its address, which is its reload; a live page
// reloads.
func (m *Manager) ReloadCompanionPage(ctx context.Context, access Access, pageID string) error {
	pageID = strings.TrimSpace(pageID)
	m.mu.Lock()
	rec := m.suspended[pageID]
	m.mu.Unlock()
	if rec != nil {
		_, _, err := m.resolvePage(ctx, access, pageID, false)
		return err
	}
	_, err := m.History(ctx, access, pageID, "reload")
	return err
}
