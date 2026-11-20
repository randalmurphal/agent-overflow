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

func (commentObj) typeName() string                { return "Comment" }
func (o commentObj) value(name string) (any, bool) { return commentFields.get(o, name) }

var commentFields = fieldTable[commentObj]{
	"id":    func(o commentObj) any { return o.c.ID },
	"issue": func(o commentObj) any { return o.w.issueRef(o.c.Issue) },
	"parent": func(o commentObj) any {
		if p := o.w.comment(o.c.Parent); p != nil {
			return commentObj{o.w, p}
		}
		return nil
	},
	"user": func(o commentObj) any { return o.w.userRef(o.c.User) },
	"body": func(o commentObj) any { return o.c.Body },
	"url": func(o commentObj) any {
		is := o.w.issue(o.c.Issue)
		return "https://linear.app/" + o.w.URLKey + "/issue/" + o.w.identifier(is) + "#comment-" + o.c.ID[:8]
	},
	"createdAt":  func(o commentObj) any { return o.c.CreatedAt },
	"updatedAt":  func(o commentObj) any { return o.c.UpdatedAt },
	"editedAt":   func(o commentObj) any { return o.c.EditedAt },
	"resolvedAt": func(o commentObj) any { return o.c.ResolvedAt },
}

type attachmentObj struct {
	w *linearWorkspace
	a *LinearAttachment
}

func (attachmentObj) typeName() string                { return "Attachment" }
func (o attachmentObj) value(name string) (any, bool) { return attachmentFields.get(o, name) }

var attachmentFields = fieldTable[attachmentObj]{
	"id":         func(o attachmentObj) any { return o.a.ID },
	"issue":      func(o attachmentObj) any { return o.w.issueRef(o.a.Issue) },
	"title":      func(o attachmentObj) any { return o.a.Title },
	"subtitle":   func(o attachmentObj) any { return stringOrNil(o.a.Subtitle) },
	"url":        func(o attachmentObj) any { return o.a.URL },
	"sourceType": func(o attachmentObj) any { return stringOrNil(o.a.SourceType) },
	"creator":    func(o attachmentObj) any { return o.w.userRef(o.a.Creator) },
	"createdAt":  func(o attachmentObj) any { return o.a.CreatedAt },
	"updatedAt":  func(o attachmentObj) any { return o.a.UpdatedAt },
}

type relationObj struct {
	w *linearWorkspace
	r *LinearRelation
}

func (relationObj) typeName() string                { return "IssueRelation" }
func (o relationObj) value(name string) (any, bool) { return relationFields.get(o, name) }

var relationFields = fieldTable[relationObj]{
	"id":           func(o relationObj) any { return o.r.ID },
	"type":         func(o relationObj) any { return o.r.Type },
	"issue":        func(o relationObj) any { return o.w.issueRef(o.r.Issue) },
	"relatedIssue": func(o relationObj) any { return o.w.issueRef(o.r.Related) },
}

type historyObj struct{ h *LinearHistory }

func (historyObj) typeName() string                { return "IssueHistory" }
func (o historyObj) value(name string) (any, bool) { return historyFields.get(o, name) }

// historyID is a history field holding an ID or text, null when unset.
func historyID(f func(h *LinearHistory) string) func(historyObj) any {
	return func(o historyObj) any { return stringOrNil(f(o.h)) }
}

// historyNum is a history field holding a number, null when unset.
func historyNum(f func(h *LinearHistory) *float64) func(historyObj) any {
	return func(o historyObj) any { return numOrNil(f(o.h)) }
}

var historyFields = fieldTable[historyObj]{
	"id":              historyID(func(h *LinearHistory) string { return h.ID }),
	"actorId":         historyID(func(h *LinearHistory) string { return h.Actor }),
	"fromStateId":     historyID(func(h *LinearHistory) string { return h.FromState }),
	"toStateId":       historyID(func(h *LinearHistory) string { return h.ToState }),
	"fromAssigneeId":  historyID(func(h *LinearHistory) string { return h.FromAssignee }),
	"toAssigneeId":    historyID(func(h *LinearHistory) string { return h.ToAssignee }),
	"fromCycleId":     historyID(func(h *LinearHistory) string { return h.FromCycle }),
	"toCycleId":       historyID(func(h *LinearHistory) string { return h.ToCycle }),
	"fromProjectId":   historyID(func(h *LinearHistory) string { return h.FromProject }),
	"toProjectId":     historyID(func(h *LinearHistory) string { return h.ToProject }),
	"fromParentId":    historyID(func(h *LinearHistory) string { return h.FromParent }),
	"toParentId":      historyID(func(h *LinearHistory) string { return h.ToParent }),
	"fromTeamId":      historyID(func(h *LinearHistory) string { return h.FromTeam }),
	"toTeamId":        historyID(func(h *LinearHistory) string { return h.ToTeam }),
	"fromTitle":       historyID(func(h *LinearHistory) string { return h.FromTitle }),
	"toTitle":         historyID(func(h *LinearHistory) string { return h.ToTitle }),
	"fromEstimate":    historyNum(func(h *LinearHistory) *float64 { return h.FromEstimate }),
	"toEstimate":      historyNum(func(h *LinearHistory) *float64 { return h.ToEstimate }),
	"fromPriority":    historyNum(func(h *LinearHistory) *float64 { return h.FromPriority }),
	"toPriority":      historyNum(func(h *LinearHistory) *float64 { return h.ToPriority }),
	"createdAt":       func(o historyObj) any { return o.h.CreatedAt },
	"updatedAt":       func(o historyObj) any { return o.h.CreatedAt },
	"addedLabelIds":   func(o historyObj) any { return listOrNil(o.h.AddedLabels) },
	"removedLabelIds": func(o historyObj) any { return listOrNil(o.h.RemovedLabels) },
	"archived": func(o historyObj) any {
		if o.h.Archived == nil {
			return nil
		}
		return *o.h.Archived
	},
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

func (spanObj) typeName() string                { return "IssueStateSpan" }
func (o spanObj) value(name string) (any, bool) { return spanFields.get(o, name) }

var spanFields = fieldTable[spanObj]{
	"id":        func(o spanObj) any { return o.id },
	"stateId":   func(o spanObj) any { return o.state },
	"startedAt": func(o spanObj) any { return o.start },
	"endedAt":   func(o spanObj) any { return o.end },
}
