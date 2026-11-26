package trackerapp

import (
	"fmt"
	"slices"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// The pane's reads of the mirror, as the DTOs the app binds. They read
// the store only, each bounded as the store's reads are, and never call a
// tracker, except IssueDetail (views_detail.go). Names come from the mirror's catalog; an ID it does not hold
// reads with no name. A read is several store reads, not one snapshot: a
// board's counts and pages may disagree by a sync that landed between
// them, and the next event carries the change.

// StatusView is a status with its name and group: backlog, todo,
// in_progress, done or canceled; both empty when the mirror does not hold
// it.
type StatusView struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Group string `json:"group,omitempty"`
}

// UserView is a user as a card or ticket names one.
type UserView struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// ChoiceView is an option, priority or label by ID with its name.
type ChoiceView struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// IssueTypeView is an issue's type; Level is -1 for a subtask, 0 for a
// standard issue and 1 for an epic.
type IssueTypeView struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Level int    `json:"level"`
}

// ParentView is an issue's parent (its epic), with its key and title when
// the mirror holds it.
type ParentView struct {
	ID    string `json:"id"`
	Key   string `json:"key,omitempty"`
	Title string `json:"title,omitempty"`
}

// CardView is an issue as a board or backlog row draws it. Times are Unix
// milliseconds, 0 for none; StatusSince is when the issue entered its
// current status, from its stored history, 0 while that is unknown. URL
// is the address the tracker gave the issue (Linear's), empty for a Jira
// card, whose address is its board's IssueURL with its key.
type CardView struct {
	ID       string         `json:"id"`
	Key      string         `json:"key"`
	Title    string         `json:"title"`
	URL      string         `json:"url,omitempty"`
	Type     *IssueTypeView `json:"type,omitempty"`
	Status   StatusView     `json:"status"`
	Assignee *UserView      `json:"assignee,omitempty"`
	Priority *ChoiceView    `json:"priority,omitempty"`
	Labels   []ChoiceView   `json:"labels,omitempty"`
	Parent   *ParentView    `json:"parent,omitempty"`
	Sprints  []string       `json:"sprints,omitempty"`
	Created  int64          `json:"created"`
	Updated  int64          `json:"updated"`
	Resolved int64          `json:"resolved,omitempty"`

	StatusSince int64 `json:"status_since,omitempty"`
}

// shownCard is a card as a view reads it from the mirror, with when its
// current status began (store MirrorCard.StatusSince).
type shownCard struct {
	card  tracker.Card
	since int64
}

// shownOf is m as a view shows it; ok is false when its blob is a miss.
func shownOf(m store.MirrorCard) (shownCard, bool) {
	c, ok := cardOf(m)
	return shownCard{card: c, since: m.StatusSince}, ok
}

// RowView is a card on one board, with that board's order key (compared
// bytewise, never read) and estimate in the board's unit.
type RowView struct {
	Card     CardView `json:"card"`
	Order    string   `json:"order"`
	Estimate *float64 `json:"estimate,omitempty"`
}

// readRoot is connection id's mirror root for a read, once the rows an
// earlier process left are deleted (dropEarlierTickets); ErrNotMirrored
// when it has none. The caller calls leave.
func (s *Service) readRoot(id string) (int64, error) {
	if err := s.enter(); err != nil {
		return 0, err
	}
	if err := s.dropEarlierTickets(); err != nil {
		s.leave()
		return 0, err
	}
	root, ok, err := s.store.GetMirrorRoot(id)
	switch {
	case err != nil:
		s.leave()
		return 0, fmt.Errorf("trackerapp: the mirror of connection %s: %w", id, err)
	case !ok:
		s.leave()
		return 0, fmt.Errorf("%w: connection %s has no mirror", ErrNotMirrored, id)
	}
	return root, nil
}

// IssueRowView is an issue's row on one board, without its card.
type IssueRowView struct {
	Board    string   `json:"board"`
	Order    string   `json:"order"`
	Estimate *float64 `json:"estimate,omitempty"`
}

// IssueView is one issue's card and the boards the mirror has it on.
// Stale is as SectionPage has it.
type IssueView struct {
	Card   CardView       `json:"card"`
	Boards []IssueRowView `json:"boards"`
	Stale  bool           `json:"stale,omitempty"`
}

// Issue is issue's card on connection id with its rows, a store page of
// rows at a time; ErrNotMirrored when the mirror does not keep the issue.
func (s *Service) Issue(id string, issue tracker.IssueID) (IssueView, error) {
	if issue == "" {
		return IssueView{}, fmt.Errorf("%w: no issue named", ErrInvalidRequest)
	}
	root, err := s.readRoot(id)
	if err != nil {
		return IssueView{}, err
	}
	defer s.leave()
	cards, err := s.store.ListMirrorCards(root, []string{string(issue)})
	if err != nil {
		return IssueView{}, fmt.Errorf("trackerapp: read issue %s from the mirror: %w", issue, err)
	}
	if len(cards) == 0 {
		return IssueView{}, fmt.Errorf("%w: issue %s", ErrNotMirrored, issue)
	}
	card, ok := shownOf(cards[0])
	views, _, err := s.cardViews(root, []shownCard{card}, nil)
	if err != nil {
		return IssueView{}, err
	}
	out := IssueView{Card: views[0], Boards: []IssueRowView{}, Stale: !ok}
	for after := ""; ; {
		rows, more, err := s.store.ListMirrorIssueRows(root, string(issue), after)
		if err != nil {
			return IssueView{}, fmt.Errorf("trackerapp: read the rows of issue %s: %w", issue, err)
		}
		for _, r := range rows {
			out.Boards = append(out.Boards, IssueRowView{Board: r.BoardID, Order: r.OrderKey, Estimate: copyNumber(r.Estimate)})
		}
		if !more || len(rows) == 0 {
			return out, nil
		}
		after = rows[len(rows)-1].BoardID
	}
}

// cardViews is cards as views, their users, statuses and parents named
// from the mirror, a store lookup's bound at a time, and the names it
// read, which name also the statuses also. At most the pages of one board
// view are asked for at once.
func (s *Service) cardViews(root int64, cards []shownCard, also []string) ([]CardView, catalogNames, error) {
	var users, statuses, parents idSet
	for _, id := range also {
		statuses.add(id)
	}
	for _, sc := range cards {
		c := sc.card
		users.add(string(c.Assignee))
		statuses.add(string(c.Status))
		parents.add(string(c.Parent))
	}
	names, err := s.names(root, users.ids, statuses.ids)
	if err != nil {
		return nil, catalogNames{}, err
	}
	parentOf := make(map[string]tracker.Card, len(parents.ids))
	for batch := range slices.Chunk(parents.ids, readBatch) {
		parentCards, err := s.store.ListMirrorCards(root, batch)
		if err != nil {
			return nil, catalogNames{}, fmt.Errorf("trackerapp: read the parents of %d cards: %w", len(cards), err)
		}
		for _, m := range parentCards {
			c, _ := cardOf(m)
			parentOf[m.IssueID] = c
		}
	}
	return cardViewsOf(cards, names, parentOf), names, nil
}

// namedCardViews is cards as views named by names, read with them, and
// the names as catalogNames.
func namedCardViews(cards []shownCard, names store.MirrorNames) ([]CardView, catalogNames) {
	catalog := catalogNamesOf(names.Catalog)
	parentOf := make(map[string]tracker.Card, len(names.Parents))
	for _, m := range names.Parents {
		c, _ := cardOf(m)
		parentOf[m.IssueID] = c
	}
	return cardViewsOf(cards, catalog, parentOf), catalog
}

// cardViewsOf is cards as views, named by names and parentOf.
func cardViewsOf(cards []shownCard, names catalogNames, parentOf map[string]tracker.Card) []CardView {
	out := make([]CardView, len(cards))
	for i, sc := range cards {
		c := sc.card
		v := cardView(c, names)
		v.StatusSince = sc.since
		if c.Parent != "" {
			p := parentOf[string(c.Parent)]
			v.Parent = &ParentView{ID: string(c.Parent), Key: p.Key, Title: p.Title}
		}
		out[i] = v
	}
	return out
}

func cardView(c tracker.Card, names catalogNames) CardView {
	v := CardView{
		ID: string(c.ID), Key: c.Key, Title: c.Title, URL: c.URL, Status: names.status(string(c.Status)),
		Created: millis(c.Created), Updated: millis(c.Updated), Resolved: millis(c.Resolved),
		Sprints: idStrings(c.Sprints),
	}
	if c.Type.ID != "" {
		v.Type = &IssueTypeView{ID: string(c.Type.ID), Name: c.Type.Name, Level: c.Type.Level}
	}
	if c.Assignee != "" {
		u := names.user(string(c.Assignee))
		v.Assignee = &u
	}
	if c.Priority.ID != "" {
		v.Priority = &ChoiceView{ID: c.Priority.ID, Name: c.Priority.Name}
	}
	for _, l := range c.Labels {
		v.Labels = append(v.Labels, ChoiceView(l))
	}
	return v
}

// idSet gathers distinct IDs in the order first seen, leaving the empty
// ID out.
type idSet struct {
	ids  []string
	seen map[string]bool
}

func (s *idSet) add(id string) {
	if id == "" || s.seen[id] {
		return
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[id] = true
	s.ids = append(s.ids, id)
}

// catalogNames are users and statuses by ID.
type catalogNames struct {
	users    map[string]tracker.User
	statuses map[string]tracker.Status
}

func (n catalogNames) user(id string) UserView {
	u := n.users[id]
	return UserView{ID: id, Name: u.Name, AvatarURL: u.AvatarURL}
}

func (n catalogNames) status(id string) StatusView {
	st, ok := n.statuses[id]
	if !ok {
		return StatusView{ID: id}
	}
	return StatusView{ID: id, Name: st.Name, Group: nameOf(groupNames[:], st.Group)}
}

// names reads users and statuses from the mirror's catalog, a store
// lookup's bound at a time.
func (s *Service) names(root int64, users, statuses []string) (catalogNames, error) {
	out := catalogNames{users: map[string]tracker.User{}, statuses: map[string]tracker.Status{}}
	for len(users)+len(statuses) > 0 {
		u, st := users[:min(len(users), readBatch)], statuses[:min(len(statuses), readBatch)]
		users, statuses = users[len(u):], statuses[len(st):]
		m, err := s.store.GetMirrorCatalog(root, u, st)
		if err != nil {
			return catalogNames{}, fmt.Errorf("trackerapp: read names from the mirror: %w", err)
		}
		out.add(m)
	}
	return out, nil
}

// catalogNamesOf is a store catalog as catalogNames.
func catalogNamesOf(m store.MirrorCatalog) catalogNames {
	out := catalogNames{users: map[string]tracker.User{}, statuses: map[string]tracker.Status{}}
	out.add(m)
	return out
}

// add adds the users and statuses of m.
func (n catalogNames) add(m store.MirrorCatalog) {
	c := catalogOf(m)
	for _, user := range c.Users {
		n.users[string(user.ID)] = user
	}
	for _, status := range c.Statuses {
		n.statuses[string(status.ID)] = status
	}
}
