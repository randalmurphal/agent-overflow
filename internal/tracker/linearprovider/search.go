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
// q.Project when set; or, with no text, the issues of q.Project (every
// team when empty) most recently updated first. Either is narrowed by
// q's filters (searchFilter). The text and the filters are passed to
// Linear as values, never as a query language.
// Either names the states and assignees of its cards in the page's
// catalog.
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
	filter := searchFilter(q)
	var hits linear.Page[linear.SearchHit]
	var err error
	if text != "" {
		hits, err = p.c.SearchIssues(ctx, linear.SearchQuery{Term: text, TeamID: team, Filter: filter, Limit: limit}, from.After)
		next.After = hits.EndCursor
	} else {
		var recent linear.RecentPage
		if team != "" {
			filter.Teams = []string{team}
		}
		recent, err = p.c.RecentIssues(ctx, linear.RecentQuery{Filter: filter, Limit: limit, From: from.position()})
		hits, next.positionJSON = recent.Page, positionOf(recent.Next)
	}
	if err != nil {
		return tracker.SearchPage{}, fmt.Errorf("linearprovider: search: %w", err)
	}
	catalog, err := hitsCatalog(hits.Items)
	if err != nil {
		return tracker.SearchPage{}, err
	}
	issues := make([]linear.Issue, len(hits.Items))
	for i, h := range hits.Items {
		issues[i] = h.Issue
	}
	cards, err := p.cards(ctx, issues)
	if err != nil {
		return tracker.SearchPage{}, err
	}
	out := tracker.SearchPage{Cards: cards, Catalog: catalog}
	if hits.HasNextPage {
		if out.Next, err = encodeSearchToken(next); err != nil {
			return tracker.SearchPage{}, err
		}
	}
	return out, nil
}

// searchFilter is q's filters as Linear's: the viewer's issues, a state
// by name or the state types of a group (stateTypes) or of open work
// (openStateTypes), and a cycle in progress.
func searchFilter(q tracker.SearchQuery) linear.IssueFilter {
	f := linear.IssueFilter{AssignedToMe: q.Mine, StateName: q.Status.Name, ActiveCycle: q.ActiveSprint}
	switch {
	case q.Status.Open:
		f.StateTypes = openStateTypes
	case q.Status.Group != 0:
		for _, k := range stateTypes {
			if k.group == q.Status.Group {
				f.StateTypes = append(f.StateTypes, k.t)
			}
		}
	}
	return f
}

// hitsCatalog is the states and assignees hits name, each once.
func hitsCatalog(hits []linear.SearchHit) (tracker.Catalog, error) {
	var c tracker.Catalog
	statuses, users := map[string]bool{}, map[string]bool{}
	for _, h := range hits {
		if s := h.State; s != nil && !statuses[s.ID] {
			if typeRank(s.Type) < 0 {
				return tracker.Catalog{}, fmt.Errorf("linearprovider: workflow state %s has unknown type %q", s.ID, s.Type)
			}
			statuses[s.ID] = true
			c.Statuses = append(c.Statuses, status(*s))
		}
		if u := h.Assignee; u != nil && !users[u.ID] {
			users[u.ID] = true
			c.Users = append(c.Users, user(*u))
		}
	}
	return c, nil
}
