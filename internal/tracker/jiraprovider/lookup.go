package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// keyShape is the shape of a Jira issue key. A text of another shape
// names no issue and is not sent; a number in particular would be read
// as an issue ID.
var keyShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)

// searchChunk is the most keys one lookup search names: search returns
// at most 100 issues a page with fields (measured).
const searchChunk = 100

// Lookup finds the issues keys name, searchChunk keys to a search. A
// search finds an issue by any key it has had (measured: key in (<old
// key>) returns the issue under its new key), so a key it does not
// return as an issue's current key names an issue only when the search
// also returned an issue no current key matched, a moved one. Then each
// such key is read alone (the read of an old key answers with the
// issue; Jira's 404 makes the key unmatched). Otherwise one more search
// of just those keys settles them: none when it finds nothing, the
// common case of mistyped or deleted keys, and else each is read alone
// (two keys naming one issue, one by an old key). Measured on Jira
// Cloud's search/jql, a key that names nothing is left out of a 200
// answer with no warning, and a search with no credential answers 200
// with no issues; the jira client takes an empty search as the user's
// only when it names the account it was made for (X-AAccountId, on every
// answer to a valid token) or a sign-in check shows it, so a lost
// credential does not read as missing keys. A 400 naming one of the keys is handled too,
// defensively (the documentation does not rule it out): the refused
// search's keys are then read one by one.
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
	var shaped []string
	for _, k := range keys {
		if keyShape.MatchString(k) {
			shaped = append(shaped, k)
		}
	}
	for chunk := range slices.Chunk(shaped, searchChunk) {
		if err := p.lookupChunk(ctx, s, chunk, found, cat); err != nil {
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

// lookupChunk finds the issues of keys, recording them in found.
func (p *Provider) lookupChunk(ctx context.Context, s site, keys []string, found map[string]tracker.Card, cat *catalog) error {
	moved, err := p.searchKeys(ctx, s, keys, found, cat)
	if err != nil {
		return err
	}
	var rest []string
	for _, k := range keys {
		if _, ok := found[k]; !ok {
			rest = append(rest, k)
		}
	}
	if len(rest) == 0 {
		return nil
	}
	if !moved {
		// Only an old key of an issue another key named currently is left
		// to find; a search of the rest says whether there is one.
		if moved, err = p.searchKeys(ctx, s, rest, found, cat); err != nil || !moved {
			return err
		}
	}
	for _, k := range rest {
		if _, ok := found[k]; ok {
			continue
		}
		is, err := p.jira.GetIssue(ctx, k, s.cardFields()...)
		if errors.Is(err, trackerapi.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("jiraprovider: look up %s: %w", k, err)
		}
		if found[k], err = s.cardOf(is, cat); err != nil {
			return err
		}
	}
	return nil
}

// searchKeys searches for keys and records each issue under the key
// asked that is its current key, case ignored; moved reports an issue
// returned that no key asked names currently, or a search Jira refused
// with a 400 naming one of the keys, so that the keys not recorded are
// read alone.
func (p *Provider) searchKeys(ctx context.Context, s site, keys []string, found map[string]tracker.Card, cat *catalog) (moved bool, err error) {
	res, err := p.jira.SearchAll(ctx, jira.SearchQuery{
		JQL: "key in " + jira.QuoteJQLList(keys...), Fields: s.cardFields(), MaxResults: searchChunk,
	}, 0)
	if err != nil {
		if namesKey(err, keys) {
			return true, nil
		}
		return false, fmt.Errorf("jiraprovider: look up %d keys: %w", len(keys), err)
	}
	for _, is := range res.Issues {
		i := slices.IndexFunc(keys, func(k string) bool { return strings.EqualFold(k, is.Key) })
		if i < 0 {
			moved = true // under its current key; an old key of it is read alone
			continue
		}
		c, err := s.cardOf(is, cat)
		if err != nil {
			return false, err
		}
		found[keys[i]] = c
	}
	return moved, nil
}

// namesKey reports a 400 whose text names one of keys.
func namesKey(err error, keys []string) bool {
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	if !ok || se.Status != http.StatusBadRequest {
		return false
	}
	text := strings.ToUpper(se.Message + " " + se.Body)
	for _, m := range se.Fields {
		text += " " + strings.ToUpper(m)
	}
	return slices.ContainsFunc(keys, func(k string) bool { return strings.Contains(text, strings.ToUpper(k)) })
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
	if err := p.jira.DownloadAttachment(ctx, string(in.Attachment), in.To, in.Limit); err != nil {
		return fmt.Errorf("jiraprovider: download attachment %s: %w", in.Attachment, err)
	}
	return nil
}
