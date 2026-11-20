package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
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
	res, err := p.listBoard(ctx, b, window)
	if isStatus(err, http.StatusBadRequest) && b.cfg.Type == jira.BoardScrum {
		p.jira.ForgetBoard(b.cfg.BoardID)
		if b, err = p.board(ctx, id); err == nil {
			res, err = p.listBoard(ctx, b, window)
		}
	}
	if err != nil {
		return tracker.Listing{}, fmt.Errorf("jiraprovider: issues of board %s: %w", id, err)
	}
	out := tracker.Listing{IDs: make([]tracker.IssueID, len(res.Issues)), Complete: res.Complete}
	for i, is := range res.Issues {
		out.IDs[i] = tracker.IssueID(is.ID)
	}
	return out, nil
}

// listBoard lists the IDs of BoardIssues.
func (p *Provider) listBoard(ctx context.Context, b board, window time.Time) (jira.SearchResult, error) {
	jql := "statusCategory != Done OR updated >= " + strconv.FormatInt(window.UnixMilli(), 10)
	if b.cfg.Type == jira.BoardScrum {
		sprints, err := p.sprints(ctx, b.id, b.cfg.BoardID, window, false)
		if err != nil {
			return jira.SearchResult{}, err
		}
		if len(sprints) > 0 {
			ids := make([]string, len(sprints))
			for i, s := range sprints {
				ids[i] = string(s.ID) // decimal, written by sprints
			}
			jql += " OR sprint in (" + strings.Join(ids, ", ") + ")"
		}
	}
	return p.jira.ListAllIssues(ctx, jira.ListQuery{List: jira.BoardIssues(b.cfg.BoardID), JQL: jql, Fields: []string{"id"}, MaxResults: idListPage}, 0)
}

// BoardRows reads issues through board, idChunk to a request, each with
// the board's rank and estimate fields. An issue the board does not list
// is Missing. Jira may answer an id clause naming an issue that no longer
// exists with a warning or by refusing the query; either way the issues
// that exist are found with a bulk read and listed again, and the others
// are Missing.
func (p *Provider) BoardRows(ctx context.Context, id tracker.BoardID, issues []tracker.IssueID) (tracker.Rows, error) {
	if err := checkIssues(issues); err != nil {
		return tracker.Rows{}, err
	}
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
	found := map[tracker.IssueID]bool{}
	for chunk := range slices.Chunk(distinct(issues), idChunk) {
		list, err := p.listByID(ctx, b, chunk, b.rowFields(s))
		if err != nil {
			return tracker.Rows{}, fmt.Errorf("jiraprovider: rows of board %s: %w", id, err)
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
	out.Missing = missing(issues, found)
	out.Catalog = cat.out
	return out, nil
}

// listByID lists the issues of ids that b lists, with fields, in full or
// not at all.
func (p *Provider) listByID(ctx context.Context, b board, ids []tracker.IssueID, fields []string) ([]*jira.Issue, error) {
	list, err := p.listIDs(ctx, b, ids, fields)
	if err == nil {
		return list, nil
	}
	if !errors.Is(err, errIncomplete) && !isStatus(err, http.StatusBadRequest) {
		return nil, err
	}
	refs := make([]string, len(ids))
	for i, id := range ids {
		refs[i] = string(id)
	}
	bulk, berr := p.jira.BulkFetch(ctx, refs, "status")
	if berr != nil {
		return nil, fmt.Errorf("find which of %d issues exist after %w: %w", len(ids), err, berr)
	}
	if len(bulk.Failed) > 0 {
		return nil, fmt.Errorf("find which of %d issues exist after %w: Jira could not read %s: %s", len(ids), err, bulk.Failed[0].ID, bulk.Failed[0].Message)
	}
	var exist []tracker.IssueID
	for _, is := range bulk.Issues {
		if slices.Contains(ids, tracker.IssueID(is.ID)) {
			exist = append(exist, tracker.IssueID(is.ID))
		}
	}
	switch len(exist) {
	case 0:
		return nil, nil
	case len(ids):
		// Every issue exists, so the failure is not about a missing one.
		return nil, err
	}
	return p.listIDs(ctx, b, exist, fields)
}

// errIncomplete is a listing Jira attached a warning to.
var errIncomplete = errors.New("jiraprovider: Jira warned that the listing may be incomplete")

// listIDs lists the issues of ids that b lists, with fields, failing with
// errIncomplete when Jira warns.
func (p *Provider) listIDs(ctx context.Context, b board, ids []tracker.IssueID, fields []string) ([]*jira.Issue, error) {
	refs := make([]string, len(ids))
	for i, id := range ids {
		refs[i] = string(id) // checked decimal
	}
	res, err := p.jira.ListAllIssues(ctx, jira.ListQuery{
		List: jira.BoardIssues(b.cfg.BoardID), JQL: "id in (" + strings.Join(refs, ", ") + ")",
		Fields: fields, MaxResults: idChunk,
	}, 0)
	if err != nil {
		return nil, err
	}
	if !res.Complete {
		return nil, errIncomplete
	}
	return res.Issues, nil
}

// Cards reads issues with no board, a hundred to a request. An issue Jira
// does not return is Missing; one it says it could not read fails the
// call, since it may exist.
func (p *Provider) Cards(ctx context.Context, issues []tracker.IssueID) (tracker.Cards, error) {
	if err := checkIssues(issues); err != nil {
		return tracker.Cards{}, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.Cards{}, err
	}
	refs := make([]string, 0, len(issues))
	for _, id := range distinct(issues) {
		refs = append(refs, string(id))
	}
	res, err := p.jira.BulkFetch(ctx, refs, s.cardFields()...)
	if err != nil {
		return tracker.Cards{}, fmt.Errorf("jiraprovider: cards: %w", err)
	}
	if len(res.Failed) > 0 {
		return tracker.Cards{}, fmt.Errorf("jiraprovider: cards: Jira could not read issue %s: %s", res.Failed[0].ID, res.Failed[0].Message)
	}
	cat := newCatalog()
	var out tracker.Cards
	found := map[tracker.IssueID]bool{}
	for _, is := range res.Issues {
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

// isStatus reports an error that is the tracker's answer with status.
func isStatus(err error, status int) bool {
	se, ok := errors.AsType[*trackerapi.StatusError](err)
	return ok && se.Status == status
}
