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

// quoted is s as a JSON string.
func quoted(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // a string always encodes
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
		"progress": 0.5,
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
// CreatedAt. Names holds what a ticket read nests of the entities the
// entry names, by field (fromAssignee, addedLabels, ...), each as JSON; a
// field it lacks is null.
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
	Names                           map[string]string
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
	for field, v := range h.Names {
		m[field] = json.RawMessage(v)
	}
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
	return `{"id":` + quoted(id) + `,"type":` + quoted(kind) + `,"issue":` + issue.JSON() + `,"relatedIssue":` + related.JSON() + `}`
}

// Detail is what the single-issue read adds to an issue; each list is
// its first page. A relation cursor, when set, says that page has more.
type Detail struct {
	// ProjectName is the name of the issue's project, when it has one.
	ProjectName                 string
	Parent                      *IssueRef
	Children                    []string
	MoreChildren                bool
	Comments                    []string
	MoreComments                bool
	CommentCursor               string
	Attachments                 []string
	MoreAttachments             bool
	Relations, InverseRelations []string
	RelationCursor              string
	InverseRelationCursor       string
	// History is the newest entries; a history cursor, when set, says
	// more follow. States are the team's states (State JSON), and
	// MoreStates says the team has more.
	History       []string
	HistoryCursor string
	States        []string
	MoreStates    bool
}

// DetailJSON is i with d, as the single-issue read answers it: one
// member for each response key, the project and parent the read selects
// twice merged as GraphQL merges them.
func DetailJSON(i Issue, d Detail) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(i.JSON()), &fields); err != nil {
		panic(err) // Issue.JSON is an object
	}
	fields["parent"] = json.RawMessage("null")
	if d.Parent != nil {
		fields["parent"] = json.RawMessage(d.Parent.JSON())
	}
	fields["project"] = json.RawMessage("null")
	if i.Project != "" {
		fields["project"] = json.RawMessage(encode(map[string]any{"id": i.Project, "name": d.ProjectName}))
	}
	b, err := json.Marshal(fields)
	if err != nil {
		panic(err) // every member is JSON
	}
	base := string(b)
	return base[:len(base)-1] +
		`,"children":` + ConnectionJSON(d.Children, d.MoreChildren, "") +
		`,"comments":` + ConnectionJSON(d.Comments, d.MoreComments, d.CommentCursor) +
		`,"attachments":` + ConnectionJSON(d.Attachments, d.MoreAttachments, "") +
		`,"relations":` + ConnectionJSON(d.Relations, d.RelationCursor != "", d.RelationCursor) +
		`,"inverseRelations":` + ConnectionJSON(d.InverseRelations, d.InverseRelationCursor != "", d.InverseRelationCursor) +
		`,"history":` + ConnectionJSON(d.History, d.HistoryCursor != "", d.HistoryCursor) +
		`,"stateTeam":{"states":` + ConnectionJSON(d.States, d.MoreStates, "") + `}}`
}

// IssueChangesJSON is one node of the batched history read: an issue's
// team, creation time and its newest history entries.
func IssueChangesJSON(id, team, createdAt string, entries []string, more bool, cursor string) string {
	return `{"id":` + quoted(id) + `,"team":{"id":` + quoted(team) + `},"createdAt":` + quoted(createdAt) + `,"history":` +
		ConnectionJSON(entries, more, cursor) + `}`
}
