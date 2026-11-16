package linear

import (
	"errors"
	"fmt"
	"time"
)

// IssueFilter selects issues. Every criterion set must hold; within one
// criterion any listed value matches. The zero value selects every issue.
// It is sent as the schema's IssueFilter input, never as query text.
type IssueFilter struct {
	Teams      []string // team IDs
	States     []string // workflow state IDs
	StateTypes []StateType
	Cycle      Match
	Assignee   Match
	Project    Match
	// Parent selects sub-issues of the listed issues, or top-level
	// issues with None.
	Parent Match
	// Labels selects issues carrying any of the label IDs.
	Labels []string
	// UpdatedSince selects issues updated at or after it.
	UpdatedSince time.Time
}

// Match selects issues by a related entity: one of IDs, or none when
// None is set. Both may be given; the zero value selects nothing out.
type Match struct {
	IDs  []string
	None bool
}

// The wire form of the schema's filter inputs, as far as IssueFilter
// uses them.
type (
	issueFilterInput struct {
		And       []issueFilterInput `json:"and,omitempty"`
		Or        []issueFilterInput `json:"or,omitempty"`
		Team      *entityFilter      `json:"team,omitempty"`
		State     *stateFilterInput  `json:"state,omitempty"`
		Cycle     *entityFilter      `json:"cycle,omitempty"`
		Assignee  *entityFilter      `json:"assignee,omitempty"`
		Project   *entityFilter      `json:"project,omitempty"`
		Parent    *entityFilter      `json:"parent,omitempty"`
		Labels    *labelsFilterInput `json:"labels,omitempty"`
		UpdatedAt *dateComparator    `json:"updatedAt,omitempty"`
	}
	entityFilter struct {
		ID   *idComparator `json:"id,omitempty"`
		Null *bool         `json:"null,omitempty"`
	}
	idComparator struct {
		Eq string   `json:"eq,omitempty"`
		In []string `json:"in,omitempty"`
	}
	stateFilterInput struct {
		ID   *idComparator `json:"id,omitempty"`
		Type *idComparator `json:"type,omitempty"` // a StringComparator's eq and in
	}
	labelsFilterInput struct {
		Some *entityFilter `json:"some"`
	}
	dateComparator struct {
		Gte string `json:"gte"`
	}
)

func idEq(id string) *entityFilter { return &entityFilter{ID: &idComparator{Eq: id}} }

func idIn(ids []string) *idComparator { return &idComparator{In: ids} }

// input builds the filter's wire form: an and of one clause per
// criterion, in a fixed order; nil for the zero filter.
func (f IssueFilter) input() (*issueFilterInput, error) {
	var and []issueFilterInput
	add := func(clause issueFilterInput) { and = append(and, clause) }
	if err := checkIDs("Teams", f.Teams); err != nil {
		return nil, err
	}
	if len(f.Teams) > 0 {
		add(issueFilterInput{Team: &entityFilter{ID: idIn(f.Teams)}})
	}
	if err := checkIDs("States", f.States); err != nil {
		return nil, err
	}
	if len(f.States) > 0 {
		add(issueFilterInput{State: &stateFilterInput{ID: idIn(f.States)}})
	}
	if len(f.StateTypes) > 0 {
		types := make([]string, len(f.StateTypes))
		for i, t := range f.StateTypes {
			if t == "" {
				return nil, errors.New("StateTypes holds an empty type")
			}
			types[i] = string(t)
		}
		add(issueFilterInput{State: &stateFilterInput{Type: idIn(types)}})
	}
	for _, m := range []struct {
		name  string
		match Match
		set   func(*issueFilterInput, *entityFilter)
	}{
		{"Cycle", f.Cycle, func(c *issueFilterInput, e *entityFilter) { c.Cycle = e }},
		{"Assignee", f.Assignee, func(c *issueFilterInput, e *entityFilter) { c.Assignee = e }},
		{"Project", f.Project, func(c *issueFilterInput, e *entityFilter) { c.Project = e }},
		{"Parent", f.Parent, func(c *issueFilterInput, e *entityFilter) { c.Parent = e }},
	} {
		clause, ok, err := m.match.clause(m.name, m.set)
		if err != nil {
			return nil, err
		}
		if ok {
			add(clause)
		}
	}
	if err := checkIDs("Labels", f.Labels); err != nil {
		return nil, err
	}
	if len(f.Labels) > 0 {
		add(issueFilterInput{Labels: &labelsFilterInput{Some: &entityFilter{ID: idIn(f.Labels)}}})
	}
	if !f.UpdatedSince.IsZero() {
		add(issueFilterInput{UpdatedAt: &dateComparator{Gte: formatTime(f.UpdatedSince)}})
	}
	if len(and) == 0 {
		return nil, nil
	}
	return &issueFilterInput{And: and}, nil
}

// clause is m's condition on the relation set writes, or false when m
// selects nothing out.
func (m Match) clause(name string, set func(*issueFilterInput, *entityFilter)) (issueFilterInput, bool, error) {
	if err := checkIDs(name, m.IDs); err != nil {
		return issueFilterInput{}, false, err
	}
	var ids, none issueFilterInput
	if len(m.IDs) > 0 {
		set(&ids, &entityFilter{ID: idIn(m.IDs)})
	}
	if m.None {
		yes := true
		set(&none, &entityFilter{Null: &yes})
	}
	switch {
	case len(m.IDs) > 0 && m.None:
		return issueFilterInput{Or: []issueFilterInput{ids, none}}, true, nil
	case len(m.IDs) > 0:
		return ids, true, nil
	case m.None:
		return none, true, nil
	}
	return issueFilterInput{}, false, nil
}

func checkIDs(name string, ids []string) error {
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("%s holds an empty ID", name)
		}
	}
	return nil
}

// formatTime writes t as Linear's DateTime: UTC with milliseconds, the
// precision Linear stores. Sub-millisecond parts are dropped, which only
// widens an at-or-after bound.
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
