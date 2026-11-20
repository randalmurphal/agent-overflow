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

// Lookup finds the issues keys name, archived issues included and
// trashed ones not; a hit is the key asked with the issue's card. Keys
// are matched to current identifiers in one request per 250 keys. Each
// key that leaves unmatched is then read alone by issue(id:), which
// answers an identifier an issue had before it moved team: one request
// per such key, since several aliased issue(id:) that name nothing
// report only the first as an error (measured), so one document cannot
// tell which of them are missing. A key that is not an identifier is
// unmatched without a request.
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
	issues, err := byKeys(ctx, parsed, func(ctx context.Context, chunk []string) (linear.Page[linear.Issue], error) {
		q := linear.IssueQuery{Filter: linear.IssueFilter{Identifiers: chunk}, IncludeArchived: true, Limit: len(chunk)}
		return p.c.ListIssues(ctx, q, "")
	})
	if err != nil {
		return tracker.KeyLookup{}, err
	}
	byKey := map[string]linear.Issue{}
	for _, is := range issues {
		if !is.Trashed {
			byKey[strings.ToUpper(is.Identifier)] = is
		}
	}
	if err := p.lookupFormer(ctx, parsed, byKey); err != nil {
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

// lookupFormer reads alone each of keys byKey lacks and adds the issues
// Linear answers that are not trashed. tracker.CheckLookupKeys refuses a
// key named twice, so each is read once.
func (p *Provider) lookupFormer(ctx context.Context, keys []string, byKey map[string]linear.Issue) error {
	for _, k := range keys {
		up := strings.ToUpper(k)
		if _, ok := byKey[up]; ok {
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
			byKey[up] = is.Issue
		}
	}
	return nil
}
