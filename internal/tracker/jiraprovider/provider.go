// Package jiraprovider is tracker.Provider for Jira Cloud, over the jira
// client, with logged time over the tempo client when the connection has
// one and over Jira's own worklogs otherwise. The design is
// docs/architecture/tracker-backend.md.
//
// The package turns neutral inputs into client calls and client answers
// into neutral values. It owns the rules that need both sides: how an
// issue becomes a card and a board row, the sync cursor, how a client
// error becomes a write's outcome (outcome.go), and which client steps a
// write's parts stand for. Jira's wire, its retries, its caches and every
// check a client makes stay in the clients.
//
// A Provider holds no state of its own beyond its configuration: no
// cache, goroutine, timer or file. The caches it relies on (users by
// account ID, the signed-in user, the field catalog, board configurations
// and sprint lists) are the jira client's, bounded and dropped there. The caller owns the clients and
// closes them; a Provider needs no release.
package jiraprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
	"agent-overflow/internal/tracker/tempo"
)

// Options configures a Provider. The caller owns the clients and closes
// them after the Provider's last call.
type Options struct {
	// Jira is the site's client. Required.
	Jira *jira.Client
	// Tempo is the site's Tempo client; nil uses Jira's own worklogs for
	// Time.
	Tempo *tempo.Client
	// Site is the site's address, such as https://example.atlassian.net,
	// which an issue's URL starts with. Required.
	Site string
}

// Provider is one Jira Cloud connection's tracker. It is safe for
// concurrent use.
type Provider struct {
	jira   *jira.Client
	origin string
	time   tracker.TimeTracker
}

var (
	_ tracker.Provider    = (*Provider)(nil)
	_ tracker.TimeTracker = (*tempoTime)(nil)
	_ tracker.TimeTracker = (*jiraTime)(nil)
)

// New builds a Provider. It refuses a missing Jira client and a Site that
// is not an http or https origin.
func New(opts Options) (*Provider, error) {
	if opts.Jira == nil {
		return nil, errors.New("jiraprovider: Options.Jira is required")
	}
	origin, err := siteOrigin(opts.Site)
	if err != nil {
		return nil, err
	}
	p := &Provider{jira: opts.Jira, origin: origin}
	if opts.Tempo != nil {
		p.time = &tempoTime{jira: opts.Jira, tempo: opts.Tempo}
	} else {
		p.time = &jiraTime{jira: opts.Jira}
	}
	return p, nil
}

// siteOrigin is s as scheme and host, with no path, query or fragment.
func siteOrigin(s string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(s, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("jiraprovider: Options.Site %q is not a site address such as https://example.atlassian.net", s)
	}
	return u.Scheme + "://" + u.Host, nil
}

// Capabilities are Jira's: every capability the neutral model names.
// DetailMovesUpdated is measured: a comment, a link, an attachment and a
// worklog each move an issue's `updated`
// (docs/references/tracker-api-measurements.md, Jira Cloud).
func (p *Provider) Capabilities() tracker.Capabilities {
	return tracker.Capabilities{IssueTypes: true, CustomFields: true, MoveFields: true, SprintWrites: true, Attachments: true,
		DetailMovesUpdated: true, HistorySettle: historySettle}
}

// Time is Tempo's worklogs when the Provider was built with a Tempo
// client, and Jira's own otherwise.
func (p *Provider) Time() tracker.TimeTracker { return p.time }

// Identity is the user the credential belongs to.
func (p *Provider) Identity(ctx context.Context) (tracker.User, error) {
	u, err := p.jira.Myself(ctx)
	if err != nil {
		return tracker.User{}, fmt.Errorf("jiraprovider: identity: %w", err)
	}
	return userOf(u), nil
}

// Projects are the projects the user can browse.
func (p *Provider) Projects(ctx context.Context) ([]tracker.Project, error) {
	list, err := p.jira.Projects(ctx, jira.ProjectQuery{})
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: projects: %w", err)
	}
	out := make([]tracker.Project, len(list.Projects))
	for i, pr := range list.Projects {
		out[i] = tracker.Project{ID: tracker.ProjectID(pr.ID), Key: pr.Key, Name: pr.Name, ChoosesAudience: pr.ServiceDesk()}
	}
	return out, nil
}

// IssueTypes are the types the user can create in project.
func (p *Provider) IssueTypes(ctx context.Context, project tracker.ProjectID) ([]tracker.IssueType, error) {
	if err := checkProject(project); err != nil {
		return nil, err
	}
	types, err := p.jira.CreateIssueTypes(ctx, string(project))
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: issue types of %s: %w", project, err)
	}
	out := make([]tracker.IssueType, len(types))
	for i, t := range types {
		out[i] = issueTypeOf(t)
	}
	return out, nil
}

// Boards are the boards whose filter names project. A board's Project is
// the project Jira locates it in, empty for a board in a user's location.
func (p *Provider) Boards(ctx context.Context, project tracker.ProjectID) ([]tracker.Board, error) {
	if err := checkProject(project); err != nil {
		return nil, err
	}
	list, err := p.jira.Boards(ctx, jira.BoardQuery{Project: string(project)})
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: boards of %s: %w", project, err)
	}
	out := make([]tracker.Board, len(list.Boards))
	for i, b := range list.Boards {
		out[i] = boardOf(b)
	}
	return out, nil
}

// LinkTypes are the site's issue link types.
func (p *Provider) LinkTypes(ctx context.Context) ([]tracker.LinkType, error) {
	types, err := p.jira.IssueLinkTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("jiraprovider: link types: %w", err)
	}
	out := make([]tracker.LinkType, len(types))
	for i, t := range types {
		out[i] = linkTypeOf(t)
	}
	return out, nil
}

func userOf(u jira.User) tracker.User {
	return tracker.User{ID: tracker.UserID(u.AccountID), Name: u.DisplayName, Email: u.Email, AvatarURL: u.AvatarURL, Active: u.Active}
}

func issueTypeOf(t jira.IssueType) tracker.IssueType {
	return tracker.IssueType{ID: tracker.IssueTypeID(t.ID), Name: t.Name, Level: t.HierarchyLevel}
}

func boardOf(b jira.Board) tracker.Board {
	out := tracker.Board{ID: tracker.BoardID(decimal(b.ID)), Name: b.Name, Sprints: b.Type == jira.BoardScrum}
	if b.Location.ProjectID > 0 {
		out.Project = tracker.ProjectID(decimal(b.Location.ProjectID))
	}
	return out
}

func linkTypeOf(t jira.LinkType) tracker.LinkType {
	return tracker.LinkType{ID: tracker.LinkTypeID(t.ID), Name: t.Name, Outward: t.Outward, Inward: t.Inward}
}
