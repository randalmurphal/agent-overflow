package trackerfake

import (
	"net/http"
	"slices"
	"strings"
)

// moveBody is a rank, sprint move or backlog move.
type moveBody struct {
	Issues            []string `json:"issues"`
	RankBeforeIssue   string   `json:"rankBeforeIssue"`
	RankAfterIssue    string   `json:"rankAfterIssue"`
	RankCustomFieldID int64    `json:"rankCustomFieldId"`
}

// maxMoved is the most issues one rank or move takes (documented).
const maxMoved = 50

// read checks a move: its issues, at most one anchor, the rank field. It
// returns the issues found and a 207 entry for each one not found.
func (c *jiraCall) readMove(in moveBody, ranked bool) ([]*JiraIssue, []any, *answer) {
	refuse := func(msg string) ([]*JiraIssue, []any, *answer) {
		a := jiraErrors(http.StatusBadRequest, msg)
		return nil, nil, &a
	}
	switch {
	case len(in.Issues) == 0 || len(in.Issues) > maxMoved:
		return refuse("Between 1 and 50 issues can be moved at once.")
	case !ranked && (in.RankBeforeIssue != "" || in.RankAfterIssue != "" || in.RankCustomFieldID != 0):
		return refuse("This move does not rank.")
	case in.RankBeforeIssue != "" && in.RankAfterIssue != "":
		return refuse("Only one of rankBeforeIssue and rankAfterIssue can be given.")
	case in.RankCustomFieldID != 0 && in.RankCustomFieldID != c.s.fieldOfType(rankFieldType).Schema.CustomID:
		return refuse("The rank field is not a rank field of the site.")
	}
	if anchor := in.RankBeforeIssue + in.RankAfterIssue; anchor != "" && c.s.issue(anchor) == nil {
		return refuse("The issue to rank against does not exist.")
	}
	var found []*JiraIssue
	var missing []any
	for _, ref := range in.Issues {
		if is := c.s.issue(ref); is != nil {
			found = append(found, is)
			continue
		}
		missing = append(missing, map[string]any{"issueKey": ref, "status": http.StatusNotFound, "errors": []string{issueNotFound}})
	}
	return found, missing, nil
}

// moveAnswer is 204 when every issue moved, or a 207 naming each one.
func moveAnswer(found []*JiraIssue, missing []any) answer {
	if len(missing) == 0 {
		return emptyAnswer(http.StatusNoContent)
	}
	entries := []any{}
	for _, is := range found {
		entries = append(entries, map[string]any{"issueId": idNumber(is.ID), "issueKey": is.Key, "status": http.StatusOK})
	}
	return jsonAnswer(http.StatusMultiStatus, map[string]any{"entries": append(entries, missing...)})
}

// rankAt places issues, in order, before or after the anchor of in; no
// anchor leaves the ranks as they are. Each issue whose rank changes
// records a Rank change.
func (c *jiraCall) rankAt(issues []*JiraIssue, in moveBody) {
	anchorRef := in.RankBeforeIssue + in.RankAfterIssue
	if anchorRef == "" {
		return
	}
	anchor := c.s.issue(anchorRef)
	moving := map[string]bool{}
	for _, is := range issues {
		moving[is.ID] = true
	}
	var order []*JiraIssue
	for i := range c.s.Issues {
		if is := &c.s.Issues[i]; !moving[is.ID] || is.ID == anchor.ID {
			order = append(order, is)
		}
	}
	sortByRank(order)
	at := slices.Index(order, anchor)
	lo, hi := "", strings.TrimPrefix(anchor.Rank, rankPrefix)
	if in.RankBeforeIssue != "" {
		if at > 0 {
			lo = strings.TrimPrefix(order[at-1].Rank, rankPrefix)
		}
	} else {
		lo, hi = hi, ""
		if at+1 < len(order) {
			hi = strings.TrimPrefix(order[at+1].Rank, rankPrefix)
		}
	}
	for _, is := range issues {
		if is.ID == anchor.ID {
			continue
		}
		pos := midRank(lo, hi)
		higher := rankPrefix+pos < is.Rank
		is.Rank, lo = rankPrefix+pos, pos
		text := "Ranked lower"
		if higher {
			text = "Ranked higher"
		}
		// Measured: Rank is a custom field whose from, to and fromString
		// are empty strings, the text in toString (its wording is the
		// fake's, unverified).
		f := c.s.fieldOfType(rankFieldType)
		empty := ""
		c.changed(is, []JiraChangeItem{{FieldID: f.ID, Field: "Rank", FieldType: "custom", From: &empty, To: &empty, FromString: &empty, ToString: &text}})
	}
}

func rankIssues(c *jiraCall) answer {
	var in moveBody
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	if in.RankBeforeIssue == "" && in.RankAfterIssue == "" {
		return jiraErrors(http.StatusBadRequest, "One of rankBeforeIssue and rankAfterIssue is required.")
	}
	found, missing, refused := c.readMove(in, true)
	if refused != nil {
		return *refused
	}
	c.rankAt(found, in)
	return moveAnswer(found, missing)
}

// moveToSprint moves issues into a future or active sprint, out of any
// other open sprint (documented).
func moveToSprint(c *jiraCall) answer {
	var in moveBody
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	sp, nf := c.foundSprint()
	if nf != nil {
		return *nf
	}
	if sp.State == "closed" {
		return jiraErrors(http.StatusBadRequest, "Issues can only be moved into a future or active sprint.")
	}
	found, missing, refused := c.readMove(in, true)
	if refused != nil {
		return *refused
	}
	if len(missing) > 0 {
		return jiraErrors(http.StatusBadRequest, "Issue "+missing[0].(map[string]any)["issueKey"].(string)+" does not exist.")
	}
	for _, is := range found {
		before := cloneIssue(*is)
		is.Sprints = slices.DeleteFunc(is.Sprints, c.s.openSprint)
		is.Sprints = append(is.Sprints, sp.ID)
		c.sprintChanged(is, &before)
	}
	c.rankAt(found, in)
	return emptyAnswer(http.StatusNoContent)
}

// moveToBacklog takes issues out of their open sprints; its closed
// sprints stay on an issue.
func moveToBacklog(c *jiraCall) answer {
	var in moveBody
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	board := c.params["board"] != ""
	if board {
		if _, nf := c.foundBoard(); nf != nil {
			return *nf
		}
	}
	found, missing, refused := c.readMove(in, board)
	if refused != nil {
		return *refused
	}
	if !board && len(missing) > 0 {
		return jiraErrors(http.StatusBadRequest, "Issue "+missing[0].(map[string]any)["issueKey"].(string)+" does not exist.")
	}
	for _, is := range found {
		before := cloneIssue(*is)
		is.Sprints = slices.DeleteFunc(is.Sprints, c.s.openSprint)
		c.sprintChanged(is, &before)
	}
	c.rankAt(found, in)
	return moveAnswer(found, missing)
}
