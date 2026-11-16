package tempo

import (
	"slices"
	"time"
)

// Draft is a worklog to create, whole.
type Draft struct {
	// Issue names the issue by ID, or by Key alone, which the client
	// resolves through IssueIDs.
	Issue Issue
	// Author is the Atlassian account ID the worklog is logged for.
	Author string
	Start  Start
	// Spent is the time worked, sent as whole seconds: more than zero and
	// at most a day.
	Spent time.Duration
	// Billable is the billable part of Spent; nil leaves it to Tempo,
	// which bills the whole time spent.
	Billable *time.Duration
	// RemainingEstimate sets the issue's remaining estimate; nil sends
	// none.
	RemainingEstimate *time.Duration
	Description       string
	// Attributes are the worklog's work attribute values, one per key,
	// none empty.
	Attributes []AttributeValue
}

// Change is an update of one worklog. Tempo replaces the whole record on
// an update, so a change starts from the record as read: every field
// the change does not set is sent as From has it, with one exception: when
// From is fully billable (Billable equal to Spent) and the change sets
// Spent but not Billable, the billable time sent is the new Spent.
type Change struct {
	// From is the worklog as read; its ID names the worklog to change.
	From Worklog
	// Issue moves the worklog to another issue, by ID or by Key alone.
	// The v4 API reference lists no issue on an update; Tempo's v4
	// migration notes say an update takes issueId. When the stored record
	// is on another issue than the one named, the outcome is NotMoved.
	Issue *Issue
	Start *Start
	Spent *time.Duration
	// Billable sets the billable time. Left nil, it follows the rule
	// above; a From whose Billable is nil sends none, which leaves it to
	// Tempo.
	Billable          *time.Duration
	RemainingEstimate *time.Duration
	Description       *string
	// SetAttributes sets these values, replacing a value From has for the
	// same key; ClearAttributes removes these keys. A key in both is a
	// problem.
	SetAttributes   []AttributeValue
	ClearAttributes []string
}

// draft is the whole record the change sends.
func (ch Change) draft() Draft {
	from := ch.From
	d := Draft{
		Issue:       from.Issue,
		Author:      from.Author,
		Start:       from.Start,
		Spent:       from.Spent,
		Description: from.Description,
	}
	if ch.Issue != nil {
		d.Issue = *ch.Issue
	}
	if ch.Start != nil {
		d.Start = *ch.Start
	}
	if ch.Spent != nil {
		d.Spent = *ch.Spent
	}
	if ch.Description != nil {
		d.Description = *ch.Description
	}
	d.RemainingEstimate = ch.RemainingEstimate
	switch {
	case ch.Billable != nil:
		d.Billable = ch.Billable
	case from.Billable != nil && *from.Billable == from.Spent:
		spent := d.Spent
		d.Billable = &spent
	case from.Billable != nil:
		billable := *from.Billable
		d.Billable = &billable
	}
	for _, a := range from.Attributes {
		if !slices.Contains(ch.ClearAttributes, a.Key) && !slices.ContainsFunc(ch.SetAttributes, func(s AttributeValue) bool { return s.Key == a.Key }) {
			d.Attributes = append(d.Attributes, a)
		}
	}
	d.Attributes = append(d.Attributes, ch.SetAttributes...)
	return d
}

// worklogBody is a worklog as a create or update sends it. Attributes are
// always sent: an update replaces them, so an empty list clears them.
type worklogBody struct {
	Author      string               `json:"authorAccountId"`
	IssueID     int64                `json:"issueId"`
	StartDate   string               `json:"startDate"`
	StartTime   string               `json:"startTime,omitempty"`
	Spent       int64                `json:"timeSpentSeconds"`
	Billable    *int64               `json:"billableSeconds,omitempty"`
	Remaining   *int64               `json:"remainingEstimateSeconds,omitempty"`
	Description string               `json:"description"`
	Attributes  []attributeValueJSON `json:"attributes"`
}

func bodyOf(d Draft, issueID int64) worklogBody {
	b := worklogBody{
		Author:      d.Author,
		IssueID:     issueID,
		StartDate:   d.Start.Date.String(),
		StartTime:   d.Start.Time.String(),
		Spent:       seconds(d.Spent),
		Description: d.Description,
		Attributes:  make([]attributeValueJSON, 0, len(d.Attributes)),
	}
	if d.Billable != nil {
		s := seconds(*d.Billable)
		b.Billable = &s
	}
	if d.RemainingEstimate != nil {
		s := seconds(*d.RemainingEstimate)
		b.Remaining = &s
	}
	for _, a := range d.Attributes {
		b.Attributes = append(b.Attributes, attributeValueJSON(a))
	}
	return b
}
