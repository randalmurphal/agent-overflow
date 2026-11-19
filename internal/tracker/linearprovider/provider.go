// Package linearprovider implements tracker.Provider for Linear over the
// linear client. A Linear team is the neutral project and its one board;
// workflow states are statuses and columns; cycles are sprints; an
// issue's sortOrder is its order key. The design is
// docs/architecture/tracker-backend.md.
//
// The package owns the mapping between Linear's records and the neutral
// model, the sync cursor's encoding, and how each write's answers become
// per-part outcomes. It owns no I/O of its own: every request goes
// through the caller's *linear.Client, which owns the transport, the
// documents and the rate limits. It keeps no state between calls.
package linearprovider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/linear"
	"agent-overflow/internal/tracker/trackerapi"
)

var _ tracker.Provider = (*Provider)(nil)

// Provider is one Linear workspace as a tracker.Provider. It holds only
// the client, which its caller owns and closes, and the clock History
// stamps CompleteTo with; it is safe for concurrent use.
type Provider struct {
	c   *linear.Client
	now func() time.Time
}

// New is a Provider over c. The caller keeps ownership of c.
func New(c *linear.Client) (*Provider, error) {
	if c == nil {
		return nil, errors.New("linearprovider: a client is required")
	}
	return &Provider{c: c, now: time.Now}, nil
}

// Capabilities are none of the optional ones: Linear has no issue types,
// no custom fields, no move screens, generates its cycles and holds links
// rather than files.
func (p *Provider) Capabilities() tracker.Capabilities { return tracker.Capabilities{} }

// Time is nil: Linear has no time tracking.
func (p *Provider) Time() tracker.TimeTracker { return nil }

// Identity is the key's user.
func (p *Provider) Identity(ctx context.Context) (tracker.User, error) {
	v, err := p.c.Viewer(ctx)
	if err != nil {
		return tracker.User{}, fmt.Errorf("linearprovider: identity: %w", err)
	}
	return user(v.User), nil
}

// user is a Linear user as the neutral model names one: the full name,
// or the display name when there is none.
func user(u linear.User) tracker.User {
	return tracker.User{ID: tracker.UserID(u.ID), Name: userName(u), Email: u.Email, AvatarURL: u.AvatarURL, Active: u.Active}
}

func userName(u linear.User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.DisplayName
}

// Projects are the teams the key can see.
func (p *Provider) Projects(ctx context.Context) ([]tracker.Project, error) {
	teams, err := p.c.Teams(ctx)
	if err != nil {
		return nil, fmt.Errorf("linearprovider: projects: %w", err)
	}
	out := make([]tracker.Project, 0, len(teams))
	for _, t := range teams {
		out = append(out, tracker.Project{ID: tracker.ProjectID(t.ID), Key: t.Key, Name: t.Name})
	}
	return out, nil
}

// IssueTypes is none: Linear issues have no type.
func (p *Provider) IssueTypes(context.Context, tracker.ProjectID) ([]tracker.IssueType, error) {
	return nil, nil
}

// Boards is the team's one board, whose ID is the team's; none for a
// team the key cannot see.
func (p *Provider) Boards(ctx context.Context, project tracker.ProjectID) ([]tracker.Board, error) {
	t, err := p.team(ctx, string(project))
	if errors.Is(err, trackerapi.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []tracker.Board{board(t)}, nil
}

func board(t linear.Team) tracker.Board {
	return tracker.Board{ID: tracker.BoardID(t.ID), Project: tracker.ProjectID(t.ID), Name: t.Name, Sprints: t.CyclesEnabled}
}

// team is one team the key can see, read with every other; one it cannot
// see matches trackerapi.ErrNotFound.
func (p *Provider) team(ctx context.Context, id string) (linear.Team, error) {
	if id == "" {
		return linear.Team{}, fmt.Errorf("linearprovider: %w: no team named", tracker.ErrInvalidInput)
	}
	teams, err := p.c.Teams(ctx)
	if err != nil {
		return linear.Team{}, fmt.Errorf("linearprovider: read team %s: %w", id, err)
	}
	i := slices.IndexFunc(teams, func(t linear.Team) bool { return t.ID == id })
	if i < 0 {
		return linear.Team{}, fmt.Errorf("linearprovider: team %s: %w", id, trackerapi.ErrNotFound)
	}
	return teams[i], nil
}

// The link types Linear can create, by its relation type. A relation read
// with another type (the deprecated "similar") is shown by linkType.
var linkTypes = []tracker.LinkType{
	{ID: tracker.LinkTypeID(linear.RelationBlocks), Name: "Blocks", Outward: "blocks", Inward: "is blocked by"},
	{ID: tracker.LinkTypeID(linear.RelationDuplicate), Name: "Duplicate", Outward: "duplicates", Inward: "is duplicated by"},
	{ID: tracker.LinkTypeID(linear.RelationRelated), Name: "Related", Outward: "relates to", Inward: "relates to"},
}

// LinkTypes are the relations Linear can create.
func (p *Provider) LinkTypes(context.Context) ([]tracker.LinkType, error) {
	return slices.Clone(linkTypes), nil
}

func linkType(t linear.RelationType) tracker.LinkType {
	for _, lt := range linkTypes {
		if lt.ID == tracker.LinkTypeID(t) {
			return lt
		}
	}
	name := string(t)
	return tracker.LinkType{ID: tracker.LinkTypeID(t), Name: name, Outward: name + " to", Inward: name + " to"}
}

// hasPrefixFold reports whether s starts with prefix, case ignored.
func hasPrefixFold(s, prefix string) bool {
	return strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix))
}
