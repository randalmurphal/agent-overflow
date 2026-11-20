package trackerfake

import (
	"encoding/json"
	"slices"
	"strings"
)

// fieldSelection is the fields an issue read returns: a list of field
// IDs, *all, *navigable and -ID exclusions, as Jira reads its fields
// parameter. A field the site lacks selects nothing, as Jira ignores it.
type fieldSelection struct {
	all, nav bool
	ids      []string
	minus    []string
}

// parseFields reads a fields list; items may themselves be comma lists.
// none is the selection when the list is empty.
func parseFields(items []string, none fieldSelection) fieldSelection {
	var sel fieldSelection
	any := false
	for _, item := range items {
		for f := range strings.SplitSeq(item, ",") {
			f = strings.TrimSpace(f)
			switch {
			case f == "":
				continue
			case f == "*all":
				sel.all = true
			case f == "*navigable":
				sel.nav = true
			case strings.HasPrefix(f, "-"):
				sel.minus = append(sel.minus, f[1:])
			default:
				sel.ids = append(sel.ids, f)
			}
			any = true
		}
	}
	if !any {
		return none
	}
	return sel
}

// selectAll is *all; selectNavigable is *navigable; selectNone returns
// IDs and keys only.
var (
	selectAll       = fieldSelection{all: true}
	selectNavigable = fieldSelection{nav: true}
	selectNone      = fieldSelection{}
)

// ids are the fields sel selects on site s.
func (sel fieldSelection) fieldIDs(s *jiraSite) []string {
	var out []string
	for _, f := range s.fields() {
		on := sel.all || slices.Contains(sel.ids, f.id) || (sel.nav && (f.custom || navigable[f.id]))
		if on && !slices.Contains(sel.minus, f.id) {
			out = append(out, f.id)
		}
	}
	return out
}

// issueJSON is is as an issue read returns it with sel's fields.
func (s *jiraSite) issueJSON(is *JiraIssue, sel fieldSelection, viewer string) map[string]any {
	out := map[string]any{"id": is.ID, "key": is.Key, "self": s.self("issue/" + is.ID), "expand": ""}
	fields := map[string]any{}
	for _, id := range sel.fieldIDs(s) {
		fields[id] = s.render(is, id, viewer)
	}
	out["fields"] = fields
	return out
}

// render is one field's value on is, null when it has none.
func (s *jiraSite) render(is *JiraIssue, id, viewer string) any {
	p := s.project(is.Project)
	switch id {
	case "summary":
		return is.Summary
	case "description":
		return rawOrNil(is.Description)
	case "status":
		return s.statusJSON(is.Status)
	case "issuetype":
		return s.issueTypeJSON(p, is.Type)
	case "project":
		return s.projectJSON(p)
	case "priority":
		return s.priorityJSON(is.Priority)
	case "resolution":
		return s.resolutionJSON(is.Resolution)
	case "assignee":
		return s.userJSON(is.Assignee)
	case "reporter":
		return s.userJSON(is.Reporter)
	case "labels":
		return orEmpty(is.Labels)
	case "components":
		return s.componentsJSON(p, is.Components)
	case "parent":
		if parent := s.issue(is.Parent); parent != nil {
			return s.issueRef(parent)
		}
		return nil
	case "duedate":
		return stringOrNil(is.DueDate)
	case "created":
		return jiraTime(is.Created)
	case "updated":
		return jiraTime(is.Updated)
	case "resolutiondate":
		if is.Resolved.IsZero() {
			return nil
		}
		return jiraTime(is.Resolved)
	case "timeoriginalestimate":
		return numberOrNil(is.OriginalEstimate)
	case "timeestimate":
		return numberOrNil(is.RemainingEstimate)
	case "timespent":
		return s.timeSpent(is)
	case "attachment":
		return s.attachmentsJSON(is)
	case "issuelinks":
		return s.linksJSON(is)
	case "subtasks":
		return s.subtasksJSON(is)
	case "comment":
		return s.commentPageJSON(is, 0, len(is.Comments), false)
	case "worklog":
		// Measured: the field embeds the first 20 with the total and
		// maxResults 20.
		return s.worklogPageJSON(is, is.Worklogs, 0, embeddedWorklogs)
	case "watches":
		return map[string]any{"self": s.self("issue/" + is.Key + "/watchers"), "watchCount": len(is.Watchers), "isWatching": slices.Contains(is.Watchers, viewer)}
	}
	return s.renderCustom(is, id)
}

func (s *jiraSite) renderCustom(is *JiraIssue, id string) any {
	f := s.field(id)
	switch f.kind {
	case kindSprint:
		if len(is.Sprints) == 0 {
			return nil
		}
		out := make([]any, 0, len(is.Sprints))
		for _, sid := range is.Sprints {
			if sp := s.sprint(sid); sp != nil {
				out = append(out, s.sprintJSON(sp))
			}
		}
		return out
	case kindRank:
		return is.Rank
	}
	return rawOrNil(is.Values[id])
}

func (s *jiraSite) resolutionJSON(id string) any {
	for _, r := range s.Resolutions {
		if r.ID == id {
			return map[string]any{"self": s.self("resolution/" + r.ID), "id": r.ID, "name": r.Name, "description": ""}
		}
	}
	return nil
}

func (s *jiraSite) componentsJSON(p *JiraProject, ids []string) any {
	out := []any{}
	for _, id := range ids {
		for _, c := range p.Components {
			if c.ID == id {
				out = append(out, map[string]any{"self": s.self("component/" + c.ID), "id": c.ID, "name": c.Name})
			}
		}
	}
	return out
}

func (s *jiraSite) timeSpent(is *JiraIssue) any {
	if len(is.Worklogs) == 0 {
		return nil
	}
	var total int64
	for _, w := range is.Worklogs {
		total += w.TimeSpent
	}
	return total
}

func (s *jiraSite) attachmentsJSON(is *JiraIssue) []any {
	out := []any{}
	for _, a := range is.Attachments {
		out = append(out, s.attachmentJSON(a))
	}
	return out
}

func (s *jiraSite) attachmentJSON(a JiraAttachment) map[string]any {
	return map[string]any{
		"self": s.self("attachment/" + a.ID), "id": a.ID, "filename": a.Filename, "mimeType": a.MimeType,
		"size": len(a.Content), "created": jiraTime(a.Created), "author": s.userJSON(a.Author),
		"content": s.self("attachment/content/" + a.ID),
	}
}

func (s *jiraSite) linksJSON(is *JiraIssue) []any {
	out := []any{}
	for _, l := range s.Links {
		t := s.linkType(l.Type)
		typ := map[string]any{"self": s.self("issueLinkType/" + t.ID), "id": t.ID, "name": t.Name, "inward": t.Inward, "outward": t.Outward}
		link := map[string]any{"id": l.ID, "self": s.self("issueLink/" + l.ID), "type": typ}
		// The link's from (outward) issue reads the other as its
		// outwardIssue; the other reads the from issue as its inwardIssue.
		switch is.ID {
		case l.Outward:
			link["outwardIssue"] = s.issueRef(s.issue(l.Inward))
		case l.Inward:
			link["inwardIssue"] = s.issueRef(s.issue(l.Outward))
		default:
			continue
		}
		out = append(out, link)
	}
	return out
}

func (s *jiraSite) subtasksJSON(is *JiraIssue) []any {
	out := []any{}
	for i := range s.Issues {
		child := &s.Issues[i]
		if child.Parent != is.ID {
			continue
		}
		if t := s.project(child.Project).issueType(child.Type); t != nil && t.Subtask {
			out = append(out, s.issueRef(child))
		}
	}
	return out
}

func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

func stringOrNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func numberOrNil(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}
