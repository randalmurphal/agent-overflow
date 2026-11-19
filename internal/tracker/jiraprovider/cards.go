package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// The custom field types of Jira Software's sprint and rank fields, whose
// IDs differ by site.
const (
	sprintType = "com.pyxis.greenhopper.jira:gh-sprint"
	rankType   = "com.pyxis.greenhopper.jira:gh-lexo-rank"
)

// cardFieldIDs are the system fields a card reads, without the site's
// sprint field. The fields of the card a write takes by name are in
// outcome.go.
var cardFieldIDs = []string{
	jiraSummary, "status", jiraIssueType, "priority", "assignee", "labels",
	"parent", "project", "created", "updated", "resolutiondate",
}

// site is what the field catalog says about the fields a read needs.
type site struct {
	fields []jira.Field
	// sprint is the sprint field's ID; empty on a site without Jira
	// Software.
	sprint string
}

// site reads the field catalog, which the jira client caches.
func (p *Provider) site(ctx context.Context) (site, error) {
	fields, err := p.jira.Fields(ctx)
	if err != nil {
		return site{}, fmt.Errorf("jiraprovider: field catalog: %w", err)
	}
	s := site{fields: fields}
	for _, f := range fields {
		if f.Schema.Custom == sprintType {
			s.sprint = f.ID
			break
		}
	}
	return s, nil
}

// field is the catalog's entry for id.
func (s site) field(id string) (jira.Field, bool) {
	i := slices.IndexFunc(s.fields, func(f jira.Field) bool { return f.ID == id })
	if i < 0 {
		return jira.Field{}, false
	}
	return s.fields[i], true
}

// cardFields are the fields a card read asks for.
func (s site) cardFields() []string {
	out := slices.Clone(cardFieldIDs)
	if s.sprint != "" {
		out = append(out, s.sprint)
	}
	return out
}

// catalog collects the users and statuses a read's records name, each
// once, in the order first seen.
type catalog struct {
	users    map[tracker.UserID]bool
	statuses map[tracker.StatusID]bool
	out      tracker.Catalog
}

func newCatalog() *catalog {
	return &catalog{users: map[tracker.UserID]bool{}, statuses: map[tracker.StatusID]bool{}}
}

func (c *catalog) user(u *jira.User) {
	if u == nil || u.AccountID == "" || c.users[tracker.UserID(u.AccountID)] {
		return
	}
	c.users[tracker.UserID(u.AccountID)] = true
	c.out.Users = append(c.out.Users, userOf(*u))
}

// merge adds the users and statuses of other.
func (c *catalog) merge(other tracker.Catalog) {
	for _, u := range other.Users {
		if !c.users[u.ID] {
			c.users[u.ID] = true
			c.out.Users = append(c.out.Users, u)
		}
	}
	for _, s := range other.Statuses {
		if !c.statuses[s.ID] {
			c.statuses[s.ID] = true
			c.out.Statuses = append(c.out.Statuses, s)
		}
	}
}

func (c *catalog) status(s *jira.Status) {
	if s == nil || s.ID == "" || c.statuses[tracker.StatusID(s.ID)] {
		return
	}
	c.statuses[tracker.StatusID(s.ID)] = true
	c.out.Statuses = append(c.out.Statuses, statusOf(*s))
}

// statusOf is s in its group by Jira's status category: new is to do,
// indeterminate in progress, done done. A category Jira does not
// document (its "undefined" category included) counts as to do: the
// status is neither known started nor known finished, and project setup
// can regroup it. Jira has no category for backlog or canceled.
func statusOf(s jira.Status) tracker.Status {
	group := tracker.GroupToDo
	switch s.Category.Key {
	case "indeterminate":
		group = tracker.GroupInProgress
	case "done":
		group = tracker.GroupDone
	}
	return tracker.Status{ID: tracker.StatusID(s.ID), Name: s.Name, Group: group}
}

// cardOf is is as a card, adding the users and statuses it names to cat.
// A card field whose value did not decode, a missing status or a missing
// update time fails it: a card that silently lacks them would mislead the
// board and the delta.
func (s site) cardOf(is *jira.Issue, cat *catalog) (tracker.Card, error) {
	for _, pr := range is.Problems {
		if slices.Contains(cardFieldIDs, pr.Field) {
			return tracker.Card{}, fmt.Errorf("jiraprovider: issue %s: field %s: %w", is.Key, pr.Field, pr.Err)
		}
	}
	if is.Status == nil || is.Updated.IsZero() {
		return tracker.Card{}, fmt.Errorf("jiraprovider: issue %s came without its status or update time", is.Key)
	}
	c := tracker.Card{
		ID: tracker.IssueID(is.ID), Key: is.Key, Title: is.Summary,
		Status: tracker.StatusID(is.Status.ID), Created: is.Created, Updated: is.Updated,
	}
	cat.status(is.Status)
	if is.Project != nil {
		c.Project = tracker.ProjectID(is.Project.ID)
	}
	if is.Type != nil {
		c.Type = issueTypeOf(*is.Type)
	}
	if is.Assignee != nil {
		c.Assignee = tracker.UserID(is.Assignee.AccountID)
		cat.user(is.Assignee)
	}
	if is.Priority != nil {
		c.Priority = tracker.Choice{ID: is.Priority.ID, Name: is.Priority.Name}
	}
	for _, l := range is.Labels {
		c.Labels = append(c.Labels, tracker.Choice{ID: l, Name: l})
	}
	if is.Parent != nil {
		c.Parent = tracker.IssueID(is.Parent.ID)
	}
	resolved, err := is.Field("resolutiondate").DateTime()
	switch {
	case err == nil:
		c.Resolved = resolved
	case !errors.Is(err, jira.ErrNoValue):
		return tracker.Card{}, fmt.Errorf("jiraprovider: issue %s: resolution date: %w", is.Key, err)
	}
	if s.sprint != "" {
		sprints, err := is.Field(s.sprint).Sprints()
		if err != nil && !errors.Is(err, jira.ErrNoValue) {
			return tracker.Card{}, fmt.Errorf("jiraprovider: issue %s: sprints: %w", is.Key, err)
		}
		for _, sp := range sprints {
			c.Sprints = append(c.Sprints, tracker.SprintID(decimal(sp.ID)))
		}
	}
	return c, nil
}

// board is a board's configuration as a row read needs it: the rank
// field and the estimate field it reads each issue through.
type board struct {
	id   tracker.BoardID
	cfg  jira.BoardConfig
	rank string
}

// board reads id's configuration, which the jira client caches until
// Structure reads it again.
func (p *Provider) board(ctx context.Context, id tracker.BoardID) (board, error) {
	n, err := boardNumber(id)
	if err != nil {
		return board{}, err
	}
	cfg, err := p.jira.BoardConfig(ctx, n)
	if err != nil {
		return board{}, fmt.Errorf("jiraprovider: board %s: %w", id, err)
	}
	if cfg.RankFieldID <= 0 {
		return board{}, fmt.Errorf("jiraprovider: board %s names no rank field, so its order is not known", id)
	}
	return board{id: id, cfg: cfg, rank: "customfield_" + decimal(cfg.RankFieldID)}, nil
}

// estimated reports a board that estimates by a field.
func (b board) estimated() bool {
	return b.cfg.Estimation.Type == jira.EstimateField && b.cfg.Estimation.FieldID != ""
}

// rowFields are the fields a row read asks for: the card's, the rank
// field and the estimate field.
func (b board) rowFields(s site) []string {
	out := append(s.cardFields(), b.rank)
	if b.estimated() {
		out = append(out, b.cfg.Estimation.FieldID)
	}
	return out
}

// rowOf is is as a row of b: its card with b's order key and estimate.
func (b board) rowOf(s site, is *jira.Issue, cat *catalog) (tracker.Row, error) {
	card, err := s.cardOf(is, cat)
	if err != nil {
		return tracker.Row{}, err
	}
	row := tracker.Row{Card: card}
	order, err := is.Field(b.rank).Text()
	switch {
	case err == nil:
		row.Order = tracker.OrderKey(order)
	case !errors.Is(err, jira.ErrNoValue):
		return tracker.Row{}, fmt.Errorf("jiraprovider: issue %s: rank: %w", is.Key, err)
	}
	if b.estimated() {
		v, set, err := b.cfg.Estimate(is)
		if err != nil {
			return tracker.Row{}, fmt.Errorf("jiraprovider: issue %s: %w", is.Key, err)
		}
		if set {
			row.Estimate = &v
		}
	}
	return row, nil
}
