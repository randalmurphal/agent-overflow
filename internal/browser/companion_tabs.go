package browser

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"agent-overflow/internal/browser/turnorder"
)

// The tab strip's actions on whole tabs: close one or several, reopen the
// last closed, duplicate, reload. The keyboard's commands on the active tab
// are active_commands.go.
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

// closedTabs is one thread lifetime's closed-tab history, most recent
// last.
type closedTabs struct {
	tabs []closedTab
}

// browsing is a thread lifetime's browsing history: its closed tabs and its
// recent addresses (recent.go). A forget of it (a jar switch, a clear of
// site data, the browser turned off) replaces the object
// (forgetBrowsingLocked), so a claim taken on the old one before the forget
// is void after it (tabClaim). Emptied by reopens, it stays until a forget.
type browsing struct {
	closed closedTabs
	recent []RecentAddress
}

// closedCountLocked is how many tabs tb can reopen. The caller holds m.mu.
func closedCountLocked(tb *threadBrowser) int {
	if tb == nil {
		return 0
	}
	return len(tb.browsing.closed.tabs)
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
func (m *Manager) CloseCompanionPage(ctx context.Context, access Access, pageID PageID) error {
	if err := access.bound(); err != nil {
		return err
	}
	return m.closePage(ctx, access, pageID, false, true)
}

// recordUserCloseLocked keeps tb's tab pageID as the user's most recent
// close. The close that takes the tab out of the tab set calls it in that
// same m.mu hold, so only the close that removed the tab records it, and
// the state that close pushes counts it. The caller holds m.mu.
func (m *Manager) recordUserCloseLocked(tb *threadBrowser, pageID PageID) {
	if jarKindOf(tb.session) == JarPrivate {
		return
	}
	for i, tab := range m.companionTabsLocked(tb) {
		if tab.id != pageID {
			continue
		}
		if tab.pending == nil && reopenable(tab.info.URL) {
			tb.browsing.closed.push(closedTab{url: tab.info.URL, label: tab.info.Label, index: i})
		}
		return
	}
}

func (s *closedTabs) push(tab closedTab) {
	s.tabs = append(s.tabs, tab)
	if len(s.tabs) > maxClosedTabs {
		s.tabs = append([]closedTab(nil), s.tabs[len(s.tabs)-maxClosedTabs:]...)
	}
}

// ReopenClosedTab reopens the thread's most recently closed tab at its
// position, selected, and loads its address as the address bar would. The
// tab and its entry's removal from the closed tabs are one admission
// (createPageAt), so an entry is never off the history without its tab, and
// a forget of the history after it (a clear, a jar switch, the thread's
// close) leaves the tab nothing to load (tabClaim). With none to reopen, or
// with ctx's numbered selection passed or its pane detached (selection.go),
// or a tab refused, it does nothing and the closed tab stays.
func (m *Manager) ReopenClosedTab(ctx context.Context, access Access) error {
	if err := access.bound(); err != nil {
		return err
	}
	turn, err := m.selectionTurn(ctx, access)
	if err != nil || turn.Standing() == turnorder.Passed {
		return err
	}
	defer turn.Release()
	m.mu.Lock()
	if err := access.admitLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	claim, ok := reopenClaimLocked(access.tb)
	m.mu.Unlock()
	if !ok || turn.Standing() == turnorder.Passed {
		return nil
	}
	if m.reopenRead != nil {
		m.reopenRead()
	}
	_, err = m.openUserTab(ctx, access, turn, claim.address, claim.label, claim.index, claim)
	if errors.Is(err, errSelectionPassed) || errors.Is(err, errClaimVoid) || errors.Is(err, errTabReplaced) {
		// Nothing to report: the closed tab was forgotten, or the tab it
		// opened was replaced before its address loaded, which then loads
		// nowhere.
		return nil
	}
	return err
}

// DuplicateCompanionPage opens a new tab right after pageID, selected, at
// pageID's address. A tab with no address yet opens a blank one. The
// address is a claim on the thread's browsing (tabClaim), void after a
// forget of it, so a clear of site data while the duplicate runs leaves it
// no address to load. With ctx's numbered selection passed or its pane
// detached (selection.go) it does nothing.
func (m *Manager) DuplicateCompanionPage(ctx context.Context, access Access, pageID PageID) error {
	if err := access.bound(); err != nil {
		return err
	}
	pageID = pageID.trim()
	turn, err := m.selectionTurn(ctx, access)
	if err != nil || turn.Standing() == turnorder.Passed {
		return err
	}
	defer turn.Release()
	m.mu.Lock()
	if err := access.admitLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	claim, ok := m.duplicateClaimLocked(access.tb, pageID)
	m.mu.Unlock()
	if !ok {
		return errPageNotFound
	}
	_, err = m.openUserTab(ctx, access, turn, claim.address, "", claim.index, claim)
	if errors.Is(err, errSelectionPassed) {
		return nil
	}
	if errors.Is(err, errClaimVoid) || errors.Is(err, errTabReplaced) {
		// The thread's browsing was forgotten (a clear, a jar switch) after
		// the duplicate read its source, or its new tab was replaced before
		// the source's address loaded: it has nothing to duplicate.
		return errPageNotFound
	}
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
func (m *Manager) CloseCompanionTabs(ctx context.Context, access Access, pageID PageID, which string) error {
	if err := access.bound(); err != nil {
		return err
	}
	pageID = pageID.trim()
	if which != CloseOtherTabs && which != CloseTabsRight {
		return fmt.Errorf("browser: unknown set of tabs to close %q", truncateUTF8(which, 32))
	}
	m.mu.Lock()
	tabs := m.companionTabsLocked(access.tb)
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
func (m *Manager) ReloadCompanionPage(ctx context.Context, access Access, pageID PageID) error {
	if err := access.bound(); err != nil {
		return err
	}
	pageID = pageID.trim()
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
