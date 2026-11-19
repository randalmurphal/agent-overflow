package jiraprovider

import (
	"context"
	"fmt"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// timeEstimateField is the system field a board estimates in when it
// estimates by original time, in seconds.
const timeEstimateField = "timeoriginalestimate"

// Structure reads board's structure from Jira: its configuration again
// (the jira client's cached configurations are dropped first, so later
// row reads use what this read returns), the board, its sprints when it
// is a scrum board, and its statuses (boardStatuses). Users are not
// listed: Jira sends them with the records.
func (p *Provider) Structure(ctx context.Context, id tracker.BoardID, window time.Time) (tracker.Structure, error) {
	n, err := boardNumber(id)
	if err != nil {
		return tracker.Structure{}, err
	}
	p.jira.ForgetBoards()
	cfg, err := p.jira.BoardConfig(ctx, n)
	if err != nil {
		return tracker.Structure{}, fmt.Errorf("jiraprovider: structure of board %s: %w", id, err)
	}
	b, err := p.jira.Board(ctx, n)
	if err != nil {
		return tracker.Structure{}, fmt.Errorf("jiraprovider: structure of board %s: %w", id, err)
	}
	out := tracker.Structure{Board: boardOf(b), Estimate: estimateOf(cfg)}
	for _, col := range cfg.Columns {
		c := tracker.Column{Name: col.Name}
		for _, s := range col.StatusIDs {
			c.Statuses = append(c.Statuses, tracker.StatusID(s))
		}
		out.Columns = append(out.Columns, c)
	}
	if b.Type == jira.BoardScrum {
		if out.Sprints, err = p.sprints(ctx, id, n, window); err != nil {
			return tracker.Structure{}, err
		}
	}
	if out.Statuses, err = p.boardStatuses(ctx, b, cfg); err != nil {
		return tracker.Structure{}, fmt.Errorf("jiraprovider: structure of board %s: %w", id, err)
	}
	return out, nil
}

// boardStatuses are the statuses of the project board b is located in
// and every status its columns name. A column status outside that
// project, every one of a board located under a user, is read from the
// site's statuses, once, only when one is missing. A column status the
// site does not list (in no active workflow, or in projects the user
// cannot browse) is left out: Jira gives no name or category for it, and
// a card in it names it with both.
func (p *Provider) boardStatuses(ctx context.Context, b jira.Board, cfg jira.BoardConfig) ([]tracker.Status, error) {
	cat := newCatalog()
	if b.Location.ProjectID > 0 {
		types, err := p.jira.ProjectStatuses(ctx, decimal(b.Location.ProjectID))
		if err != nil {
			return nil, err
		}
		for _, t := range types {
			for _, s := range t.Statuses {
				cat.status(&s)
			}
		}
	}
	missing := map[string]bool{}
	for _, col := range cfg.Columns {
		for _, s := range col.StatusIDs {
			if !cat.statuses[tracker.StatusID(s)] {
				missing[s] = true
			}
		}
	}
	if len(missing) == 0 {
		return cat.out.Statuses, nil
	}
	site, err := p.jira.Statuses(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range site {
		if missing[s.ID] {
			cat.status(&s)
		}
	}
	return cat.out.Statuses, nil
}

// estimateOf is a board's estimate: original time in seconds, points in
// any other field, or none for a board that estimates by issue count or
// by nothing (a kanban board, whose estimation Jira does not give).
func estimateOf(cfg jira.BoardConfig) tracker.Estimate {
	switch {
	case cfg.Estimation.Type != jira.EstimateField || cfg.Estimation.FieldID == "":
		return tracker.Estimate{Unit: tracker.UnitNone}
	case cfg.Estimation.FieldID == timeEstimateField:
		return tracker.Estimate{Unit: tracker.UnitTime}
	}
	return tracker.Estimate{Unit: tracker.UnitPoints}
}

// sprints are board's sprints that are active or future, or closed at or
// after window: completed then, or ending then when Jira gives no
// completion. A sprint whose dates did not decode fails the read, since
// whether it is kept cannot be told.
func (p *Provider) sprints(ctx context.Context, id tracker.BoardID, n int64, window time.Time) ([]tracker.Sprint, error) {
	list, err := p.jira.Sprints(ctx, jira.SprintQuery{Board: n})
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: sprints of board %s: %w", id, err)
	}
	var out []tracker.Sprint
	for _, s := range list.Sprints {
		if len(s.Problems) > 0 {
			return nil, fmt.Errorf("jiraprovider: sprint %d of board %s: %s: %w", s.ID, id, s.Problems[0].Field, s.Problems[0].Err)
		}
		sp := tracker.Sprint{
			ID: tracker.SprintID(decimal(s.ID)), Board: id, Name: s.Name, Goal: s.Goal,
			Start: s.Start, End: s.End, Completed: s.Done,
		}
		switch s.State {
		case jira.SprintActive:
			sp.State = tracker.SprintActive
		case jira.SprintFuture:
			sp.State = tracker.SprintFuture
		case jira.SprintClosed:
			sp.State = tracker.SprintClosed
			if closedAt(s).Before(window) {
				continue
			}
		default:
			return nil, fmt.Errorf("jiraprovider: sprint %d of board %s is in state %q", s.ID, id, s.State)
		}
		out = append(out, sp)
	}
	return out, nil
}

// closedAt is when a closed sprint ended: its completion, or its end when
// Jira gives no completion.
func closedAt(s jira.Sprint) time.Time {
	if !s.Done.IsZero() {
		return s.Done
	}
	return s.End
}
