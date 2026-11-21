package linearprovider

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// stateTypes are Linear's workflow state types in the order its board
// shows them, each with the neutral group it counts in: triage and
// backlog are the backlog, unstarted is to do, started is in progress,
// completed is done, and canceled and duplicate are canceled.
var stateTypes = []stateKind{
	{linear.StateTriage, tracker.GroupBacklog},
	{linear.StateBacklog, tracker.GroupBacklog},
	{linear.StateUnstarted, tracker.GroupToDo},
	{linear.StateStarted, tracker.GroupInProgress},
	{linear.StateCompleted, tracker.GroupDone},
	{linear.StateCanceled, tracker.GroupCanceled},
	{linear.StateDuplicate, tracker.GroupCanceled},
}

type stateKind struct {
	t     linear.StateType
	group tracker.StatusGroup
}

// openStateTypes are the types of the states an issue is not done in.
var openStateTypes = []linear.StateType{linear.StateTriage, linear.StateBacklog, linear.StateUnstarted, linear.StateStarted}

// typeRank is a state type's place in board order; -1 for a type this
// package does not know.
func typeRank(t linear.StateType) int {
	return slices.IndexFunc(stateTypes, func(k stateKind) bool { return k.t == t })
}

// boardStates is states in board order: by type, then position. A state
// of a type Linear did not document is an error, not a guessed group.
func boardStates(states []linear.WorkflowState) ([]linear.WorkflowState, error) {
	for _, s := range states {
		if typeRank(s.Type) < 0 {
			return nil, fmt.Errorf("linearprovider: workflow state %s has unknown type %q", s.ID, s.Type)
		}
	}
	out := slices.Clone(states)
	slices.SortStableFunc(out, func(a, b linear.WorkflowState) int {
		return cmp.Or(cmp.Compare(typeRank(a.Type), typeRank(b.Type)), cmp.Compare(a.Position, b.Position))
	})
	return out, nil
}

// status is a state with the group of its type; boardStates has checked
// the type.
func status(s linear.WorkflowState) tracker.Status {
	return tracker.Status{ID: tracker.StatusID(s.ID), Name: s.Name, Group: stateTypes[typeRank(s.Type)].group}
}

// Structure is the team's board: one column per workflow state in board
// order, the team's estimate scale, its cycles that are open or that
// ended or completed at or after window, every workspace user and the
// team's statuses, read in one request (linear's TeamBoard) unless a
// list is longer than its first page.
func (p *Provider) Structure(ctx context.Context, b tracker.BoardID, window time.Time) (tracker.Structure, error) {
	if b == "" {
		return tracker.Structure{}, fmt.Errorf("linearprovider: structure: %w: no board named", tracker.ErrInvalidInput)
	}
	read, err := p.c.TeamBoard(ctx, string(b), window)
	if err != nil {
		return tracker.Structure{}, fmt.Errorf("linearprovider: structure of team %s: %w", b, err)
	}
	states, err := boardStates(read.States)
	if err != nil {
		return tracker.Structure{}, err
	}
	t := read.Team
	s := tracker.Structure{Board: board(t), Estimate: estimate(t.Estimation), Sprints: sprints(read.Cycles)}
	for _, st := range states {
		s.Columns = append(s.Columns, tracker.Column{Name: st.Name, Statuses: []tracker.StatusID{tracker.StatusID(st.ID)}})
		s.Statuses = append(s.Statuses, status(st))
	}
	for _, u := range read.Users {
		s.Users = append(s.Users, user(u))
	}
	return s, nil
}

// keptCycles are a team's cycles that are active or future, or ended or
// completed at or after window, oldest first. Linear selects them, so
// the read does not grow with the team's past cycles.
func (p *Provider) keptCycles(ctx context.Context, team string, window time.Time) ([]tracker.Sprint, error) {
	cycles, err := p.c.Cycles(ctx, linear.CycleQuery{TeamID: team, KeptSince: window})
	if err != nil {
		return nil, fmt.Errorf("linearprovider: cycles of team %s: %w", team, err)
	}
	return sprints(cycles), nil
}

// sprints are cycles as sprints, oldest first.
func sprints(cycles []linear.Cycle) []tracker.Sprint {
	out := make([]tracker.Sprint, 0, len(cycles))
	for _, c := range cycles {
		out = append(out, sprint(c))
	}
	slices.SortStableFunc(out, func(a, b tracker.Sprint) int { return a.Start.Compare(b.Start) })
	return out
}

// sprint is a cycle: active, future, or closed once past. Start and End
// are the cycle's planned dates, which Linear sets for every cycle;
// Completed is zero until Linear completes it.
func sprint(c linear.Cycle) tracker.Sprint {
	s := tracker.Sprint{
		ID: tracker.SprintID(c.ID), Board: tracker.BoardID(c.TeamID), Name: c.Name, Goal: c.Description,
		Start: c.StartsAt, End: c.EndsAt,
	}
	if s.Name == "" {
		s.Name = "Cycle " + strconv.Itoa(c.Number)
	}
	switch {
	case c.IsActive:
		s.State = tracker.SprintActive
	case c.IsFuture:
		s.State = tracker.SprintFuture
	default:
		s.State = tracker.SprintClosed
	}
	if c.CompletedAt != nil {
		s.Completed = *c.CompletedAt
	}
	return s
}

// estimateScales are Linear's fixed scales
// (https://linear.app/docs/issue-property): the standard values, then
// the two the extended scale adds. T-shirt sizes are stored as the
// Fibonacci numbers Linear maps them to. Static values and a switch
// rather than a map, so linking the package builds nothing at start.
type estimateScale struct {
	values, extended []float64
	labels           []string
}

var (
	exponentialScale = estimateScale{values: []float64{1, 2, 4, 8, 16}, extended: []float64{32, 64}}
	fibonacciScale   = estimateScale{values: []float64{1, 2, 3, 5, 8}, extended: []float64{13, 21}}
	linearScale      = estimateScale{values: []float64{1, 2, 3, 4, 5}, extended: []float64{6, 7}}
	tShirtScale      = estimateScale{values: []float64{1, 2, 3, 5, 8}, extended: []float64{13, 21},
		labels: []string{"XS", "S", "M", "L", "XL", "XXL", "XXXL"}}
)

// estimateScales is the documented scale s names, false for another.
func estimateScales(s linear.EstimateScale) (estimateScale, bool) {
	switch s {
	case linear.EstimateExponential:
		return exponentialScale, true
	case linear.EstimateFibonacci:
		return fibonacciScale, true
	case linear.EstimateLinear:
		return linearScale, true
	case linear.EstimateTShirt:
		return tShirtScale, true
	case linear.EstimateNotUsed:
		// No estimates, so no scale.
	}
	return estimateScale{}, false
}

// estimates reports whether a team with scale s estimates its issues.
func estimates(s linear.EstimateScale) bool {
	return s != linear.EstimateNotUsed && s != ""
}

// estimate is a team's estimate: none when estimates are off, else
// points on the team's scale, with zero first when the team allows it.
// A scale Linear did not document is points with no fixed choices.
func estimate(e linear.Estimation) tracker.Estimate {
	if !estimates(e.Scale) {
		return tracker.Estimate{Unit: tracker.UnitNone}
	}
	scale, ok := estimateScales(e.Scale)
	if !ok {
		return tracker.Estimate{Unit: tracker.UnitPoints}
	}
	values := slices.Clone(scale.values)
	if e.Extended {
		values = append(values, scale.extended...)
	}
	out := tracker.Estimate{Unit: tracker.UnitPoints}
	if e.AllowZero {
		out.Choices = append(out.Choices, tracker.EstimateChoice{Value: 0, Label: "0"})
	}
	for i, v := range values {
		label := strconv.FormatFloat(v, 'f', -1, 64)
		if scale.labels != nil {
			label = scale.labels[i]
		}
		out.Choices = append(out.Choices, tracker.EstimateChoice{Value: v, Label: label})
	}
	return out
}
