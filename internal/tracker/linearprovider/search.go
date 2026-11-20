package linearprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
)

// searchTokenVersion is the version of the search page token this
// package writes; a token of any other version is refused. Version 1
// held no drain of a tie larger than a cohort.
const searchTokenVersion = 2

// searchToken is a search page token: the project the search pages (empty
// for every team) and where its next page starts, Linear's page cursor
// for a text search (After) or the position of a listing most recently
// updated first.
type searchToken struct {
	Version int    `json:"v"`
	Team    string `json:"team,omitempty"`
	After   string `json:"after,omitempty"`
	positionJSON
}

func encodeSearchToken(t searchToken) (tracker.PageToken, error) {
	t.Version = searchTokenVersion
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("linearprovider: encode the search page token: %w", err)
	}
	return tracker.PageToken(b), nil
}

// decodeSearchToken reads a token encodeSearchToken wrote for a search of
// team, a text search when text is set. Any other token is refused,
// wrapping tracker.ErrInvalidInput, so a token is never misread.
func decodeSearchToken(b tracker.PageToken, team string, text bool) (searchToken, error) {
	var t searchToken
	if err := decodeStrict([]byte(b), &t); err != nil {
		return searchToken{}, fmt.Errorf("linearprovider: %w: the search page token is not one this package wrote", tracker.ErrInvalidInput)
	}
	switch {
	case t.Version != searchTokenVersion:
		return searchToken{}, fmt.Errorf("linearprovider: %w: search page token version %d, want %d", tracker.ErrInvalidInput, t.Version, searchTokenVersion)
	case t.Team != team:
		return searchToken{}, fmt.Errorf("linearprovider: %w: the search page token is for project %q, not %q", tracker.ErrInvalidInput, t.Team, team)
	case text != (t.After != "") || text == !t.Bound.IsZero() || text && (t.At != nil || t.Drain != nil || t.Past):
		return searchToken{}, fmt.Errorf("linearprovider: %w: the search page token is not one of this kind of search", tracker.ErrInvalidInput)
	}
	return t, nil
}

// Search is one page of the cards q matches, archived issues left out:
// Linear's full-text search of q.Text in its order of relevance, within
// q.Project when set; or, with no text, q.Project's issues most recently
// updated first. The text is passed to Linear as a value, never as a
// query language.
//
// The listing pages by position (linear.RecentIssues), never by Linear's
// cursor, which follows its issue: an issue updated between pages moves
// to the front and is not shown again, and no other issue is skipped or
// shown twice. A text search pages by Linear's cursor, since relevance
// has no value a position could bound; what an update between its pages
// does to that order is not measured.
func (p *Provider) Search(ctx context.Context, q tracker.SearchQuery) (tracker.SearchPage, error) {
	if err := q.Check(); err != nil {
		return tracker.SearchPage{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = tracker.MaxSearchPage
	}
	team, text := string(q.Project), strings.TrimSpace(q.Text)
	var from searchToken
	if q.Page != "" {
		var err error
		if from, err = decodeSearchToken(q.Page, team, text != ""); err != nil {
			return tracker.SearchPage{}, err
		}
	}
	next := searchToken{Team: team}
	var page linear.Page[linear.Issue]
	var err error
	if text != "" {
		page, err = p.c.SearchIssues(ctx, linear.SearchQuery{Term: text, TeamID: team, Limit: limit}, from.After)
		next.After = page.EndCursor
	} else {
		var recent linear.RecentPage
		recent, err = p.c.RecentIssues(ctx, linear.RecentQuery{Filter: linear.IssueFilter{Teams: []string{team}}, Limit: limit,
			From: from.position()})
		page, next.positionJSON = recent.Page, positionOf(recent.Next)
	}
	if err != nil {
		return tracker.SearchPage{}, fmt.Errorf("linearprovider: search: %w", err)
	}
	cards, err := p.cards(ctx, page.Items)
	if err != nil {
		return tracker.SearchPage{}, err
	}
	out := tracker.SearchPage{Cards: cards}
	if page.HasNextPage {
		if out.Next, err = encodeSearchToken(next); err != nil {
			return tracker.SearchPage{}, err
		}
	}
	return out, nil
}
