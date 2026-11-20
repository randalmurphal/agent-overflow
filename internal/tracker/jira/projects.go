package jira

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"agent-overflow/internal/tracker/trackerapi"
)

// Project is a Jira project. IssueTypes is filled by Project, not by
// Projects or an issue's project field.
type Project struct {
	ID   string
	Key  string
	Name string
	// TypeKey is software, service_desk, business or product_discovery.
	TypeKey    string
	Simplified bool // team-managed
	Archived   bool
	IssueTypes []IssueType
}

// ServiceDesk reports a Jira Service Management project, where a comment
// is internal or reaches the customer.
func (p Project) ServiceDesk() bool { return p.TypeKey == "service_desk" }

type projectJSON struct {
	ID         string          `json:"id"`
	Key        string          `json:"key"`
	Name       string          `json:"name"`
	TypeKey    string          `json:"projectTypeKey"`
	Simplified bool            `json:"simplified"`
	Archived   bool            `json:"archived"`
	IssueTypes []issueTypeJSON `json:"issueTypes"`
}

func (p projectJSON) project() Project {
	out := Project{ID: p.ID, Key: p.Key, Name: p.Name, TypeKey: p.TypeKey, Simplified: p.Simplified, Archived: p.Archived}
	for _, t := range p.IssueTypes {
		out.IssueTypes = append(out.IssueTypes, t.issueType())
	}
	return out
}

// IssueType is an issue type. HierarchyLevel is -1 for a subtask, 0 for
// a standard type and 1 for an epic.
type IssueType struct {
	ID             string
	Name           string
	Description    string
	Subtask        bool
	HierarchyLevel int
}

type issueTypeJSON struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Subtask        bool   `json:"subtask"`
	HierarchyLevel int    `json:"hierarchyLevel"`
}

func (t issueTypeJSON) issueType() IssueType {
	return IssueType(t)
}

// Status is a workflow status with its category.
type Status struct {
	ID       string
	Name     string
	Category StatusCategory
}

// StatusCategory groups statuses: Key is new, indeterminate or done.
type StatusCategory struct {
	ID        int
	Key       string
	Name      string
	ColorName string
}

type statusJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category struct {
		ID        int    `json:"id"`
		Key       string `json:"key"`
		Name      string `json:"name"`
		ColorName string `json:"colorName"`
	} `json:"statusCategory"`
}

func (s statusJSON) status() Status {
	return Status{ID: s.ID, Name: s.Name, Category: StatusCategory(s.Category)}
}

// Priority is an issue priority.
type Priority struct {
	ID    string
	Name  string
	Color string
}

type priorityJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"statusColor"`
}

// LinkType is an issue link type: Outward is how the outward issue
// relates ("blocks"), Inward how the inward one does ("is blocked by").
type LinkType struct {
	ID      string
	Name    string
	Inward  string
	Outward string
}

// ProjectQuery filters Projects.
type ProjectQuery struct {
	// Query matches a project's key or name, case ignored.
	Query string
	// Limit bounds the projects returned; zero means all.
	Limit int
}

// ProjectList is the projects a listing returned. Complete is false when
// q.Limit stopped it before the last project.
type ProjectList struct {
	Projects []Project
	Complete bool
}

// Projects returns the projects the user can browse, reading every page
// up to q.Limit. A failure after some pages returns them with the error.
func (c *Client) Projects(ctx context.Context, q ProjectQuery) (ProjectList, error) {
	if err := c.admit(ctx); err != nil {
		return ProjectList{}, err
	}
	if q.Limit < 0 {
		return ProjectList{}, notSent("list projects: a negative limit")
	}
	v := url.Values{"orderBy": {"key"}}
	if q.Query != "" {
		v.Set("query", q.Query)
	}
	found, complete, _, err := offsetPages[projectJSON](ctx, c, apiPath+"project/search", v, 100, q.Limit)
	out := ProjectList{Projects: make([]Project, len(found)), Complete: complete}
	for i, p := range found {
		out.Projects[i] = p.project()
	}
	if err != nil {
		return out, fmt.Errorf("jira: list projects: %w", err)
	}
	return out, nil
}

// Project returns a project by key or ID, with its issue types.
func (c *Client) Project(ctx context.Context, ref string) (Project, error) {
	if err := c.admit(ctx); err != nil {
		return Project{}, err
	}
	if err := checkProject(ref); err != nil {
		return Project{}, err
	}
	var p projectJSON
	if err := c.get(ctx, "project/"+url.PathEscape(ref), url.Values{"expand": {"issueTypes"}}, &p); err != nil {
		return Project{}, fmt.Errorf("jira: project %s: %w", ref, err)
	}
	return p.project(), nil
}

// TypeStatuses are the statuses one issue type of a project moves through.
type TypeStatuses struct {
	IssueType IssueType
	Statuses  []Status
}

// ProjectStatuses returns a project's statuses for each of its issue types.
func (c *Client) ProjectStatuses(ctx context.Context, project string) ([]TypeStatuses, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if err := checkProject(project); err != nil {
		return nil, err
	}
	var found []struct {
		issueTypeJSON
		Statuses []statusJSON `json:"statuses"`
	}
	if err := c.get(ctx, "project/"+url.PathEscape(project)+"/statuses", nil, &found); err != nil {
		return nil, fmt.Errorf("jira: statuses of %s: %w", project, err)
	}
	out := make([]TypeStatuses, len(found))
	for i, t := range found {
		out[i].IssueType = t.issueType()
		for _, s := range t.Statuses {
			out[i].Statuses = append(out[i].Statuses, s.status())
		}
	}
	return out, nil
}

// Statuses returns the site's statuses that are in an active workflow of
// a project the user can browse, in one unpaged read. Jira's bulk read
// by ID (GET statuses) needs project administration, so this is the
// read that names a status outside any one project.
func (c *Client) Statuses(ctx context.Context) ([]Status, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	var found []statusJSON
	if err := c.get(ctx, "status", nil, &found); err != nil {
		return nil, fmt.Errorf("jira: list statuses: %w", err)
	}
	out := make([]Status, len(found))
	for i, s := range found {
		out[i] = s.status()
	}
	return out, nil
}

// Priorities returns the site's priorities, reading every page.
func (c *Client) Priorities(ctx context.Context) ([]Priority, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	found, _, _, err := offsetPages[priorityJSON](ctx, c, apiPath+"priority/search", url.Values{}, 100, 0)
	out := make([]Priority, len(found))
	for i, p := range found {
		out[i] = Priority(p)
	}
	if err != nil {
		return out, fmt.Errorf("jira: list priorities: %w", err)
	}
	return out, nil
}

// IssueLinkTypes returns the site's issue link types.
func (c *Client) IssueLinkTypes(ctx context.Context) ([]LinkType, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	var v struct {
		Types []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Inward  string `json:"inward"`
			Outward string `json:"outward"`
		} `json:"issueLinkTypes"`
	}
	if err := c.get(ctx, "issueLinkType", nil, &v); err != nil {
		return nil, fmt.Errorf("jira: list issue link types: %w", err)
	}
	out := make([]LinkType, len(v.Types))
	for i, t := range v.Types {
		out[i] = LinkType(t)
	}
	return out, nil
}

// offsetPages reads path's pages (a path from the site root, under any
// of the APIs that page by offset) of size from the start until the last
// one, or until limit items when limit is above zero; complete reports
// that the last page was read with every item kept, and date is the first
// answer's (answerDate). A failure returns the items read before it.
func offsetPages[T any](ctx context.Context, c *Client, path string, q url.Values, size, limit int) (out []T, complete bool, date time.Time, err error) {
	for start := 0; ; {
		q.Set("startAt", strconv.Itoa(start))
		q.Set("maxResults", strconv.Itoa(size))
		var page struct {
			Values []T   `json:"values"`
			IsLast bool  `json:"isLast"`
			Total  int64 `json:"total"`
		}
		resp, err := c.call(ctx, trackerapi.Request{Path: path, Query: q}, &page)
		if err != nil {
			return out, false, date, err
		}
		if start == 0 {
			date = answerDate(resp)
		}
		next := start + len(page.Values)
		last := page.IsLast || (page.Total > 0 && int64(next) >= page.Total)
		for _, item := range page.Values {
			if limit > 0 && len(out) == limit {
				return out, false, date, nil // items past the limit
			}
			out = append(out, item)
		}
		switch {
		case last:
			return out, true, date, nil
		case limit > 0 && len(out) == limit:
			return out, false, date, nil
		}
		if len(page.Values) == 0 {
			return out, false, date, fmt.Errorf("an empty page at %d that is not the last", start)
		}
		start = next
	}
}
