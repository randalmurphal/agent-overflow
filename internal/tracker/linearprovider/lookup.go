package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/trackerapi"
)

// Lookup finds the issues keys name. Keys are matched to current
// identifiers in requests of 250 (archived issues included, trashed ones
// not). A key still unmatched whose team key names a visible team, which
// one read of the teams by those keys tells, is then read alone, light,
// since Linear may answer an identifier an issue had before it moved
// team; a hit is the key asked with the issue's card. A key that is not
// an identifier is unmatched without a request.
func (p *Provider) Lookup(ctx context.Context, keys []string) (tracker.KeyLookup, error) {
	if err := tracker.CheckLookupKeys(keys); err != nil {
		return tracker.KeyLookup{}, err
	}
	var parsed []string
	for _, k := range keys {
		if _, _, ok := linear.ParseIdentifier(k); ok {
			parsed = append(parsed, k)
		}
	}
	byKey := map[string]linear.Issue{}
	for chunk := range slices.Chunk(parsed, idChunk) {
		q := linear.IssueQuery{Filter: linear.IssueFilter{Identifiers: chunk}, IncludeArchived: true, Limit: len(chunk)}
		issues, err := p.listAll(ctx, q, 0)
		if err != nil {
			return tracker.KeyLookup{}, err
		}
		for _, is := range issues {
			if !is.Trashed {
				byKey[strings.ToUpper(is.Identifier)] = is
			}
		}
	}
	if err := p.lookupMoved(ctx, parsed, byKey); err != nil {
		return tracker.KeyLookup{}, err
	}
	var found []linear.Issue
	for _, k := range keys {
		if is, ok := byKey[strings.ToUpper(k)]; ok {
			found = append(found, is)
		}
	}
	cards, err := p.cards(ctx, found)
	if err != nil {
		return tracker.KeyLookup{}, err
	}
	var out tracker.KeyLookup
	for _, k := range keys {
		is, ok := byKey[strings.ToUpper(k)]
		if !ok {
			out.Unmatched = append(out.Unmatched, k)
			continue
		}
		i := slices.IndexFunc(found, func(f linear.Issue) bool { return f.ID == is.ID })
		out.Found = append(out.Found, tracker.KeyMatch{Key: k, Card: cards[i]})
	}
	return out, nil
}

// lookupMoved reads alone each of keys byKey lacks whose team key names a
// team the key can see, and adds those Linear answers. The teams are read
// by the team keys left, not as a catalog.
func (p *Provider) lookupMoved(ctx context.Context, keys []string, byKey map[string]linear.Issue) error {
	var left, teamKeys []string
	for _, k := range keys {
		if _, ok := byKey[strings.ToUpper(k)]; !ok {
			left = append(left, k)
			teamKey, _, _ := linear.ParseIdentifier(k)
			teamKeys = append(teamKeys, teamKey)
		}
	}
	if len(left) == 0 {
		return nil
	}
	teams, err := p.c.TeamsByKey(ctx, teamKeys)
	if err != nil {
		return fmt.Errorf("linearprovider: lookup: teams by key: %w", err)
	}
	for _, k := range left {
		teamKey, _, _ := linear.ParseIdentifier(k)
		if !slices.ContainsFunc(teams, func(t linear.Team) bool { return strings.EqualFold(t.Key, teamKey) }) {
			continue
		}
		is, err := p.c.IssueSummary(ctx, k)
		if errors.Is(err, trackerapi.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("linearprovider: lookup %s: %w", k, err)
		}
		if !is.Trashed {
			byKey[strings.ToUpper(k)] = *is
		}
	}
	return nil
}

// Search is one page of the cards q matches, archived issues left out:
// Linear's full-text search of q.Text in its order of relevance, within
// q.Project when set; or, with no text, q.Project's issues most recently
// updated first. The text is passed to Linear as a value, never as a
// query language.
func (p *Provider) Search(ctx context.Context, q tracker.SearchQuery) (tracker.SearchPage, error) {
	if err := q.Check(); err != nil {
		return tracker.SearchPage{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = tracker.MaxSearchPage
	}
	var page linear.Page[linear.Issue]
	var err error
	if text := strings.TrimSpace(q.Text); text != "" {
		page, err = p.c.SearchIssues(ctx, linear.SearchQuery{Term: text, TeamID: string(q.Project), Limit: limit}, string(q.Page))
	} else {
		filter := linear.IssueFilter{Teams: []string{string(q.Project)}}
		page, err = p.c.ListIssues(ctx, linear.IssueQuery{Filter: filter, Order: linear.OrderUpdatedNewest, Limit: limit}, string(q.Page))
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
		out.Next = tracker.PageToken(page.EndCursor)
	}
	return out, nil
}
