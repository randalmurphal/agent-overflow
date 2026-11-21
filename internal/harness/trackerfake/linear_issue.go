package trackerfake

import (
	"strconv"
	"strings"
	"time"
)

// issueObj is an issue, read as an Issue or, from a search, as an
// IssueSearchResult with the same fields.
type issueObj struct {
	w      *linearWorkspace
	is     *LinearIssue
	search bool
}

func (o issueObj) typeName() string {
	if o.search {
		return "IssueSearchResult"
	}
	return "Issue"
}

// archived is an archived or trashed issue, left out of a list unless
// includeArchived (measured).
func (o issueObj) archived() bool { return !o.is.ArchivedAt.IsZero() || o.is.Trashed }

func (o issueObj) sortKey(field string) (float64, bool) {
	f, ok := issueSorts[field]
	if !ok {
		return 0, false
	}
	return f(o.is), true
}

func (o issueObj) value(name string) (any, bool) { return issueFields.get(o, name) }

var issueFields = fieldTable[issueObj]{
	"id":         func(o issueObj) any { return o.is.ID },
	"identifier": func(o issueObj) any { return o.w.identifier(o.is) },
	"number":     func(o issueObj) any { return o.is.Number },
	"title":      func(o issueObj) any { return o.is.Title },
	"description": func(o issueObj) any {
		return stringOrNil(o.is.Description)
	},
	"url": func(o issueObj) any {
		return "https://linear.app/" + o.w.URLKey + "/issue/" + o.w.identifier(o.is) + "/" + slug(o.is.Title)
	},
	"branchName":        func(o issueObj) any { return strings.ToLower(o.w.identifier(o.is)) + "-" + slug(o.is.Title) },
	"priority":          func(o issueObj) any { return o.is.Priority },
	"estimate":          func(o issueObj) any { return numOrNil(o.is.Estimate) },
	"sortOrder":         func(o issueObj) any { return o.is.SortOrder },
	"prioritySortOrder": func(o issueObj) any { return o.is.PrioritySortOrder },
	"subIssueSortOrder": func(o issueObj) any { return numOrNil(o.is.SubIssueSortOrder) },
	"team":              func(o issueObj) any { return o.w.teamRef(o.is.Team) },
	"state":             func(o issueObj) any { return o.w.stateRef(o.is.State) },
	"assignee":          func(o issueObj) any { return o.w.userRef(o.is.Assignee) },
	"creator":           func(o issueObj) any { return o.w.userRef(o.is.Creator) },
	"cycle":             func(o issueObj) any { return o.w.cycleRef(o.is.Cycle) },
	"project":           func(o issueObj) any { return o.w.projectRef(o.is.Project) },
	"parent":            func(o issueObj) any { return o.w.issueRef(o.is.Parent) },
	"labelIds":          func(o issueObj) any { return append([]string{}, o.is.Labels...) },
	"labels": func(o issueObj) any {
		var out []gqlObject
		for _, id := range o.is.Labels {
			out = append(out, labelObj{o.w, o.w.label(id)})
		}
		return out
	},
	"dueDate":     func(o issueObj) any { return stringOrNil(o.is.DueDate) },
	"createdAt":   func(o issueObj) any { return o.is.CreatedAt },
	"updatedAt":   func(o issueObj) any { return o.is.UpdatedAt },
	"startedAt":   func(o issueObj) any { return o.is.StartedAt },
	"completedAt": func(o issueObj) any { return o.is.CompletedAt },
	"canceledAt":  func(o issueObj) any { return o.is.CanceledAt },
	"archivedAt":  func(o issueObj) any { return o.is.ArchivedAt },
	"trashed": func(o issueObj) any {
		if !o.is.Trashed {
			return nil
		}
		return true
	},
	// documented: the identifiers an issue had before it moved team.
	"previousIdentifiers": func(o issueObj) any { return append([]string{}, o.is.FormerKeys...) },
	"children": func(o issueObj) any {
		var out []gqlObject
		for i := range o.w.Issues {
			if o.w.Issues[i].Parent == o.is.ID {
				out = append(out, issueObj{w: o.w, is: &o.w.Issues[i]})
			}
		}
		return out
	},
	"comments": func(o issueObj) any {
		var out []gqlObject
		for i := range o.w.Comments {
			if o.w.Comments[i].Issue == o.is.ID {
				out = append(out, commentObj{o.w, &o.w.Comments[i]})
			}
		}
		return out
	},
	"attachments": func(o issueObj) any {
		var out []gqlObject
		for i := range o.w.Attachments {
			if o.w.Attachments[i].Issue == o.is.ID {
				out = append(out, attachmentObj{o.w, &o.w.Attachments[i]})
			}
		}
		return out
	},
	"relations":        func(o issueObj) any { return o.relations(func(r *LinearRelation) string { return r.Issue }) },
	"inverseRelations": func(o issueObj) any { return o.relations(func(r *LinearRelation) string { return r.Related }) },
	"history": func(o issueObj) any {
		var out []gqlObject
		for i := range o.is.History {
			out = append(out, historyObj{&o.is.History[i]})
		}
		return out
	},
	"stateHistory": func(o issueObj) any {
		var out []gqlObject
		for _, sp := range o.w.stateSpans(o.is) {
			out = append(out, sp)
		}
		return out
	},
}

// relations are the relations whose end (side) is the issue.
func (o issueObj) relations(side func(*LinearRelation) string) []gqlObject {
	var out []gqlObject
	for i := range o.w.Relations {
		if r := &o.w.Relations[i]; side(r) == o.is.ID {
			out = append(out, relationObj{o.w, r})
		}
	}
	return out
}

// stateSpans are the intervals the issue spent in each state, from its
// creation through its history's state changes; the last is open.
func (w *linearWorkspace) stateSpans(is *LinearIssue) []spanObj {
	type change struct {
		at       time.Time
		from, to string
	}
	var changes []change
	for _, h := range is.History {
		if h.ToState != "" {
			changes = append(changes, change{h.CreatedAt, h.FromState, h.ToState})
		}
	}
	state := is.State
	if len(changes) > 0 {
		state = changes[0].from
	}
	start := is.CreatedAt
	var out []spanObj
	for i, c := range changes {
		out = append(out, spanObj{id: is.ID + "-span-" + strconv.Itoa(i), state: state, start: start, end: c.at})
		state, start = c.to, c.at
	}
	return append(out, spanObj{id: is.ID + "-span-" + strconv.Itoa(len(changes)), state: state, start: start})
}
