package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
// in this call, a person opening it. On a tracker whose update time moves
// with everything a detail shows of the issue
// (tracker.Capabilities.DetailMovesUpdated), a detail the mirror holds is
// checked with one cards read of the issue (which refreshes the mirror's
// card too): when the issue's card has the update time of the stored
// detail's card, the stored detail is returned, marked opened now. The
// detail is read instead (one Details read of the issue) when the card's
// update time differs, when the mirror's card is already newer than the
// detail's, when the detail's read began more than detailMaxAge ago, and
// when the mirror no longer holds the document checked. On any other
// tracker, or with no detail stored, the detail is read: stored when the
// mirror keeps the issue, returned either way. A linked issue shows the
// mirror's card of it when the mirror keeps one, and otherwise what the
// last full read of the detail found, at most detailMaxAge old. Changes
// are what the call stored. An issue the tracker did not return fails
// wrapping ErrIssueNotFound. A tracker failure leaves the stored detail as
// it is (MirroredDetail reads it), is settled as the connection's problem
// and is returned, the transport's typed error reachable through
// errors.As. A connection turned off or removed fails wrapping ErrDisabled
// or ErrNotFound.
func (s *Service) IssueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency) (DetailView, Changes, error) {
	return s.issueDetail(ctx, id, issue, u, false)
}

// RefreshIssueDetail is IssueDetail for a person's refresh: the detail is
// read in full whatever the mirror holds.
func (s *Service) RefreshIssueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency) (DetailView, Changes, error) {
	return s.issueDetail(ctx, id, issue, u, true)
}

// issueDetail is IssueDetail, the detail read in full when full is set: a
// read of one ticket with nothing held of it (readDetails).
func (s *Service) issueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency, full bool) (DetailView, Changes, error) {
	if issue == "" {
		return DetailView{}, Changes{}, fmt.Errorf("%w: no issue named", ErrInvalidRequest)
	}
	rd := &ticketRead{issue: issue, full: full}
	res, err := s.readDetails(ctx, id, []*ticketRead{rd}, u, millis(s.now()))
	ch := res.ch
	// An open ticket of the issue, or one whose view shows it, is told what
	// the read stored.
	s.publish(res.root, res.gen, id, ch, nil, nil)
	switch {
	case err != nil:
		return DetailView{}, ch, err
	case rd.err != nil:
		return DetailView{}, ch, fmt.Errorf("%w: issue %s of %s", rd.err, issue, id)
	case rd.view == nil:
		// A read that began later stored another document meanwhile.
		v, _, err := s.MirroredDetail(id, issue)
		return v, ch, err
	}
	return *rd.view, ch, nil
}

// MirroredDetail is issue's detail on connection id as the mirror holds
// it, for a first paint before IssueDetail answers, with when its read
// began (Unix milliseconds). It builds no client and makes no tracker
// call, and marks the detail opened now. It fails wrapping ErrNotMirrored
// when the mirror holds no detail of the issue that this version reads. A
// detail another read rewrote, or a sweep reached, between its read and
// its marking is read again: only the mirror holding none is an answer of
// not mirrored.
func (s *Service) MirroredDetail(id string, issue tracker.IssueID) (DetailView, int64, error) {
	if issue == "" {
		return DetailView{}, 0, fmt.Errorf("%w: no issue named", ErrInvalidRequest)
	}
	root, err := s.readRoot(id)
	if err != nil {
		return DetailView{}, 0, err
	}
	defer s.leave()
	for {
		m, d, ok, err := s.storedDetail(root, issue)
		switch {
		case err != nil:
			return DetailView{}, 0, err
		case !ok:
			return DetailView{}, 0, fmt.Errorf("%w: the detail of issue %s", ErrNotMirrored, issue)
		}
		confirmed, err := s.store.ConfirmMirrorDetail(root, m, millis(s.now()))
		switch {
		case errors.Is(err, store.ErrTrackerCacheGone):
			return DetailView{}, 0, fmt.Errorf("%w: the mirror of issue %s went while it was read", ErrNotMirrored, issue)
		case err != nil:
			return DetailView{}, 0, fmt.Errorf("trackerapp: mark the detail of %s opened: %w", issue, err)
		case confirmed.Stored:
			v, err := s.detailView(root, d)
			return v, m.ReadAt, err
		}
	}
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

// detailView is d as a view (detailViews).
func (s *Service) detailView(root int64, d tracker.Detail) (DetailView, error) {
	views, err := s.detailViews(root, []tracker.Detail{d})
	if err != nil {
		return DetailView{}, err
	}
	return views[0], nil
}

// detailViews is each of ds as a view, its names from its own catalog and,
// for the IDs that leaves out, the mirror's, with when its status began
// (only when the mirror's card describes the version it shows,
// statusSinceOf), its parent's key and title, and each linked issue's key,
// title and status from the mirror's card of it when the mirror keeps one
// (else from the detail). The cards of every detail are read in one
// lookup, and then the names of every detail in one, a store lookup's
// bound at a time; a detail linking more than that bound shows the rest
// as it has them.
func (s *Service) detailViews(root int64, ds []tracker.Detail) ([]DetailView, error) {
	wants := make([][]string, len(ds))
	var all idSet
	for i, d := range ds {
		wants[i] = append(wants[i], string(d.Card.ID))
		for _, id := range showsOf(d) {
			if len(wants[i]) == readBatch {
				break
			}
			wants[i] = append(wants[i], string(id))
		}
		for _, id := range wants[i] {
			all.add(id)
		}
	}
	mirrored := make(map[string]store.MirrorCard, len(all.ids))
	for batch := range slices.Chunk(all.ids, readBatch) {
		cards, err := s.store.ListMirrorCards(root, batch)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: read the cards %d details show from the mirror: %w", len(ds), err)
		}
		for _, m := range cards {
			mirrored[m.IssueID] = m
		}
	}
	shown := make([]shownDetail, len(ds))
	var users, statuses idSet
	for i, d := range ds {
		shown[i] = shownDetailOf(d, wants[i], mirrored)
		shown[i].names(&users, &statuses)
	}
	names, err := s.names(root, users.ids, statuses.ids)
	if err != nil {
		return nil, err
	}
	views := make([]DetailView, len(ds))
	for i := range ds {
		views[i] = shown[i].view(names)
	}
	return views, nil
}

// shownDetail is a detail with what the mirror's cards show of it: its
// links named from them, its parent's card and its own.
type shownDetail struct {
	d      tracker.Detail
	links  []tracker.Link
	parent *tracker.Card
	own    *store.MirrorCard
}

// shownDetailOf is d shown from the cards of mirrored that want names: its
// own issue first, then the others it shows.
func shownDetailOf(d tracker.Detail, want []string, mirrored map[string]store.MirrorCard) shownDetail {
	out := shownDetail{d: d, links: slices.Clone(d.Links)}
	cards := make(map[tracker.IssueID]tracker.Card, len(want))
	for _, id := range want {
		m, ok := mirrored[id]
		switch {
		case !ok:
		case id == string(d.Card.ID):
			out.own = &m
		default:
			if c, ok := cardOf(m); ok {
				cards[c.ID] = c
			}
		}
	}
	for i := range out.links {
		if c, ok := cards[out.links[i].Issue.ID]; ok {
			ref := &out.links[i].Issue
			ref.Key, ref.Title, ref.Status = c.Key, c.Title, c.Status
		}
	}
	if p, ok := cards[d.Card.Parent]; ok && d.Card.Parent != "" {
		out.parent = &p
	}
	return out
}

// names adds the users and statuses the view names.
func (sd shownDetail) names(users, statuses *idSet) {
	d := sd.d
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
	for _, l := range sd.links {
		statuses.add(string(l.Issue.Status))
	}
	for _, m := range d.Moves {
		statuses.add(string(m.To))
	}
}

// view is the detail's view, named from its own catalog over mirrored, the
// mirror's names.
func (sd shownDetail) view(mirrored catalogNames) DetailView {
	d := sd.d
	names := mirrored
	if len(d.Users)+len(d.Statuses) > 0 {
		names = catalogNames{users: maps.Clone(mirrored.users), statuses: maps.Clone(mirrored.statuses)}
		for _, u := range d.Users {
			names.users[string(u.ID)] = u
		}
		for _, st := range d.Statuses {
			names.statuses[string(st.ID)] = st
		}
	}
	card := cardView(d.Card, names)
	if sd.own != nil {
		card.StatusSince = statusSinceOf(*sd.own, d.Card)
	}
	if d.Card.Parent != "" {
		card.Parent = &ParentView{ID: string(d.Card.Parent)}
		if p := sd.parent; p != nil {
			card.Parent.Key, card.Parent.Title = p.Key, p.Title
		}
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
	for _, l := range sd.links {
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
	return v
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
