package jiraprovider

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// timeEstimateField is the system field a board estimates in when it
// estimates by original time, in seconds.
const timeEstimateField = "timeoriginalestimate"

// Structure reads board's structure from Jira: its configuration again,
// which names the board and where it is located (the jira client's cached
// configuration and sprint listing of this board are dropped first, so
// later row reads use what this read returns; other boards keep theirs),
// then together its sprints when it is a scrum board and its statuses
// (boardStatuses).
// Users are not listed: Jira sends them with the records.
func (p *Provider) Structure(ctx context.Context, id tracker.BoardID, window time.Time) (tracker.Structure, error) {
	n, err := boardNumber(id)
	if err != nil {
		return tracker.Structure{}, err
	}
	p.jira.ForgetBoard(n)
	cfg, err := p.jira.BoardConfig(ctx, n)
	if err != nil {
		return tracker.Structure{}, fmt.Errorf("jiraprovider: structure of board %s: %w", id, err)
	}
	if cfg.BoardID != n {
		return tracker.Structure{}, fmt.Errorf("jiraprovider: structure of board %s: the configuration read is of board %d", id, cfg.BoardID)
	}
	out := tracker.Structure{Board: configBoard(cfg), Estimate: estimateOf(cfg)}
	for _, col := range cfg.Columns {
		c := tracker.Column{Name: col.Name}
		for _, s := range col.StatusIDs {
			c.Statuses = append(c.Statuses, tracker.StatusID(s))
		}
		out.Columns = append(out.Columns, c)
	}
	// The sprints and the statuses are read together; the first to fail
	// ends the other and is the error.
	g, gctx := errgroup.WithContext(ctx)
	if cfg.Type == jira.BoardScrum {
		g.Go(func() (err error) {
			// The board was just dropped, so the listing is read now.
			out.Sprints, err = p.sprints(gctx, id, n, window, true)
			return err
		})
	}
	g.Go(func() (err error) {
		if out.Statuses, err = p.boardStatuses(gctx, cfg); err != nil {
			return fmt.Errorf("jiraprovider: structure of board %s: %w", id, err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return tracker.Structure{}, err
	}
	return out, nil
}

// configBoard is the board a configuration names. A board located on a
// user has no project.
func configBoard(cfg jira.BoardConfig) tracker.Board {
	return tracker.Board{
		ID: tracker.BoardID(decimal(cfg.BoardID)), Name: cfg.Name, Sprints: cfg.Type == jira.BoardScrum,
		Project: tracker.ProjectID(cfg.Location.ProjectID()),
	}
}

// boardStatuses are the statuses of the project the board is located in
// and every status its columns name. A column status outside that
// project, every one of a board located on a user, is read from the
// site's statuses, once, only when one is missing. A column status the
// site does not list (in no active workflow, or in projects the user
// cannot browse) is left out: Jira gives no name or category for it, and
// a card in it names it with both.
func (p *Provider) boardStatuses(ctx context.Context, cfg jira.BoardConfig) ([]tracker.Status, error) {
	cat := newCatalog()
	if project := cfg.Location.ProjectID(); project != "" {
		types, err := p.jira.ProjectStatuses(ctx, project)
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

// estimateOf is a board's estimate, of its estimation field: original
// time in seconds, points in any other field, or none for a board that
// estimates by issue count or by nothing (a kanban board, whose
// estimation Jira does not give).
func estimateOf(cfg jira.BoardConfig) tracker.Estimate {
	switch {
	case cfg.Estimation.Type != jira.EstimateField || cfg.Estimation.FieldID == "":
		return tracker.Estimate{Unit: tracker.UnitNone}
	case cfg.Estimation.FieldID == timeEstimateField:
		return tracker.Estimate{Unit: tracker.UnitTime, Field: timeEstimateField}
	}
	return tracker.Estimate{Unit: tracker.UnitPoints, Field: cfg.Estimation.FieldID}
}

// sprints are board's sprints that are active or future, or closed at or
// after window: completed then, or ending then when Jira gives no
// completion, each with the board it was made on (Jira's originBoardId),
// which another board's sprint can name. A sprint whose dates did not
// decode fails the read, since whether it is kept cannot be told.
//
// The listing is the board's the jira client holds, read by the last
// Structure (which drops it first), or read now and held when none is.
// Jira's clock read it at Read: a sprint created since has no issue that
// was not updated since (measured: a sprint change moves the issue's
// updated, 3 of 3 issues whose newest change was one), and a sprint open
// then and closed since closed after Read, so when Read is at or after
// window the held listing selects
// the same issues as a fresh one would in BoardIssues, which also takes
// every issue updated at or after window. A held listing read before
// window, or of unknown age (Jira sent no Date), is read again; fresh
// says the caller dropped the board just before, so the listing is new
// whatever its Read.
func (p *Provider) sprints(ctx context.Context, id tracker.BoardID, n int64, window time.Time, fresh bool) ([]tracker.Sprint, error) {
	list, err := p.jira.BoardSprints(ctx, n)
	if err == nil && !fresh && (list.Read.IsZero() || list.Read.Before(window)) {
		list, err = p.jira.Sprints(ctx, jira.SprintQuery{Board: n})
	}
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: sprints of board %s: %w", id, err)
	}
	var out []tracker.Sprint
	for _, s := range list.Sprints {
		if len(s.Problems) > 0 {
			return nil, fmt.Errorf("jiraprovider: sprint %d of board %s: %s: %w", s.ID, id, s.Problems[0].Field, s.Problems[0].Err)
		}
		sp := tracker.Sprint{
			ID: tracker.SprintID(decimal(s.ID)), Name: s.Name, Goal: s.Goal,
			Start: s.Start, End: s.End, Completed: s.Done,
		}
		if s.BoardID > 0 {
			sp.Board = tracker.BoardID(decimal(s.BoardID))
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
