package trackerapp

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// FieldValueView is a field's value: its kind (tracker.Kind's name) and
// its values of that kind. Dates are YYYY-MM-DD and times Unix
// milliseconds. An empty value has no kind.
type FieldValueView struct {
	Kind    string       `json:"kind,omitempty"`
	Texts   []string     `json:"texts,omitempty"`
	Numbers []float64    `json:"numbers,omitempty"`
	Dates   []string     `json:"dates,omitempty"`
	Times   []int64      `json:"times,omitempty"`
	Choices []ChoiceView `json:"choices,omitempty"`
}

// FieldView is one field of a ticket with its metadata.
type FieldView struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	List     bool           `json:"list,omitempty"`
	Required bool           `json:"required,omitempty"`
	Editable bool           `json:"editable,omitempty"`
	Value    FieldValueView `json:"value"`
}

// CommentView is a comment, its body as Markdown.
type CommentView struct {
	ID       string   `json:"id"`
	Author   UserView `json:"author"`
	Body     string   `json:"body"`
	Created  int64    `json:"created"`
	Updated  int64    `json:"updated,omitempty"`
	Internal bool     `json:"internal,omitempty"`
}

// LinkedIssueView is the other issue of a link.
type LinkedIssueView struct {
	ID     string     `json:"id"`
	Key    string     `json:"key"`
	Title  string     `json:"title"`
	Status StatusView `json:"status"`
}

// LinkView is a link from the ticket to Issue: Relation is how the ticket
// relates to it ("blocks", "is blocked by"), from the link type's side
// the ticket is on.
type LinkView struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Relation string          `json:"relation"`
	Issue    LinkedIssueView `json:"issue"`
}

// AttachmentView is a file attached to the ticket.
type AttachmentView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	MimeType string   `json:"mime_type,omitempty"`
	Size     int64    `json:"size"`
	Created  int64    `json:"created"`
	Author   UserView `json:"author"`
}

// EventView is one change in the ticket's history; Author is nil for a
// change the tracker made itself.
type EventView struct {
	At     int64     `json:"at"`
	Author *UserView `json:"author,omitempty"`
	Field  string    `json:"field"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
}

// MoveView is a move the ticket's workflow allows now and the fields it
// takes.
type MoveView struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	To     StatusView  `json:"to"`
	Fields []FieldView `json:"fields,omitempty"`
}

// DetailView is one ticket as its view shows it. Comments is the first
// page, newest first; MoreComments is the token of the next page, empty
// when there is none. History is complete from HistoryFrom (Unix
// milliseconds, 0 for from creation).
type DetailView struct {
	Card         CardView         `json:"card"`
	URL          string           `json:"url"`
	Description  string           `json:"description"`
	Fields       []FieldView      `json:"fields"`
	Comments     []CommentView    `json:"comments"`
	MoreComments string           `json:"more_comments,omitempty"`
	Links        []LinkView       `json:"links"`
	Attachments  []AttachmentView `json:"attachments"`
	History      []EventView      `json:"history"`
	HistoryFrom  int64            `json:"history_from,omitempty"`
	Moves        []MoveView       `json:"moves"`
}

// IssueDetail is issue's detail on connection id as the tracker says it is
// in this call. On a tracker whose update time moves with everything a
// detail shows (tracker.Capabilities.DetailMovesUpdated), a detail the
// mirror holds is checked with the issue's card (the cards resource read,
// which refreshes the mirror's card too): it is returned, marked opened
// now, when the card's update time is that of the stored detail's card,
// and the detail is read otherwise. On any other tracker, or with no
// detail stored, the detail is read (the detail resource read): stored
// when the mirror keeps the issue, returned either way. Changes are what
// the call stored. A tracker failure leaves the stored detail as it is
// (MirroredDetail reads it), is settled as the connection's problem and is
// returned, the transport's typed error reachable through errors.As. A
// connection turned off or removed fails wrapping ErrDisabled or
// ErrNotFound.
func (s *Service) IssueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency) (DetailView, Changes, error) {
	if issue == "" {
		return DetailView{}, Changes{}, fmt.Errorf("%w: no issue named", ErrInvalidRequest)
	}
	r, err := s.start(ctx, id, u)
	if err != nil {
		return DetailView{}, Changes{}, err
	}
	defer r.finish()
	var d tracker.Detail
	var ch Changes
	// The linked scope's sync state carries the root's sweep the detail's
	// and the card's writes name.
	r.scope = Scope{Connection: id}
	readErr := r.startErr
	if readErr == nil {
		readErr = r.readState()
	}
	if readErr == nil {
		d, ch, readErr = r.verifiedDetail(issue)
	}
	var out Outcome
	callErr, stop, err := r.failed(&out, readErr)
	switch {
	case err != nil:
		return DetailView{}, ch, err
	case stop:
		return DetailView{}, ch, fmt.Errorf("%w: connection %s stopped being used while issue %s was read", ErrDisabled, id, issue)
	}
	if err := r.settleCalls(callErr); err != nil {
		return DetailView{}, ch, err
	}
	if out.Failure != nil {
		return DetailView{}, ch, readErr
	}
	v, err := s.detailView(r.root, d)
	return v, ch, err
}

// verifiedDetail is issue's detail as the tracker says it is now, and what
// reading it stored (IssueDetail).
func (r *run) verifiedDetail(issue tracker.IssueID) (tracker.Detail, Changes, error) {
	read := func() (tracker.Detail, Changes, error) {
		var d tracker.Detail
		var ch Changes
		err := r.againIfSwept(func() error {
			var err error
			d, ch, err = r.detail(issue)
			return err
		})
		return d, ch, err
	}
	if !r.p.Capabilities().DetailMovesUpdated {
		return read()
	}
	held, _, ok, err := r.s.storedDetail(r.root, issue)
	if err != nil {
		return tracker.Detail{}, Changes{}, err
	}
	if !ok {
		return read()
	}
	c, found, ch, err := r.card(issue)
	if err != nil {
		return tracker.Detail{}, ch, err
	}
	if !found || millis(c.Updated) != millis(held.Card.Updated) {
		d, dch, err := read()
		ch.add(dch)
		return d, ch, err
	}
	if _, err := r.s.store.TouchMirrorDetail(r.root, string(issue), millis(r.s.now())); err != nil {
		return tracker.Detail{}, ch, fmt.Errorf("trackerapp: mark the detail of %s opened: %w", issue, err)
	}
	return held, ch, nil
}

// MirroredDetail is issue's detail on connection id as the mirror holds
// it, for a first paint before IssueDetail answers, with when its read
// began (Unix milliseconds). It builds no client and makes no tracker
// call, and marks the detail opened now. It fails wrapping ErrNotMirrored
// when the mirror holds no detail of the issue that this version reads.
func (s *Service) MirroredDetail(id string, issue tracker.IssueID) (DetailView, int64, error) {
	if issue == "" {
		return DetailView{}, 0, fmt.Errorf("%w: no issue named", ErrInvalidRequest)
	}
	root, err := s.readRoot(id)
	if err != nil {
		return DetailView{}, 0, err
	}
	defer s.leave()
	d, readAt, ok, err := s.storedDetail(root, issue)
	switch {
	case err != nil:
		return DetailView{}, 0, err
	case !ok:
		return DetailView{}, 0, fmt.Errorf("%w: the detail of issue %s", ErrNotMirrored, issue)
	}
	if _, err := s.store.TouchMirrorDetail(root, string(issue), millis(s.now())); err != nil && !errors.Is(err, store.ErrTrackerCacheGone) {
		return DetailView{}, 0, fmt.Errorf("trackerapp: mark the detail of %s opened: %w", issue, err)
	}
	v, err := s.detailView(root, d)
	return v, readAt, err
}

// storedDetail is issue's detail as the mirror holds it, with when its read
// began; ok is false when the mirror holds none this version reads.
func (s *Service) storedDetail(root int64, issue tracker.IssueID) (tracker.Detail, int64, bool, error) {
	m, ok, err := s.store.GetMirrorDetail(root, string(issue))
	if err != nil {
		return tracker.Detail{}, 0, false, fmt.Errorf("trackerapp: read the detail of %s from the mirror: %w", issue, err)
	}
	if !ok {
		return tracker.Detail{}, 0, false, nil
	}
	d, ok := detailOf(m)
	return d, m.ReadAt, ok, nil
}

// detailView is d as a view, its names from its own catalog and, for the
// IDs that leaves out, the mirror's, with when its status began (only
// when the mirror's card describes the version d shows, statusSinceOf) and
// its parent's key and title from the mirror's cards.
func (s *Service) detailView(root int64, d tracker.Detail) (DetailView, error) {
	var users, statuses idSet
	users.add(string(d.Card.Assignee))
	statuses.add(string(d.Card.Status))
	for _, c := range d.Comments.Comments {
		users.add(string(c.Author))
	}
	for _, a := range d.Attachments {
		users.add(string(a.Author))
	}
	for _, e := range d.History {
		users.add(string(e.Author))
	}
	for _, l := range d.Links {
		statuses.add(string(l.Issue.Status))
	}
	for _, m := range d.Moves {
		statuses.add(string(m.To))
	}
	names, err := s.names(root, users.ids, statuses.ids)
	if err != nil {
		return DetailView{}, err
	}
	for _, u := range d.Users {
		names.users[string(u.ID)] = u
	}
	for _, st := range d.Statuses {
		names.statuses[string(st.ID)] = st
	}
	card, err := s.detailCard(root, d, names)
	if err != nil {
		return DetailView{}, err
	}
	v := DetailView{
		Card: card, URL: d.URL, Description: d.Description, Fields: fieldViews(d.Fields),
		Comments: []CommentView{}, MoreComments: string(d.Comments.Next), Links: []LinkView{},
		Attachments: []AttachmentView{}, History: []EventView{}, HistoryFrom: millis(d.HistoryFrom), Moves: []MoveView{},
	}
	for _, c := range d.Comments.Comments {
		v.Comments = append(v.Comments, CommentView{
			ID: string(c.ID), Author: names.user(string(c.Author)), Body: c.Body,
			Created: millis(c.Created), Updated: millis(c.Updated), Internal: c.Internal,
		})
	}
	for _, l := range d.Links {
		relation := l.Type.Inward
		if l.Outward {
			relation = l.Type.Outward
		}
		v.Links = append(v.Links, LinkView{ID: string(l.ID), Type: string(l.Type.ID), Relation: relation, Issue: LinkedIssueView{
			ID: string(l.Issue.ID), Key: l.Issue.Key, Title: l.Issue.Title, Status: names.status(string(l.Issue.Status)),
		}})
	}
	for _, a := range d.Attachments {
		v.Attachments = append(v.Attachments, AttachmentView{
			ID: string(a.ID), Name: a.Name, MimeType: a.MimeType, Size: a.Size, Created: millis(a.Created), Author: names.user(string(a.Author)),
		})
	}
	for _, e := range d.History {
		ev := EventView{At: millis(e.At), Field: e.Field, From: e.From, To: e.To}
		if e.Author != "" {
			u := names.user(string(e.Author))
			ev.Author = &u
		}
		v.History = append(v.History, ev)
	}
	for _, m := range d.Moves {
		v.Moves = append(v.Moves, MoveView{ID: string(m.ID), Name: m.Name, To: names.status(string(m.To)), Fields: fieldViews(m.Fields)})
	}
	return v, nil
}

// detailCard is d's card as a view: the issue's card and its parent's
// read from the mirror in one lookup.
func (s *Service) detailCard(root int64, d tracker.Detail, names catalogNames) (CardView, error) {
	card := cardView(d.Card, names)
	want := []string{string(d.Card.ID)}
	if d.Card.Parent != "" {
		card.Parent = &ParentView{ID: string(d.Card.Parent)}
		want = append(want, string(d.Card.Parent))
	}
	cards, err := s.store.ListMirrorCards(root, want)
	if err != nil {
		return CardView{}, fmt.Errorf("trackerapp: read the card and parent of %s from the mirror: %w", d.Card.ID, err)
	}
	for _, m := range cards {
		switch m.IssueID {
		case string(d.Card.ID):
			card.StatusSince = statusSinceOf(m, d.Card)
		case string(d.Card.Parent):
			p, _ := cardOf(m)
			card.Parent.Key, card.Parent.Title = p.Key, p.Title
		}
	}
	return card, nil
}

// statusSinceOf is when c's status began, from m, the mirror's card of
// the same issue, when m is the version c shows: the same update time and
// status. Otherwise it is unknown (0): the mirror's time belongs to
// another status, or to an earlier or a later stay in this one.
func statusSinceOf(m store.MirrorCard, c tracker.Card) int64 {
	if m.StatusID != string(c.Status) || m.UpdatedAt != millis(c.Updated) {
		return 0
	}
	return m.StatusSince
}

func fieldViews(fields []tracker.Field) []FieldView {
	out := make([]FieldView, 0, len(fields))
	for _, f := range fields {
		out = append(out, FieldView{
			ID: string(f.ID), Name: f.Name, Kind: f.Kind.String(), List: f.List, Required: f.Required, Editable: f.Editable,
			Value: fieldValueView(f.Value),
		})
	}
	return out
}

func fieldValueView(v tracker.Value) FieldValueView {
	if v.Empty() {
		return FieldValueView{}
	}
	out := FieldValueView{Kind: v.Kind().String(), Texts: v.Texts(), Numbers: v.Numbers()}
	for _, d := range v.Dates() {
		out.Dates = append(out.Dates, d.String())
	}
	for _, t := range v.Times() {
		out.Times = append(out.Times, millis(t))
	}
	for _, c := range v.Choices() {
		out.Choices = append(out.Choices, ChoiceView(c))
	}
	return out
}
