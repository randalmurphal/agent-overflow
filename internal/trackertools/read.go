package trackertools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/trackerapp"
)

type readArgs struct {
	Key          string `json:"key"`
	History      bool   `json:"history"`
	MoreComments string `json:"more_comments"`
}

// maxKeyRunes bounds a key: longer text names no ticket on either tracker,
// and a refusal quotes the key back.
const maxKeyRunes = 64

// ticket is where a key resolved: the connection that reads it, its site
// as a person names it, the tracker's issue ID, and the key as the model
// named it.
type ticket struct {
	connection, site, issue, key string
	// linked is the thread's own ticket.
	linked bool
}

// commentsCursor is where an answer's older comments go on: the tracker's
// page of them (empty for the first page) and how many of its comments
// earlier answers showed.
type commentsCursor struct {
	Connection string `json:"c"`
	Issue      string `json:"i"`
	Page       string `json:"p,omitempty"`
	Offset     int    `json:"o,omitempty"`
}

type readComment struct {
	By       string `json:"by,omitempty"`
	At       string `json:"at"`
	Body     string `json:"body"`
	Internal bool   `json:"internal,omitempty"`
}

type readLink struct {
	Relation string `json:"relation"`
	Key      string `json:"key"`
	Title    string `json:"title,omitempty"`
	Status   string `json:"status,omitempty"`
}

type readChange struct {
	At    string `json:"at"`
	By    string `json:"by,omitempty"`
	Field string `json:"field"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// readAnswer is one ticket. No member is empty: an empty one is left out,
// so the model never reads a field as cleared that the ticket simply does
// not have. Fields holds every filled field the members above do not, in
// the tracker's order.
type readAnswer struct {
	Key          string          `json:"key"`
	Title        string          `json:"title"`
	Status       string          `json:"status,omitempty"`
	Type         string          `json:"type,omitempty"`
	Assignee     string          `json:"assignee,omitempty"`
	Priority     string          `json:"priority,omitempty"`
	Labels       []string        `json:"labels,omitempty"`
	Parent       string          `json:"parent,omitempty"`
	URL          string          `json:"url,omitempty"`
	Description  string          `json:"description,omitempty"`
	Fields       json.RawMessage `json:"fields,omitempty"`
	Links        []readLink      `json:"links,omitempty"`
	Attachments  []string        `json:"attachments,omitempty"`
	Comments     []readComment   `json:"comments,omitempty"`
	MoreComments string          `json:"more_comments,omitempty"`
	History      []readChange    `json:"history,omitempty"`
	// HistorySince is the time the tracker's history of the ticket starts
	// from, when it does not reach back to the ticket's creation.
	HistorySince string `json:"history_since,omitempty"`
	OlderHistory int    `json:"older_history,omitempty"`
}

// commentsAnswer is a more_comments answer: the comments alone.
type commentsAnswer struct {
	Comments     []readComment `json:"comments"`
	MoreComments string        `json:"more_comments,omitempty"`
}

func (s *Server) read(ctx context.Context, threadID string, raw json.RawMessage) (any, error) {
	var args readArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.MoreComments != "" {
		if args.Key != "" || args.History {
			return nil, invalidf("more_comments answers those comments alone; leave out key and history.")
		}
		return s.moreComments(ctx, args.MoreComments)
	}
	t, err := s.resolve(ctx, threadID, args.Key)
	if err != nil {
		return nil, err
	}
	d, err := s.t.IssueDetail(ctx, t.connection, t.issue)
	switch {
	case notFound(err) && t.linked:
		return nil, errorsx.Public(CodeNotFound, fmt.Sprintf(
			"%s, the ticket this thread is linked to, is not on %s any more, or the user's account cannot see it.", t.key, t.site), err)
	case notFound(err):
		return nil, errorsx.Public(CodeNotFound, fmt.Sprintf("No ticket %s on %s.", t.key, t.site), err)
	case err != nil:
		return nil, err
	}
	return ticketAnswer(d, t, args.History), nil
}

// resolve is the ticket key names for thread threadID: with no key, the
// ticket the thread is linked to; a key the link names, the link's (no
// request); any other, the first ticket source whose tracker finds it.
func (s *Server) resolve(ctx context.Context, threadID, key string) (ticket, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if len([]rune(key)) > maxKeyRunes {
		return ticket{}, invalidf("key is longer than any ticket key.")
	}
	link, linked, err := s.t.LinkedTicket(threadID)
	var refused *trackerapp.LinkRefusal
	switch {
	case key == "" && err != nil:
		return ticket{}, err
	case key == "" && !linked:
		return ticket{}, errorsx.Public(CodeNoTicket, "This thread is not linked to a ticket; name one with key.", nil)
	case key == "":
		return ticket{connection: link.Connection, site: link.Site, issue: link.Issue, key: link.Key, linked: true}, nil
	case errors.As(err, &refused):
		// The thread's ticket cannot be read here, which says nothing of
		// the ticket key names: the sources look for it.
	case err != nil:
		return ticket{}, err
	case linked && strings.EqualFold(link.Key, key):
		return ticket{connection: link.Connection, site: link.Site, issue: link.Issue, key: link.Key, linked: true}, nil
	}
	sources, err := s.sources()
	if err != nil {
		return ticket{}, err
	}
	var failed error
	for _, src := range sources {
		found, err := s.t.Lookup(ctx, src.ID, []string{key})
		if err != nil {
			// As ticket_find: a refusal the model can read leaves the other
			// sources asked; anything else fails the call.
			if _, _, public := errorsx.PublicDetails(err); !public || ctx.Err() != nil {
				return ticket{}, onSite(src.Site, err)
			}
			if failed == nil {
				failed = onSite(src.Site, err)
			}
			continue
		}
		if len(found.Found) > 0 {
			return ticket{connection: src.ID, site: src.Site, issue: found.Found[0].Card.ID, key: key}, nil
		}
	}
	if failed != nil {
		// A source that could not answer may hold the key.
		return ticket{}, failed
	}
	return ticket{}, errorsx.Public(CodeNotFound, fmt.Sprintf("No ticket %s on %s.", key, siteList(sources)), nil)
}

// ticketAnswer is d as the model reads it.
func ticketAnswer(d trackerapp.DetailView, t ticket, history bool) readAnswer {
	c := d.Card
	out := readAnswer{Key: c.Key, Title: c.Title, Status: c.Status.Name, URL: d.URL, Description: d.Description}
	if c.Type != nil {
		out.Type = c.Type.Name
	}
	if c.Assignee != nil {
		out.Assignee = c.Assignee.Name
	}
	if c.Priority != nil {
		out.Priority = c.Priority.Name
	}
	for _, l := range c.Labels {
		if l.Name != "" {
			out.Labels = append(out.Labels, l.Name)
		}
	}
	if c.Parent != nil {
		out.Parent = strings.TrimSpace(c.Parent.Key + " " + c.Parent.Title)
	}
	out.Fields = fieldsObject(d.Fields)
	for _, l := range d.Links {
		out.Links = append(out.Links, readLink{Relation: l.Relation, Key: l.Issue.Key, Title: l.Issue.Title, Status: l.Issue.Status.Name})
	}
	for _, a := range d.Attachments {
		out.Attachments = append(out.Attachments, a.Name)
	}
	shown := d.Comments[:min(len(d.Comments), commentsShown)]
	out.Comments = readComments(shown)
	switch {
	case len(d.Comments) > commentsShown:
		out.MoreComments = encodeCursor(commentsCursor{Connection: t.connection, Issue: c.ID, Offset: commentsShown})
	case d.MoreComments != "":
		out.MoreComments = encodeCursor(commentsCursor{Connection: t.connection, Issue: c.ID, Page: d.MoreComments})
	}
	if history {
		changes := slices.Clone(d.History)
		slices.SortStableFunc(changes, func(a, b trackerapp.EventView) int { return cmp.Compare(b.At, a.At) })
		for _, e := range changes[:min(len(changes), historyShown)] {
			ch := readChange{At: stamp(e.At), Field: e.Field, From: e.From, To: e.To}
			if e.Author != nil {
				ch.By = e.Author.Name
			}
			out.History = append(out.History, ch)
		}
		out.OlderHistory = max(0, len(changes)-historyShown)
		if d.HistoryFrom > 0 {
			out.HistorySince = stamp(d.HistoryFrom)
		}
	}
	return out
}

// moreComments answers the comments a ticket_read's more_comments names.
func (s *Server) moreComments(ctx context.Context, text string) (any, error) {
	var at commentsCursor
	if err := decodeCursor("more_comments", text, &at); err != nil {
		return nil, err
	}
	if at.Connection == "" || at.Issue == "" || at.Offset < 0 {
		return nil, invalidf("more_comments is not one these tools gave; read the ticket again.")
	}
	page, err := s.t.Comments(ctx, at.Connection, at.Issue, at.Page)
	if err != nil {
		return nil, err
	}
	rest := page.Comments[min(at.Offset, len(page.Comments)):]
	out := commentsAnswer{Comments: readComments(rest[:min(len(rest), commentsShown)])}
	switch {
	case len(rest) > commentsShown:
		at.Offset += commentsShown
		out.MoreComments = encodeCursor(at)
	case page.Next != "":
		out.MoreComments = encodeCursor(commentsCursor{Connection: at.Connection, Issue: at.Issue, Page: page.Next})
	}
	return out, nil
}

func readComments(comments []trackerapp.CommentView) []readComment {
	out := make([]readComment, len(comments))
	for i, c := range comments {
		out[i] = readComment{By: c.Author.Name, At: stamp(c.Created), Body: c.Body, Internal: c.Internal}
	}
	return out
}

// fieldsObject is fields as one JSON object of name to value, in the
// tracker's order, empty fields left out; nil when none is filled. A name
// two fields share is told apart by each one's ID.
func fieldsObject(fields []trackerapp.FieldView) json.RawMessage {
	names := map[string]int{}
	for _, f := range fields {
		if f.Value.Kind != "" {
			names[f.Name]++
		}
	}
	var b bytes.Buffer
	for _, f := range fields {
		if f.Value.Kind == "" {
			continue
		}
		name := f.Name
		if names[name] > 1 {
			name += " (" + f.ID + ")"
		}
		if b.Len() == 0 {
			b.WriteByte('{')
		} else {
			b.WriteByte(',')
		}
		b.Write(mustJSON(name))
		b.WriteByte(':')
		b.Write(mustJSON(fieldValue(f)))
	}
	if b.Len() == 0 {
		return nil
	}
	b.WriteByte('}')
	return b.Bytes()
}

// fieldValue is a field's value as text, a number, or a list of them for
// a field that holds a list.
func fieldValue(f trackerapp.FieldView) any {
	v := f.Value
	var values []any
	for _, t := range v.Texts {
		values = append(values, t)
	}
	for _, n := range v.Numbers {
		values = append(values, n)
	}
	for _, d := range v.Dates {
		values = append(values, d)
	}
	for _, t := range v.Times {
		values = append(values, stamp(t))
	}
	for _, c := range v.Choices {
		values = append(values, c.Name)
	}
	if !f.List && len(values) == 1 {
		return values[0]
	}
	return values
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		// Field names and values are strings and finite numbers.
		panic("trackertools: encode a field: " + err.Error())
	}
	return raw
}

// stamp is a Unix-milliseconds time as the model reads it: UTC, to the
// minute.
func stamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04Z")
}
