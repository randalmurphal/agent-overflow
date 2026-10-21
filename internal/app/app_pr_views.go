package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// MarkPRViewed records that the user opened pr at headSHA (pr_views), with
// the comment and commit counts the list pump's latest row for it holds
// (none when it holds no row, or a commit count its forge's list does not
// give). The list pump republishes with the row's Seen and Since from it;
// the answer is the recorded PRSeen.
//
//ao:scope git:operate
//ao:route selected
func (a *App) MarkPRViewed(ctx context.Context, pr gitops.PRReference, headSHA string) (PRSeen, error) {
	if a.shuttingDown.Load() {
		return PRSeen{}, ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return PRSeen{}, err
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" || len(headSHA) > 64 || strings.ContainsFunc(headSHA, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) {
		return PRSeen{}, fmt.Errorf("mark PR viewed: head %q is not a commit id", headSHA)
	}
	view := store.PRView{PRKey: pr.Key(), OpenedAt: time.Now().UnixMilli(), HeadSHA: headSHA}
	if row, ok := a.prListRow(pr.Key()); ok {
		comments := row.Comments
		view.Comments = &comments
		// A forge whose list gives no commit count answers 0; no PR has
		// none.
		if row.Commits > 0 {
			commits := row.Commits
			view.Commits = &commits
		}
	}
	stored, err := a.store.PutPRView(view)
	if err != nil {
		return PRSeen{}, fmt.Errorf("mark PR viewed: %w", err)
	}
	a.republishPRList()
	return *prSeenOf(stored), nil
}

// republishPRList has the list pump, when one runs, rebuild its snapshot
// from what it holds.
func (a *App) republishPRList() {
	a.prUpdates.mu.Lock()
	pump := a.prUpdates.list
	a.prUpdates.mu.Unlock()
	if pump == nil {
		return
	}
	select {
	case pump.republish <- struct{}{}:
	default:
	}
}

// prListRow is the list pump's latest row for a PR key.
func (a *App) prListRow(key string) (gitops.PRListRow, bool) {
	a.prUpdates.mu.Lock()
	defer a.prUpdates.mu.Unlock()
	pump := a.prUpdates.list
	if pump == nil {
		return gitops.PRListRow{}, false
	}
	for _, state := range pump.projects {
		for _, row := range state.rows {
			if row.Ref.Key() == key {
				return row, true
			}
		}
	}
	return gitops.PRListRow{}, false
}
