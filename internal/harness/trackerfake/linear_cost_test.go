package trackerfake

import (
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
	g := newClockRig(t, txnFixture(), testStart.Add(time.Hour))
	for _, c := range []struct {
		name, query string
		want        int
	}{
		{"card page of up to 100 (measured 8)", `query Q { issues(first: 100) { nodes { ` + card + ` } pageInfo { hasNextPage endCursor } } }`, 8},
		{"card page of up to 250 (measured 8)", `query Q { issues(first: 250) { nodes { ` + card + ` } pageInfo { hasNextPage endCursor } } }`, 8},
		{"IDs and update times (measured 4)", `query Q { issues(first: 250) { nodes { id updatedAt } pageInfo { hasNextPage endCursor } } }`, 4},
		{"history of 50 (measured 203)", history(50), 204},
		{"history of 100 (measured 403)", history(100), 404},
		{"history of 250 (measured 1,003)", history(250), 1004},
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
