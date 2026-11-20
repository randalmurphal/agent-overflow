package trackerfake

import (
	"fmt"
	"net/http"
	"slices"
)

// indexed is issues without those the index does not show yet (see
// JiraIssue.Indexed). Callers hold mu.
func (c *jiraCall) indexed(issues []*JiraIssue) []*JiraIssue {
	now := c.e.now()
	return slices.DeleteFunc(issues, func(is *JiraIssue) bool { return now.Before(is.Indexed) })
}

// listIssues are the issues a Jira Software list holds, in rank order,
// or its not-found answer. A board's backlog is its issues in no open
// sprint and not in its done column (unverified for the done column).
func (c *jiraCall) listIssues(backlog bool) ([]*JiraIssue, *answer) {
	out, nf := c.listed(backlog)
	if nf != nil {
		return nil, nf
	}
	return c.indexed(out), nil
}

func (c *jiraCall) listed(backlog bool) ([]*JiraIssue, *answer) {
	var b *JiraBoard
	if c.params["board"] != "" {
		var nf *answer
		if b, nf = c.foundBoard(); nf != nil {
			return nil, nf
		}
	}
	var sp *JiraSprint
	if c.params["sprint"] != "" {
		var nf *answer
		if sp, nf = c.foundSprint(); nf != nil {
			return nil, nf
		}
	}
	var out []*JiraIssue
	switch {
	case c.params["epic"] != "":
		epic := c.s.issue(c.params["epic"])
		if epic == nil {
			a := jiraErrors(http.StatusNotFound, issueNotFound)
			return nil, &a
		}
		if c.s.project(epic.Project).issueType(epic.Type).HierarchyLevel != 1 {
			a := jiraErrors(http.StatusBadRequest, "Issue "+epic.Key+" is not an epic.")
			return nil, &a
		}
		for i := range c.s.Issues {
			if is := &c.s.Issues[i]; is.Parent == epic.ID {
				out = append(out, is)
			}
		}
		sortByRank(out)
	case b != nil:
		out = c.s.boardIssues(b)
	default:
		for i := range c.s.Issues {
			out = append(out, &c.s.Issues[i])
		}
		sortByRank(out)
	}
	if sp != nil {
		out = slices.DeleteFunc(out, func(is *JiraIssue) bool { return !slices.Contains(is.Sprints, sp.ID) })
	}
	if backlog {
		done := b.doneStatuses()
		out = slices.DeleteFunc(out, func(is *JiraIssue) bool {
			return slices.ContainsFunc(is.Sprints, c.s.openSprint) || slices.Contains(done, is.Status)
		})
	}
	return out, nil
}

// filtered narrows issues by the jql parameter; an ORDER BY in it
// replaces rank order.
func (c *jiraCall) filtered(issues []*JiraIssue, strictSprints bool) ([]*JiraIssue, *answer) {
	src := c.query.Get("jql")
	if src == "" {
		return issues, nil
	}
	q, err := parseJQL(src)
	if err != nil {
		a := jqlAnswer(err)
		return nil, &a
	}
	// Measured: a sprint ID that names nothing answers 400 on the board
	// issue list (strictSprints) and matches nothing on search/jql. The
	// message is unverified.
	for _, id := range q.sprints {
		if strictSprints && c.s.sprint(id) == nil {
			a := jiraErrors(http.StatusBadRequest, fmt.Sprintf("The value '%d' does not exist for the field 'sprint'.", id))
			return nil, &a
		}
	}
	issues = slices.DeleteFunc(slices.Clone(issues), func(is *JiraIssue) bool { return !q.where(c.s, is) })
	if len(q.order) > 0 {
		sortIssues(issues, q.order)
	}
	return issues, nil
}

// softwareList answers a token-paged list; backlog selects a board's
// backlog. A page holds what search would, except on the board issue
// list (unclamped), which holds up to 5000 whatever the fields (measured:
// pages above 100 are honored, every issue of a 2,687-issue board in one
// page with ten card fields, and 500 IDs in one id in (...) clause all
// returned). The other lists' clamp is unverified.
func softwareList(backlog, unclamped bool) func(c *jiraCall) answer {
	return func(c *jiraCall) answer { return c.softwareList(backlog, unclamped) }
}

func (c *jiraCall) softwareList(backlog, unclamped bool) answer {
	issues, nf := c.listIssues(backlog)
	if nf == nil {
		issues, nf = c.filtered(issues, unclamped)
	}
	if nf != nil {
		return *nf
	}
	scope := c.x.r.URL.Path + " " + c.query.Get("jql")
	offset, err := readPageToken(scope, c.query.Get("nextPageToken"))
	if err != nil {
		return jiraErrors(http.StatusBadRequest, "The provided next page token is invalid or expired.")
	}
	asked, err := c.intParam("maxResults", 50)
	if err != nil {
		return unhandledf("%v", err)
	}
	sel := parseFields(c.query["fields"], selectNavigable)
	size := searchSize(asked, sel, c.s)
	if unclamped {
		size = searchSize(asked, selectNone, c.s)
	}
	page, next, last := c.tokenPage(issues, scope, offset, size, sel)
	out := map[string]any{"issues": page, "isLast": last, "warningMessages": []string{}}
	if next != "" {
		out["nextPageToken"] = next
	}
	return jsonAnswer(http.StatusOK, out)
}

// softwareCount answers a list's approximate count.
func softwareCount(backlog bool) func(c *jiraCall) answer {
	return func(c *jiraCall) answer { return c.softwareCount(backlog) }
}

func (c *jiraCall) softwareCount(backlog bool) answer {
	issues, nf := c.listIssues(backlog)
	if nf == nil {
		issues, nf = c.filtered(issues, false)
	}
	if nf != nil {
		return *nf
	}
	return jsonAnswer(http.StatusOK, map[string]any{"count": len(issues)})
}
