package linear

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// IssueFilter selects issues. Every criterion set must hold; within one
// criterion any listed value matches. The zero value selects every issue.
// It is sent as the schema's IssueFilter input, never as query text.
type IssueFilter struct {
	// IDs selects issues by ID (Issue.ID, a lowercase UUID); any other
	// spelling is refused before sending, since Linear would fail the
	// whole read on it.
	IDs []string
	// Identifiers selects issues by their current identifier, such as
	// WEB-12: team key (case ignored) and number. An identifier the issue
	// had before it moved team does not match. Anything ParseIdentifier
	// refuses is refused before sending.
	Identifiers []string
	Teams       []string // team IDs
	States      []string // workflow state IDs
	StateTypes  []StateType
	Cycle       Match
	Assignee    Match
	Project     Match
	// Parent selects sub-issues of the listed issues, or top-level
	// issues with None.
	Parent Match
	// Labels selects issues carrying any of the label IDs.
	Labels []string
	// UpdatedSince selects issues updated at or after it.
	UpdatedSince time.Time
	// AnyOf selects issues that at least one of its filters selects
	// (Linear's or, measured to be accepted); each must select something
	// out. An empty AnyOf selects nothing out.
	AnyOf []IssueFilter
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
		ID        *idComparator      `json:"id,omitempty"`
		Number    *numberComparator  `json:"number,omitempty"`
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
		Key  *keyCompare   `json:"key,omitempty"`
		Null *bool         `json:"null,omitempty"`
	}
	keyCompare struct {
		EqIgnoreCase string `json:"eqIgnoreCase"`
	}
	numberComparator struct {
		In []int `json:"in"`
	}
	idComparator struct {
		Eq  string   `json:"eq,omitempty"`
		In  []string `json:"in,omitempty"`
		Nin []string `json:"nin,omitempty"`
	}
	stateFilterInput struct {
		ID   *idComparator `json:"id,omitempty"`
		Type *idComparator `json:"type,omitempty"` // a StringComparator's eq and in
	}
	labelsFilterInput struct {
		Some *entityFilter `json:"some"`
	}
	dateComparator struct {
		Eq  string `json:"eq,omitempty"`
		Gte string `json:"gte,omitempty"`
		Gt  string `json:"gt,omitempty"`
		Lte string `json:"lte,omitempty"`
		Lt  string `json:"lt,omitempty"`
	}
)

func idEq(id string) *entityFilter { return &entityFilter{ID: &idComparator{Eq: id}} }

func idIn(ids []string) *idComparator { return &idComparator{In: ids} }

// input builds the filter's wire form: an and of one clause per
// criterion, in a fixed order; nil for the zero filter.
func (f IssueFilter) input() (*issueFilterInput, error) {
	var and []issueFilterInput
	add := func(clause issueFilterInput) { and = append(and, clause) }
	for _, id := range f.IDs {
		if !IsIssueID(id) {
			return nil, fmt.Errorf("IDs holds %q, not an issue ID (a lowercase UUID)", id)
		}
	}
	if len(f.IDs) > 0 {
		add(issueFilterInput{ID: idIn(f.IDs)})
	}
	if len(f.Identifiers) > 0 {
		clause, err := identifierClause(f.Identifiers)
		if err != nil {
			return nil, err
		}
		add(clause)
	}
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
	if len(f.AnyOf) > 0 {
		or := make([]issueFilterInput, 0, len(f.AnyOf))
		for i, sub := range f.AnyOf {
			in, err := sub.input()
			if err != nil {
				return nil, fmt.Errorf("AnyOf[%d]: %w", i, err)
			}
			if in == nil {
				return nil, fmt.Errorf("AnyOf[%d] selects every issue", i)
			}
			or = append(or, *in)
		}
		add(issueFilterInput{Or: or})
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

// identifierClause selects issues by identifier: one alternative per
// team key, matching the key with case ignored and any of its numbers.
func identifierClause(identifiers []string) (issueFilterInput, error) {
	var keys []string
	numbers := map[string][]int{}
	for _, s := range identifiers {
		key, n, ok := ParseIdentifier(s)
		if !ok {
			return issueFilterInput{}, fmt.Errorf("the identifiers hold %q, not an issue identifier", s)
		}
		key = strings.ToUpper(key)
		if _, seen := numbers[key]; !seen {
			keys = append(keys, key)
		}
		if !slices.Contains(numbers[key], n) {
			numbers[key] = append(numbers[key], n)
		}
	}
	or := make([]issueFilterInput, 0, len(keys))
	for _, key := range keys {
		or = append(or, issueFilterInput{
			Team:   &entityFilter{Key: &keyCompare{EqIgnoreCase: key}},
			Number: &numberComparator{In: numbers[key]},
		})
	}
	return issueFilterInput{Or: or}, nil
}

// ParseIdentifier splits an issue identifier such as WEB-12 into its
// team key and number. The key is ASCII letters and digits; the number
// is a positive decimal of at most nine digits with no sign or leading
// zero. ok is false for anything else.
func ParseIdentifier(s string) (teamKey string, number int, ok bool) {
	i := strings.LastIndexByte(s, '-')
	if i <= 0 || i == len(s)-1 {
		return "", 0, false
	}
	key, digits := s[:i], s[i+1:]
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return "", 0, false
		}
	}
	if digits[0] == '0' || len(digits) > 9 {
		return "", 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", 0, false
		}
		number = number*10 + int(r-'0')
	}
	return key, number, true
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
