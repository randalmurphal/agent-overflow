package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// Lookup finds the issues keys name with one bulk fetch of the keys of
// an issue key's shape, with the card fields (MaxLookupKeys is within one
// request's capacity, see jira.Client.BulkFetch). The bulk fetch finds an
// issue by an old key and by a key in another case (documented) and
// returns it under its current key, so an issue returned under a key
// asked, case ignored, is that key's. Every key left over is read alone,
// detailsAtOnce of those reads in flight together: it is an old key of an
// issue, which the read answers with the issue, or names nothing, which
// the read's 404 says (measured, the bulk fetch
// leaves such a key out with no issueErrors entry, so only the read alone
// tells the two apart). The jira client takes an answer that leaves
// issues out, or a 404, as the user's only when it names the account it
// was made for (X-AAccountId, on every answer to a valid token) or a
// sign-in check shows it, so a lost credential does not read as missing
// keys.
func (p *Provider) Lookup(ctx context.Context, keys []string) (tracker.KeyLookup, error) {
	if err := tracker.CheckLookupKeys(keys); err != nil {
		return tracker.KeyLookup{}, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.KeyLookup{}, err
	}
	cat := newCatalog()
	found := map[string]tracker.Card{} // by key as asked
	// Text of another shape names no issue and is not sent; a number in
	// particular would be read as an issue ID.
	var shaped []string
	for _, k := range keys {
		if jira.IsIssueKey(k) {
			shaped = append(shaped, k)
		}
	}
	if len(shaped) > 0 {
		if err := p.lookupKeys(ctx, s, shaped, found, cat); err != nil {
			return tracker.KeyLookup{}, err
		}
	}
	var out tracker.KeyLookup
	for _, k := range keys {
		if c, ok := found[k]; ok {
			out.Found = append(out.Found, tracker.KeyMatch{Key: k, Card: c})
		} else {
			out.Unmatched = append(out.Unmatched, k)
		}
	}
	out.Catalog = cat.out
	return out, nil
}

// lookupKeys finds the issues of keys, recording each card under the key
// asked.
func (p *Provider) lookupKeys(ctx context.Context, s site, keys []string, found map[string]tracker.Card, cat *catalog) error {
	res, err := p.jira.BulkFetch(ctx, keys, s.cardFields()...)
	if err != nil {
		return fmt.Errorf("jiraprovider: look up %d keys: %w", len(keys), err)
	}
	for _, is := range res.Issues {
		i := slices.IndexFunc(keys, func(k string) bool { return strings.EqualFold(k, is.Key) })
		if i < 0 {
			continue // asked by an old key, which is read alone
		}
		if found[keys[i]], err = s.cardOf(is, cat); err != nil {
			return err
		}
	}
	var left []string
	for _, k := range keys {
		if _, ok := found[k]; !ok {
			left = append(left, k)
		}
	}
	// The keys left are read alone, detailsAtOnce at a time; their cards
	// are made in key order once every read is in, the first to fail ends
	// the others and is the error.
	alone := make([]*jira.Issue, len(left))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(detailsAtOnce)
	for i, k := range left {
		g.Go(func() error {
			is, err := p.jira.GetIssue(gctx, k, s.cardFields()...)
			if errors.Is(err, trackerapi.ErrNotFound) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("jiraprovider: look up %s: %w", k, err)
			}
			alone[i] = is
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for i, is := range alone {
		if is == nil {
			continue
		}
		var err error
		if found[left[i]], err = s.cardOf(is, cat); err != nil {
			return err
		}
	}
	return nil
}

// Search is one page of the cards q's text matches, most recently
// updated first. The text reaches Jira's text search only through
// jira.QuoteJQLText, so it is searched for as words, never as operators
// or wildcards.
func (p *Provider) Search(ctx context.Context, q tracker.SearchQuery) (tracker.SearchPage, error) {
	if err := q.Check(); err != nil {
		return tracker.SearchPage{}, err
	}
	var clauses []string
	if text := strings.TrimSpace(q.Text); text != "" {
		clauses = append(clauses, "text ~ "+jira.QuoteJQLText(text))
	}
	if q.Project != "" {
		if err := checkProject(q.Project); err != nil {
			return tracker.SearchPage{}, err
		}
		clauses = append(clauses, "project = "+jira.QuoteJQL(string(q.Project)))
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.SearchPage{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = tracker.MaxSearchPage
	}
	page, err := p.jira.Search(ctx, jira.SearchQuery{
		JQL:    strings.Join(clauses, " AND ") + " ORDER BY updated DESC",
		Fields: s.cardFields(), MaxResults: limit, PageToken: string(q.Page),
	})
	if err != nil {
		return tracker.SearchPage{}, fmt.Errorf("jiraprovider: search: %w", err)
	}
	cat := newCatalog()
	out := tracker.SearchPage{Next: tracker.PageToken(page.NextPageToken)}
	for _, is := range page.Issues {
		c, err := s.cardOf(is, cat)
		if err != nil {
			return tracker.SearchPage{}, err
		}
		out.Cards = append(out.Cards, c)
	}
	out.Catalog = cat.out
	return out, nil
}

// Download streams one of an issue's attachments, after reading the
// issue's attachments to see that it is one of them.
func (p *Provider) Download(ctx context.Context, in tracker.DownloadInput) error {
	if err := in.Check(); err != nil {
		return err
	}
	if err := checkIssue(in.Issue); err != nil {
		return err
	}
	if _, err := numericID("attachment", string(in.Attachment)); err != nil {
		return err
	}
	atts, err := p.jira.Attachments(ctx, string(in.Issue))
	if err != nil {
		return fmt.Errorf("jiraprovider: attachments of %s: %w", in.Issue, err)
	}
	if !slices.ContainsFunc(atts, func(a jira.Attachment) bool { return a.ID == string(in.Attachment) }) {
		return invalidf("attachment %s is not issue %s's", in.Attachment, in.Issue)
	}
	if err := p.jira.DownloadAttachment(ctx, string(in.Attachment), in.To, jira.MaxAttachmentBytes); err != nil {
		return fmt.Errorf("jiraprovider: download attachment %s: %w", in.Attachment, err)
	}
	return nil
}
