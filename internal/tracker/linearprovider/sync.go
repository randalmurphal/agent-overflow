package linearprovider

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// cursorVersion is the version of the cursor encoding this package
// writes; a cursor of any other version is refused. Version 1 carried
// Linear's page cursor, which a poll no longer uses; version 2 held no
// drain of a tie larger than a cohort.
const cursorVersion = 3

// cursorJSON is a sync cursor's bytes: the version, the team the walk
// polls and the client's whole PollCursor.
type cursorJSON struct {
	Version int    `json:"v"`
	Team    string `json:"team"`
	positionJSON
	Walk *walkJSON `json:"walk,omitempty"`
}

// positionJSON is a linear.Position's bytes, in a sync cursor and in a
// search page token.
type positionJSON struct {
	Bound time.Time `json:"bound,omitzero"`
	At    []string  `json:"at,omitempty"`
	Drain *string   `json:"drain,omitempty"`
	Past  bool      `json:"past,omitempty"`
}

func positionOf(p linear.Position) positionJSON {
	return positionJSON{Bound: p.Bound, At: p.At, Drain: p.Drain, Past: p.Past}
}

func (p positionJSON) position() linear.Position {
	return linear.Position{Bound: p.Bound, At: p.At, Drain: p.Drain, Past: p.Past}
}

type walkJSON struct {
	Start     time.Time `json:"start,omitzero"`
	Delivered bool      `json:"delivered,omitempty"`
}

func encodeCursor(team tracker.BoardID, c linear.PollCursor) (tracker.Cursor, error) {
	out := cursorJSON{Version: cursorVersion, Team: string(team), positionJSON: positionOf(c.Position)}
	if c.Walk != nil {
		out.Walk = &walkJSON{Start: c.Walk.Start, Delivered: c.Walk.Delivered}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("linearprovider: encode the sync cursor: %w", err)
	}
	return b, nil
}

// decodeStrict reads b, one JSON value, into v: an unknown or repeated
// member, a name in another case and anything after the value are
// refused, so bytes this package did not write are never half read.
func decodeStrict(b []byte, v any) error {
	return jsonv2.Unmarshal(b, v, jsonv2.RejectUnknownMembers(true))
}

// decodeCursor reads a cursor encodeCursor wrote for team. Bytes that are
// not one, another version and another team's cursor are refused,
// wrapping tracker.ErrInvalidInput, so a cursor is never misread.
func decodeCursor(team tracker.BoardID, b tracker.Cursor) (linear.PollCursor, error) {
	var c cursorJSON
	if err := decodeStrict(b, &c); err != nil {
		return linear.PollCursor{}, fmt.Errorf("linearprovider: %w: the sync cursor is not one this package wrote", tracker.ErrInvalidInput)
	}
	switch {
	case c.Version != cursorVersion:
		return linear.PollCursor{}, fmt.Errorf("linearprovider: %w: sync cursor version %d, want %d", tracker.ErrInvalidInput, c.Version, cursorVersion)
	case c.Team != string(team):
		return linear.PollCursor{}, fmt.Errorf("linearprovider: %w: the sync cursor is for team %q, not %q", tracker.ErrInvalidInput, c.Team, team)
	}
	out := linear.PollCursor{Position: c.position()}
	if c.Walk != nil {
		out.Walk = &linear.PollWalk{Start: c.Walk.Start, Delivered: c.Walk.Delivered}
	}
	return out, nil
}

// ChangedRows is the next page of the team's issues updated at or after
// the cursor, oldest update first, archived and trashed issues left out.
// Archiving or trashing an issue takes it out of the poll and out of
// BoardIssues, so the next reconcile removes its row; unarchiving it
// updates it, so the delta brings it back. A walk's last page returns
// where the next walk starts (nextWalk).
func (p *Provider) ChangedRows(ctx context.Context, q tracker.DeltaQuery) (tracker.RowPage, error) {
	if q.Board == "" {
		return tracker.RowPage{}, fmt.Errorf("linearprovider: changed rows: %w: no board named", tracker.ErrInvalidInput)
	}
	from := linear.Restart(q.Start)
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
	resume := page.Next
	if !page.HasNextPage {
		resume = nextWalk(page.Next)
	}
	next, err := encodeCursor(q.Board, resume)
	if err != nil {
		return tracker.RowPage{}, err
	}
	return tracker.RowPage{Rows: rows, Next: next, More: page.HasNextPage}, nil
}

// nextWalk is where the walk after the finished walk c starts. A walk
// that delivered a row resumes by tracker.DeltaResume of the newest row
// it delivered (c's bound) and Linear's clock at its first page: exactly
// after what it delivered, so an idle board delivers nothing, or again
// from the time before which every update was visible to it. A walk that
// delivered nothing resumes where it began.
func nextWalk(c linear.PollCursor) linear.PollCursor {
	if c.Walk == nil || !c.Walk.Delivered {
		return c.Resume()
	}
	restart, exact := tracker.DeltaResume(c.Bound, c.Walk.Start)
	if exact {
		return c.Resume()
	}
	return linear.Restart(restart)
}

// Listing bounds: BoardIssues reads at most listingPages pages of
// listingPage issues (10,000 issues) and reports the listing incomplete
// past that.
const (
	listingPage  = 250
	listingPages = 40
)

// errTooLong is a walk listAll cut short at its page bound.
var errTooLong = errors.New("linearprovider: the list is longer than its bound")

// BoardIssues lists the team's issues that are open (triage, backlog,
// unstarted or started), updated at or after window (a superset of
// those completed or canceled since), or in a cycle Structure keeps,
// reading only their IDs and orders, in one walk of the three
// conditions together, oldest created first: an order no change moves an
// issue in, so the walk by cursor neither skips nor repeats one.
// Archived and trashed issues, and any answered as another team's, are
// not listed. A walk past its bound
// makes the listing incomplete; a walk ends on the first page that says
// no more follow, empty or not.
func (p *Provider) BoardIssues(ctx context.Context, b tracker.BoardID, window time.Time) (tracker.Listing, error) {
	if b == "" {
		return tracker.Listing{}, fmt.Errorf("linearprovider: board issues: %w: no board named", tracker.ErrInvalidInput)
	}
	filter := linear.IssueFilter{Teams: []string{string(b)}}
	if !window.IsZero() {
		cycles, err := p.keptCycles(ctx, string(b), window)
		if err != nil {
			return tracker.Listing{}, err
		}
		filter.AnyOf = []linear.IssueFilter{{StateTypes: openStateTypes}, {UpdatedSince: window}}
		if len(cycles) > 0 {
			ids := make([]string, 0, len(cycles))
			for _, c := range cycles {
				ids = append(ids, string(c.ID))
			}
			filter.AnyOf = append(filter.AnyOf, linear.IssueFilter{Cycle: linear.Match{IDs: ids}})
		}
	}
	q := linear.IssueQuery{Filter: filter, Order: linear.OrderCreatedOldest, Limit: listingPage}
	issues, err := walk(ctx, func(ctx context.Context, after string) (linear.Page[linear.IssuePosition], error) {
		return p.c.ListIssuePositions(ctx, q, after)
	}, listingPages)
	out := tracker.Listing{Complete: true}
	if errors.Is(err, errTooLong) {
		out.Complete = false
	} else if err != nil {
		return tracker.Listing{}, fmt.Errorf("linearprovider: board issues of %s: %w", b, err)
	}
	seen := make(map[string]bool, len(issues))
	for _, is := range issues {
		if is.TeamID == string(b) && !seen[is.ID] {
			seen[is.ID] = true
			out.IDs = append(out.IDs, tracker.IssueID(is.ID))
		}
	}
	return out, nil
}
