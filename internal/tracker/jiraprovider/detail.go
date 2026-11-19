package jiraprovider

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
)

// commentPage is the most comments a page holds.
const commentPage = 50

// detailSkipped are the fields Detail shows elsewhere than in Fields:
// the card's, the description, and the comments, attachments, links and
// worklogs it lists itself.
var detailSkipped = append(slices.Clone(cardFieldIDs), "description", "comment", "attachment", "issuelinks", "worklog")

// Detail reads one issue whole: every field with the edit screen's
// metadata, the first page of comments, links, attachments, the whole
// changelog, and the moves its workflow allows now.
func (p *Provider) Detail(ctx context.Context, issue tracker.IssueID) (tracker.Detail, error) {
	if err := checkIssue(issue); err != nil {
		return tracker.Detail{}, err
	}
	s, err := p.site(ctx)
	if err != nil {
		return tracker.Detail{}, err
	}
	is, err := p.jira.GetIssue(ctx, string(issue))
	if err != nil {
		return tracker.Detail{}, fmt.Errorf("jiraprovider: detail of %s: %w", issue, err)
	}
	screen, err := p.jira.EditMeta(ctx, string(issue))
	if err != nil {
		return tracker.Detail{}, fmt.Errorf("jiraprovider: detail of %s: %w", issue, err)
	}
	cat := newCatalog()
	d := tracker.Detail{URL: p.origin + "/browse/" + url.PathEscape(is.Key)}
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
	d.Fields = detailFields(s, is, screen, cat)
	for _, l := range is.Links {
		d.Links = append(d.Links, linkOf(l, cat))
	}
	for _, a := range is.Attachments {
		d.Attachments = append(d.Attachments, attachmentOf(a, cat))
	}
	if d.Comments, err = p.commentPage(ctx, issue, ""); err != nil {
		return tracker.Detail{}, err
	}
	cat.merge(d.Comments.Catalog)
	if d.History, err = p.events(ctx, issue, cat); err != nil {
		return tracker.Detail{}, err
	}
	if d.Moves, err = p.moves(ctx, issue, cat); err != nil {
		return tracker.Detail{}, err
	}
	d.Catalog = cat.out
	return d, nil
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
		if slices.Contains(detailSkipped, id) || id == s.sprint {
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

// events is issue's whole changelog as history events, oldest first.
func (p *Provider) events(ctx context.Context, issue tracker.IssueID, cat *catalog) ([]tracker.Event, error) {
	log, err := p.jira.Changelog(ctx, string(issue))
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: history of %s: %w", issue, err)
	}
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
	return out, nil
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
// page ends; the first page is read from the end of the list, which
// takes a second request when the issue has more comments than a page.
// A comment deleted between pages can make one repeat; none is skipped.
func (p *Provider) commentPage(ctx context.Context, issue tracker.IssueID, page tracker.PageToken) (tracker.CommentPage, error) {
	var got jira.CommentPage
	var err error
	if page == "" {
		got, err = p.comments(ctx, issue, 0, commentPage)
		if err == nil && got.Total > len(got.Comments) {
			got, err = p.comments(ctx, issue, got.Total-commentPage, commentPage)
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
	return out, nil
}
