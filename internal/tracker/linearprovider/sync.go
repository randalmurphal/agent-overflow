package linearprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// cursorVersion is the version of the cursor encoding this package
// writes; a cursor of any other version is refused.
const cursorVersion = 1

// cursorJSON is a sync cursor's bytes: the version, the team the walk
// polls and the client's whole PollCursor.
type cursorJSON struct {
	Version int       `json:"v"`
	Team    string    `json:"team"`
	Since   time.Time `json:"since"`
	After   string    `json:"after,omitempty"`
	Floor   time.Time `json:"floor"`
}

func encodeCursor(team tracker.BoardID, c linear.PollCursor) (tracker.Cursor, error) {
	b, err := json.Marshal(cursorJSON{Version: cursorVersion, Team: string(team), Since: c.Since, After: c.After, Floor: c.Floor})
	if err != nil {
		return nil, fmt.Errorf("linearprovider: encode the sync cursor: %w", err)
	}
	return b, nil
}

// decodeCursor reads a cursor encodeCursor wrote for team. Bytes that are
// not one, another version and another team's cursor are refused,
// wrapping tracker.ErrInvalidInput, so a cursor is never misread.
func decodeCursor(team tracker.BoardID, b tracker.Cursor) (linear.PollCursor, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c cursorJSON
	if err := dec.Decode(&c); err != nil || dec.More() {
		return linear.PollCursor{}, fmt.Errorf("linearprovider: %w: the sync cursor is not one this package wrote", tracker.ErrInvalidInput)
	}
	switch {
	case c.Version != cursorVersion:
		return linear.PollCursor{}, fmt.Errorf("linearprovider: %w: sync cursor version %d, want %d", tracker.ErrInvalidInput, c.Version, cursorVersion)
	case c.Team != string(team):
		return linear.PollCursor{}, fmt.Errorf("linearprovider: %w: the sync cursor is for team %q, not %q", tracker.ErrInvalidInput, c.Team, team)
	}
	return linear.PollCursor{Since: c.Since, After: c.After, Floor: c.Floor}, nil
}

// ChangedRows is the next page of the team's issues updated at or after
// the cursor, oldest update first, archived and trashed issues left out.
// Archiving or trashing an issue takes it out of the poll and out of
// BoardIssues, so the next reconcile removes its row; unarchiving it
// updates it, so the delta brings it back.
func (p *Provider) ChangedRows(ctx context.Context, q tracker.DeltaQuery) (tracker.RowPage, error) {
	if q.Board == "" {
		return tracker.RowPage{}, fmt.Errorf("linearprovider: changed rows: %w: no board named", tracker.ErrInvalidInput)
	}
	from := linear.PollCursor{Since: q.Start}
	if q.From != nil {
		var err error
		if from, err = decodeCursor(q.Board, q.From); err != nil {
			return tracker.RowPage{}, err
		}
	}
	page, err := p.c.PollIssues(ctx, linear.PollQuery{TeamID: string(q.Board), From: from})
	if err != nil {
		return tracker.RowPage{}, fmt.Errorf("linearprovider: changed rows of %s: %w", q.Board, err)
	}
	rows, err := p.rows(ctx, q.Board, page.Items)
	if err != nil {
		return tracker.RowPage{}, err
	}
	next, err := encodeCursor(q.Board, page.Next)
	if err != nil {
		return tracker.RowPage{}, err
	}
	return tracker.RowPage{Rows: rows, Next: next, More: page.HasNextPage}, nil
}

// Listing bounds: each of BoardIssues's walks reads at most
// listingPages pages of listingPage issues (10,000 issues) and reports
// the listing incomplete past that.
const (
	listingPage  = 250
	listingPages = 40
)

// errTooLong is a walk listAll cut short at its page bound.
var errTooLong = errors.New("linearprovider: the list is longer than its bound")

// BoardIssues lists the team's issues that are open (triage, backlog,
// unstarted or started), updated at or after window (a superset of
// those completed or canceled since), or in a cycle Structure keeps.
// Archived and trashed issues are not listed. A walk past its bound
// makes the listing incomplete.
func (p *Provider) BoardIssues(ctx context.Context, b tracker.BoardID, window time.Time) (tracker.Listing, error) {
	if b == "" {
		return tracker.Listing{}, fmt.Errorf("linearprovider: board issues: %w: no board named", tracker.ErrInvalidInput)
	}
	team := []string{string(b)}
	filters := []linear.IssueFilter{
		{Teams: team, StateTypes: openStateTypes},
		{Teams: team, UpdatedSince: window},
	}
	cycles, err := p.keptCycles(ctx, string(b), window)
	if err != nil {
		return tracker.Listing{}, err
	}
	if len(cycles) > 0 {
		ids := make([]string, 0, len(cycles))
		for _, c := range cycles {
			ids = append(ids, string(c.ID))
		}
		filters = append(filters, linear.IssueFilter{Teams: team, Cycle: linear.Match{IDs: ids}})
	}
	out := tracker.Listing{Complete: true}
	seen := map[string]bool{}
	for _, f := range filters {
		issues, err := p.listAll(ctx, linear.IssueQuery{Filter: f, Limit: listingPage}, listingPages)
		if errors.Is(err, errTooLong) {
			out.Complete = false
		} else if err != nil {
			return tracker.Listing{}, fmt.Errorf("linearprovider: board issues of %s: %w", b, err)
		}
		for _, is := range issues {
			if !seen[is.ID] {
				seen[is.ID] = true
				out.IDs = append(out.IDs, tracker.IssueID(is.ID))
			}
		}
	}
	return out, nil
}
