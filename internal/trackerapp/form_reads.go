package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"io"

	"agent-overflow/internal/tracker"
)

// The reads a write form needs. Each goes to the tracker: the mirror holds
// none of them. Projects, issue types, boards, link types and the fields a
// create takes are configuration the mirror never reads; the moves an
// issue allows come from its workflow, which moves no update time, so a
// stored detail's moves are stale whenever the workflow changed; pick
// lists, lookups and searches answer typed text against the whole tracker;
// later comment pages and attachments are not stored. Each read holds the
// connection as a write does, is interactive, and settles the connection's
// problem with its outcome (ask). A tracker failure is returned with the
// transport's typed error reachable through errors.As; ClassifyFailure
// sorts it.

// ask runs read, one provider read and what it reads of the mirror, on a
// hold of connection id for a person, and settles the connection's
// problem with the provider read's outcome: its failure, which read marks
// with call, or a success. An input the provider refused before any
// request, a read the tracker cannot serve, and a read whose context ended
// leave the problem as it is. A failure is returned unmarked.
func ask[T any](s *Service, ctx context.Context, id string, read func(r *run) (T, error)) (T, error) {
	r, err := s.interact(ctx, id)
	if err != nil {
		var zero T
		return zero, err
	}
	defer r.finish()
	v, err := read(r)
	c, failed := errors.AsType[*callError](err)
	var callErr error
	if failed {
		callErr, err = c.err, c.err
	}
	switch {
	case errors.Is(callErr, tracker.ErrInvalidInput), errors.Is(callErr, tracker.ErrUnsupported), r.ctx.Err() != nil:
	default:
		r.calls++
		// A settlement Stop overtook changes nothing: the read's outcome
		// stands.
		_ = r.settleCalls(callErr)
	}
	return v, err
}

// ProjectView is a tracker project: a Jira project or a Linear team.
// ChoosesAudience is a project whose new comments say who they reach.
type ProjectView struct {
	ID              string `json:"id"`
	Key             string `json:"key"`
	Name            string `json:"name"`
	ChoosesAudience bool   `json:"chooses_audience,omitempty"`
}

// Projects are the projects the connection's user can see.
func (s *Service) Projects(ctx context.Context, id string) ([]ProjectView, error) {
	return ask(s, ctx, id, func(r *run) ([]ProjectView, error) {
		ps, err := r.p.Projects(r.ctx)
		out := make([]ProjectView, len(ps))
		for i, p := range ps {
			out[i] = ProjectView{ID: string(p.ID), Key: p.Key, Name: p.Name, ChoosesAudience: p.ChoosesAudience}
		}
		return out, call(err)
	})
}

// IssueTypes are the types an issue can be created with in project; none
// on a tracker without issue types.
func (s *Service) IssueTypes(ctx context.Context, id, project string) ([]IssueTypeView, error) {
	return ask(s, ctx, id, func(r *run) ([]IssueTypeView, error) {
		ts, err := r.p.IssueTypes(r.ctx, tracker.ProjectID(project))
		out := make([]IssueTypeView, len(ts))
		for i, t := range ts {
			out[i] = IssueTypeView{ID: string(t.ID), Name: t.Name, Level: t.Level}
		}
		return out, call(err)
	})
}

// BoardInfoView is a board a project has, for picking the boards it
// lists.
type BoardInfoView struct {
	ID           string `json:"id"`
	Project      string `json:"project,omitempty"`
	Name         string `json:"name"`
	PlansSprints bool   `json:"plans_sprints"`
}

// Boards are project's boards.
func (s *Service) Boards(ctx context.Context, id, project string) ([]BoardInfoView, error) {
	return ask(s, ctx, id, func(r *run) ([]BoardInfoView, error) {
		bs, err := r.p.Boards(r.ctx, tracker.ProjectID(project))
		out := make([]BoardInfoView, len(bs))
		for i, b := range bs {
			out[i] = BoardInfoView{ID: string(b.ID), Project: string(b.Project), Name: b.Name, PlansSprints: b.Sprints}
		}
		return out, call(err)
	})
}

// CreateFields are the fields a create in project with issueType takes
// besides the title, those it requires marked required.
func (s *Service) CreateFields(ctx context.Context, id, project, issueType string) ([]FieldView, error) {
	return ask(s, ctx, id, func(r *run) ([]FieldView, error) {
		fs, err := r.p.CreateFields(r.ctx, tracker.ProjectID(project), tracker.IssueTypeID(issueType))
		return fieldViews(fs), call(err)
	})
}

// Moves are the moves issue's workflow allows now, with the fields each
// takes; the statuses they lead to are named from the mirror.
func (s *Service) Moves(ctx context.Context, id, issue string) ([]MoveView, error) {
	return ask(s, ctx, id, func(r *run) ([]MoveView, error) {
		ms, err := r.p.Moves(r.ctx, tracker.IssueID(issue))
		if err != nil {
			return nil, call(err)
		}
		var statuses idSet
		for _, m := range ms {
			statuses.add(string(m.To))
		}
		names, err := s.namesOf(r.root, tracker.Catalog{}, nil, statuses.ids)
		if err != nil {
			return nil, err
		}
		out := make([]MoveView, len(ms))
		for i, m := range ms {
			out[i] = MoveView{ID: string(m.ID), Name: m.Name, To: names.status(string(m.To)), Fields: fieldViews(m.Fields)}
		}
		return out, nil
	})
}

// FieldScopeInput is where a pick list's values are asked for: an existing
// Issue's fields, or a create's in Project with Type.
type FieldScopeInput struct {
	Issue   string `json:"issue,omitempty"`
	Project string `json:"project,omitempty"`
	Type    string `json:"type,omitempty"`
}

func (in FieldScopeInput) model() (tracker.FieldScope, error) {
	switch {
	case in.Issue != "" && in.Project == "" && in.Type == "":
		return tracker.OnIssue(tracker.IssueID(in.Issue)), nil
	case in.Issue == "" && in.Project != "":
		return tracker.OnCreate(tracker.ProjectID(in.Project), tracker.IssueTypeID(in.Type)), nil
	}
	return tracker.FieldScope{}, fmt.Errorf("%w: a field scope names an issue, or a project and its issue type", ErrInvalidRequest)
}

// UserListView is the users a pick list offers; More reports that the
// tracker may hold matches it leaves out, so a user missing from it may
// still exist.
type UserListView struct {
	Users []UserView `json:"users"`
	More  bool       `json:"more,omitempty"`
}

// AssignableUsers are the users who can be assigned in scope whose name or
// email starts with text.
func (s *Service) AssignableUsers(ctx context.Context, id string, scope FieldScopeInput, text string) (UserListView, error) {
	sc, err := scope.model()
	if err != nil {
		return UserListView{}, err
	}
	return ask(s, ctx, id, func(r *run) (UserListView, error) {
		l, err := r.p.AssignableUsers(r.ctx, sc, text)
		out := UserListView{Users: make([]UserView, len(l.Users)), More: l.More}
		for i, u := range l.Users {
			out.Users[i] = UserView{ID: string(u.ID), Name: u.Name, AvatarURL: u.AvatarURL}
		}
		return out, call(err)
	})
}

// ChoiceListView is the values a pick list offers, with More as
// UserListView has it.
type ChoiceListView struct {
	Choices []ChoiceView `json:"choices"`
	More    bool         `json:"more,omitempty"`
}

// Choices are the values field takes in scope whose name starts with
// text.
func (s *Service) Choices(ctx context.Context, id string, scope FieldScopeInput, field, text string) (ChoiceListView, error) {
	sc, err := scope.model()
	if err != nil {
		return ChoiceListView{}, err
	}
	return ask(s, ctx, id, func(r *run) (ChoiceListView, error) {
		l, err := r.p.Choices(r.ctx, sc, tracker.FieldID(field), text)
		out := ChoiceListView{Choices: make([]ChoiceView, len(l.Choices)), More: l.More}
		for i, c := range l.Choices {
			out.Choices[i] = ChoiceView(c)
		}
		return out, call(err)
	})
}

// LinkTypeView is a kind of link: the outward issue relates to the inward
// one by Outward ("blocks"), the inward to the outward by Inward.
type LinkTypeView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Outward string `json:"outward"`
	Inward  string `json:"inward"`
}

// LinkTypes are the kinds of link the tracker offers.
func (s *Service) LinkTypes(ctx context.Context, id string) ([]LinkTypeView, error) {
	return ask(s, ctx, id, func(r *run) ([]LinkTypeView, error) {
		ts, err := r.p.LinkTypes(r.ctx)
		out := make([]LinkTypeView, len(ts))
		for i, t := range ts {
			out[i] = LinkTypeView{ID: string(t.ID), Name: t.Name, Outward: t.Outward, Inward: t.Inward}
		}
		return out, call(err)
	})
}

// KeyMatchView is a key and the issue it names; Card.Key differs from Key
// when the issue had that key before it moved.
type KeyMatchView struct {
	Key  string   `json:"key"`
	Card CardView `json:"card"`
}

// LookupView is what a lookup found: a match for each key that names an
// issue the user can see, in the order asked, and the other keys.
type LookupView struct {
	Found     []KeyMatchView `json:"found"`
	Unmatched []string       `json:"unmatched,omitempty"`
}

// Lookup finds the issues keys name, at most tracker.MaxLookupKeys.
func (s *Service) Lookup(ctx context.Context, id string, keys []string) (LookupView, error) {
	return ask(s, ctx, id, func(r *run) (LookupView, error) {
		l, err := r.p.Lookup(r.ctx, keys)
		if err != nil {
			return LookupView{}, call(err)
		}
		cards := make([]tracker.Card, len(l.Found))
		for i, m := range l.Found {
			cards[i] = m.Card
		}
		views, err := s.foundCards(r.root, cards, l.Catalog)
		out := LookupView{Found: make([]KeyMatchView, len(l.Found)), Unmatched: l.Unmatched}
		for i, m := range l.Found {
			out.Found[i] = KeyMatchView{Key: m.Key, Card: views[i]}
		}
		return out, err
	})
}

// SearchRequest is a search for issues by free text, optionally within
// one project; Page is the token the previous page returned, empty for the
// first, and Limit the most cards a page holds (0: the most there are).
type SearchRequest struct {
	Text    string `json:"text,omitempty"`
	Project string `json:"project,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Page    string `json:"page,omitempty"`
}

// SearchPageView is a page of the cards a search matched, in the order the
// tracker ranks them; Next is empty on the last page.
type SearchPageView struct {
	Cards []CardView `json:"cards"`
	Next  string     `json:"next,omitempty"`
}

// Search is one page of the cards in's text matches.
func (s *Service) Search(ctx context.Context, id string, in SearchRequest) (SearchPageView, error) {
	return ask(s, ctx, id, func(r *run) (SearchPageView, error) {
		page, err := r.p.Search(r.ctx, tracker.SearchQuery{Text: in.Text, Project: tracker.ProjectID(in.Project), Limit: in.Limit, Page: tracker.PageToken(in.Page)})
		if err != nil {
			return SearchPageView{}, call(err)
		}
		views, err := s.foundCards(r.root, page.Cards, page.Catalog)
		return SearchPageView{Cards: views, Next: string(page.Next)}, err
	})
}

// CommentPageView is a page of an issue's comments, newest first; Next is
// empty on the last page.
type CommentPageView struct {
	Comments []CommentView `json:"comments"`
	Next     string        `json:"next,omitempty"`
}

// Comments is the page of issue's comments that page, the token a
// ticket's MoreComments or the previous page gave, names.
func (s *Service) Comments(ctx context.Context, id, issue, page string) (CommentPageView, error) {
	return ask(s, ctx, id, func(r *run) (CommentPageView, error) {
		p, err := r.p.Comments(r.ctx, tracker.IssueID(issue), tracker.PageToken(page))
		if err != nil {
			return CommentPageView{}, call(err)
		}
		var users idSet
		for _, c := range p.Comments {
			users.add(string(c.Author))
		}
		names, err := s.namesOf(r.root, p.Catalog, users.ids, nil)
		out := CommentPageView{Comments: make([]CommentView, len(p.Comments)), Next: string(p.Next)}
		for i, c := range p.Comments {
			out.Comments[i] = CommentView{ID: string(c.ID), Author: names.user(string(c.Author)), Body: c.Body,
				Created: millis(c.Created), Updated: millis(c.Updated), Internal: c.Internal}
		}
		return out, err
	})
}

// Download streams one of issue's attachments to to, at most the largest
// file the tracker stores (tracker.Provider.Download). to may hold part
// of the file after a failure, which the caller discards.
func (s *Service) Download(ctx context.Context, id, issue, attachment string, to io.Writer) error {
	_, err := ask(s, ctx, id, func(r *run) (struct{}, error) {
		return struct{}{}, call(r.p.Download(r.ctx, tracker.DownloadInput{Issue: tracker.IssueID(issue),
			Attachment: tracker.AttachmentID(attachment), To: to}))
	})
	return err
}

// foundCards are cards a lookup or search found as views, named from
// catalog and, for what it leaves out, from root's names. A card's parent
// is named by ID.
func (s *Service) foundCards(root int64, cards []tracker.Card, catalog tracker.Catalog) ([]CardView, error) {
	var users, statuses idSet
	for _, c := range cards {
		users.add(string(c.Assignee))
		statuses.add(string(c.Status))
	}
	names, err := s.namesOf(root, catalog, users.ids, statuses.ids)
	out := make([]CardView, len(cards))
	for i, c := range cards {
		out[i] = cardView(c, names)
		if c.Parent != "" {
			out[i].Parent = &ParentView{ID: string(c.Parent)}
		}
	}
	return out, err
}

// namesOf are the users and statuses of catalog and, of users and
// statuses, those it leaves out as root's names hold them: no store read
// when catalog names them all.
func (s *Service) namesOf(root int64, catalog tracker.Catalog, users, statuses []string) (catalogNames, error) {
	have := catalogNames{users: map[string]tracker.User{}, statuses: map[string]tracker.Status{}}
	for _, u := range catalog.Users {
		have.users[string(u.ID)] = u
	}
	for _, st := range catalog.Statuses {
		have.statuses[string(st.ID)] = st
	}
	var missingUsers, missingStatuses []string
	for _, u := range users {
		if _, ok := have.users[u]; !ok {
			missingUsers = append(missingUsers, u)
		}
	}
	for _, st := range statuses {
		if _, ok := have.statuses[st]; !ok {
			missingStatuses = append(missingStatuses, st)
		}
	}
	if len(missingUsers)+len(missingStatuses) == 0 {
		return have, nil
	}
	mirrored, err := s.names(root, missingUsers, missingStatuses)
	if err != nil {
		return have, err
	}
	for id, u := range mirrored.users {
		have.users[id] = u
	}
	for id, st := range mirrored.statuses {
		have.statuses[id] = st
	}
	return have, nil
}
