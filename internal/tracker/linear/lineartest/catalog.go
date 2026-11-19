package lineartest

import "encoding/json"

// The builders below write invented records with every field the
// client's fragments select. An empty ID or time is null.

func encode(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // every value the builders put in encodes
	}
	return string(b)
}

func ref(id string) any {
	if id == "" {
		return nil
	}
	return map[string]string{"id": id}
}

func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Team is a team with its issue view settings.
type Team struct {
	ID, Key, Name string
	// Scale is issueEstimationType: notUsed, exponential, fibonacci,
	// linear or tShirt.
	Scale               string
	Extended, AllowZero bool
	Cycles              bool
}

// NewTeam is team-<key> named Web, with cycles and a Fibonacci scale that
// allows zero.
func NewTeam(key string) Team {
	return Team{ID: "team-" + key, Key: key, Name: "Web", Scale: "fibonacci", AllowZero: true, Cycles: true}
}

func (t Team) JSON() string {
	return encode(map[string]any{
		"id": t.ID, "key": t.Key, "name": t.Name, "description": nil, "timezone": "Europe/Dublin",
		"issueEstimationType": t.Scale, "issueEstimationExtended": t.Extended, "issueEstimationAllowZero": t.AllowZero,
		"defaultIssueEstimate": 1, "cyclesEnabled": t.Cycles, "cycleDuration": 2, "activeCycle": ref("cycle-1"),
		"triageEnabled": true, "defaultIssueState": ref("state-backlog"), "triageIssueState": ref("state-triage"),
		"setIssueSortOrderOnStateChange": "first", "archivedAt": nil, "updatedAt": "2026-11-01T09:00:00.000Z",
	})
}

// State is a workflow state.
type State struct {
	ID, Name, Type string
	Position       float64
	Team           string
}

func (s State) JSON() string {
	return encode(map[string]any{
		"id": s.ID, "name": s.Name, "color": "#aaa", "description": nil, "type": s.Type, "position": s.Position,
		"team": ref(s.Team), "archivedAt": nil,
	})
}

// User is a workspace user.
type User struct {
	ID, Name, DisplayName, Email string
	Active, Admin, App           bool
}

func (u User) JSON() string {
	return encode(map[string]any{
		"id": u.ID, "name": u.Name, "displayName": u.DisplayName, "email": orNull(u.Email),
		"avatarUrl": "https://example.com/" + u.ID + ".png", "active": u.Active, "admin": u.Admin,
		"guest": false, "app": u.App, "isMe": false,
	})
}

// Label is an issue label; an empty Team is a workspace label.
type Label struct {
	ID, Name, Parent, Team string
	IsGroup                bool
}

func (l Label) JSON() string {
	return encode(map[string]any{
		"id": l.ID, "name": l.Name, "color": "#f00", "description": nil, "isGroup": l.IsGroup,
		"parent": ref(l.Parent), "team": ref(l.Team), "archivedAt": nil,
	})
}

// Project is a project.
type Project struct {
	ID, Name, Lead, StartDate string
}

func (p Project) JSON() string {
	return encode(map[string]any{
		"id": p.ID, "name": p.Name, "url": "https://linear.app/example/project/" + p.ID,
		"status": map[string]string{"id": "ps-1", "name": "In Progress", "type": "started"},
		"lead":   ref(p.Lead), "startDate": orNull(p.StartDate), "targetDate": nil,
		"completedAt": nil, "canceledAt": nil, "archivedAt": nil, "updatedAt": "2026-11-01T09:00:00.000Z",
	})
}

// Cycle is a cycle; exactly one of Active, Future and Past should hold.
type Cycle struct {
	ID                             string
	Number                         int
	Name, Team                     string
	StartsAt, EndsAt, CompletedAt  string
	Active, Future, Past, Previous bool
}

func (c Cycle) JSON() string {
	return encode(map[string]any{
		"id": c.ID, "number": c.Number, "name": orNull(c.Name), "description": nil, "team": ref(c.Team),
		"startsAt": c.StartsAt, "endsAt": c.EndsAt, "completedAt": orNull(c.CompletedAt), "archivedAt": nil,
		"isActive": c.Active, "isFuture": c.Future, "isPast": c.Past, "isNext": false, "isPrevious": c.Previous,
		"progress": 0.5, "scopeHistory": []float64{3, 5}, "completedScopeHistory": []float64{0, 2},
		"inProgressScopeHistory": []float64{1, 1}, "issueCountHistory": []float64{2, 3}, "completedIssueCountHistory": []float64{0, 1},
	})
}

// Comment is a comment; an empty Parent is a top-level comment.
type Comment struct {
	ID, Issue, Parent, User, Body string
	CreatedAt                     string
}

// NewComment is a reply by user-1 on issue-1.
func NewComment() Comment {
	return Comment{ID: "comment-2", Issue: "issue-1", Parent: "comment-1", User: "user-1", Body: "Agreed, **ship it**.", CreatedAt: "2026-11-02T10:00:00.000Z"}
}

func (c Comment) JSON() string {
	return encode(map[string]any{
		"id": c.ID, "issue": ref(c.Issue), "parent": ref(c.Parent), "user": ref(c.User), "body": c.Body,
		"url": "https://linear.app/example/issue/" + c.Issue + "#" + c.ID, "createdAt": c.CreatedAt, "updatedAt": c.CreatedAt,
		"editedAt": nil, "resolvedAt": nil,
	})
}

// HistoryEntry is one recorded change. Each pair of From and To is null
// when both are empty, as Linear sends a change the entry does not
// record; a number pair is null when both are nil. An empty UpdatedAt is
// CreatedAt.
type HistoryEntry struct {
	ID, CreatedAt, UpdatedAt, Actor string
	FromState, ToState              string
	FromAssignee, ToAssignee        string
	FromCycle, ToCycle              string
	FromProject, ToProject          string
	FromParent, ToParent            string
	FromTeam, ToTeam                string
	FromTitle, ToTitle              string
	FromEstimate, ToEstimate        *float64
	FromPriority, ToPriority        *float64
	AddedLabelIDs, RemovedLabelIDs  []string
	Archived                        *bool
}

func (h HistoryEntry) JSON() string {
	pair := func(m map[string]any, from, to, a, b string) {
		m[from], m[to] = orNull(a), orNull(b)
	}
	updated := h.UpdatedAt
	if updated == "" {
		updated = h.CreatedAt
	}
	m := map[string]any{
		"id": h.ID, "createdAt": h.CreatedAt, "updatedAt": updated, "actorId": orNull(h.Actor),
		"fromEstimate": h.FromEstimate, "toEstimate": h.ToEstimate, "fromPriority": h.FromPriority, "toPriority": h.ToPriority,
		"addedLabelIds": h.AddedLabelIDs, "removedLabelIds": h.RemovedLabelIDs, "archived": h.Archived,
	}
	pair(m, "fromStateId", "toStateId", h.FromState, h.ToState)
	pair(m, "fromAssigneeId", "toAssigneeId", h.FromAssignee, h.ToAssignee)
	pair(m, "fromCycleId", "toCycleId", h.FromCycle, h.ToCycle)
	pair(m, "fromProjectId", "toProjectId", h.FromProject, h.ToProject)
	pair(m, "fromParentId", "toParentId", h.FromParent, h.ToParent)
	pair(m, "fromTeamId", "toTeamId", h.FromTeam, h.ToTeam)
	pair(m, "fromTitle", "toTitle", h.FromTitle, h.ToTitle)
	return encode(m)
}

// IssueRef is a related issue as a detail or relation names it.
type IssueRef struct{ ID, Identifier, Title, State string }

func (r IssueRef) JSON() string {
	return encode(map[string]any{"id": r.ID, "identifier": r.Identifier, "title": r.Title, "state": ref(r.State)})
}

// RelationJSON is a relation: issue blocks, duplicates or relates to
// related.
func RelationJSON(id, kind string, issue, related IssueRef) string {
	return `{"id":"` + id + `","type":"` + kind + `","issue":` + issue.JSON() + `,"relatedIssue":` + related.JSON() + `}`
}

// Detail is what the single-issue read adds to an issue; each list is
// its first page.
type Detail struct {
	Parent                      *IssueRef
	Children                    []string
	MoreChildren                bool
	Comments                    []string
	MoreComments                bool
	CommentCursor               string
	Attachments                 []string
	MoreAttachments             bool
	Relations, InverseRelations []string
}

// DetailJSON is i with d, as the single-issue read answers it.
func DetailJSON(i Issue, d Detail) string {
	base := i.JSON()
	parent := "null"
	if d.Parent != nil {
		parent = d.Parent.JSON()
	}
	return base[:len(base)-1] + `,"parent":` + parent +
		`,"children":` + ConnectionJSON(d.Children, d.MoreChildren, "") +
		`,"comments":` + ConnectionJSON(d.Comments, d.MoreComments, d.CommentCursor) +
		`,"attachments":` + ConnectionJSON(d.Attachments, d.MoreAttachments, "") +
		`,"relations":` + ConnectionJSON(d.Relations, false, "") +
		`,"inverseRelations":` + ConnectionJSON(d.InverseRelations, false, "") + `}`
}

// IssueChangesJSON is one node of the batched history read: an issue's
// team, creation time and its newest history entries.
func IssueChangesJSON(id, team, createdAt string, entries []string, more bool, cursor string) string {
	return `{"id":"` + id + `","team":{"id":"` + team + `"},"createdAt":"` + createdAt + `","history":` +
		ConnectionJSON(entries, more, cursor) + `}`
}
