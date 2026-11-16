package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
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

// Projects returns the projects the user can browse, reading every page
// up to q.Limit. A failure after some pages returns them with the error.
func (c *Client) Projects(ctx context.Context, q ProjectQuery) ([]Project, error) {
	v := url.Values{"orderBy": {"key"}}
	if q.Query != "" {
		v.Set("query", q.Query)
	}
	found, err := offsetPages[projectJSON](ctx, c, "project/search", v, 100, q.Limit)
	out := make([]Project, len(found))
	for i, p := range found {
		out[i] = p.project()
	}
	if err != nil {
		return out, fmt.Errorf("jira: list projects: %w", err)
	}
	return out, nil
}

// Project returns a project by key or ID, with its issue types.
func (c *Client) Project(ctx context.Context, ref string) (Project, error) {
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

// Priorities returns the site's priorities, reading every page.
func (c *Client) Priorities(ctx context.Context) ([]Priority, error) {
	found, err := offsetPages[priorityJSON](ctx, c, "priority/search", url.Values{}, 100, 0)
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

// offsetPage is a page of the platform's offset paging.
type offsetPage struct {
	Values  []json.RawMessage `json:"values"`
	IsLast  bool              `json:"isLast"`
	StartAt int64             `json:"startAt"`
	Total   int64             `json:"total"`
}

// offsetPages reads path's pages of size from the start until the last
// one, or until limit items when limit is above zero. A failure returns
// the items read before it.
func offsetPages[T any](ctx context.Context, c *Client, path string, q url.Values, size, limit int) ([]T, error) {
	var out []T
	for start := 0; ; {
		q.Set("startAt", strconv.Itoa(start))
		q.Set("maxResults", strconv.Itoa(size))
		var page offsetPage
		if err := c.get(ctx, path, q, &page); err != nil {
			return out, err
		}
		for _, raw := range page.Values {
			var item T
			if err := json.Unmarshal(raw, &item); err != nil {
				return out, fmt.Errorf("decode an item at %d: %w", start, err)
			}
			out = append(out, item)
			if limit > 0 && len(out) == limit {
				return out, nil
			}
		}
		next := start + len(page.Values)
		if page.IsLast || (page.Total > 0 && int64(next) >= page.Total) {
			return out, nil
		}
		if len(page.Values) == 0 {
			return out, fmt.Errorf("an empty page at %d that is not the last", start)
		}
		start = next
	}
}
