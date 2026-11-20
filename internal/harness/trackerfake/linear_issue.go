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
	switch field {
	case "updatedAt":
		return float64(o.is.UpdatedAt.UnixMilli()), true
	case "createdAt":
		return float64(o.is.CreatedAt.UnixMilli()), true
	case "manual":
		return o.is.SortOrder, true
	case "priority":
		// No priority sorts after low (unverified).
		if o.is.Priority == 0 {
			return 5, true
		}
		return o.is.Priority, true
	}
	return 0, false
}

func (o issueObj) value(name string) (any, bool) {
	is, w := o.is, o.w
	switch name {
	case "id":
		return is.ID, true
	case "identifier":
		return w.identifier(is), true
	case "number":
		return is.Number, true
	case "title":
		return is.Title, true
	case "description":
		return stringOrNil(is.Description), true
	case "url":
		return "https://linear.app/" + w.URLKey + "/issue/" + w.identifier(is) + "/" + slug(is.Title), true
	case "branchName":
		return strings.ToLower(w.identifier(is)) + "-" + slug(is.Title), true
	case "priority":
		return is.Priority, true
	case "estimate":
		return numOrNil(is.Estimate), true
	case "sortOrder":
		return is.SortOrder, true
	case "prioritySortOrder":
		return is.PrioritySortOrder, true
	case "subIssueSortOrder":
		return numOrNil(is.SubIssueSortOrder), true
	case "team":
		return w.teamRef(is.Team), true
	case "state":
		return w.stateRef(is.State), true
	case "assignee":
		return w.userRef(is.Assignee), true
	case "creator":
		return w.userRef(is.Creator), true
	case "cycle":
		return w.cycleRef(is.Cycle), true
	case "project":
		return w.projectRef(is.Project), true
	case "parent":
		return w.issueRef(is.Parent), true
	case "labelIds":
		return append([]string{}, is.Labels...), true
	case "labels":
		var out []gqlObject
		for _, id := range is.Labels {
			out = append(out, labelObj{w, w.label(id)})
		}
		return out, true
	case "dueDate":
		return stringOrNil(is.DueDate), true
	case "createdAt":
		return is.CreatedAt, true
	case "updatedAt":
		return is.UpdatedAt, true
	case "startedAt":
		return is.StartedAt, true
	case "completedAt":
		return is.CompletedAt, true
	case "canceledAt":
		return is.CanceledAt, true
	case "archivedAt":
		return is.ArchivedAt, true
	case "trashed":
		if !is.Trashed {
			return nil, true
		}
		return true, true
	}
	return o.related(name)
}

// related is the issue's connections to other records.
func (o issueObj) related(name string) (any, bool) {
	is, w := o.is, o.w
	var out []gqlObject
	switch name {
	case "children":
		for i := range w.Issues {
			if w.Issues[i].Parent == is.ID {
				out = append(out, issueObj{w: w, is: &w.Issues[i]})
			}
		}
	case "comments":
		for i := range w.Comments {
			if w.Comments[i].Issue == is.ID {
				out = append(out, commentObj{w, &w.Comments[i]})
			}
		}
	case "attachments":
		for i := range w.Attachments {
			if w.Attachments[i].Issue == is.ID {
				out = append(out, attachmentObj{w, &w.Attachments[i]})
			}
		}
	case "relations", "inverseRelations":
		for i := range w.Relations {
			r := &w.Relations[i]
			if (name == "relations" && r.Issue == is.ID) || (name == "inverseRelations" && r.Related == is.ID) {
				out = append(out, relationObj{w, r})
			}
		}
	case "history":
		for i := range is.History {
			out = append(out, historyObj{&is.History[i]})
		}
	case "stateHistory":
		for _, sp := range w.stateSpans(is) {
			out = append(out, sp)
		}
	default:
		return nil, false
	}
	return out, true
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
