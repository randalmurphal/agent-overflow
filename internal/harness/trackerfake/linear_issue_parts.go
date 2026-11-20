package trackerfake

import (
	"slices"
	"time"
)

// The objects that hang off an issue: its comments, attachments,
// relations, history entries and state spans.

type commentObj struct {
	w *linearWorkspace
	c *LinearComment
}

func (commentObj) typeName() string { return "Comment" }

func (o commentObj) value(name string) (any, bool) {
	c := o.c
	switch name {
	case "id":
		return c.ID, true
	case "issue":
		return o.w.issueRef(c.Issue), true
	case "parent":
		if p := o.w.comment(c.Parent); p != nil {
			return commentObj{o.w, p}, true
		}
		return nil, true
	case "user":
		return o.w.userRef(c.User), true
	case "body":
		return c.Body, true
	case "url":
		is := o.w.issue(c.Issue)
		return "https://linear.app/" + o.w.URLKey + "/issue/" + o.w.identifier(is) + "#comment-" + c.ID[:8], true
	case "createdAt":
		return c.CreatedAt, true
	case "updatedAt":
		return c.UpdatedAt, true
	case "editedAt":
		return c.EditedAt, true
	case "resolvedAt":
		return c.ResolvedAt, true
	}
	return nil, false
}

type attachmentObj struct {
	w *linearWorkspace
	a *LinearAttachment
}

func (attachmentObj) typeName() string { return "Attachment" }

func (o attachmentObj) value(name string) (any, bool) {
	a := o.a
	switch name {
	case "id":
		return a.ID, true
	case "issue":
		return o.w.issueRef(a.Issue), true
	case "title":
		return a.Title, true
	case "subtitle":
		return stringOrNil(a.Subtitle), true
	case "url":
		return a.URL, true
	case "sourceType":
		return stringOrNil(a.SourceType), true
	case "creator":
		return o.w.userRef(a.Creator), true
	case "createdAt":
		return a.CreatedAt, true
	case "updatedAt":
		return a.UpdatedAt, true
	}
	return nil, false
}

type relationObj struct {
	w *linearWorkspace
	r *LinearRelation
}

func (relationObj) typeName() string { return "IssueRelation" }

func (o relationObj) value(name string) (any, bool) {
	switch name {
	case "id":
		return o.r.ID, true
	case "type":
		return o.r.Type, true
	case "issue":
		return o.w.issueRef(o.r.Issue), true
	case "relatedIssue":
		return o.w.issueRef(o.r.Related), true
	}
	return nil, false
}

type historyObj struct{ h *LinearHistory }

func (historyObj) typeName() string { return "IssueHistory" }

func (o historyObj) value(name string) (any, bool) {
	h := o.h
	ids := map[string]string{
		"actorId": h.Actor, "fromStateId": h.FromState, "toStateId": h.ToState, "fromAssigneeId": h.FromAssignee,
		"toAssigneeId": h.ToAssignee, "fromCycleId": h.FromCycle, "toCycleId": h.ToCycle, "fromProjectId": h.FromProject,
		"toProjectId": h.ToProject, "fromParentId": h.FromParent, "toParentId": h.ToParent, "fromTeamId": h.FromTeam,
		"toTeamId": h.ToTeam, "fromTitle": h.FromTitle, "toTitle": h.ToTitle, "id": h.ID,
	}
	if v, ok := ids[name]; ok {
		return stringOrNil(v), true
	}
	nums := map[string]*float64{"fromEstimate": h.FromEstimate, "toEstimate": h.ToEstimate, "fromPriority": h.FromPriority, "toPriority": h.ToPriority}
	if v, ok := nums[name]; ok {
		return numOrNil(v), true
	}
	switch name {
	case "createdAt", "updatedAt":
		return h.CreatedAt, true
	case "addedLabelIds":
		return listOrNil(h.AddedLabels), true
	case "removedLabelIds":
		return listOrNil(h.RemovedLabels), true
	case "archived":
		if h.Archived == nil {
			return nil, true
		}
		return *h.Archived, true
	}
	return nil, false
}

func numOrNil(n *float64) any {
	if n == nil {
		return nil
	}
	return *n
}

func listOrNil(list []string) any {
	if len(list) == 0 {
		return nil
	}
	return slices.Clone(list)
}

type spanObj struct {
	id, state  string
	start, end time.Time
}

func (spanObj) typeName() string { return "IssueStateSpan" }

func (o spanObj) value(name string) (any, bool) {
	switch name {
	case "id":
		return o.id, true
	case "stateId":
		return o.state, true
	case "startedAt":
		return o.start, true
	case "endedAt":
		return o.end, true
	}
	return nil, false
}
