package trackerapp

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jiraprovider"
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
	// Allowed are the values a create screen lists for the field, which
	// its picker offers with no read; absent when the screen lists none.
	// Defaulted marks a Required field of a create's or a move's screen
	// that the tracker fills when it is given none: offered, never
	// cleared, not demanded (tracker.ScreenField).
	Allowed   []ChoiceView `json:"allowed,omitempty"`
	Defaulted bool         `json:"defaulted,omitempty"`
}

// CommentView is a comment, its body as Markdown. Thread is the thread
// whose agent wrote it through the tracker tools.
type CommentView struct {
	ID       string          `json:"id"`
	Author   UserView        `json:"author"`
	Body     string          `json:"body"`
	Created  int64           `json:"created"`
	Updated  int64           `json:"updated,omitempty"`
	Internal bool            `json:"internal,omitempty"`
	Thread   *ThreadMarkView `json:"thread,omitempty"`
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
// change the tracker made itself. Agent is the thread whose agent made
// it through the tracker tools.
type EventView struct {
	At     int64           `json:"at"`
	Author *UserView       `json:"author,omitempty"`
	Field  string          `json:"field"`
	From   string          `json:"from,omitempty"`
	To     string          `json:"to,omitempty"`
	Agent  *ThreadMarkView `json:"agent,omitempty"`
}

// TicketRowView is the ticket's issue on one board the mirror shows: its
// estimate there, in the board's unit (none, points or time in seconds),
// which SetTrackerEstimate changes on that board.
type TicketRowView struct {
	Board    string   `json:"board"`
	Estimate *float64 `json:"estimate,omitempty"`
	Unit     string   `json:"unit"`
}

// TicketSprintView is a sprint the ticket's card names, with its name and
// state from a board the ticket has a row on; both are empty for a
// sprint none of those boards holds.
type TicketSprintView struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	State string `json:"state,omitempty"`
}

// MoveView is a move the ticket's workflow allows now and the fields it
// takes.
type MoveView struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	To     StatusView  `json:"to"`
	Fields []FieldView `json:"fields,omitempty"`
}

// DetailView is one ticket as its view shows it: the issue record's card
// (the one every view of the issue shows) with its body. Comments is the
// first page, newest first; MoreComments is the token of the next page,
// empty when there is none. History is complete from HistoryFrom (Unix
// milliseconds, 0 for from creation). Rows are the issue's rows on the
// boards the mirror shows, by board ID, each with the estimate the
// ticket's own read found when it carried the board's field (issueRows).
// BodyBehind reports a body read for
// an older version of the issue than its card: a read of the card found a
// change the body does not show yet, and the next read of the ticket
// reads it.
type DetailView struct {
	Card         CardView           `json:"card"`
	BodyBehind   bool               `json:"body_behind,omitempty"`
	URL          string             `json:"url"`
	Description  string             `json:"description"`
	Fields       []FieldView        `json:"fields"`
	Comments     []CommentView      `json:"comments"`
	MoreComments string             `json:"more_comments,omitempty"`
	Links        []LinkView         `json:"links"`
	Attachments  []AttachmentView   `json:"attachments"`
	History      []EventView        `json:"history"`
	HistoryFrom  int64              `json:"history_from,omitempty"`
	Moves        []MoveView         `json:"moves"`
	Rows         []TicketRowView    `json:"rows"`
	Sprints      []TicketSprintView `json:"sprints"`
}

// detailMaxAge is how long after its read began a stored detail is checked
// rather than read: an older one is read in full. Its moves and the
// metadata of its fields come from the workflow and the screens, which
// change without moving the issue's update time; this is the board
// structure's cadence, at which the mirror reads that configuration again.
const detailMaxAge = 30 * time.Minute

// IssueDetail is issue's detail on connection id as the tracker says it is
// in this call, a person or an agent reading it. On a tracker whose update
// time moves with everything a detail shows of the issue
// (tracker.Capabilities.DetailMovesUpdated), a detail the mirror holds is
// checked with one cards read of the issue (which refreshes the mirror's
// card too): when the issue's card has the update time of the stored
// detail's card, the stored detail is shown, marked opened now. The
// detail is read instead (one Details read of the issue) when the card's
// update time differs, when the mirror's card is already newer than the
// detail's, when the detail's read began more than detailMaxAge ago, and
// when the mirror no longer holds the detail checked. On any other
// tracker, or with no detail stored, the detail is read: stored when the
// mirror keeps the issue, and shown as read either way. A linked issue
// shows the mirror's card of it when the mirror keeps one, and otherwise
// what the last full read of the detail found, at most detailMaxAge old.
// Changes are what the call stored, which the change funnel told every
// open ticket. An issue the tracker did not return fails wrapping
// ErrIssueNotFound. A tracker failure leaves the stored detail as it is,
// is settled as the connection's problem and is returned, the transport's
// typed error reachable through errors.As. A connection turned off or
// removed fails wrapping ErrDisabled or ErrNotFound.
func (s *Service) IssueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency) (DetailView, Changes, error) {
	return s.issueDetail(ctx, id, issue, u, false, true)
}

// UnmarkedIssueDetail is IssueDetail without the agents' marks: what a
// reader that shows none of them, the tracker tools, reads.
func (s *Service) UnmarkedIssueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency) (DetailView, Changes, error) {
	return s.issueDetail(ctx, id, issue, u, false, false)
}

// issueDetail is IssueDetail, the detail read in full when full is set: a
// read of one ticket no pump makes (readDetails), with the agents' marks
// when marked is set.
func (s *Service) issueDetail(ctx context.Context, id string, issue tracker.IssueID, u Urgency, full, marked bool) (DetailView, Changes, error) {
	if issue == "" {
		return DetailView{}, Changes{}, fmt.Errorf("%w: no issue named", ErrInvalidRequest)
	}
	rd := &ticketRead{issue: issue, full: full}
	res, err := s.readDetails(ctx, id, []*ticketRead{rd}, u, true)
	ch := res.ch
	switch {
	case err != nil:
		return DetailView{}, ch, err
	case rd.err != nil:
		return DetailView{}, ch, fmt.Errorf("%w: issue %s of %s", rd.err, issue, id)
	}
	if err := s.enter(); err != nil {
		return DetailView{}, ch, err
	}
	defer s.leave()
	row, err := s.viewRow(id)
	if err != nil {
		return DetailView{}, ch, err
	}
	v, _, err := s.detailView(res.root, row, *rd.detail, marked)
	return v, ch, err
}

// ticketDoc is a ticket's document: its detail, the update time of the
// read that brought its body (version), and the evidence of its status
// time when the detail was read and is held in memory (status; nil for a
// body the mirror holds, whose record decides it). A body the mirror
// holds (record) carries its issue record's card, read in the statement
// that read the body (bodyOf), with the time its status began (since):
// the view shows that card, and reads no other of the issue.
type ticketDoc struct {
	d       tracker.Detail
	version int64
	status  *store.MirrorStatusEvidence
	record  bool
	since   int64
}

// viewRow is connection id's row as its views read it (urlOf): the hold's
// while the connection's client is in use, else read.
func (s *Service) viewRow(id string) (store.TrackerConnection, error) {
	s.opMu.Lock()
	h := s.heldLocked(id)
	s.opMu.Unlock()
	if h != nil {
		return h.row, nil
	}
	return s.get(id)
}

// urlOf is the web address of c on connection row: the card's own
// (Linear), or its site and key (Jira, jiraprovider.IssueURL), so a key a
// project's rename changed shows in the address as in the card.
func urlOf(row store.TrackerConnection, c tracker.Card) string {
	if c.URL != "" || row.Kind != store.TrackerKindJira {
		return c.URL
	}
	return jiraprovider.IssueURL(row.Site, c.Key)
}

// boardIssueURL is BoardView.IssueURL for a board of connection row: the
// address urlOf gives a Jira card less its key, and none on Linear, whose
// cards carry their own.
func boardIssueURL(row store.TrackerConnection) string {
	if row.Kind != store.TrackerKindJira {
		return ""
	}
	return jiraprovider.IssueURLPrefix(row.Site)
}

// detailView is doc as a view of connection row, with the other issues
// it shows (detailViews).
func (s *Service) detailView(root int64, row store.TrackerConnection, doc ticketDoc, marked bool) (DetailView, []tracker.IssueID, error) {
	views, shows, err := s.detailViews(root, row, []ticketDoc{doc}, marked)
	if err != nil {
		return DetailView{}, nil, err
	}
	return views[0], shows[0], nil
}

// detailViews is each of docs as a view of connection row. The card is
// the issue record's when the mirror keeps the issue, with the time its
// status began as the record decided it: for a body the mirror holds, the
// one read with it (ticketDoc.record), which is not read again; else the
// document's own, with that time from the read's evidence
// (store.MirrorStatusSince). Its names
// are the mirror's shared catalog's and, for the IDs that leaves out (a
// comment's author no card names), its own catalog's; its parent's key
// and title, and each linked issue's key, title and status, from the
// mirror's card of it when the mirror keeps one (else from the detail).
// The parent is the shown card's, so a parent another read changed since
// the body's is looked up too. The cards of every document are read in
// one lookup (and the parents only the record names in a second), and
// then the names of every document in one, a store lookup's bound at a
// time; a document linking more than that bound shows the rest as it has
// them. Each view's rows on the boards are read with every document's
// (issueRows), and, when marked is set, each view's agent marks
// in one statement of its own (markAgentWrites). It returns each view with
// the other issues it shows (showsOf of the card it rendered), the
// dependencies its pump watches.
func (s *Service) detailViews(root int64, row store.TrackerConnection, docs []ticketDoc, marked bool) ([]DetailView, [][]tracker.IssueID, error) {
	wants := make([][]string, len(docs))
	var all idSet
	for i, doc := range docs {
		d := doc.d
		if !doc.record {
			wants[i] = append(wants[i], string(d.Card.ID))
		}
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
	lookup := func(ids []string) error {
		for batch := range slices.Chunk(ids, readBatch) {
			cards, err := s.store.ListMirrorCards(root, batch)
			if err != nil {
				return fmt.Errorf("trackerapp: read the cards %d tickets show from the mirror: %w", len(docs), err)
			}
			for _, m := range cards {
				mirrored[m.IssueID] = m
			}
		}
		return nil
	}
	if err := lookup(all.ids); err != nil {
		return nil, nil, err
	}
	shown := make([]shownDetail, len(docs))
	var later idSet
	for i, doc := range docs {
		// The parent shown is the shown card's (recordCard).
		p := string(doc.d.Card.Parent)
		if m, ok := recordCard(doc, mirrored); ok && !doc.record {
			p = m.ParentID
		}
		if p != "" && !slices.Contains(wants[i], p) {
			wants[i] = append(wants[i], p)
			if !all.seen[p] {
				later.add(p)
			}
		}
	}
	if len(later.ids) > 0 {
		if err := lookup(later.ids); err != nil {
			return nil, nil, err
		}
	}
	var users, statuses idSet
	shows := make([][]tracker.IssueID, len(docs))
	for i, doc := range docs {
		shown[i] = shownDetailOf(doc, wants[i], mirrored)
		shown[i].names(&users, &statuses)
		shows[i] = showsOf(shown[i].d)
	}
	names, err := s.names(root, users.ids, statuses.ids)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.ticketRows(root, docs, shown)
	if err != nil {
		return nil, nil, err
	}
	views := make([]DetailView, len(docs))
	for i := range docs {
		views[i] = shown[i].view(names, row)
		views[i].Rows = rows[docs[i].d.Card.ID]
		if views[i].Rows == nil {
			views[i].Rows = []TicketRowView{}
		}
		if marked {
			s.markAgentWrites(root, row, &views[i])
		}
	}
	if err := s.nameSprints(root, shown, views); err != nil {
		return nil, nil, err
	}
	return views, shows, nil
}

// nameSprints sets each view's sprints: those its shown card names, the
// active first, then the future, the closed and those no board of its
// rows holds, each kind in the card's order, named from the boards of its
// rows in one lookup of every view's (store.ListMirrorSprintNames), a
// lookup's bound of boards and of sprints at a time.
func (s *Service) nameSprints(root int64, shown []shownDetail, views []DetailView) error {
	var boards, sprints idSet
	for i, sd := range shown {
		for _, r := range views[i].Rows {
			boards.add(r.Board)
		}
		for _, sp := range sd.d.Card.Sprints {
			sprints.add(string(sp))
		}
	}
	named := map[string]store.MirrorSprintName{}
	if len(sprints.ids) == 0 {
		boards.ids = nil
	}
	for bs := range slices.Chunk(boards.ids, readBatch) {
		for ss := range slices.Chunk(sprints.ids, readBatch) {
			held, err := s.store.ListMirrorSprintNames(root, bs, ss)
			if err != nil {
				return fmt.Errorf("trackerapp: read the names of %d sprints from the mirror: %w", len(sprints.ids), err)
			}
			for _, sp := range held {
				named[sp.SprintID] = sp
			}
		}
	}
	order := map[int]int{store.MirrorSprintActive: 0, store.MirrorSprintFuture: 1, store.MirrorSprintClosed: 2}
	rank := func(id tracker.SprintID) int {
		if sp, ok := named[string(id)]; ok {
			return order[sp.State]
		}
		return len(order)
	}
	for i, sd := range shown {
		ids := slices.Clone(sd.d.Card.Sprints)
		slices.SortStableFunc(ids, func(a, b tracker.SprintID) int { return rank(a) - rank(b) })
		views[i].Sprints = make([]TicketSprintView, len(ids))
		for j, id := range ids {
			v := TicketSprintView{ID: string(id)}
			if sp, ok := named[string(id)]; ok {
				v.Name, v.State = sp.Name, nameOf(sprintNames[:], tracker.SprintState(sp.State))
			}
			views[i].Sprints[j] = v
		}
	}
	return nil
}

// ticketRows are the rows of docs' issues on the boards the mirror shows,
// by issue, in board order, each estimate as the shown document has it
// (shownDetail.estimates).
func (s *Service) ticketRows(root int64, docs []ticketDoc, shown []shownDetail) (map[tracker.IssueID][]TicketRowView, error) {
	ids := make([]string, len(docs))
	own := make(map[tracker.IssueID]*tracker.Detail, len(docs))
	for i, doc := range docs {
		ids[i] = string(doc.d.Card.ID)
		if shown[i].estimates() {
			own[doc.d.Card.ID] = &shown[i].d
		}
	}
	return s.issueRows(root, ids, own)
}

// issueRows are the rows of issues ids on the boards the mirror shows, by
// issue, in board order, read store.MirrorMaxBodies issues a statement:
// a caller may name any number of issues. Which boards an issue is on, and
// each board's unit, are the mirror's; the estimate is own's for the
// issues it holds, when its read carried the board's field
// (tracker.Detail.EstimateBy), and the mirror's row's otherwise.
func (s *Service) issueRows(root int64, ids []string, own map[tracker.IssueID]*tracker.Detail) (map[tracker.IssueID][]TicketRowView, error) {
	out := make(map[tracker.IssueID][]TicketRowView, len(ids))
	for batch := range slices.Chunk(ids, store.MirrorMaxBodies) {
		held, err := s.store.ListMirrorTicketRows(root, batch)
		if err != nil {
			return nil, fmt.Errorf("trackerapp: read the rows of %d tickets from the mirror: %w", len(ids), err)
		}
		for _, r := range held {
			id := tracker.IssueID(r.IssueID)
			unit := tracker.EstimateUnit(r.EstimateUnit)
			v := copyNumber(r.Estimate)
			if d := own[id]; d != nil {
				if e, carried := d.EstimateBy(tracker.Estimate{Unit: unit, Field: r.EstimateField}); carried {
					v = e
				}
			}
			out[id] = append(out[id], TicketRowView{Board: r.BoardID, Estimate: v, Unit: nameOf(unitNames[:], unit)})
		}
	}
	return out, nil
}

// shownDetail is a document with what the mirror's cards show of it: its
// card, the time its status began, its links named from the cards, its
// parent's card, and whether its body is of an older version than its
// card (behind).
type shownDetail struct {
	d      tracker.Detail
	since  int64
	behind bool
	links  []tracker.Link
	parent *tracker.Card
}

// recordCard is the issue record's card of doc when the view shows it:
// the mirror keeps one, of doc's version or later. It holds every field a
// card holds as the latest read of any path found it (a project key or a
// label renamed at an unchanged update time included), and the time its
// status began as the record decided it. A record card older than the
// document (a board read of an earlier answer stored the issue's first
// keep after a direct read of it) is not shown: the document's own card
// is, so a view never renders a card older than its document.
func recordCard(doc ticketDoc, mirrored map[string]store.MirrorCard) (store.MirrorCard, bool) {
	m, ok := mirrored[string(doc.d.Card.ID)]
	return m, ok && m.UpdatedAt >= doc.version
}

// shownDetailOf is doc shown from the cards of mirrored that want names:
// its own card (recordCard, else the document's), then the others it
// shows. The body is behind when the card shown is of a later version
// than the body.
func shownDetailOf(doc ticketDoc, want []string, mirrored map[string]store.MirrorCard) shownDetail {
	d := doc.d
	out := shownDetail{d: d, links: slices.Clone(d.Links)}
	switch m, ok := recordCard(doc, mirrored); {
	case doc.record:
		out.since = doc.since
	case ok:
		out.d.Card, _ = cardOf(m)
		out.since = m.StatusSince
	case doc.status != nil:
		out.since, _ = store.MirrorStatusSince(*doc.status)
	}
	cards := make(map[tracker.IssueID]tracker.Card, len(want))
	for _, id := range want {
		m, ok := mirrored[id]
		switch {
		case !ok, id == string(d.Card.ID):
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
	if p, ok := cards[out.d.Card.Parent]; ok && out.d.Card.Parent != "" {
		out.parent = &p
	}
	// Judged from the card rendered, wherever it came from: a body's own
	// card is the record's at its read (bodyOf), which may be newer than
	// the body when the mirror stopped keeping the issue since.
	out.behind = millis(out.d.Card.Updated) > doc.version
	return out
}

// estimates reports whether the view shows the document's own estimates:
// when the card it shows is of the body's version. A card a later read
// brought is newer than the body, whose estimate may be what that change
// replaced: the rows are then the mirror's, as the boards' reads left
// them, until the ticket is read again.
func (sd shownDetail) estimates() bool { return !sd.behind }

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

// view is the document's view on connection row, named from mirrored,
// the mirror's shared names, and its own catalog for the IDs mirrored
// lacks: a user or status renamed with no issue update shows its new name
// on the ticket as on the board.
func (sd shownDetail) view(mirrored catalogNames, row store.TrackerConnection) DetailView {
	d := sd.d
	names := mirrored
	if len(d.Users)+len(d.Statuses) > 0 {
		names = catalogNames{users: maps.Clone(mirrored.users), statuses: maps.Clone(mirrored.statuses)}
		for _, u := range d.Users {
			if _, ok := names.users[string(u.ID)]; !ok {
				names.users[string(u.ID)] = u
			}
		}
		for _, st := range d.Statuses {
			if _, ok := names.statuses[string(st.ID)]; !ok {
				names.statuses[string(st.ID)] = st
			}
		}
	}
	card := cardView(d.Card, names)
	card.StatusSince = sd.since
	if d.Card.Parent != "" {
		card.Parent = &ParentView{ID: string(d.Card.Parent)}
		if p := sd.parent; p != nil {
			card.Parent.Key, card.Parent.Title = p.Key, p.Title
		}
	}
	v := DetailView{
		Card: card, BodyBehind: sd.behind, URL: urlOf(row, d.Card), Description: d.Description, Fields: fieldViews(d.Fields),
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
		v.Moves = append(v.Moves, MoveView{ID: string(m.ID), Name: m.Name, To: names.status(string(m.To)), Fields: screenFieldViews(m.Fields)})
	}
	return v
}

func fieldViews(fields []tracker.Field) []FieldView {
	out := make([]FieldView, 0, len(fields))
	for _, f := range fields {
		out = append(out, fieldView(f))
	}
	return out
}

func fieldView(f tracker.Field) FieldView {
	return FieldView{
		ID: string(f.ID), Name: f.Name, Kind: f.Kind.String(), List: f.List, Required: f.Required, Editable: f.Editable,
		Value: fieldValueView(f.Value),
	}
}

// screenFieldViews are a create's or a move's screen fields, with the
// values a create screen lists.
func screenFieldViews(fields []tracker.ScreenField) []FieldView {
	out := make([]FieldView, 0, len(fields))
	for _, f := range fields {
		v := fieldView(f.Field)
		v.Defaulted = f.Defaulted
		for _, c := range f.Allowed {
			v.Allowed = append(v.Allowed, ChoiceView{ID: c.ID, Name: c.Name})
		}
		out = append(out, v)
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
