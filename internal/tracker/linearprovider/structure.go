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

// teamStates reads a team's states in board order.
func (p *Provider) teamStates(ctx context.Context, team string) ([]linear.WorkflowState, error) {
	states, err := p.c.WorkflowStates(ctx, team)
	if err != nil {
		return nil, fmt.Errorf("linearprovider: workflow states of team %s: %w", team, err)
	}
	return boardStates(states)
}

// Structure is the team's board: one column per workflow state in board
// order, the team's estimate scale, its cycles that are open or that
// ended or completed at or after window, every workspace user and the
// team's statuses.
func (p *Provider) Structure(ctx context.Context, b tracker.BoardID, window time.Time) (tracker.Structure, error) {
	t, err := p.team(ctx, string(b))
	if err != nil {
		return tracker.Structure{}, err
	}
	states, err := p.teamStates(ctx, t.ID)
	if err != nil {
		return tracker.Structure{}, err
	}
	sprints, err := p.keptCycles(ctx, t.ID, window)
	if err != nil {
		return tracker.Structure{}, err
	}
	users, err := p.c.Users(ctx, true)
	if err != nil {
		return tracker.Structure{}, fmt.Errorf("linearprovider: users: %w", err)
	}
	s := tracker.Structure{Board: board(t), Estimate: estimate(t.Estimation), Sprints: sprints}
	for _, st := range states {
		s.Columns = append(s.Columns, tracker.Column{Name: st.Name, Statuses: []tracker.StatusID{tracker.StatusID(st.ID)}})
		s.Statuses = append(s.Statuses, status(st))
	}
	for _, u := range users {
		s.Users = append(s.Users, user(u))
	}
	return s, nil
}

// keptCycles are a team's cycles that are active or future, or ended or
// completed at or after window, oldest first.
func (p *Provider) keptCycles(ctx context.Context, team string, window time.Time) ([]tracker.Sprint, error) {
	cycles, err := p.c.Cycles(ctx, linear.CycleQuery{TeamID: team})
	if err != nil {
		return nil, fmt.Errorf("linearprovider: cycles of team %s: %w", team, err)
	}
	var out []tracker.Sprint
	for _, c := range cycles {
		s := sprint(c)
		if s.State != tracker.SprintClosed || !s.End.Before(window) || !s.Completed.Before(window) {
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b tracker.Sprint) int { return a.Start.Compare(b.Start) })
	return out, nil
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
// Fibonacci numbers Linear maps them to.
var estimateScales = map[linear.EstimateScale]struct {
	values, extended []float64
	labels           []string
}{
	linear.EstimateExponential: {values: []float64{1, 2, 4, 8, 16}, extended: []float64{32, 64}},
	linear.EstimateFibonacci:   {values: []float64{1, 2, 3, 5, 8}, extended: []float64{13, 21}},
	linear.EstimateLinear:      {values: []float64{1, 2, 3, 4, 5}, extended: []float64{6, 7}},
	linear.EstimateTShirt: {values: []float64{1, 2, 3, 5, 8}, extended: []float64{13, 21},
		labels: []string{"XS", "S", "M", "L", "XL", "XXL", "XXXL"}},
}

// estimate is a team's estimate: none when estimates are off, else
// points on the team's scale, with zero first when the team allows it.
// A scale Linear did not document is points with no fixed choices.
func estimate(e linear.Estimation) tracker.Estimate {
	if e.Scale == linear.EstimateNotUsed || e.Scale == "" {
		return tracker.Estimate{Unit: tracker.UnitNone}
	}
	scale, ok := estimateScales[e.Scale]
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
