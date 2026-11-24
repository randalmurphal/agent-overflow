package jiraprovider

import (
	"strings"
	"testing"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira/jiratest"
)

// malformedBulk are bulk fetch answers to a request of 10001 and 10002,
// each issue whole, that do not answer it: an issue with no ID, one not
// asked, one twice.
func malformedBulk() map[string]string {
	at := "2026-11-10T10:00:00.000+0000"
	return map[string]string{
		"no ID":     page("", issue("", "WEB-1", at, nil)),
		"not asked": page("", issue("10003", "WEB-3", at, nil)),
		"twice":     page("", issue("10001", "WEB-1", at, nil), issue("10001", "WEB-1", at, nil)),
	}
}

// Every read that asks a bulk fetch by ID fails on a malformed answer and
// reads no issue asked as absent from it: the cards of issues on no board,
// and which issues without changes a history read finds.
func TestEveryBulkReadByIDFailsOnAMalformedAnswer(t *testing.T) {
	for name, answer := range malformedBulk() {
		t.Run(name, func(t *testing.T) {
			f, p := newSite(t)
			f.On("POST", bulkFetchPath, jiratest.Reply(200, answer))
			if got, err := p.Cards(t.Context(), []tracker.IssueID{"10001", "10002"}); err == nil || !strings.Contains(err.Error(), "malformed answer") {
				t.Errorf("cards: %+v, %v; want a malformed answer", got, err)
			}
			f.On("POST", changelogsPath, jiratest.Reply(200, `{"issueChangeLogs":[]}`))
			if got, err := p.History(t.Context(), tracker.HistoryQuery{Issues: []tracker.IssueID{"10001", "10002"}}); err == nil || !strings.Contains(err.Error(), "malformed answer") {
				t.Errorf("history: %+v, %v; want a malformed answer", got, err)
			}
		})
	}
}
