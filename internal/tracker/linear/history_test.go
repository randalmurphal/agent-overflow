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

// IssueHistories reads ten issues per request, continues one issue's
// longer history on its own, and leaves out an issue the answer omits.
func TestIssueHistories(t *testing.T) {
	t.Parallel()
	var ids []string
	for i := 1; i <= 11; i++ {
		ids = append(ids, uuidN(i))
	}
	idList := func(ids []string) string { return `["` + strings.Join(ids, `","`) + `"]` }
	newest := historyEntryJSON("h3", "2026-11-05T10:00:00.000Z", "s2", "s3")
	older := historyEntryJSON("h2", "2026-11-04T10:00:00.000Z", "s1", "s2")
	oldest := historyEntryJSON("h1", "2026-11-03T10:00:00.000Z", "s0", "s1")
	// The first batch answers nine of its ten: uuidN(2) is missing.
	var first []string
	for _, id := range ids[:10] {
		switch id {
		case uuidN(1):
			first = append(first, historiesNode(id, "team-web", []string{newest}, true, "hc1"))
		case uuidN(2):
		default:
			first = append(first, historiesNode(id, "team-web", nil, false, ""))
		}
	}
	f := newFake(t,
		lineartest.Exchange{Op: "IssueHistories", Vars: `{"filter":{"and":[{"id":{"in":` + idList(ids[:10]) + `}}]},"first":10,"entries":50}`,
			Body: lineartest.Data(lineartest.PageJSON(first, false, ""))},
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + uuidN(1) + `","first":100,"after":"hc1"}`,
			Body: `{"data":{` + lineartest.NodePageJSON([]string{older}, true, "hc2") + `}}`},
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + uuidN(1) + `","first":100,"after":"hc2"}`,
			Body: `{"data":{` + lineartest.NodePageJSON([]string{oldest}, false, "") + `}}`},
		lineartest.Exchange{Op: "IssueHistories", Vars: `{"filter":{"and":[{"id":{"in":` + idList(ids[10:]) + `}}]},"first":10,"entries":50}`,
			Body: lineartest.Data(lineartest.PageJSON([]string{historiesNode(uuidN(11), "team-app", nil, false, "")}, false, ""))},
	)
	got, err := f.client(t).IssueHistories(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("%d issues, want 10", len(got))
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
	if byID[uuidN(11)].TeamID != "team-app" {
		t.Errorf("issue 11: %+v", byID[uuidN(11)])
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

// A batch answer that claims a second page for at most ten IDs, or a
// continuation whose cursor does not move, fails the read.
func TestIssueHistoriesRefusesBadPaging(t *testing.T) {
	t.Parallel()
	vars := `{"filter":{"and":[{"id":{"in":["` + uuid1 + `"]}}]},"first":10,"entries":50}`
	node := historiesNode(uuid1, "team-web", nil, true, "hc1")
	f := newFake(t,
		lineartest.Exchange{Op: "IssueHistories", Vars: vars, Body: lineartest.Data(lineartest.PageJSON(nil, true, "p2"))},
		lineartest.Exchange{Op: "IssueHistories", Vars: vars, Body: lineartest.Data(lineartest.PageJSON([]string{node}, false, ""))},
		lineartest.Exchange{Op: "IssueHistory", Vars: `{"id":"` + uuid1 + `","first":100,"after":"hc1"}`,
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
