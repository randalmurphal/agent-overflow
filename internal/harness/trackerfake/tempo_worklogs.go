package trackerfake

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// tempoWorklogJSON is a worklog as Tempo reads it back.
func (c *tempoCall) worklogJSON(w *TempoWorklog) map[string]any {
	id := strconv.FormatInt(w.ID, 10)
	values := []any{}
	for _, a := range w.Attributes {
		values = append(values, map[string]any{"key": a.Key, "value": a.Value})
	}
	out := map[string]any{
		"self": tempoBase + "worklogs/" + id, "tempoWorklogId": w.ID,
		"issue":     map[string]any{"self": "https://" + c.s.Site + "/rest/api/2/issue/" + w.Issue, "id": idNumber(w.Issue)},
		"author":    map[string]any{"self": "https://" + c.s.Site + "/rest/api/2/user?accountId=" + w.Author, "accountId": w.Author},
		"startDate": w.StartDate, "startTime": w.StartTime, "startDateTimeUtc": c.startUTC(w).Format(time.RFC3339),
		"timeSpentSeconds": w.Spent, "billableSeconds": w.Spent, "description": w.Description,
		"attributes": map[string]any{"self": tempoBase + "worklogs/" + id + "/work-attribute-values", "values": values},
		"createdAt":  w.Created.UTC().Format(time.RFC3339), "updatedAt": w.Updated.UTC().Format(time.RFC3339),
	}
	if w.Billable != nil {
		out["billableSeconds"] = *w.Billable
	}
	return out
}

// startUTC is a worklog's start in UTC: its date and time in the author's
// time zone.
func (c *tempoCall) startUTC(w *TempoWorklog) time.Time {
	loc := time.UTC
	if u := c.s.user(w.Author); u != nil {
		if l, err := time.LoadLocation(u.TimeZone); err == nil {
			loc = l
		}
	}
	t, _ := time.ParseInLocation(time.DateOnly+" 15:04:05", w.StartDate+" "+w.StartTime, loc)
	return t.UTC()
}

func (t *TempoAccount) worklog(id string) *TempoWorklog {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil
	}
	for i := range t.Worklogs {
		if t.Worklogs[i].ID == n {
			return &t.Worklogs[i]
		}
	}
	return nil
}

func tempoUserWorklogs(c *tempoCall) answer {
	from, to, bad := c.dateRange()
	if bad != nil {
		return *bad
	}
	var since time.Time
	if raw := c.query.Get("updatedFrom"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return tempoErrors(http.StatusBadRequest, "updatedFrom must be a date-time (yyyy-MM-ddTHH:mm:ssZ)")
		}
		since = t
	}
	offset, limit, err := c.window()
	if err != nil {
		return unhandledf("%v", err)
	}
	var found []*TempoWorklog
	for i := range c.t.Worklogs {
		w := &c.t.Worklogs[i]
		if w.Author == c.params["account"] && from <= w.StartDate && w.StartDate <= to && !w.Updated.Before(since) {
			found = append(found, w)
		}
	}
	slices.SortStableFunc(found, func(a, b *TempoWorklog) int {
		return strings.Compare(a.StartDate+a.StartTime+fmt.Sprintf("%020d", a.ID), b.StartDate+b.StartTime+fmt.Sprintf("%020d", b.ID))
	})
	values := make([]any, len(found))
	for i, w := range found {
		values[i] = c.worklogJSON(w)
	}
	return c.page(values, offset, limit)
}

// tempoSearchWorklogs answers a worklog search by issue IDs and an
// optional first start date, every author's worklogs in the account, by
// start and ID: the spec's default order. That a token sees other
// authors' worklogs here is the fake's model, not a measurement.
func tempoSearchWorklogs(c *tempoCall) answer {
	var in struct {
		IssueIDs []int64 `json:"issueIds"`
		From     string  `json:"from"`
	}
	if err := decodeStrict(c.x.body, &in, false); err != nil {
		return unhandledf("%v", err)
	}
	if in.From != "" && !isDate(in.From) {
		return tempoErrors(http.StatusBadRequest, "from must be a date (YYYY-MM-DD)")
	}
	offset, limit, err := c.window()
	if err != nil {
		return unhandledf("%v", err)
	}
	var found []*TempoWorklog
	for i := range c.t.Worklogs {
		w := &c.t.Worklogs[i]
		if slices.Contains(in.IssueIDs, idNumber(w.Issue)) && in.From <= w.StartDate {
			found = append(found, w)
		}
	}
	slices.SortStableFunc(found, func(a, b *TempoWorklog) int {
		return strings.Compare(a.StartDate+a.StartTime+fmt.Sprintf("%020d", a.ID), b.StartDate+b.StartTime+fmt.Sprintf("%020d", b.ID))
	})
	values := make([]any, len(found))
	for i, w := range found {
		values[i] = c.worklogJSON(w)
	}
	return c.page(values, offset, limit)
}

func tempoGetWorklog(c *tempoCall) answer {
	w := c.t.worklog(c.params["id"])
	if w == nil {
		return tempoErrors(http.StatusNotFound, "Worklog not found")
	}
	return jsonAnswer(http.StatusOK, c.worklogJSON(w))
}

// tempoWorklogBody is a worklog create or update.
type tempoWorklogBody struct {
	Author      *string         `json:"authorAccountId"`
	IssueID     *int64          `json:"issueId"`
	StartDate   *string         `json:"startDate"`
	StartTime   string          `json:"startTime"`
	Spent       *int64          `json:"timeSpentSeconds"`
	Billable    *int64          `json:"billableSeconds"`
	Remaining   *int64          `json:"remainingEstimateSeconds"`
	Description string          `json:"description"`
	Attributes  []TempoAttrPair `json:"attributes"`
}

// check reads a body into w, or Tempo's refusals.
func (c *tempoCall) check(in tempoWorklogBody, w *TempoWorklog) []string {
	var problems []string
	switch {
	case in.Author == nil || in.IssueID == nil || in.StartDate == nil || in.Spent == nil:
		return []string{"authorAccountId, issueId, startDate and timeSpentSeconds are required"}
	case c.s.user(*in.Author) == nil:
		problems = append(problems, "Account "+*in.Author+" does not exist")
	case c.s.issue(strconv.FormatInt(*in.IssueID, 10)) == nil:
		problems = append(problems, "Issue "+strconv.FormatInt(*in.IssueID, 10)+" does not exist")
	}
	if !isDate(*in.StartDate) {
		problems = append(problems, "startDate must be a date (yyyy-MM-dd)")
	}
	if in.StartTime == "" {
		in.StartTime = "00:00:00"
	}
	if _, err := time.Parse("15:04:05", in.StartTime); err != nil {
		problems = append(problems, "startTime must be a time (HH:mm:ss)")
	}
	if *in.Spent < 0 || (in.Billable != nil && *in.Billable < 0) {
		problems = append(problems, "timeSpentSeconds and billableSeconds cannot be negative")
	}
	problems = append(problems, c.checkAttributes(in.Attributes)...)
	if len(problems) > 0 {
		return problems
	}
	*w = TempoWorklog{
		ID: w.ID, Issue: strconv.FormatInt(*in.IssueID, 10), Author: *in.Author, StartDate: *in.StartDate, StartTime: in.StartTime,
		Spent: *in.Spent, Billable: in.Billable, Description: in.Description, Attributes: slices.Clone(in.Attributes), Created: w.Created,
	}
	return nil
}

// checkAttributes refuses an unknown attribute, a static list value it
// does not list, and a required attribute left out (documented).
func (c *tempoCall) checkAttributes(pairs []TempoAttrPair) []string {
	var problems []string
	for _, p := range pairs {
		i := slices.IndexFunc(c.t.Attributes, func(a TempoAttribute) bool { return a.Key == p.Key })
		switch {
		case i < 0:
			problems = append(problems, "Work attribute "+p.Key+" does not exist")
		case c.t.Attributes[i].Type == "STATIC_LIST" && !slices.Contains(c.t.Attributes[i].Values, p.Value):
			problems = append(problems, "Value "+p.Value+" is not allowed for work attribute "+p.Key)
		}
	}
	for _, a := range c.t.Attributes {
		if a.Required && !slices.ContainsFunc(pairs, func(p TempoAttrPair) bool { return p.Key == a.Key && p.Value != "" }) {
			problems = append(problems, "Work attribute "+a.Name+" is required")
		}
	}
	return problems
}

// locked refuses a write to a day of a submitted or approved timesheet.
func (c *tempoCall) locked(w *TempoWorklog) []string {
	if a := c.t.approvalOf(w.Author, w.StartDate); a.Status != "OPEN" {
		return []string{"The timesheet for " + w.StartDate + " is " + strings.ToLower(strings.ReplaceAll(a.Status, "_", " ")) + " and its worklogs cannot change"}
	}
	return nil
}

// setRemaining writes a worklog's remaining estimate to its Jira issue.
func (c *tempoCall) setRemaining(w *TempoWorklog, remaining *int64) {
	if remaining == nil {
		return
	}
	is := c.s.issue(w.Issue)
	before := cloneIssue(*is)
	v := *remaining
	is.RemainingEstimate = &v
	if item, ok := c.s.changeItem(systemField("timeestimate"), &before, is); ok {
		c.s.addChange(c.e, is, w.Author, c.e.now(), []JiraChangeItem{item})
		is.Updated = c.e.now()
	}
}

func tempoCreateWorklog(c *tempoCall) answer {
	var in tempoWorklogBody
	if err := decodeStrict(c.x.body, &in, false); err != nil {
		return unhandledf("%v", err)
	}
	w := TempoWorklog{ID: c.e.nextID(), Created: c.e.now()}
	problems := c.check(in, &w)
	if problems == nil {
		problems = c.locked(&w)
	}
	if problems != nil {
		return tempoErrors(http.StatusBadRequest, problems...)
	}
	w.Updated = w.Created
	c.t.Worklogs = append(c.t.Worklogs, w)
	c.setRemaining(&w, in.Remaining)
	return jsonAnswer(http.StatusOK, c.worklogJSON(&c.t.Worklogs[len(c.t.Worklogs)-1]))
}

func tempoUpdateWorklog(c *tempoCall) answer {
	var in tempoWorklogBody
	if err := decodeStrict(c.x.body, &in, false); err != nil {
		return unhandledf("%v", err)
	}
	cur := c.t.worklog(c.params["id"])
	if cur == nil {
		return tempoErrors(http.StatusNotFound, "Worklog not found")
	}
	next := *cur
	problems := c.locked(cur)
	if problems == nil {
		problems = c.check(in, &next)
	}
	if problems == nil {
		problems = c.locked(&next)
	}
	if problems != nil {
		return tempoErrors(http.StatusBadRequest, problems...)
	}
	next.Updated = c.e.now()
	*cur = next
	c.setRemaining(cur, in.Remaining)
	return jsonAnswer(http.StatusOK, c.worklogJSON(cur))
}

func tempoDeleteWorklog(c *tempoCall) answer {
	w := c.t.worklog(c.params["id"])
	if w == nil {
		return tempoErrors(http.StatusNotFound, "Worklog not found")
	}
	if problems := c.locked(w); problems != nil {
		return tempoErrors(http.StatusBadRequest, problems...)
	}
	id := w.ID
	c.t.Worklogs = slices.DeleteFunc(c.t.Worklogs, func(x TempoWorklog) bool { return x.ID == id })
	return emptyAnswer(http.StatusNoContent)
}
