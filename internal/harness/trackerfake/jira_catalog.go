package trackerfake

import (
	"net/http"
	"slices"
	"strings"
)

func listFields(c *jiraCall) answer {
	out := []any{}
	for _, f := range c.s.fields() {
		out = append(out, f.catalogEntry())
	}
	return jsonAnswer(http.StatusOK, out)
}

func searchProjects(c *jiraCall) answer {
	if ob := c.query.Get("orderBy"); ob != "" && ob != "key" {
		return unhandledf("orderBy=%q", ob)
	}
	start, size, err := c.pageWindow(50, 100)
	if err != nil {
		return unhandledf("%v", err)
	}
	q := strings.ToLower(c.query.Get("query"))
	var ps []*JiraProject
	for i := range c.s.Projects {
		p := &c.s.Projects[i]
		if q == "" || strings.Contains(strings.ToLower(p.Key), q) || strings.Contains(strings.ToLower(p.Name), q) {
			ps = append(ps, p)
		}
	}
	slices.SortFunc(ps, func(a, b *JiraProject) int { return strings.Compare(a.Key, b.Key) })
	values := make([]any, len(ps))
	for i, p := range ps {
		values[i] = c.s.projectJSON(p)
	}
	return jsonAnswer(http.StatusOK, offsetPage(values, start, size))
}

// foundProject is the project {project} names, or Jira's not-found.
func (c *jiraCall) foundProject() (*JiraProject, *answer) {
	if p := c.s.project(c.params["project"]); p != nil {
		return p, nil
	}
	a := jiraErrors(http.StatusNotFound, "No project could be found with key '"+c.params["project"]+"'.")
	return nil, &a
}

func getProject(c *jiraCall) answer {
	if exp := c.query.Get("expand"); exp != "" && exp != "issueTypes" {
		return unhandledf("expand=%q", exp)
	}
	p, nf := c.foundProject()
	if nf != nil {
		return *nf
	}
	out := c.s.projectJSON(p)
	types := []any{}
	for _, t := range p.IssueTypes {
		types = append(types, c.s.issueTypeJSON(p, t.ID))
	}
	out["issueTypes"] = types
	return jsonAnswer(http.StatusOK, out)
}

func projectStatuses(c *jiraCall) answer {
	p, nf := c.foundProject()
	if nf != nil {
		return *nf
	}
	out := []any{}
	for _, t := range p.IssueTypes {
		tj := c.s.issueTypeJSON(p, t.ID).(map[string]any)
		var sts []any
		for _, id := range p.Statuses {
			sts = append(sts, c.s.statusJSON(id))
		}
		tj["statuses"] = sts
		out = append(out, tj)
	}
	return jsonAnswer(http.StatusOK, out)
}

func listStatuses(c *jiraCall) answer {
	out := []any{}
	for _, st := range c.s.Statuses {
		out = append(out, c.s.statusJSON(st.ID))
	}
	return jsonAnswer(http.StatusOK, out)
}

func searchPriorities(c *jiraCall) answer {
	start, size, err := c.pageWindow(50, 100)
	if err != nil {
		return unhandledf("%v", err)
	}
	values := make([]any, len(c.s.Priorities))
	for i, p := range c.s.Priorities {
		pj := c.s.priorityJSON(p.ID).(map[string]any)
		pj["statusColor"], pj["description"], pj["isDefault"] = "#cccccc", "", p.ID == "3"
		values[i] = pj
	}
	return jsonAnswer(http.StatusOK, offsetPage(values, start, size))
}

func listLinkTypes(c *jiraCall) answer {
	out := []any{}
	for _, t := range c.s.LinkTypes {
		out = append(out, map[string]any{"self": c.s.self("issueLinkType/" + t.ID), "id": t.ID, "name": t.Name, "inward": t.Inward, "outward": t.Outward})
	}
	return jsonAnswer(http.StatusOK, map[string]any{"issueLinkTypes": out})
}
