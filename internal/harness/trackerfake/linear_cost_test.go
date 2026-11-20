package trackerfake

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each request whose cost was measured answers the points the fit
// gives: the measured value, or within the fit's stated error. The fake
// answers the same whatever the page returns.
func TestLinearCostModel(t *testing.T) {
	const card = `id identifier number title url branchName priority estimate sortOrder prioritySortOrder subIssueSortOrder
		team { id issueEstimationType } state { id } assignee { id } creator { id } cycle { id } project { id } parent { id }
		labelIds labels(first: 250) { nodes { id name } }
		dueDate createdAt updatedAt startedAt completedAt canceledAt archivedAt trashed`
	const label = `id name color description isGroup parent { id } team { id } archivedAt`
	const team = `id key name description timezone issueEstimationType issueEstimationExtended issueEstimationAllowZero defaultIssueEstimate
		cyclesEnabled cycleDuration activeCycle { id } triageEnabled defaultIssueState { id } triageIssueState { id }
		setIssueSortOrderOnStateChange archivedAt updatedAt`
	history := func(first int) string {
		return `query Q { issues(first: 42) { nodes { id createdAt history(first: ` + strconv.Itoa(first) +
			`) { nodes { id createdAt actorId fromStateId toStateId } pageInfo { hasNextPage } } } pageInfo { hasNextPage } } }`
	}
	members := func(issues, first int) string {
		return `query Q { issues(first: ` + strconv.Itoa(issues) + `) { nodes { id team { members(first: ` + strconv.Itoa(first) + `) { nodes { id } } } } } }`
	}
	g := newClockRig(t, txnFixture(), testStart.Add(time.Hour))
	for _, c := range []struct {
		name, query string
		want        int
	}{
		{"card page of up to 100 (measured 8)", `query Q { issues(first: 100) { nodes { ` + card + ` } pageInfo { hasNextPage endCursor } } }`, 8},
		{"card page of up to 250 (measured 8)", `query Q { issues(first: 250) { nodes { ` + card + ` } pageInfo { hasNextPage endCursor } } }`, 8},
		{"IDs (measured 2)", `query Q { issues(first: 10) { nodes { id } } }`, 2},
		{"team, assignee and state (measured 4)", `query Q { issues(first: 10) { nodes { id team { id name } assignee { id } state { id } } } }`, 4},
		{"positions (measured 4)", `query Q { issues(first: 250) { nodes { id sortOrder team { id } } pageInfo { hasNextPage endCursor } } }`, 4},
		{"members of 1 under the team, 1 issue (measured 4)", members(1, 1), 4},
		{"members of 1 under the team, 50 issues (measured 4)", members(50, 1), 4},
		{"members of 50 under the team, 10 issues (measured 14)", members(10, 50), 14},
		{"members of 250 under the team, 1 issue (measured 54)", members(1, 250), 54},
		{"members of 250 under the team, 50 issues (measured 54)", members(50, 250), 54},
		{"history of 50 (measured 203)", history(50), 203},
		{"history of 100 (measured 403)", history(100), 403},
		{"history of 250 (measured 1,003)", history(250), 1003},
		{"labels by one ID (measured 4)", `query Q { issueLabels(filter: {id: {in: ["l1"]}}, first: 1) { nodes { ` + label + ` } } }`, 4},
		{"labels by three IDs (measured 10)", `query Q { issueLabels(filter: {id: {in: ["l1", "l2", "l3"]}}, first: 3) { nodes { ` + label + ` } } }`, 10},
		{"teams by two keys (measured 9)", `query Q { teams(filter: {or: [{key: {eqIgnoreCase: "WEB"}}, {key: {eqIgnoreCase: "APP"}}]}, first: 2) { nodes { ` + team + ` } } }`, 9},
		{"three aliased issueUpdate (measured 7)", `mutation Q { a: issueUpdate(id: "i1", input: {sortOrder: 1}) { success issue { id sortOrder } }
			b: issueUpdate(id: "i2", input: {sortOrder: 2}) { success issue { id sortOrder } }
			c: issueUpdate(id: "i1", input: {sortOrder: 3}) { success issue { id sortOrder } } }`, 7},
		{"issueBatchUpdate of three (measured 3)", `mutation Q { issueBatchUpdate(ids: ["i1", "i2", "i1"], input: {title: "Same"}) { success issues { id } } }`, 3},
	} {
		rec := g.do(linearReq("Q", c.query))
		if rec.Code != 200 || strings.Contains(rec.Body.String(), `"errors"`) {
			t.Errorf("%s: answered %d %s", c.name, rec.Code, rec.Body)
			continue
		}
		if got := rec.Header().Get("X-Complexity"); got != strconv.Itoa(c.want) {
			t.Errorf("%s: %s points, want %d", c.name, got, c.want)
		}
	}
}

// A catalog under an issue's object, at any depth, costs more for a
// larger first and the same for any page first: the page's first does
// not hide it.
func TestLinearCostNestedCatalogs(t *testing.T) {
	g := newClockRig(t, txnFixture(), testStart.Add(time.Hour))
	cost := func(query string) int {
		t.Helper()
		rec := g.do(linearReq("Q", query))
		if rec.Code != 200 || strings.Contains(rec.Body.String(), `"errors"`) {
			t.Fatalf("answered %d %s", rec.Code, rec.Body)
		}
		n, err := strconv.Atoi(rec.Header().Get("X-Complexity"))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for name, shape := range map[string]string{
		"members under the team":          `team { members(first: %d) { nodes { id } } }`,
		"members under the parent's team": `parent { team { members(first: %d) { nodes { id } } } }`,
		"labels under the team":           `team { labels(first: %d) { nodes { id } } }`,
		"states under the team":           `team { states(first: %d) { nodes { id name } } }`,
	} {
		prev := 0
		for _, first := range []int{1, 50, 100, 250} {
			var at []int
			for _, page := range []int{1, 50, 250} {
				at = append(at, cost(fmt.Sprintf(`query Q { issues(first: %d) { nodes { id %s } } }`, page, fmt.Sprintf(shape, first))))
			}
			if at[0] != at[1] || at[1] != at[2] {
				t.Errorf("%s, first %d: %v points for pages of 1, 50 and 250", name, first, at)
			}
			if first > 1 && at[0] <= prev {
				t.Errorf("%s: first %d costs %d, no more than %d", name, first, at[0], prev)
			}
			prev = at[0]
		}
	}
}
