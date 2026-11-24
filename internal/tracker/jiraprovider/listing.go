package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// The page sizes of the board issue list (docs/references/tracker-api-
// measurements.md). The list is not clamped the way search is: it honors
// pages above 100, and one page of maxResults=5000 held every issue of a
// 2,687-issue board with fields=id. An id clause of 500 IDs returned all
// 500 on the board list and on search.
const (
	// idListPage is the most issue IDs one page of BoardIssues asks for.
	idListPage = 5000
	// idChunk is the most issue IDs one JQL id clause names, and so the
	// most rows one BoardRows page asks for.
	idChunk = 500
)

// BoardIssues lists the IDs on board within the window: the issues not
// done, those updated at or after the window (a superset of those done
// in it), and on a scrum board those in a sprint Structure keeps, read
// from the sprint listing the last Structure held (see sprints). The
// window is given to JQL in milliseconds since the epoch, which it
// compares with no time zone (see the delta's comment). Complete is false
// when Jira attached any warning to the listing, since a warning may mean
// issues were left out. A listing Jira refuses (400) while naming held
// sprints is listed once more with the board's sprints read again:
// measured, a sprint ID that names nothing answers 400 on the board issue
// list. The rule holds only there; search/jql answers the same JQL 200
// with no issues, so a caller of search could not tell a deleted sprint.
func (p *Provider) BoardIssues(ctx context.Context, id tracker.BoardID, window time.Time) (tracker.Listing, error) {
	b, err := p.board(ctx, id)
	if err != nil {
		return tracker.Listing{}, err
	}
	res, rule, err := p.listBoard(ctx, b, window)
	if queryRefused(err) && b.cfg.Type == jira.BoardScrum {
		p.jira.ForgetBoard(b.cfg.BoardID)
		if b, err = p.board(ctx, id); err == nil {
			res, rule, err = p.listBoard(ctx, b, window)
		}
	}
	if err != nil {
		return tracker.Listing{}, fmt.Errorf("jiraprovider: issues of board %s: %w", id, err)
	}
	out := tracker.Listing{IDs: make([]tracker.IssueID, len(res.Issues)), Complete: res.Complete, Rule: rule}
	for i, is := range res.Issues {
		out.IDs[i] = tracker.IssueID(is.ID)
	}
	return out, nil
}

// listBoard lists the IDs of BoardIssues, and the rule it listed by.
func (p *Provider) listBoard(ctx context.Context, b board, window time.Time) (jira.SearchResult, tracker.Eligibility, error) {
	rule := tracker.Eligibility{Window: window}
	if b.cfg.Type == jira.BoardScrum {
		sprints, err := p.sprints(ctx, b.id, b.cfg.BoardID, window, false)
		if err != nil {
			return jira.SearchResult{}, rule, err
		}
		for _, s := range sprints {
			rule.Sprints = append(rule.Sprints, s.ID)
		}
	}
	res, err := p.jira.ListAllIssues(ctx, jira.ListQuery{List: jira.BoardIssues(b.cfg.BoardID), JQL: ruleJQL(rule), Fields: []string{"id"}, MaxResults: idListPage}, 0)
	return res, rule, err
}

// ruleJQL is the JQL of rule, empty for the zero rule. Its sprint IDs are
// decimal, as sprints writes them.
func ruleJQL(rule tracker.Eligibility) string {
	if rule.Window.IsZero() && len(rule.Sprints) == 0 {
		return ""
	}
	jql := "statusCategory != Done OR updated >= " + strconv.FormatInt(rule.Window.UnixMilli(), 10)
	if len(rule.Sprints) > 0 {
		ids := make([]string, len(rule.Sprints))
		for i, s := range rule.Sprints {
			ids[i] = string(s)
		}
		jql += " OR sprint in (" + strings.Join(ids, ", ") + ")"
	}
	return jql
}

// andRule is jql narrowed to what rule lists.
func andRule(jql string, rule tracker.Eligibility) string {
	if r := ruleJQL(rule); r != "" {
		return "(" + jql + ") AND (" + r + ")"
	}
	return jql
}

// BoardRows reads issues through board, idChunk to a request, each with
// the board's rank and estimate fields. Jira may answer an id clause
// naming an issue that no longer exists with a warning or by refusing the
// query; either way the issues that exist are found with a bulk read and
// listed again. The list comes from Jira's search index, which shows a
// new issue late, so an issue it leaves out is Missing only once
// jql/match does not place it on the board (membership.go); one it does
// is read by the bulk fetch. The confirmation costs one match request
// per 50 issues left out and one bulk fetch when any is placed; none
// when the list returns every issue. When the match cannot judge the
// board under the configuration held (a board given another filter
// since), the configuration is read again and the rows once more; a board
// still unjudged fails the read, so that no issue reads as Missing on
// the list's word alone. An issue the bulk fetch shows does not exist is
// Missing and is not sent to the match; a bulk answer that is malformed
// fails the read (issuesByID). A 400 on a request that carries rule (the
// list or the match) while rule names sprints fails the read with
// tracker.ErrRuleRefused (refusedRule), whatever its cause.
func (p *Provider) BoardRows(ctx context.Context, id tracker.BoardID, issues []tracker.IssueID, rule tracker.Eligibility) (tracker.Rows, error) {
	if err := checkIssues(issues); err != nil {
		return tracker.Rows{}, err
	}
	out, err := p.boardRows(ctx, id, issues, rule)
	if n, unjudged := isUnjudged(err); unjudged {
		p.jira.ForgetBoard(n)
		out, err = p.boardRows(ctx, id, issues, rule)
	}
	if err != nil {
		return tracker.Rows{}, err
	}
	return out, nil
}

// refusedRule is err, the failure of a call whose query carried rule, as
// tracker.ErrRuleRefused when Jira refused that query (queryRefused) and
// rule names sprints: measured, a sprint ID that names nothing answers
// 400 on the board issue list, so the listing that captured rule may be
// what no longer holds. The refusal's cause is not judged. A failure of
// any other request the call made, such as the sign-in check that proves
// an answer, is not a refusal of the rule.
func refusedRule(err error, rule tracker.Eligibility) error {
	if len(rule.Sprints) == 0 || !queryRefused(err) {
		return err
	}
	return fmt.Errorf("%w: %w", tracker.ErrRuleRefused, err)
}

// queryRefused reports err as Jira's 400 to the request that carried the
// call's JQL (jira.QueryRefusedError).
func queryRefused(err error) bool {
	_, ok := errors.AsType[*jira.QueryRefusedError](err)
	return ok
}

// boardRows is BoardRows under the board configuration the client holds.
func (p *Provider) boardRows(ctx context.Context, id tracker.BoardID, issues []tracker.IssueID, rule tracker.Eligibility) (tracker.Rows, error) {
	b, err := p.board(ctx, id)
	if err != nil {
		return tracker.Rows{}, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.Rows{}, err
	}
	cat := newCatalog()
	var out tracker.Rows
	// found are the issues read; absent, those the bulk fetch showed do not
	// exist, which no match is asked about.
	found, absent := map[tracker.IssueID]bool{}, map[tracker.IssueID]bool{}
	for chunk := range slices.Chunk(distinct(issues), idChunk) {
		list, gone, err := p.listByID(ctx, b, chunk, b.rowFields(s), rule)
		if err != nil {
			return tracker.Rows{}, fmt.Errorf("jiraprovider: rows of board %s: %w", id, err)
		}
		for _, g := range gone {
			absent[g] = true
		}
		for _, is := range list {
			row, err := b.rowOf(s, is, cat)
			if err != nil {
				return tracker.Rows{}, err
			}
			if !found[row.Card.ID] {
				found[row.Card.ID] = true
				out.Rows = append(out.Rows, row)
			}
		}
	}
	if gone := slices.DeleteFunc(missing(issues, found), func(id tracker.IssueID) bool { return absent[id] }); len(gone) > 0 {
		held, err := p.confirmMissing(ctx, b, s, gone, rule, cat)
		if err != nil {
			return tracker.Rows{}, fmt.Errorf("jiraprovider: rows of board %s: %w", id, err)
		}
		for _, row := range held {
			found[row.Card.ID] = true
			out.Rows = append(out.Rows, row)
		}
	}
	out.Missing = missing(issues, found)
	out.Catalog = cat.out
	return out, nil
}

// listByID lists the issues of ids that b lists under rule, with fields,
// in full or not at all, and absent, the issues of ids that a bulk fetch
// made after a refused or warned listing shows do not exist.
func (p *Provider) listByID(ctx context.Context, b board, ids []tracker.IssueID, fields []string, rule tracker.Eligibility) (list []*jira.Issue, absent []tracker.IssueID, err error) {
	list, err = p.listIDs(ctx, b, ids, fields, rule)
	if err == nil {
		return list, nil, nil
	}
	if !errors.Is(err, errIncomplete) && !queryRefused(err) {
		return nil, nil, err
	}
	// The bulk read's failure is the cause, the listing's its context: it
	// carries no rule.
	bulk, berr := p.issuesByID(ctx, ids, []string{"status"})
	if berr != nil {
		return nil, nil, fmt.Errorf("find which of %d issues exist after %v: %w", len(ids), err, berr)
	}
	exist := make([]tracker.IssueID, len(bulk))
	for i, is := range bulk {
		exist[i] = tracker.IssueID(is.ID)
	}
	absent = slices.DeleteFunc(slices.Clone(ids), func(id tracker.IssueID) bool { return slices.Contains(exist, id) })
	switch len(exist) {
	case 0:
		return nil, absent, nil
	case len(ids):
		// Every issue exists, so the failure is not about a missing one.
		return nil, nil, err
	}
	if list, err = p.listIDs(ctx, b, exist, fields, rule); err != nil {
		return nil, nil, err
	}
	return list, absent, nil
}

// errIncomplete is a listing Jira attached a warning to.
var errIncomplete = errors.New("jiraprovider: Jira warned that the listing may be incomplete")

// listIDs lists the issues of ids that b lists under rule, with fields,
// failing with errIncomplete when Jira warns.
func (p *Provider) listIDs(ctx context.Context, b board, ids []tracker.IssueID, fields []string, rule tracker.Eligibility) ([]*jira.Issue, error) {
	refs := make([]string, len(ids))
	for i, id := range ids {
		refs[i] = string(id) // checked decimal
	}
	res, err := p.jira.ListAllIssues(ctx, jira.ListQuery{
		List: jira.BoardIssues(b.cfg.BoardID), JQL: andRule("id in ("+strings.Join(refs, ", ")+")", rule),
		Fields: fields, MaxResults: idChunk,
	}, 0)
	if err != nil {
		return nil, refusedRule(err, rule)
	}
	if !res.Complete {
		return nil, errIncomplete
	}
	return res.Issues, nil
}

// Cards reads issues with no board in one bulk fetch (issuesByID, up to
// 500 issues a request with the card fields). An issue Jira does not
// return is Missing; one it says it could not read, and a malformed
// answer, fail the call.
func (p *Provider) Cards(ctx context.Context, issues []tracker.IssueID) (tracker.Cards, error) {
	if err := checkIssues(issues); err != nil {
		return tracker.Cards{}, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.Cards{}, err
	}
	read, err := p.issuesByID(ctx, distinct(issues), s.cardFields())
	if err != nil {
		return tracker.Cards{}, fmt.Errorf("jiraprovider: cards: %w", err)
	}
	cat := newCatalog()
	var out tracker.Cards
	found := map[tracker.IssueID]bool{}
	for _, is := range read {
		c, err := s.cardOf(is, cat)
		if err != nil {
			return tracker.Cards{}, err
		}
		found[c.ID] = true
		out.Cards = append(out.Cards, c)
	}
	out.Missing = missing(issues, found)
	out.Catalog = cat.out
	return out, nil
}

// distinct is ids without repeats, in order.
func distinct(ids []tracker.IssueID) []tracker.IssueID {
	seen := make(map[tracker.IssueID]bool, len(ids))
	out := make([]tracker.IssueID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// missing are the ids found lacks, each once, in order.
func missing(ids []tracker.IssueID, found map[tracker.IssueID]bool) []tracker.IssueID {
	var out []tracker.IssueID
	for _, id := range distinct(ids) {
		if !found[id] {
			out = append(out, id)
		}
	}
	return out
}
