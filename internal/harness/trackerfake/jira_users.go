package trackerfake

import (
	"net/http"
	"slices"
	"strings"
)

// userWindow is the users keep accepts in the window startAt and
// maxResults selects, up to the thousandth user. filterFirst windows the
// accepted users (user/search); otherwise the window is over every user
// and filtered after (assignable search), so a short page does not mean
// the end. Both are documented.
func (c *jiraCall) userWindow(filterFirst bool, keep func(u *JiraUser) bool) answer {
	start, size, err := c.pageWindow(50, 1000)
	if err != nil {
		return unhandledf("%v", err)
	}
	var users []*JiraUser
	for i := range c.s.Users {
		if u := &c.s.Users[i]; !filterFirst || keep(u) {
			users = append(users, u)
		}
	}
	out := []any{}
	page, _ := offsetWindow(users[:min(len(users), 1000)], start, size)
	for _, u := range page {
		if filterFirst || keep(u) {
			out = append(out, c.s.userJSON(u.AccountID))
		}
	}
	return jsonAnswer(http.StatusOK, out)
}

// userMatches reports whether query (case ignored) starts the user's
// display name, a word of it, or their email (documented: a prefix of
// the attribute's value), or is their account ID.
func userMatches(u *JiraUser, query, account string) bool {
	if account != "" {
		return u.AccountID == account
	}
	q := strings.ToLower(query)
	if strings.HasPrefix(strings.ToLower(u.Email), q) || strings.HasPrefix(strings.ToLower(u.DisplayName), q) {
		return true
	}
	return slices.ContainsFunc(strings.Fields(strings.ToLower(u.DisplayName)), func(w string) bool { return strings.HasPrefix(w, q) })
}

func myself(c *jiraCall) answer { return jsonAnswer(http.StatusOK, c.s.userJSON(c.viewer)) }

func getUser(c *jiraCall) answer {
	u := c.s.user(c.query.Get("accountId"))
	if u == nil {
		return jiraErrors(http.StatusNotFound, "Specified user does not exist or you do not have required permissions")
	}
	return jsonAnswer(http.StatusOK, c.s.userJSON(u.AccountID))
}

func searchUsers(c *jiraCall) answer {
	query, account := c.query.Get("query"), c.query.Get("accountId")
	if (query == "") == (account == "") {
		return jiraErrors(http.StatusBadRequest, "One of 'query' or 'accountId' parameters must be specified.")
	}
	return c.userWindow(true, func(u *JiraUser) bool { return !u.Inactive && userMatches(u, query, account) })
}

func assignableUsers(c *jiraCall) answer {
	query, account := c.query.Get("query"), c.query.Get("accountId")
	if query != "" && account != "" {
		return jiraErrors(http.StatusBadRequest, "Cannot specify both query and accountId.")
	}
	var p *JiraProject
	switch ref := c.query.Get("issueKey") + c.query.Get("issueId"); {
	case ref != "" && c.query.Get("project") == "":
		is := c.s.issue(ref)
		if is == nil {
			return jiraErrors(http.StatusNotFound, issueNotFound)
		}
		p = c.s.project(is.Project)
	case ref == "" && c.query.Get("project") != "":
		if p = c.s.project(c.query.Get("project")); p == nil {
			return jiraErrors(http.StatusNotFound, "No project could be found with key '"+c.query.Get("project")+"'.")
		}
	default:
		return jiraErrors(http.StatusBadRequest, "Exactly one of the issueKey, issueId and project parameters must be specified.")
	}
	return c.userWindow(false, func(u *JiraUser) bool {
		return c.s.assignable(p, u.AccountID) && (query+account == "" || userMatches(u, query, account))
	})
}
