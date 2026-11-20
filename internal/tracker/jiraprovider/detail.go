package jiraprovider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"

	"golang.org/x/sync/errgroup"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/trackerapi"
)

// commentPage is the most comments a page holds.
const commentPage = 50

// detailSkipped are the fields Detail shows elsewhere than in Fields,
// besides the card's (cardFieldIDs): the description, and the comments,
// attachments, links and worklogs it lists itself.
var detailSkipped = []string{"description", "comment", "attachment", "issuelinks", "worklog"}

// Detail reads one issue whole: every field with the edit screen's
// metadata, its links and attachments, the whole changelog and the moves
// its workflow allows now in one read (jira.IssueDetail, which reads the
// older changelog when Jira embeds only its newest entries), then the
// newest page of comments in another, while the field catalog is read;
// the first to fail ends the others and is the error. The comments are
// read only once the issue's read, which gives the card's update time,
// has returned, so nothing the detail holds is older than that time
// (tracker.Capabilities.DetailMovesUpdated).
func (p *Provider) Detail(ctx context.Context, issue tracker.IssueID) (tracker.Detail, error) {
	if err := checkIssue(issue); err != nil {
		return tracker.Detail{}, err
	}
	var (
		s    site
		read issueRead
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		s, err = p.site(gctx)
		return err
	})
	g.Go(func() (err error) {
		read, err = p.readIssue(gctx, issue)
		return err
	})
	if err := g.Wait(); err != nil {
		return tracker.Detail{}, err
	}
	return p.detailOf(s, issue, read)
}

// issueRead is what the requests on one issue's own paths returned.
type issueRead struct {
	full     jira.IssueDetail
	comments tracker.CommentPage
}

// readIssue makes the requests on issue's own paths: the issue, then its
// comments. Each names the issue, so a not found from it is the issue's:
// gone, or not visible to the user.
func (p *Provider) readIssue(ctx context.Context, issue tracker.IssueID) (read issueRead, err error) {
	if read.full, err = p.jira.IssueDetail(ctx, string(issue)); err != nil {
		return issueRead{}, fmt.Errorf("jiraprovider: detail of %s: %w", issue, err)
	}
	if read.comments, err = p.commentPage(ctx, issue, ""); err != nil {
		return issueRead{}, err
	}
	return read, nil
}

// detailOf is issue's detail from its read and the site's field catalog.
func (p *Provider) detailOf(s site, issue tracker.IssueID, read issueRead) (tracker.Detail, error) {
	full, comments := read.full, read.comments
	is := full.Issue
	cat := newCatalog()
	d := tracker.Detail{URL: p.origin + "/browse/" + url.PathEscape(is.Key)}
	var err error
	if d.Card, err = s.cardOf(is, cat); err != nil {
		return tracker.Detail{}, err
	}
	for _, pr := range is.Problems {
		if pr.Field == "description" {
			return tracker.Detail{}, fmt.Errorf("jiraprovider: detail of %s: description: %w", issue, pr.Err)
		}
	}
	d.Description = adf.ToMarkdown(is.Description)
	cat.user(is.Reporter)
	d.Fields = detailFields(s, is, full.Edit, cat)
	for _, l := range is.Links {
		d.Links = append(d.Links, linkOf(l, cat))
	}
	for _, a := range is.Attachments {
		d.Attachments = append(d.Attachments, attachmentOf(a, cat))
	}
	d.Comments = comments
	cat.merge(d.Comments.Catalog)
	d.History = events(full.Changelog, cat)
	d.Moves = movesOf(full.Transitions, cat)
	d.Catalog = cat.out
	return d, nil
}

// detailsAtOnce is how many issues Details reads at a time.
const detailsAtOnce = 4

// Details reads each issue as Detail does, detailsAtOnce at a time, with
// one read of the field catalog for all: Jira reads one issue's detail a
// request, so N issues cost N times an issue's requests. An issue whose
// own requests Jira answers 404 is Missing; any other failure, a 404 of
// the field catalog included, ends the reads in flight and is the error.
func (p *Provider) Details(ctx context.Context, issues []tracker.IssueID) (tracker.Details, error) {
	for i, issue := range issues {
		if err := checkIssue(issue); err != nil {
			return tracker.Details{}, err
		}
		if slices.Contains(issues[:i], issue) {
			return tracker.Details{}, invalidf("issue %s is named twice", issue)
		}
	}
	var s site
	reads := make([]issueRead, len(issues))
	missing := make([]bool, len(issues))
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		s, err = p.site(gctx)
		return err
	})
	g.Go(func() error {
		each, ectx := errgroup.WithContext(gctx)
		each.SetLimit(detailsAtOnce)
		for i, issue := range issues {
			each.Go(func() (err error) {
				reads[i], err = p.readIssue(ectx, issue)
				if errors.Is(err, trackerapi.ErrNotFound) {
					missing[i], err = true, nil
				}
				return err
			})
		}
		return each.Wait()
	})
	if err := g.Wait(); err != nil {
		return tracker.Details{}, err
	}
	var out tracker.Details
	for i, issue := range issues {
		if missing[i] {
			out.Missing = append(out.Missing, issue)
			continue
		}
		d, err := p.detailOf(s, issue, reads[i])
		if err != nil {
			return tracker.Details{}, err
		}
		out.Details = append(out.Details, d)
	}
	return out, nil
}

// detailFields are is's fields other than those detailSkipped names and
// the sprint and rank fields: every field with a value, and every field
// the edit screen offers. A value that does not have its kind's shape is
// shown read-only.
func detailFields(s site, is *jira.Issue, screen jira.Screen, cat *catalog) []tracker.Field {
	ids := map[string]bool{}
	for id, raw := range is.Raw {
		if !jira.FieldValue(raw).Null() {
			ids[id] = true
		}
	}
	for _, id := range []string{"reporter", "components", "fixVersions", "resolution", "duedate", "subtasks"} {
		if v, ok := valueOf(is, id, tracker.KindReadOnly, false, cat); ok && !v.Empty() {
			ids[id] = true
		}
	}
	for _, f := range screen.Fields {
		ids[f.ID] = true
	}
	var out []tracker.Field
	for id := range ids {
		if slices.Contains(cardFieldIDs, id) || slices.Contains(detailSkipped, id) || id == s.sprint {
			continue
		}
		meta, onScreen := screenField(screen, id)
		cf, inCatalog := s.field(id)
		schema, name := cf.Schema, cf.Name
		if !inCatalog {
			schema, name = meta.Schema, cmp.Or(meta.Name, id)
		}
		if schema.Custom == rankType {
			continue
		}
		kind, list := kindOf(schema)
		f := tracker.Field{ID: neutralField(id), Name: name, Kind: kind, List: list}
		if onScreen {
			f.Required, f.Editable = meta.Required, settable(meta, kind)
		}
		v, ok := valueOf(is, id, kind, list, cat)
		if !ok {
			f.Kind, f.List, f.Editable = tracker.KindReadOnly, false, false
			v = tracker.ReadOnlyValue(display(is.Raw[id]))
		}
		f.Value = v
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b tracker.Field) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out
}

// screenField is the field with exactly id on screen.
func screenField(screen jira.Screen, id string) (jira.FieldMeta, bool) {
	i := slices.IndexFunc(screen.Fields, func(f jira.FieldMeta) bool { return f.ID == id })
	if i < 0 {
		return jira.FieldMeta{}, false
	}
	return screen.Fields[i], true
}

func linkOf(l jira.IssueLink, cat *catalog) tracker.Link {
	out := tracker.Link{
		ID: tracker.LinkID(l.ID), Type: linkTypeOf(l.Type), Outward: l.Direction == jira.Outward,
		Issue: tracker.IssueRef{ID: tracker.IssueID(l.Issue.ID), Key: l.Issue.Key, Title: l.Issue.Summary},
	}
	if l.Issue.Status != nil {
		out.Issue.Status = tracker.StatusID(l.Issue.Status.ID)
		cat.status(l.Issue.Status)
	}
	return out
}

func attachmentOf(a jira.Attachment, cat *catalog) tracker.Attachment {
	out := tracker.Attachment{ID: tracker.AttachmentID(a.ID), Name: a.Filename, MimeType: a.MimeType, Size: a.Size, Created: a.Created}
	if a.Author != nil {
		out.Author = tracker.UserID(a.Author.AccountID)
		cat.user(a.Author)
	}
	return out
}

// events is a changelog as history events, oldest first.
func events(log jira.Changelog, cat *catalog) []tracker.Event {
	var out []tracker.Event
	for _, e := range log.Entries {
		var author tracker.UserID
		if e.Author != nil {
			author = tracker.UserID(e.Author.AccountID)
			cat.user(e.Author)
		}
		for _, it := range e.Items {
			out = append(out, tracker.Event{At: e.Created, Author: author, Field: it.Field, From: it.FromText, To: it.ToText})
		}
	}
	return out
}

// Comments is a page of issue's comments, newest first.
func (p *Provider) Comments(ctx context.Context, issue tracker.IssueID, page tracker.PageToken) (tracker.CommentPage, error) {
	if err := checkIssue(issue); err != nil {
		return tracker.CommentPage{}, err
	}
	return p.commentPage(ctx, issue, page)
}

// commentPage reads a page of comments newest first. Jira pages them
// oldest first by offset, so a page token is the offset at which the
// page ends; the first page is the newest comments, read newest first in
// one request (jira.LatestComments). A comment deleted between pages can
// make one repeat; none is skipped.
func (p *Provider) commentPage(ctx context.Context, issue tracker.IssueID, page tracker.PageToken) (tracker.CommentPage, error) {
	var got jira.CommentPage
	var err error
	if page == "" {
		if got, err = p.jira.LatestComments(ctx, string(issue), commentPage); err != nil {
			err = fmt.Errorf("jiraprovider: comments of %s: %w", issue, err)
		}
	} else {
		end, perr := strconv.Atoi(string(page))
		if perr != nil || end <= 0 || strconv.Itoa(end) != string(page) {
			return tracker.CommentPage{}, invalidf("%q is not a comment page of issue %s", page, issue)
		}
		start := max(0, end-commentPage)
		got, err = p.comments(ctx, issue, start, end-start)
	}
	if err != nil {
		return tracker.CommentPage{}, err
	}
	var out tracker.CommentPage
	cat := newCatalog()
	for i := len(got.Comments) - 1; i >= 0; i-- {
		out.Comments = append(out.Comments, commentOf(got.Comments[i], cat))
	}
	if got.StartAt > 0 {
		out.Next = tracker.PageToken(strconv.Itoa(got.StartAt))
	}
	out.Catalog = cat.out
	return out, nil
}

// comments reads n comments from start. A page that starts elsewhere, or
// holds fewer than asked while more follow it, fails the read: paging
// from it would skip the comments between it and the next page.
func (p *Provider) comments(ctx context.Context, issue tracker.IssueID, start, n int) (jira.CommentPage, error) {
	got, err := p.jira.Comments(ctx, string(issue), start, n)
	switch {
	case err != nil:
		return jira.CommentPage{}, fmt.Errorf("jiraprovider: comments of %s: %w", issue, err)
	case got.StartAt != start || len(got.Comments) < n && start+len(got.Comments) < got.Total:
		return jira.CommentPage{}, fmt.Errorf("jiraprovider: comments of %s: asked for %d from %d, Jira sent %d from %d of %d",
			issue, n, start, len(got.Comments), got.StartAt, got.Total)
	}
	return got, nil
}

func commentOf(c jira.Comment, cat *catalog) tracker.Comment {
	out := tracker.Comment{ID: tracker.CommentID(c.ID), Body: adf.ToMarkdown(c.Body), Created: c.Created, Updated: c.Updated, Internal: c.Internal}
	if c.Author != nil {
		out.Author = tracker.UserID(c.Author.AccountID)
		cat.user(c.Author)
	}
	return out
}

// Moves are the transitions the user can make on issue now, with the
// fields their screens take.
func (p *Provider) Moves(ctx context.Context, issue tracker.IssueID) ([]tracker.Move, error) {
	if err := checkIssue(issue); err != nil {
		return nil, err
	}
	return p.moves(ctx, issue, newCatalog())
}

func (p *Provider) moves(ctx context.Context, issue tracker.IssueID, cat *catalog) ([]tracker.Move, error) {
	ts, err := p.jira.Transitions(ctx, string(issue))
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: moves of %s: %w", issue, err)
	}
	return movesOf(ts, cat), nil
}

// movesOf are the transitions of ts the user can make now.
func movesOf(ts []jira.Transition, cat *catalog) []tracker.Move {
	var out []tracker.Move
	for _, t := range ts {
		if !t.Available {
			continue
		}
		m := tracker.Move{ID: tracker.MoveID(t.ID), Name: t.Name, To: tracker.StatusID(t.To.ID)}
		cat.status(&t.To)
		for _, f := range t.Fields.Fields {
			m.Fields = append(m.Fields, metaField(f))
		}
		out = append(out, m)
	}
	return out
}
