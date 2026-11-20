package trackerfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (s *jiraSite) worklogJSON(is *JiraIssue, w *JiraWorklog) map[string]any {
	out := map[string]any{
		"self": s.self("issue/" + is.ID + "/worklog/" + w.ID), "id": w.ID, "issueId": is.ID,
		"author": s.userJSON(w.Author), "updateAuthor": s.userJSON(orString(w.UpdateAuthor, w.Author)),
		"started": jiraTime(w.Started), "timeSpent": spentText(w.TimeSpent), "timeSpentSeconds": w.TimeSpent,
		"created": jiraTime(w.Created), "updated": jiraTime(w.Updated),
	}
	if len(w.Comment) > 0 {
		out["comment"] = w.Comment
	}
	if len(w.Visibility) > 0 {
		out["visibility"] = w.Visibility
	}
	return out
}

// spentText is a duration as Jira writes it: 1h 30m.
func spentText(secs int64) string {
	var parts []string
	for _, u := range []struct {
		n    int64
		unit string
	}{{3600, "h"}, {60, "m"}} {
		if secs >= u.n {
			parts = append(parts, strconv.FormatInt(secs/u.n, 10)+u.unit)
			secs %= u.n
		}
	}
	if secs > 0 || len(parts) == 0 {
		parts = append(parts, strconv.FormatInt(secs, 10)+"s")
	}
	return strings.Join(parts, " ")
}

// worklogPageJSON is the page of ws from start, at most size long: a
// worklog list page, or the worklog field's value.
func (s *jiraSite) worklogPageJSON(is *JiraIssue, ws []JiraWorklog, start, size int) map[string]any {
	list := make([]any, len(ws))
	for i := range ws {
		list[i] = s.worklogJSON(is, &ws[i])
	}
	return listPage("worklogs", list, start, size)
}

func listWorklogs(c *jiraCall) answer {
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	start, size, err := c.pageWindow(5000, 5000)
	if err != nil {
		return unhandledf("%v", err)
	}
	after, err1 := c.intParam("startedAfter", 0)
	before, err2 := c.intParam("startedBefore", 0)
	if err1 != nil || err2 != nil {
		return unhandledf("startedAfter or startedBefore is not milliseconds")
	}
	var ws []JiraWorklog
	for _, w := range is.Worklogs {
		ms := w.Started.UnixMilli()
		if ms >= int64(after) && (before == 0 || ms < int64(before)) {
			ws = append(ws, w)
		}
	}
	return jsonAnswer(http.StatusOK, c.s.worklogPageJSON(is, ws, start, size))
}

// foundWorklog is the worklog {id} names on the issue {issue} names; one
// of another issue is not found (measured for reads; the writes that use
// it refuse the same way, unverified).
func (c *jiraCall) foundWorklog() (*JiraIssue, *JiraWorklog, *answer) {
	is, nf := c.found()
	if nf != nil {
		return nil, nil, nf
	}
	w := is.worklog(c.params["id"])
	if w == nil {
		a := jiraErrors(http.StatusNotFound, "Cannot find worklog with id: "+c.params["id"]+".")
		return nil, nil, &a
	}
	return is, w, nil
}

func getWorklog(c *jiraCall) answer {
	is, w, nf := c.foundWorklog()
	if nf != nil {
		return *nf
	}
	return jsonAnswer(http.StatusOK, c.s.worklogJSON(is, w))
}

// worklogBody is a worklog add or update.
type worklogBody struct {
	Started    *string         `json:"started"`
	TimeSpent  *int64          `json:"timeSpentSeconds"`
	Comment    json.RawMessage `json:"comment"`
	Visibility json.RawMessage `json:"visibility"`
}

// read checks the body: a start Jira's format reads and a positive time.
func (b worklogBody) read() (started time.Time, refused map[string]string) {
	if b.Started != nil {
		t, err := time.Parse("2006-01-02T15:04:05.000-0700", *b.Started)
		if err != nil {
			return started, map[string]string{"started": "Invalid date format. Please enter the date in the format \"yyyy-MM-dd'T'HH:mm:ss.SSSZ\"."}
		}
		started = t.UTC()
	}
	if b.TimeSpent != nil && *b.TimeSpent <= 0 {
		return started, map[string]string{"timeLogged": "You must indicate the time spent working."}
	}
	if len(b.Comment) > 0 && !isNull(b.Comment) && !validDoc(b.Comment) {
		return started, map[string]string{"comment": "Comment body is not an Atlassian Document."}
	}
	return started, nil
}

// adjust applies the adjustEstimate parameters to is's remaining
// estimate: delta is the time logged by the write, negative for time
// given back. modes are what the route takes.
func (c *jiraCall) adjust(is *JiraIssue, delta int64, modes ...string) (refused, unhandled string) {
	mode := c.query.Get("adjustEstimate")
	if mode == "" {
		mode = "auto"
	}
	if !slices.Contains(modes, mode) {
		return "", "adjustEstimate=" + mode
	}
	for _, p := range []string{"newEstimate", "reduceBy", "increaseBy"} {
		want := map[string]string{"newEstimate": "new", "reduceBy": "manual", "increaseBy": "manual"}[p]
		if c.query.Has(p) && mode != want {
			return "", p + " with adjustEstimate=" + mode
		}
	}
	set := func(v int64) { v = max(v, 0); is.RemainingEstimate = &v }
	remaining := int64(0)
	if is.RemainingEstimate != nil {
		remaining = *is.RemainingEstimate
	}
	switch mode {
	case "auto":
		if is.RemainingEstimate != nil {
			set(remaining - delta)
		}
	case "new":
		d, err := jiraDuration(c.query.Get("newEstimate"))
		if err != nil {
			return "Invalid new estimate specified.", ""
		}
		set(d)
	case "manual":
		by := c.query.Get("reduceBy")
		sign := int64(-1)
		if c.query.Has("increaseBy") {
			by, sign = c.query.Get("increaseBy"), 1
		}
		d, err := jiraDuration(by)
		if err != nil {
			return "Invalid estimate adjustment specified.", ""
		}
		set(remaining + sign*d)
	}
	return "", ""
}

// jiraDuration reads a duration in the forms the clients write: whole
// minutes or hours, as 90m or 2h.
func jiraDuration(v string) (int64, error) {
	for unit, secs := range map[string]int64{"m": 60, "h": 3600} {
		if n, ok := strings.CutSuffix(v, unit); ok {
			if x, err := strconv.ParseInt(n, 10, 64); err == nil && x >= 0 {
				return x * secs, nil
			}
		}
	}
	return 0, fmt.Errorf("duration %q", v)
}

// worklogChanged records a worklog write's effect on the issue's time
// fields.
func (c *jiraCall) worklogChanged(is *JiraIssue, before *JiraIssue) {
	var items []JiraChangeItem
	for _, id := range []string{"timeestimate", "timespent"} {
		from, to := c.s.timeChange(id, before), c.s.timeChange(id, is)
		if !eqPtr(from.raw, to.raw) {
			items = append(items, JiraChangeItem{FieldID: id, Field: id, FieldType: "jira", From: from.raw, To: to.raw, FromString: from.text, ToString: to.text})
		}
	}
	if len(items) == 0 {
		c.touch(is)
		return
	}
	c.changed(is, items)
}

func (s *jiraSite) timeChange(id string, is *JiraIssue) changeValue {
	if id == "timeestimate" {
		return secondsChange(is.RemainingEstimate)
	}
	if t, ok := s.timeSpent(is).(int64); ok {
		return secondsChange(&t)
	}
	return changeValue{}
}

func addWorklog(c *jiraCall) answer {
	var in worklogBody
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	if in.Visibility != nil {
		return unhandledf("a new worklog's visibility")
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	started, refused := in.read()
	if refused == nil && (in.Started == nil || in.TimeSpent == nil) {
		refused = map[string]string{"timeLogged": "You must indicate the time spent working."}
	}
	if refused != nil {
		return jiraFieldErrors(http.StatusBadRequest, refused)
	}
	before := cloneIssue(*is)
	if msg, why := c.adjust(is, *in.TimeSpent, "auto", "leave", "new", "manual"); why != "" || msg != "" {
		*is = before
		return refusedOr(msg, why)
	}
	if c.query.Has("increaseBy") {
		*is = before
		return unhandledf("increaseBy on a new worklog")
	}
	now := c.e.now()
	w := JiraWorklog{ID: strconv.FormatInt(c.e.nextID(), 10), Author: c.viewer, UpdateAuthor: c.viewer, Started: started,
		TimeSpent: *in.TimeSpent, Created: now, Updated: now}
	if !isNull(in.Comment) {
		w.Comment = slices.Clone(in.Comment)
	}
	is.Worklogs = append(is.Worklogs, w)
	c.worklogChanged(is, &before)
	return jsonAnswer(http.StatusCreated, c.s.worklogJSON(is, &is.Worklogs[len(is.Worklogs)-1]))
}

func refusedOr(msg, why string) answer {
	if why != "" {
		return unhandledf("%s", why)
	}
	return jiraErrors(http.StatusBadRequest, msg)
}

func updateWorklog(c *jiraCall) answer {
	var in worklogBody
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	is, w, nf := c.foundWorklog()
	if nf != nil {
		return *nf
	}
	started, refused := in.read()
	if refused != nil {
		return jiraFieldErrors(http.StatusBadRequest, refused)
	}
	before := cloneIssue(*is)
	delta := int64(0)
	if in.TimeSpent != nil {
		delta = *in.TimeSpent - w.TimeSpent
	}
	if msg, why := c.adjust(is, delta, "auto", "leave", "new"); why != "" || msg != "" {
		*is = before
		return refusedOr(msg, why)
	}
	w = is.worklog(c.params["id"])
	if in.Started != nil {
		w.Started = started
	}
	if in.TimeSpent != nil {
		w.TimeSpent = *in.TimeSpent
	}
	if in.Comment != nil {
		w.Comment = nil
		if !isNull(in.Comment) {
			w.Comment = slices.Clone(in.Comment)
		}
	}
	if in.Visibility != nil {
		w.Visibility = slices.Clone(in.Visibility)
	}
	w.UpdateAuthor, w.Updated = c.viewer, c.e.now()
	c.worklogChanged(is, &before)
	return jsonAnswer(http.StatusOK, c.s.worklogJSON(is, w))
}

func deleteWorklog(c *jiraCall) answer {
	is, w, nf := c.foundWorklog()
	if nf != nil {
		return *nf
	}
	before := cloneIssue(*is)
	if msg, why := c.adjust(is, -w.TimeSpent, "auto", "leave", "new", "manual"); why != "" || msg != "" {
		*is = before
		return refusedOr(msg, why)
	}
	if c.query.Has("reduceBy") {
		*is = before
		return unhandledf("reduceBy on a worklog delete")
	}
	id := w.ID
	is.Worklogs = slices.DeleteFunc(is.Worklogs, func(x JiraWorklog) bool { return x.ID == id })
	c.s.DeletedWorklogs = append(c.s.DeletedWorklogs, JiraDeletion{ID: idNumber(id), At: c.e.now()})
	c.worklogChanged(is, &before)
	return emptyAnswer(http.StatusNoContent)
}

// worklogSyncLag is how recent a change worklog/updated and
// worklog/deleted leave out (documented: the last minute).
const worklogSyncLag = time.Minute

// worklogSyncPage is the most changes a sync page holds (documented).
const worklogSyncPage = 1000

type worklogStamp struct {
	id int64
	at time.Time
}

func worklogSync(c *jiraCall, stamps []worklogStamp) answer {
	since, err := c.intParam("since", 0)
	if err != nil {
		return unhandledf("%v", err)
	}
	cutoff := c.e.now().Add(-worklogSyncLag)
	var found []worklogStamp
	for _, st := range stamps {
		if ms := st.at.UnixMilli(); ms > int64(since) && !st.at.After(cutoff) {
			found = append(found, st)
		}
	}
	slices.SortStableFunc(found, func(a, b worklogStamp) int { return a.at.Compare(b.at) })
	last := len(found) <= worklogSyncPage
	found = found[:min(len(found), worklogSyncPage)]
	values := []any{}
	until := int64(since)
	for _, st := range found {
		values = append(values, map[string]any{"worklogId": st.id, "updatedTime": st.at.UnixMilli(), "properties": []any{}})
		until = st.at.UnixMilli()
	}
	out := map[string]any{"values": values, "since": since, "until": until, "lastPage": last, "self": c.s.self(c.x.r.URL.Path)}
	if !last {
		out["nextPage"] = c.s.self(strings.TrimPrefix(c.x.r.URL.Path, "/rest/api/3/") + "?since=" + strconv.FormatInt(until, 10))
	}
	return jsonAnswer(http.StatusOK, out)
}

func updatedWorklogs(c *jiraCall) answer {
	var stamps []worklogStamp
	for _, is := range c.s.Issues {
		for _, w := range is.Worklogs {
			stamps = append(stamps, worklogStamp{idNumber(w.ID), w.Updated})
		}
	}
	return worklogSync(c, stamps)
}

func deletedWorklogs(c *jiraCall) answer {
	var stamps []worklogStamp
	for _, d := range c.s.DeletedWorklogs {
		stamps = append(stamps, worklogStamp{d.ID, d.At})
	}
	return worklogSync(c, stamps)
}

func worklogsByID(c *jiraCall) answer {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	if len(in.IDs) > 1000 {
		return jiraErrors(http.StatusBadRequest, "The number of worklog IDs must be at most 1000.")
	}
	out := []any{}
	for i := range c.s.Issues {
		is := &c.s.Issues[i]
		for j := range is.Worklogs {
			if slices.Contains(in.IDs, idNumber(is.Worklogs[j].ID)) {
				out = append(out, c.s.worklogJSON(is, &is.Worklogs[j]))
			}
		}
	}
	return jsonAnswer(http.StatusOK, out)
}
