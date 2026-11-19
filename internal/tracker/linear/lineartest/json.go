package lineartest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Data wraps fields as a GraphQL answer's data.
func Data(fields string) string { return `{"data":{` + fields + `}}` }

// PageJSON is a connection's JSON, aliased as page.
func PageJSON(nodes []string, hasNext bool, cursor string) string {
	return `"page":` + ConnectionJSON(nodes, hasNext, cursor)
}

// ConnectionJSON is a connection's JSON object.
func ConnectionJSON(nodes []string, hasNext bool, cursor string) string {
	return fmt.Sprintf(`{"nodes":[%s],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}`, strings.Join(nodes, ","), hasNext, cursor)
}

// NodePageJSON is a connection hanging off one entity, aliased as page
// under node.
func NodePageJSON(nodes []string, hasNext bool, cursor string) string {
	return `"node":{` + PageJSON(nodes, hasNext, cursor) + `}`
}

// Result is a mutation's successful answer carrying value, as the
// client's documents alias it.
func Result(value string) string {
	return `{"data":{"result":{"success":true,"value":` + value + `}}}`
}

// Issue is an invented issue with the fields the client's issue fragment
// selects. Related entities are IDs; an empty ID or time is null.
type Issue struct {
	ID, Identifier     string
	Number             int
	Title, Description string
	Priority           int
	Estimate           *float64
	SortOrder          float64
	PrioritySortOrder  float64
	SubIssueSortOrder  *float64
	Team, State        string
	Assignee, Creator  string
	Cycle, Project     string
	Parent             string
	LabelIDs           []string
	DueDate            string
	CreatedAt          string
	UpdatedAt          string
	StartedAt          string
	CompletedAt        string
	CanceledAt         string
	ArchivedAt         string
	Trashed            bool
}

// NewIssue is an issue of team team-web in state state-todo with every
// other field set to an invented value.
func NewIssue(id, identifier, updatedAt string, sortOrder float64) Issue {
	three := 3.0
	return Issue{
		ID: id, Identifier: identifier, Number: 1, Title: "Fix the login page", Description: "Steps:\n1. open",
		Priority: 2, Estimate: &three, SortOrder: sortOrder, PrioritySortOrder: -10.5,
		Team: "team-web", State: "state-todo", Assignee: "user-1", Creator: "user-1", Project: "project-1",
		LabelIDs: []string{"label-bug"}, DueDate: "2026-12-01", CreatedAt: "2026-11-01T09:00:00.000Z", UpdatedAt: updatedAt,
	}
}

// IssueJSON is NewIssue's JSON.
func IssueJSON(id, identifier, updatedAt string, sortOrder float64) string {
	return NewIssue(id, identifier, updatedAt, sortOrder).JSON()
}

// JSON is the issue as an answer carries it.
func (i Issue) JSON() string {
	ref := func(id string) any {
		if id == "" {
			return nil
		}
		return map[string]string{"id": id}
	}
	null := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	labels := i.LabelIDs
	if labels == nil {
		labels = []string{}
	}
	var trashed any
	if i.Trashed {
		trashed = true
	}
	b, err := json.Marshal(map[string]any{
		"id": i.ID, "identifier": i.Identifier, "number": i.Number, "title": i.Title, "description": null(i.Description),
		"url": "https://linear.app/example/issue/" + i.Identifier, "branchName": "user1/" + i.Identifier,
		"priority": i.Priority, "estimate": i.Estimate, "sortOrder": i.SortOrder, "prioritySortOrder": i.PrioritySortOrder,
		"subIssueSortOrder": i.SubIssueSortOrder, "team": ref(i.Team), "state": ref(i.State),
		"assignee": ref(i.Assignee), "creator": ref(i.Creator),
		"cycle": ref(i.Cycle), "project": ref(i.Project), "parent": ref(i.Parent),
		"labelIds": labels, "dueDate": null(i.DueDate), "createdAt": i.CreatedAt, "updatedAt": i.UpdatedAt,
		"startedAt": null(i.StartedAt), "completedAt": null(i.CompletedAt), "canceledAt": null(i.CanceledAt),
		"archivedAt": null(i.ArchivedAt), "trashed": trashed,
	})
	if err != nil {
		panic(err) // every value above encodes
	}
	return string(b)
}
