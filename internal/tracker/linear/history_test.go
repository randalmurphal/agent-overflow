package linear

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/tracker/linear/lineartest"
	"agent-overflow/internal/tracker/trackerapi"
)

func historyEntryJSON(id, at, fromState, toState string) string {
	return lineartest.HistoryEntry{ID: id, CreatedAt: at, Actor: "user-1", FromState: fromState, ToState: toState}.JSON()
}

func historiesNode(id, team string, entries []string, more bool, cursor string) string {
	return lineartest.IssueChangesJSON(id, team, "2026-11-01T09:00:00.000Z", entries, more, cursor)
}

// uuidN is the nth invented issue ID.
func uuidN(n int) string { return fmt.Sprintf("6f1c2a3b-0000-4000-8000-%012d", n) }

// IssueHistories reads historyBatchIssues issues per request, continues one issue's
// longer history on its own, and leaves out an issue the answer omits.
func TestIssueHistories(t *testing.T) {
	t.Parallel()
	var ids []string
	for i := 1; i <= historyBatchIssues+1; i++ {
		ids = append(ids, uuidN(i))
	}
	idList := func(ids []string) string { return `["` + strings.Join(ids, `","`) + `"]` }
	newest := historyEntryJSON("h3", "2026-11-05T10:00:00.000Z", "s2", "s3")
	older := historyEntryJSON("h2", "2026-11-04T10:00:00.000Z", "s1", "s2")
	oldest := historyEntryJSON("h1", "2026-11-03T10:00:00.000Z", "s0", "s1")
	// The first batch answers all but uuidN(2).
	var first []string
	for _, id := range ids[:historyBatchIssues] {
		switch id {
		case uuidN(1):
			first = append(first, historiesNode(id, "team-web", []string{newest}, true, "hc1"))
		case uuidN(2):
		default:
			first = append(first, historiesNode(id, "team-web", nil, false, ""))
		}
	}
	f := newFake(t,
		lineartest.Exchange{Op: "IssueHistories", Vars: `{"filter":{"and":[{"id":{"in":` + idList(ids[:historyBatchIssues]) + `}}]},"first":50,"entries":250}`,
			Body: lineartest.Data(lineartest.PageJSON(first, false, ""))},
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + uuidN(1) + `","first":250,"after":"hc1"}`,
			Body: `{"data":{` + lineartest.NodePageJSON([]string{older}, true, "hc2") + `}}`},
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + uuidN(1) + `","first":250,"after":"hc2"}`,
			Body: `{"data":{` + lineartest.NodePageJSON([]string{oldest}, false, "") + `}}`},
		lineartest.Exchange{Op: "IssueHistories", Vars: `{"filter":{"and":[{"id":{"in":` + idList(ids[historyBatchIssues:]) + `}}]},"first":1,"entries":250}`,
			Body: lineartest.Data(lineartest.PageJSON([]string{historiesNode(uuidN(historyBatchIssues+1), "team-app", nil, false, "")}, false, ""))},
	)
	got, err := f.client(t).IssueHistories(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != historyBatchIssues {
		t.Fatalf("%d issues, want %d", len(got), historyBatchIssues)
	}
	byID := map[string]IssueChanges{}
	for _, c := range got {
		byID[c.IssueID] = c
	}
	one := byID[uuidN(1)]
	if len(one.Entries) != 3 || one.Entries[0].ID != "h3" || one.Entries[2].ID != "h1" || one.TeamID != "team-web" || one.CreatedAt.IsZero() {
		t.Errorf("issue 1: %+v", one)
	}
	if _, ok := byID[uuidN(2)]; ok {
		t.Error("an issue the answer omitted is reported")
	}
	if last := byID[uuidN(historyBatchIssues+1)]; last.TeamID != "team-app" || last.Served.IsZero() {
		t.Errorf("the last issue: %+v", last)
	}
}

// Only issue IDs, each once, are sent.
func TestIssueHistoriesRefusesBadIDs(t *testing.T) {
	t.Parallel()
	c := newFake(t).client(t)
	for name, ids := range map[string][]string{
		"identifier": {uuid1, "WEB-1"},
		"twice":      {uuid1, uuid2, uuid1},
		"empty":      {""},
	} {
		if _, err := c.IssueHistories(context.Background(), ids); !errors.Is(err, trackerapi.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got, err := c.IssueHistories(context.Background(), nil); err != nil || got != nil {
		t.Errorf("no IDs: %v %v", got, err)
	}
}

// A batch answer that claims a second page for fewer issues than asked, or a
// continuation whose cursor does not move, fails the read.
func TestIssueHistoriesRefusesBadPaging(t *testing.T) {
	t.Parallel()
	vars := `{"filter":{"and":[{"id":{"in":["` + uuid1 + `"]}}]},"first":1,"entries":250}`
	node := historiesNode(uuid1, "team-web", nil, true, "hc1")
	f := newFake(t,
		lineartest.Exchange{Op: "IssueHistories", Vars: vars, Body: lineartest.Data(lineartest.PageJSON(nil, true, "p2"))},
		lineartest.Exchange{Op: "IssueHistories", Vars: vars, Body: lineartest.Data(lineartest.PageJSON([]string{node}, false, ""))},
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + uuid1 + `","first":250,"after":"hc1"}`,
			Body: `{"data":{` + lineartest.NodePageJSON(nil, true, "hc1") + `}}`},
	)
	c := f.client(t)
	if _, err := c.IssueHistories(context.Background(), []string{uuid1}); err == nil || !strings.Contains(err.Error(), "more than one page") {
		t.Errorf("a second page: %v", err)
	}
	if _, err := c.IssueHistories(context.Background(), []string{uuid1}); err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Errorf("a stuck cursor: %v", err)
	}
}

// A batch of historyBatchIssues issues, each with historyPage entries of
// the largest kind (every change pair set, two titles of 255 three-byte
// runes, ten labels added and ten removed), fits in historyBodyBytes.
func TestHistoryBatchFitsItsBound(t *testing.T) {
	t.Parallel()
	title := strings.Repeat("€", 255)
	five := 5.0
	archived := true
	var labels []string
	for i := range 10 {
		labels = append(labels, uuidN(100+i))
	}
	entry := lineartest.HistoryEntry{
		ID: uuidN(1), CreatedAt: "2026-11-05T10:00:00.000Z", UpdatedAt: "2026-11-05T10:00:00.000Z", Actor: uuidN(2),
		FromState: uuidN(3), ToState: uuidN(4), FromAssignee: uuidN(5), ToAssignee: uuidN(6), FromCycle: uuidN(7), ToCycle: uuidN(8),
		FromProject: uuidN(9), ToProject: uuidN(10), FromParent: uuidN(11), ToParent: uuidN(12), FromTeam: uuidN(13), ToTeam: uuidN(14),
		FromTitle: title, ToTitle: title, FromEstimate: &five, ToEstimate: &five, FromPriority: &five, ToPriority: &five,
		AddedLabelIDs: labels, RemovedLabelIDs: labels, Archived: &archived,
	}.JSON()
	entries := make([]string, historyPage)
	for i := range entries {
		entries[i] = entry
	}
	node := historiesNode(uuidN(1), uuidN(2), entries, true, strings.Repeat("c", 100))
	size := len(lineartest.Data(lineartest.PageJSON(nil, false, ""))) + historyBatchIssues*(len(node)+1)
	if size > historyBodyBytes {
		t.Errorf("a worst-case batch is %d bytes, over the bound of %d (entry %d bytes)", size, historyBodyBytes, len(entry))
	}
	t.Logf("worst-case entry %d bytes, batch %d of %d", len(entry), size, historyBodyBytes)
}
