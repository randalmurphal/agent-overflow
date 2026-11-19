package tempo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// DayKind is how Tempo's schedule classes a day.
type DayKind string

// The day kinds Tempo documents.
const (
	WorkingDay              DayKind = "WORKING_DAY"
	NonWorkingDay           DayKind = "NON_WORKING_DAY"
	HolidayDay              DayKind = "HOLIDAY"
	HolidayAndNonWorkingDay DayKind = "HOLIDAY_AND_NON_WORKING_DAY"
)

// ScheduleDay is one day of a user's schedule.
type ScheduleDay struct {
	Date Date
	Kind DayKind
	// Required is the time the user is expected to log that day, after
	// any holiday.
	Required time.Duration
	// Holiday is the holiday on that day, nil when there is none.
	Holiday *Holiday
}

// Holiday is a holiday as the user's holiday scheme defines it.
type Holiday struct {
	Name        string
	Description string
	// Duration is how much of the day the holiday takes.
	Duration time.Duration
	// Floating reports a holiday the user picks, as opposed to a fixed
	// date.
	Floating bool
}

type scheduleJSON struct {
	Results []struct {
		Date     string `json:"date"`
		Type     string `json:"type"`
		Required int64  `json:"requiredSeconds"`
		Holiday  *struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Duration    int64  `json:"durationSeconds"`
			Type        string `json:"type"`
		} `json:"holiday"`
	} `json:"results"`
}

// Schedule reads a user's schedule for each day from from to to, both
// included: whether it is a working day, a holiday or neither, and the
// time required.
func (c *Client) Schedule(ctx context.Context, accountID string, from, to Date) ([]ScheduleDay, error) {
	if err := c.admit(); err != nil {
		return nil, err
	}
	if err := checkRange(accountID, from, to); err != nil {
		return nil, trackerapi.InvalidInput(fmt.Errorf("tempo: read schedule: %w", err))
	}
	var raw scheduleJSON
	r := trackerapi.Request{
		Path:  path("4/user-schedule", accountID),
		Query: url.Values{"from": {from.String()}, "to": {to.String()}},
	}
	resp, err := c.http.JSON(ctx, r, &raw)
	if err != nil {
		return nil, fmt.Errorf("tempo: read schedule of %s from %s to %s: %w", accountID, from, to, err)
	}
	out := make([]ScheduleDay, 0, len(raw.Results))
	for _, d := range raw.Results {
		date, err := ParseDate(d.Date)
		if err != nil {
			return nil, fmt.Errorf("tempo: read schedule of %s: %w", accountID, unreadable(resp, err))
		}
		day := ScheduleDay{Date: date, Kind: DayKind(resp.Scrub(d.Type)), Required: duration(d.Required)}
		if h := d.Holiday; h != nil {
			day.Holiday = &Holiday{Name: resp.Scrub(h.Name), Description: resp.Scrub(h.Description), Duration: duration(h.Duration), Floating: h.Type == "FLOATING"}
		}
		out = append(out, day)
	}
	return out, nil
}

// ApprovalStatus is the state of a timesheet in Tempo's approval flow.
type ApprovalStatus string

// The approval statuses Tempo documents.
const (
	// ApprovalOpen is a timesheet not submitted, or reopened.
	ApprovalOpen ApprovalStatus = "OPEN"
	// ApprovalInReview is a submitted timesheet. Tempo's help says the
	// user recalls it to correct it, so its worklogs cannot change.
	ApprovalInReview ApprovalStatus = "IN_REVIEW"
	// ApprovalApproved is an approved timesheet; its worklogs cannot
	// change.
	ApprovalApproved ApprovalStatus = "APPROVED"
)

// Approval is a user's timesheet for one approval period.
type Approval struct {
	From, To Date // the period, both days included
	Status   ApprovalStatus
	Required time.Duration
	Spent    time.Duration
	// Reviewer is the account ID of the person asked to approve; empty
	// when none is named.
	Reviewer string
	// Actor is the account ID of whoever set Status; empty when none.
	Actor string
	// Changed is when Status was set; zero when Tempo did not say.
	Changed time.Time
}

// locked reports a timesheet whose worklogs cannot change.
func (a Approval) locked() bool {
	return a.Status == ApprovalApproved || a.Status == ApprovalInReview
}

type approvalJSON struct {
	Period struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"period"`
	Required int64 `json:"requiredSeconds"`
	Spent    int64 `json:"timeSpentSeconds"`
	Reviewer *struct {
		AccountID string `json:"accountId"`
	} `json:"reviewer"`
	Status *struct {
		Key   string `json:"key"`
		Actor *struct {
			AccountID string `json:"accountId"`
		} `json:"actor"`
		UpdatedAt string `json:"updatedAt"`
	} `json:"status"`
}

// Approval reads the approval of the user's timesheet for the period
// that holds day.
func (c *Client) Approval(ctx context.Context, accountID string, day Date) (Approval, error) {
	if err := c.admit(); err != nil {
		return Approval{}, err
	}
	a, err := c.approval(ctx, accountID, day)
	if err != nil {
		return Approval{}, fmt.Errorf("tempo: read approval of %s on %s: %w", accountID, day, err)
	}
	return a, nil
}

func (c *Client) approval(ctx context.Context, accountID string, day Date) (Approval, error) {
	if err := checkRange(accountID, day, day); err != nil {
		return Approval{}, trackerapi.InvalidInput(err)
	}
	var raw approvalJSON
	r := trackerapi.Request{
		Path:  path("4/timesheet-approvals/user", accountID),
		Query: url.Values{"from": {day.String()}, "to": {day.String()}},
	}
	resp, err := c.http.JSON(ctx, r, &raw)
	if err != nil {
		return Approval{}, err
	}
	a, err := raw.approval(day, resp.Scrub)
	if err != nil {
		return Approval{}, unreadable(resp, err)
	}
	return a, nil
}

// approval reads raw, the answer for day; every string kept is scrubbed.
func (raw approvalJSON) approval(day Date, scrub func(string) string) (Approval, error) {
	a := Approval{Required: duration(raw.Required), Spent: duration(raw.Spent), Status: ApprovalOpen}
	var err error
	if a.From, err = ParseDate(raw.Period.From); err != nil {
		return Approval{}, fmt.Errorf("period start: %w", err)
	}
	if a.To, err = ParseDate(raw.Period.To); err != nil {
		return Approval{}, fmt.Errorf("period end: %w", err)
	}
	if day.Before(a.From) || day.After(a.To) {
		return Approval{}, fmt.Errorf("the answer is for the period %s to %s, which does not hold %s", a.From, a.To, day)
	}
	if raw.Reviewer != nil {
		a.Reviewer = scrub(raw.Reviewer.AccountID)
	}
	if s := raw.Status; s != nil {
		if s.Key != "" {
			a.Status = ApprovalStatus(scrub(s.Key))
		}
		if s.Actor != nil {
			a.Actor = scrub(s.Actor.AccountID)
		}
		if s.UpdatedAt != "" {
			if a.Changed, err = parseTimestamp(s.UpdatedAt); err != nil {
				return Approval{}, fmt.Errorf("status updatedAt: %w", err)
			}
		}
	}
	return a, nil
}

func checkRange(accountID string, from, to Date) error {
	switch {
	case accountID == "":
		return errors.New("no account ID")
	case from.IsZero() || to.IsZero():
		return errors.New("the range needs both a start and an end date")
	case to.Before(from):
		return fmt.Errorf("the range ends on %s, before it starts on %s", to, from)
	}
	return nil
}
