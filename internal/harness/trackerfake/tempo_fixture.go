package trackerfake

import (
	"fmt"
	"slices"
	"time"
)

// TempoAccount is a Jira site's Tempo: its settings, work attributes,
// billing accounts, holidays, timesheet approvals and worklogs. Tempo is
// one origin for every site; the token names the site.
type TempoAccount struct {
	// Token is the API token (Bearer).
	Token      string           `json:"token,omitempty"`
	Settings   TempoSettings    `json:"settings"`
	Attributes []TempoAttribute `json:"attributes,omitempty"`
	Accounts   []TempoBilling   `json:"accounts,omitempty"`
	// WorkdaySeconds is the time required on each Monday to Friday; zero
	// seeds eight hours. Saturday and Sunday are not working days.
	WorkdaySeconds int64           `json:"workdaySeconds,omitempty"`
	Holidays       []TempoHoliday  `json:"holidays,omitempty"`
	Approvals      []TempoApproval `json:"approvals,omitempty"`
	Worklogs       []TempoWorklog  `json:"worklogs,omitempty"`
}

// TempoSettings is Tempo's global configuration.
type TempoSettings struct {
	AllowLoggingOnClosedAccount bool `json:"allowLoggingOnClosedAccount,omitempty"`
	// ApprovalPeriod is WEEKLY (the default) or MONTHLY.
	ApprovalPeriod string `json:"approvalPeriod,omitempty"`
	// WeekStart numbers days 1 (Monday, the default) to 7 (Sunday).
	WeekStart                 int   `json:"weekStart,omitempty"`
	DescriptionOptional       *bool `json:"descriptionOptional,omitempty"`
	RemainingEstimateOptional *bool `json:"remainingEstimateOptional,omitempty"`
	DaysIntoFuture            int   `json:"daysIntoFuture,omitempty"`
}

// TempoAttribute is a work attribute. Type is STATIC_LIST, INPUT_FIELD,
// INPUT_NUMERIC, CHECKBOX or ACCOUNT; Values and Names are a static
// list's values and what Tempo shows for each.
type TempoAttribute struct {
	Key      string            `json:"key"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Required bool              `json:"required,omitempty"`
	Values   []string          `json:"values,omitempty"`
	Names    map[string]string `json:"names,omitempty"`
}

// TempoBilling is a Tempo account time is billed to; Status is OPEN,
// CLOSED or ARCHIVED.
type TempoBilling struct {
	ID     int64  `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Global bool   `json:"global,omitempty"`
}

// TempoHoliday is a holiday on Date (YYYY-MM-DD) for every user; Seconds
// is how much of the day it takes, zero for the whole day.
type TempoHoliday struct {
	Date        string `json:"date"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Seconds     int64  `json:"seconds,omitempty"`
	Floating    bool   `json:"floating,omitempty"`
}

// TempoApproval is a user's timesheet for the period From to To (both
// YYYY-MM-DD, both included); Status is OPEN, IN_REVIEW or APPROVED. A
// period no approval names is OPEN.
type TempoApproval struct {
	Account  string    `json:"account"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Status   string    `json:"status"`
	Reviewer string    `json:"reviewer,omitempty"`
	Actor    string    `json:"actor,omitempty"`
	Changed  time.Time `json:"changed,omitzero"`
}

// TempoWorklog is a Tempo worklog on the Jira issue Issue (its ID).
// StartDate is YYYY-MM-DD and StartTime HH:MM:SS in the author's time
// zone.
type TempoWorklog struct {
	ID          int64           `json:"id"`
	Issue       string          `json:"issue"`
	Author      string          `json:"author"`
	StartDate   string          `json:"startDate"`
	StartTime   string          `json:"startTime,omitempty"`
	Spent       int64           `json:"timeSpentSeconds"`
	Billable    *int64          `json:"billableSeconds,omitempty"`
	Description string          `json:"description,omitempty"`
	Attributes  []TempoAttrPair `json:"attributes,omitempty"`
	Created     time.Time       `json:"created,omitzero"`
	Updated     time.Time       `json:"updated,omitzero"`
	// OmitsUTCStart is a worklog whose answer names no
	// startDateTimeUtc, which Tempo's spec does not require.
	OmitsUTCStart bool `json:"omitsUtcStart,omitempty"`
}

// TempoAttrPair is a worklog's value of a work attribute.
type TempoAttrPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// seedTempo checks and completes the site's Tempo account.
func (s *jiraSite) seedTempo(e *Engine) error {
	t := s.Tempo
	s.tempoToken, t.Token = t.Token, ""
	if err := checkToken("the Tempo token", s.tempoToken); err != nil {
		return err
	}
	if t.Settings.ApprovalPeriod == "" {
		t.Settings.ApprovalPeriod = "WEEKLY"
	}
	if t.Settings.ApprovalPeriod != "WEEKLY" && t.Settings.ApprovalPeriod != "MONTHLY" {
		return fmt.Errorf("approval period %q is not WEEKLY or MONTHLY", t.Settings.ApprovalPeriod)
	}
	if t.Settings.WeekStart == 0 {
		t.Settings.WeekStart = 1
	}
	if t.WorkdaySeconds == 0 {
		t.WorkdaySeconds = 8 * 3600
	}
	for _, a := range t.Attributes {
		if !slices.Contains([]string{"STATIC_LIST", "INPUT_FIELD", "INPUT_NUMERIC", "CHECKBOX", "ACCOUNT"}, a.Type) || a.Key == "" {
			return fmt.Errorf("work attribute %q has type %q", a.Key, a.Type)
		}
	}
	for _, h := range t.Holidays {
		if !isDate(h.Date) {
			return fmt.Errorf("holiday %q is on %q, not a date", h.Name, h.Date)
		}
	}
	for _, a := range t.Approvals {
		if s.user(a.Account) == nil || !isDate(a.From) || !isDate(a.To) || a.To < a.From ||
			!slices.Contains([]string{"OPEN", "IN_REVIEW", "APPROVED"}, a.Status) {
			return fmt.Errorf("approval of %q from %q to %q is not a user's period with a status", a.Account, a.From, a.To)
		}
	}
	for i := range t.Worklogs {
		w := &t.Worklogs[i]
		if w.ID == 0 {
			w.ID = e.nextID()
		}
		e.reserveID(w.ID)
		if w.StartTime == "" {
			w.StartTime = "00:00:00"
		}
		if s.issue(w.Issue) == nil || s.user(w.Author) == nil || !isDate(w.StartDate) || w.Spent <= 0 {
			return fmt.Errorf("worklog %d needs an issue and author of the site, a start date and time spent", w.ID)
		}
		w.Issue = s.issue(w.Issue).ID
		w.Created = orNow(w.Created, e.now())
		w.Updated = orNow(w.Updated, w.Created)
	}
	return nil
}
