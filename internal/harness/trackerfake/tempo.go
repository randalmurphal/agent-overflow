package trackerfake

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// tempoService serves Tempo's REST API v4 for every site's Tempo account;
// the bearer token selects the account.
type tempoService struct{}

type tempoRoute = route[*tempoCall]

// tempoCall is one request a Tempo route handles.
type tempoCall struct {
	e      *Engine
	s      *jiraSite
	t      *TempoAccount
	x      *exchange
	params map[string]string
	query  url.Values
}

// tempoBase is the address Tempo's answers name themselves by.
const tempoBase = "https://api.tempo.io/4/"

func (tempoService) authenticate(e *Engine, x *exchange) *answer {
	token, ok := strings.CutPrefix(x.r.Header.Get("Authorization"), "Bearer ")
	x.secrets = append(x.secrets, token)
	if s := e.tempoTenant(token); ok && s != nil {
		x.tenant = s.Site
		return nil
	}
	// Documented: 401 with no message.
	return &answer{status: http.StatusUnauthorized, header: http.Header{"Content-Type": {"text/html"}}}
}

func (tempoService) route(e *Engine, x *exchange) match {
	s := e.jira[x.tenant]
	r, params, ok := findRoute(tempoRoutes, x)
	if !ok {
		return match{unhandled: "no route matches"}
	}
	q := x.r.URL.Query()
	if why := r.checkQuery(q); why != "" {
		return match{unhandled: why}
	}
	c := &tempoCall{e: e, s: s, t: s.Tempo, x: x, params: params, query: q}
	return match{name: r.name(), run: func() answer { return r.handle(c) }}
}

// rateLimited is Tempo's 429, which documents no headers and no body.
func (tempoService) rateLimited(*Engine, *exchange, int) answer {
	return answer{status: http.StatusTooManyRequests}
}

func (tempoService) unhandledAnswer(detail string) answer {
	return tempoErrors(http.StatusNotImplemented, "trackerfake: unhandled: "+detail)
}

func (tempoService) routeNames() []string { return routeNames(tempoRoutes) }

// tempoErrors is Tempo's error body.
func tempoErrors(status int, messages ...string) answer {
	list := make([]map[string]string, len(messages))
	for i, m := range messages {
		list[i] = map[string]string{"message": m}
	}
	return jsonAnswer(status, map[string]any{"errors": list})
}

var tempoPage = []string{"offset", "limit"}

var tempoRoutes = []tempoRoute{
	{method: "GET", pattern: "4/worklogs/user/{account}", query: with(tempoPage, "from", "to", "updatedFrom"), handle: tempoUserWorklogs},
	{method: "GET", pattern: "4/worklogs/{id}", handle: tempoGetWorklog},
	{method: "POST", pattern: "4/worklogs", handle: tempoCreateWorklog},
	{method: "PUT", pattern: "4/worklogs/{id}", handle: tempoUpdateWorklog},
	{method: "DELETE", pattern: "4/worklogs/{id}", handle: tempoDeleteWorklog},
	{method: "GET", pattern: "4/work-attributes", query: tempoPage, handle: tempoAttributes},
	{method: "GET", pattern: "4/globalconfiguration", handle: tempoSettings},
	{method: "POST", pattern: "4/accounts/search", query: tempoPage, handle: tempoAccounts},
	{method: "GET", pattern: "4/user-schedule/{account}", query: []string{"from", "to"}, handle: tempoSchedule},
	{method: "GET", pattern: "4/timesheet-approvals/user/{account}", query: []string{"from", "to"}, handle: tempoApproval},
}

// window reads offset and limit: limit 50 when absent, at most 5000
// (documented).
func (c *tempoCall) window() (int, int, error) {
	offset, limit := 0, 50
	for _, p := range []struct {
		name string
		v    *int
	}{{"offset", &offset}, {"limit", &limit}} {
		if raw := c.query.Get(p.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return 0, 0, fmt.Errorf("query parameter %s=%q is not a count", p.name, raw)
			}
			*p.v = n
		}
	}
	return offset, min(max(limit, 1), 5000), nil
}

// page is Tempo's list envelope over values from offset.
func (c *tempoCall) page(values []any, offset, limit int) answer {
	end := min(offset+limit, len(values))
	results := values[min(offset, len(values)):end]
	meta := map[string]any{"count": len(results), "offset": offset, "limit": limit}
	if end < len(values) {
		q := cloneQuery(c.query)
		q.Set("offset", strconv.Itoa(end))
		meta["next"] = tempoBase + strings.TrimPrefix(c.x.r.URL.Path, "/4/") + "?" + q.Encode()
	}
	return jsonAnswer(http.StatusOK, map[string]any{"self": tempoBase + strings.TrimPrefix(c.x.r.URL.Path, "/4/"), "metadata": meta, "results": orEmpty(results)})
}

func cloneQuery(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = slices.Clone(v)
	}
	return out
}

// dateRange reads the from and to dates a route requires.
func (c *tempoCall) dateRange() (string, string, *answer) {
	from, to := c.query.Get("from"), c.query.Get("to")
	if !isDate(from) || !isDate(to) || to < from {
		a := tempoErrors(http.StatusBadRequest, "from and to must be dates (YYYY-MM-DD), from not after to")
		return "", "", &a
	}
	return from, to, nil
}

func tempoAttributes(c *tempoCall) answer {
	offset, limit, err := c.window()
	if err != nil {
		return unhandledf("%v", err)
	}
	values := make([]any, len(c.t.Attributes))
	for i, a := range c.t.Attributes {
		values[i] = map[string]any{
			"self": tempoBase + "work-attributes/" + a.Key, "key": a.Key, "name": a.Name, "type": a.Type,
			"required": a.Required, "values": orEmpty(a.Values), "names": a.Names,
		}
	}
	return c.page(values, offset, limit)
}

func tempoSettings(c *tempoCall) answer {
	st := c.t.Settings
	return jsonAnswer(http.StatusOK, map[string]any{
		"allowLoggingOnClosedAccount": st.AllowLoggingOnClosedAccount, "approvalPeriod": st.ApprovalPeriod,
		"weekStart": st.WeekStart, "worklogDescriptionOptional": st.DescriptionOptional == nil || *st.DescriptionOptional,
		"remainingEstimateOptional":     st.RemainingEstimateOptional == nil || *st.RemainingEstimateOptional,
		"numberOfDaysAllowedIntoFuture": st.DaysIntoFuture,
	})
}

func tempoAccounts(c *tempoCall) answer {
	var in struct {
		Statuses []string `json:"statuses"`
		Keys     []string `json:"keys"`
	}
	if err := decodeStrict(c.x.body, &in, false); err != nil {
		return unhandledf("%v", err)
	}
	offset, limit, err := c.window()
	if err != nil {
		return unhandledf("%v", err)
	}
	values := []any{}
	for _, a := range c.t.Accounts {
		if (len(in.Statuses) == 0 || slices.Contains(in.Statuses, a.Status)) && (len(in.Keys) == 0 || slices.Contains(in.Keys, a.Key)) {
			values = append(values, map[string]any{"self": tempoBase + "accounts/" + a.Key, "id": a.ID, "key": a.Key, "name": a.Name, "status": a.Status, "global": a.Global})
		}
	}
	return c.page(values, offset, limit)
}

// holiday is the holiday on day, or nil.
func (t *TempoAccount) holiday(day string) *TempoHoliday {
	for i := range t.Holidays {
		if t.Holidays[i].Date == day {
			return &t.Holidays[i]
		}
	}
	return nil
}

// required is the time day requires and its kind.
func (t *TempoAccount) required(day time.Time) (int64, string) {
	working := day.Weekday() != time.Saturday && day.Weekday() != time.Sunday
	h := t.holiday(day.Format(time.DateOnly))
	switch {
	case h != nil && !working:
		return 0, "HOLIDAY_AND_NON_WORKING_DAY"
	case h != nil:
		return max(t.WorkdaySeconds-cmp.Or(h.Seconds, t.WorkdaySeconds), 0), "HOLIDAY"
	case !working:
		return 0, "NON_WORKING_DAY"
	}
	return t.WorkdaySeconds, "WORKING_DAY"
}

func tempoSchedule(c *tempoCall) answer {
	if c.s.user(c.params["account"]) == nil {
		return tempoErrors(http.StatusNotFound, "User not found")
	}
	from, to, bad := c.dateRange()
	if bad != nil {
		return *bad
	}
	results := []any{}
	first, _ := time.Parse(time.DateOnly, from)
	last, _ := time.Parse(time.DateOnly, to)
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		secs, kind := c.t.required(d)
		day := map[string]any{"date": d.Format(time.DateOnly), "requiredSeconds": secs, "type": kind}
		if h := c.t.holiday(d.Format(time.DateOnly)); h != nil {
			typ := "FIXED"
			if h.Floating {
				typ = "FLOATING"
			}
			day["holiday"] = map[string]any{"name": h.Name, "description": h.Description, "durationSeconds": cmp.Or(h.Seconds, c.t.WorkdaySeconds), "type": typ}
		}
		results = append(results, day)
	}
	return jsonAnswer(http.StatusOK, map[string]any{"self": tempoBase + "user-schedule/" + c.params["account"], "metadata": map[string]any{"count": len(results)}, "results": results})
}

// period is the approval period that holds day.
func (t *TempoAccount) period(day time.Time) (time.Time, time.Time) {
	if t.Settings.ApprovalPeriod == "MONTHLY" {
		first := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
		return first, first.AddDate(0, 1, -1)
	}
	start := time.Weekday(t.Settings.WeekStart % 7)
	back := (int(day.Weekday()) - int(start) + 7) % 7
	first := day.AddDate(0, 0, -back)
	return first, first.AddDate(0, 0, 6)
}

// approvalOf is account's approval of the period holding day, OPEN when
// the account names none.
func (t *TempoAccount) approvalOf(account, day string) TempoApproval {
	for _, a := range t.Approvals {
		if a.Account == account && a.From <= day && day <= a.To {
			return a
		}
	}
	d, _ := time.Parse(time.DateOnly, day)
	from, to := t.period(d)
	return TempoApproval{Account: account, From: from.Format(time.DateOnly), To: to.Format(time.DateOnly), Status: "OPEN"}
}

func tempoApproval(c *tempoCall) answer {
	account := c.params["account"]
	if c.s.user(account) == nil {
		return tempoErrors(http.StatusNotFound, "User not found")
	}
	from, _, bad := c.dateRange()
	if bad != nil {
		return *bad
	}
	a := c.t.approvalOf(account, from)
	var required, spent int64
	first, _ := time.Parse(time.DateOnly, a.From)
	last, _ := time.Parse(time.DateOnly, a.To)
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		secs, _ := c.t.required(d)
		required += secs
	}
	for _, w := range c.t.Worklogs {
		if w.Author == account && a.From <= w.StartDate && w.StartDate <= a.To {
			spent += w.Spent
		}
	}
	status := map[string]any{"key": a.Status}
	if a.Actor != "" {
		status["actor"] = map[string]any{"accountId": a.Actor}
	}
	if !a.Changed.IsZero() {
		status["updatedAt"] = a.Changed.UTC().Format(time.RFC3339)
	}
	out := map[string]any{
		"self": tempoBase + "timesheet-approvals/user/" + account, "user": map[string]any{"accountId": account},
		"period": map[string]any{"from": a.From, "to": a.To}, "requiredSeconds": required, "timeSpentSeconds": spent, "status": status,
	}
	if a.Reviewer != "" {
		out["reviewer"] = map[string]any{"accountId": a.Reviewer}
	}
	return jsonAnswer(http.StatusOK, out)
}
