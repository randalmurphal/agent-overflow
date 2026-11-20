package jira

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
)

// IssueDetail is one issue read whole: every field but its comments and
// worklogs, its edit screen, the transitions the user can make on it now
// with their screens' fields, and its whole changelog.
type IssueDetail struct {
	Issue       *Issue
	Edit        Screen
	Transitions []Transition
	// Changelog is every change, oldest first.
	Changelog Changelog
}

// The issue read IssueDetail makes. The comments are left out (a caller
// reads the newest page with LatestComments) and so are the worklogs,
// which Jira's own time tracking reads by author; editmeta, transitions
// with their fields and changelog are embedded in the same answer.
// Measured on Jira Cloud (docs/references/tracker-api-measurements.md):
// fields=*all,-comment leaves the comments out, the expansions embed all
// three, and the embedded changelog is the newest 100 entries with
// total, while the changelog read pages oldest first.
const (
	detailFields = "*all,-comment,-worklog"
	detailExpand = "editmeta,changelog,transitions,transitions.fields"
	// changelogPage is the most changelog entries a page holds, both
	// embedded and on the changelog read.
	changelogPage = 100
	// latestCommentPage is the most comments LatestComments asks for:
	// the largest page measured honored newest first.
	latestCommentPage = 100
)

// detailJSON is the issue read IssueDetail makes.
type detailJSON struct {
	ID       string      `json:"id"`
	Key      string      `json:"key"`
	Fields   issueFields `json:"fields"`
	EditMeta struct {
		Fields map[string]fieldMetaJSON `json:"fields"`
	} `json:"editmeta"`
	Transitions transitionList `json:"transitions"`
	Changelog   struct {
		StartAt   int               `json:"startAt"`
		Total     int               `json:"total"`
		Histories []changeEntryJSON `json:"histories"`
	} `json:"changelog"`
}

// IssueDetail reads ref whole in one request, and the older part of its
// changelog in more, oldest first, when it has more entries than Jira
// embeds. A changelog page that does not join the embedded entries (one
// that starts elsewhere, or a total that the entries contradict) fails
// the read rather than show a history with a gap.
func (c *Client) IssueDetail(ctx context.Context, ref string) (IssueDetail, error) {
	if err := c.admit(ctx); err != nil {
		return IssueDetail{}, err
	}
	path, err := issuePath(ref)
	if err != nil {
		return IssueDetail{}, err
	}
	var v detailJSON
	q := url.Values{"fields": {detailFields}, "expand": {detailExpand}}
	if err := c.get(ctx, path, q, &v); err != nil {
		return IssueDetail{}, fmt.Errorf("jira: detail of %s: %w", ref, err)
	}
	is := issueJSON{ID: v.ID, Key: v.Key, Fields: v.Fields}.decode()
	out := IssueDetail{Issue: is, Edit: screenOf(v.EditMeta.Fields), Transitions: v.Transitions.transitions()}
	log := v.Changelog
	if log.StartAt != 0 || len(log.Histories) > log.Total {
		return IssueDetail{}, fmt.Errorf("jira: detail of %s: %w", ref, &AnswerError{
			Message: fmt.Sprintf("the embedded changelog holds %d entries from %d of %d", len(log.Histories), log.StartAt, log.Total)})
	}
	// Embedded newest first; the entries before them are read oldest
	// first.
	older := log.Total - len(log.Histories)
	if older > 0 {
		found, _, _, err := offsetPages[changeEntryJSON](ctx, c, apiPath+path+"/changelog", url.Values{}, changelogPage, older)
		if err == nil && len(found) != older {
			err = &AnswerError{Message: "the changelog holds " + strconv.Itoa(len(found)) + " entries before the newest, not " + strconv.Itoa(older)}
		}
		if err != nil {
			return IssueDetail{}, fmt.Errorf("jira: detail of %s: the older changelog: %w", ref, err)
		}
		for _, e := range found {
			out.Changelog.Entries = append(out.Changelog.Entries, e.entry())
		}
	}
	for _, e := range slices.Backward(log.Histories) {
		out.Changelog.Entries = append(out.Changelog.Entries, e.entry())
	}
	out.Changelog.Complete = true
	return out, nil
}

// LatestComments returns an issue's newest n comments (zero means 100,
// the most: measured honored), oldest first, read newest first in one
// request; StartAt is where they begin in the oldest-first list, so
// Comments reads on from there.
func (c *Client) LatestComments(ctx context.Context, issue string, n int) (CommentPage, error) {
	if err := c.admit(ctx); err != nil {
		return CommentPage{}, err
	}
	path, err := issuePath(issue)
	if err != nil {
		return CommentPage{}, err
	}
	if n < 0 {
		return CommentPage{}, notSent("comments of %s: a negative page", issue)
	}
	n = pageSize(n, latestCommentPage, latestCommentPage)
	q := url.Values{"startAt": {"0"}, "maxResults": {strconv.Itoa(n)}, "orderBy": {"-created"}}
	page, err := c.commentPage(ctx, issue, path, q)
	if err != nil {
		return page, err
	}
	if page.StartAt != 0 || len(page.Comments) > page.Total || len(page.Comments) < min(n, page.Total) {
		return CommentPage{}, fmt.Errorf("jira: comments of %s: %w", issue, &AnswerError{
			Message: fmt.Sprintf("asked for the newest %d, Jira sent %d from %d of %d", n, len(page.Comments), page.StartAt, page.Total)})
	}
	slices.Reverse(page.Comments)
	page.StartAt = page.Total - len(page.Comments)
	return page, nil
}
