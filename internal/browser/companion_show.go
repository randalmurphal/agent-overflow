package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-overflow/internal/browser/turnorder"
)

// ShowOutcome is what the chat header's show did (ShowCompanion).
type ShowOutcome int

const (
	// ShowPassed did nothing: its numbered turn passed (its pane is gone),
	// or the tab it opened was closed while it was created.
	ShowPassed ShowOutcome = iota
	// ShowShown showed the tab selected at its turn.
	ShowShown
	// ShowOpened opened a blank tab, selected and shown, because the thread
	// had none.
	ShowOpened
)

// ShowCompanion is the chat header's show. It names no tab: in one
// Manager.mu hold, at its turn (selection.go), it shows the tab the thread
// has selected then, or with none selected the most recently used, and
// with no tab at all it adds one blank tab, selected and shown, and
// creates its page after the hold. So a select or an open an agent made
// before the turn wins, two shows at once open one tab between them, and a
// show never names a page the user did not see. It answers the tab it
// showed or opened.
func (m *Manager) ShowCompanion(ctx context.Context, access Access) (ShowOutcome, PageID, error) {
	if access.ThreadID.trim() == "" || strings.TrimSpace(access.Workspace) == "" {
		return ShowPassed, "", fmt.Errorf("browser: invalid caller scope")
	}
	turn, err := m.selectionTurn(ctx, access)
	if err != nil || turn.Standing() == turnorder.Passed {
		return ShowPassed, "", err
	}
	defer turn.Release()
	if err := m.lockStructure(ctx, access.tb); err != nil {
		return ShowPassed, "", err
	}
	if err := access.admitLocked(); err != nil {
		m.mu.Unlock()
		return ShowPassed, "", err
	}
	tb := access.tb
	tabs := m.companionTabsLocked(tb)
	var pageID PageID
	for _, tab := range tabs {
		if tab.id == tb.session.activePage() {
			pageID = tab.id
		}
	}
	if pageID == "" {
		pageID = recentTabLocked(tabs)
	}
	if pageID != "" {
		if turn.Commit() == turnorder.Passed {
			m.mu.Unlock()
			return ShowPassed, "", nil
		}
		tb.session.ActivePageID, tb.session.Visible = string(pageID), true
		tb.session.UpdatedAt = time.Now()
		m.mu.Unlock()
		m.emitThreadState(access.ThreadID)
		m.syncPanePresentation(access.ThreadID)
		return ShowShown, pageID, nil
	}
	reserved, err := m.reserveTabLocked(ctx, access, turn, -1, nil)
	m.mu.Unlock()
	if errors.Is(err, errSelectionPassed) {
		return ShowPassed, "", nil
	}
	if err != nil {
		return ShowPassed, "", err
	}
	pageID = reserved.pending.p.id
	_, err = m.createReserved(reserved)
	var onTab *shownOnTab
	switch {
	case errors.Is(err, errClosedWhileCreating):
		return ShowPassed, "", nil
	case errors.As(err, &onTab):
		// The tab stays, shown with its failure.
		return ShowOpened, pageID, nil
	case err != nil:
		return ShowPassed, "", err
	}
	return ShowOpened, pageID, nil
}
