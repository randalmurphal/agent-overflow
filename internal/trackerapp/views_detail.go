package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

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

// detailMaxAge is how long after its read began a stored detail is checked
// rather than read: an older one is read in full. Its moves and the
// metadata of its fields come from the workflow and the screens, which
// change without moving the issue's update time; this is the board
// structure's cadence, at which the mirror reads that configuration again.
const detailMaxAge = 30 * time.Minute

// IssueDetail is issue's detail on connection id as the tracker says it is
// in this call. On a tracker whose update time moves with everything a
// detail shows (tracker.Capabilities.DetailMovesUpdated), a detail the
// mirror holds is checked with one cards read of the issue and of the
// issues it links (the cards resource read, which refreshes the mirror's
// cards too): when the issue's card has the update time of the stored
// detail's card, the stored detail is returned, marked opened now, its
// linked issues' keys, titles and statuses from the cards read (stored
// again when they changed; a linked issue the tracker did not return keeps
// what the detail holds). The detail is read instead (the detail resource
// read) when the card's update time differs, when the mirror's card is
// already newer than the detail's, when the detail's read began more than
// detailMaxAge ago, when it links more issues than one cards read takes,
// and when the mirror no longer holds the document checked. On any other
// tracker, or with no detail stored, the detail is read: stored when the
// mirror keeps the issue, returned either way. Changes are what the call
// stored. A tracker failure leaves the stored detail as it is
// (MirroredDetail reads it), is settled as the connection's problem and is
// returned, the transport's typed error reachable through errors.As. A
// connection turned off or removed fails wrapping ErrDisabled or
// ErrNotFound.
func (s *Service) IssueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency) (DetailView, Changes, error) {
	return s.issueDetail(ctx, id, issue, u, false)
}

// RefreshIssueDetail is IssueDetail for a person's refresh: the detail is
// read in full whatever the mirror holds.
func (s *Service) RefreshIssueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency) (DetailView, Changes, error) {
	return s.issueDetail(ctx, id, issue, u, true)
}

// issueDetail is IssueDetail, the detail read in full when full is set.
func (s *Service) issueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency, full bool) (DetailView, Changes, error) {
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
		d, ch, readErr = r.verifiedDetail(issue, full)
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
// reading it stored (IssueDetail); full reads it whatever the mirror
// holds.
func (r *run) verifiedDetail(issue tracker.IssueID, full bool) (tracker.Detail, Changes, error) {
	var out Changes
	read := func() (tracker.Detail, Changes, error) {
		var d tracker.Detail
		err := r.againIfSwept(func() error {
			var ch Changes
			var err error
			d, ch, err = r.detail(issue)
			out.add(ch)
			return err
		})
		return d, out, err
	}
	if full || !r.p.Capabilities().DetailMovesUpdated {
		return read()
	}
	m, held, ok, err := r.s.storedDetail(r.root, issue)
	if err != nil {
		return tracker.Detail{}, out, err
	}
	if !ok {
		return read()
	}
	now := r.s.now()
	checked := checkedIssues(held)
	if now.Sub(timeOf(m.ReadAt)) > detailMaxAge || m.CardUpdatedAt > millis(held.Card.Updated) || len(checked) > readBatch {
		return read()
	}
	cards, ch, err := r.cardsNow(checked)
	out.add(ch)
	if err != nil {
		return tracker.Detail{}, out, err
	}
	i := slices.IndexFunc(cards, func(c tracker.Card) bool { return c.ID == issue })
	if i < 0 || millis(cards[i].Updated) != millis(held.Card.Updated) {
		return read()
	}
	var replace []byte
	if refreshLinks(&held, cards) {
		if replace, err = mirrorDetail(held); err != nil {
			return tracker.Detail{}, out, err
		}
	}
	confirmed, err := r.s.store.ConfirmMirrorDetail(r.root, m, millis(now), replace)
	switch {
	case err != nil:
		return tracker.Detail{}, out, fmt.Errorf("trackerapp: mark the detail of %s opened: %w", issue, err)
	case !confirmed.Stored:
		return read()
	case confirmed.Changed:
		out.add(Changes{Details: []tracker.IssueID{issue}})
	}
	return held, out, nil
}

// checkedIssues are the issues a check of d reads the cards of: d's own
// first, then each issue it links once.
func checkedIssues(d tracker.Detail) []tracker.IssueID {
	out := make([]tracker.IssueID, 1, 1+len(d.Links))
	out[0] = d.Card.ID
	for _, l := range d.Links {
		if l.Issue.ID != "" && !slices.Contains(out, l.Issue.ID) {
			out = append(out, l.Issue.ID)
		}
	}
	return out
}

// refreshLinks sets the key, title and status of the issues d links from
// cards read since d; one cards lacks keeps what d holds. It reports
// whether any changed.
func refreshLinks(d *tracker.Detail, cards []tracker.Card) bool {
	changed := false
	for i := range d.Links {
		ref := &d.Links[i].Issue
		j := slices.IndexFunc(cards, func(c tracker.Card) bool { return c.ID == ref.ID })
		if j < 0 {
			continue
		}
		if c := cards[j]; ref.Key != c.Key || ref.Title != c.Title || ref.Status != c.Status {
			ref.Key, ref.Title, ref.Status = c.Key, c.Title, c.Status
			changed = true
		}
	}
	return changed
}

// MirroredDetail is issue's detail on connection id as the mirror holds
// it, for a first paint before IssueDetail answers, with when its read
// began (Unix milliseconds). It builds no client and makes no tracker
// call, and marks the detail opened now. It fails wrapping ErrNotMirrored
// when the mirror holds no detail of the issue that this version reads,
// or no longer holds the one it read when it marks it opened.
func (s *Service) MirroredDetail(id string, issue tracker.IssueID) (DetailView, int64, error) {
	if issue == "" {
		return DetailView{}, 0, fmt.Errorf("%w: no issue named", ErrInvalidRequest)
	}
	root, err := s.readRoot(id)
	if err != nil {
		return DetailView{}, 0, err
	}
	defer s.leave()
	m, d, ok, err := s.storedDetail(root, issue)
	switch {
	case err != nil:
		return DetailView{}, 0, err
	case !ok:
		return DetailView{}, 0, fmt.Errorf("%w: the detail of issue %s", ErrNotMirrored, issue)
	}
	confirmed, err := s.store.ConfirmMirrorDetail(root, m, millis(s.now()), nil)
	switch {
	case errors.Is(err, store.ErrTrackerCacheGone) || err == nil && !confirmed.Stored:
		return DetailView{}, 0, fmt.Errorf("%w: the detail of issue %s went while it was read", ErrNotMirrored, issue)
	case err != nil:
		return DetailView{}, 0, fmt.Errorf("trackerapp: mark the detail of %s opened: %w", issue, err)
	}
	v, err := s.detailView(root, d)
	return v, m.ReadAt, err
}

// storedDetail is issue's detail as the mirror holds it, stored and
// decoded; ok is false when the mirror holds none this version reads.
func (s *Service) storedDetail(root int64, issue tracker.IssueID) (store.MirrorDetail, tracker.Detail, bool, error) {
	m, ok, err := s.store.GetMirrorDetail(root, string(issue))
	if err != nil {
		return store.MirrorDetail{}, tracker.Detail{}, false, fmt.Errorf("trackerapp: read the detail of %s from the mirror: %w", issue, err)
	}
	if !ok {
		return store.MirrorDetail{}, tracker.Detail{}, false, nil
	}
	d, ok := detailOf(m)
	return m, d, ok, nil
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
