package trackerfake

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"
)

// changeValue is one side of a changelog item: the raw value and what a
// person reads; nil is none.
type changeValue struct{ raw, text *string }

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// changeItem is the changelog item of field f's change from before to
// after; changed is false when the value did not change.
func (s *jiraSite) changeItem(f *fieldDef, before, after *JiraIssue) (JiraChangeItem, bool) {
	from, to := s.changeValue(f, before), s.changeValue(f, after)
	if eqPtr(from.raw, to.raw) && eqPtr(from.text, to.text) {
		return JiraChangeItem{}, false
	}
	item := JiraChangeItem{FieldID: f.id, Field: f.id, FieldType: "jira", From: from.raw, To: to.raw, FromString: from.text, ToString: to.text}
	if f.custom {
		item.Field, item.FieldType = f.name, "custom"
	}
	switch {
	case f.id == "parent":
		// Measured: a parent change has no fieldId and names the field
		// IssueParentAssociation; the keys in its strings are unverified.
		item.FieldID, item.Field = "", "IssueParentAssociation"
	case f.kind == kindNumber && item.ToString == nil:
		// Measured: a cleared custom number reads toString "", while a
		// first value reads fromString null.
		empty := ""
		item.ToString = &empty
	}
	return item, true
}

// changeItems are the changelog items of field f's change. Components
// write one item per component added or removed, the ID on the side that
// has it and null on the other (measured); every other field writes one.
func (s *jiraSite) changeItems(f *fieldDef, before, after *JiraIssue) []JiraChangeItem {
	if f.id != "components" {
		if item, ok := s.changeItem(f, before, after); ok {
			return []JiraChangeItem{item}
		}
		return nil
	}
	p := s.project(after.Project)
	name := func(id string) *string {
		for _, c := range p.Components {
			if c.ID == id {
				return &c.Name
			}
		}
		return nil
	}
	var out []JiraChangeItem
	for _, id := range before.Components {
		if !slices.Contains(after.Components, id) {
			out = append(out, JiraChangeItem{FieldID: "components", Field: "Component", FieldType: "jira", From: &id, FromString: name(id)})
		}
	}
	for _, id := range after.Components {
		if !slices.Contains(before.Components, id) {
			out = append(out, JiraChangeItem{FieldID: "components", Field: "Component", FieldType: "jira", To: &id, ToString: name(id)})
		}
	}
	return out
}

func eqPtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// changeValue is field f's value on is as the changelog records it.
// Measured (docs/references/tracker-api-measurements.md, Jira Cloud,
// "Where a changelog item holds its values depends on the field"):
// status, resolution, priority and issuetype carry their IDs and assignee
// its account ID as the raw value, names as text; summary, description,
// labels and a custom number field are text only, the raw value null;
// timeoriginalestimate and timeestimate carry seconds as both, null when
// unset; Sprint carries its sprint IDs. A custom text, date or labels
// field is text only like the number field, and an empty description is
// null (unverified).
func (s *jiraSite) changeValue(f *fieldDef, is *JiraIssue) changeValue {
	switch f.id {
	case "summary":
		return changeValue{text: strPtr(is.Summary)}
	case "description":
		return changeValue{text: strPtr(strings.TrimSpace(docText(is.Description)))}
	case "status":
		st := s.status(is.Status)
		return changeValue{raw: strPtr(st.ID), text: strPtr(st.Name)}
	case "resolution":
		return s.namedChange(s.Resolutions, is.Resolution)
	case "priority":
		return s.namedChange(s.Priorities, is.Priority)
	case "issuetype":
		t := s.project(is.Project).issueType(is.Type)
		return changeValue{raw: strPtr(t.ID), text: strPtr(t.Name)}
	case "assignee", "reporter":
		id := is.Assignee
		if f.id == "reporter" {
			id = is.Reporter
		}
		return s.userChange(id)
	case "labels":
		// Measured: no labels is the empty string, not null.
		text := strings.Join(is.Labels, " ")
		return changeValue{text: &text}
	case "components":
		// Only compared here; changeItems writes the items.
		return changeValue{raw: strPtr(strings.Join(is.Components, ", "))}
	case "parent":
		if parent := s.issue(is.Parent); parent != nil {
			return changeValue{raw: strPtr(parent.ID), text: strPtr(parent.Key)}
		}
		return changeValue{}
	case "duedate":
		return changeValue{raw: strPtr(is.DueDate), text: strPtr(is.DueDate)}
	case "timeoriginalestimate":
		return secondsChange(is.OriginalEstimate)
	case "timeestimate":
		return secondsChange(is.RemainingEstimate)
	}
	return s.customChange(f, is)
}

func (s *jiraSite) namedChange(list []JiraNamed, id string) changeValue {
	for _, n := range list {
		if n.ID == id {
			return changeValue{raw: strPtr(n.ID), text: strPtr(n.Name)}
		}
	}
	return changeValue{}
}

func (s *jiraSite) userChange(id string) changeValue {
	if u := s.user(id); u != nil {
		return changeValue{raw: strPtr(u.AccountID), text: strPtr(u.DisplayName)}
	}
	return changeValue{}
}

func secondsChange(n *int64) changeValue {
	if n == nil {
		return changeValue{}
	}
	v := strconv.FormatInt(*n, 10)
	return changeValue{raw: &v, text: &v}
}

// customChange is a custom field's change value, read from its stored
// value.
func (s *jiraSite) customChange(f *fieldDef, is *JiraIssue) changeValue {
	// The sprint field is read from is.Sprints, every other kind from its
	// stored value.
	raw := is.Values[f.id]
	if f.kind != kindSprint && len(raw) == 0 {
		return changeValue{}
	}
	switch f.kind {
	case kindSprint:
		var ids, names []string
		for _, id := range is.Sprints {
			if sp := s.sprint(id); sp != nil {
				ids = append(ids, strconv.FormatInt(id, 10))
				names = append(names, sp.Name)
			}
		}
		// Measured: no sprint is the empty string in all four members.
		raw, text := strings.Join(ids, ", "), strings.Join(names, ", ")
		return changeValue{raw: &raw, text: &text}
	case kindRank:
		return changeValue{}
	case kindUser:
		var u struct{ AccountID string }
		_ = json.Unmarshal(raw, &u)
		return s.userChange(u.AccountID)
	case kindOption, kindCascade:
		var o struct {
			ID, Value string
			Child     *struct{ ID, Value string }
		}
		_ = json.Unmarshal(raw, &o)
		text := o.Value
		if o.Child != nil {
			text += " / " + o.Child.Value
		}
		return changeValue{raw: strPtr(o.ID), text: strPtr(text)}
	case kindUsers, kindOptions:
		var list []struct{ AccountID, ID, Value, DisplayName string }
		_ = json.Unmarshal(raw, &list)
		var ids, texts []string
		for _, v := range list {
			ids = append(ids, v.ID+v.AccountID)
			texts = append(texts, v.Value+v.DisplayName)
		}
		return changeValue{raw: strPtr(strings.Join(ids, ", ")), text: strPtr(strings.Join(texts, ", "))}
	case kindLabels:
		var list []string
		_ = json.Unmarshal(raw, &list)
		return changeValue{text: strPtr(strings.Join(list, " "))}
	case kindDoc:
		return changeValue{text: strPtr(strings.TrimSpace(docText(raw)))}
	case kindUnsupported, kindReadOnly, kindText, kindNumber, kindDate, kindDateTime,
		kindPriority, kindResolution, kindIssueType, kindComponents, kindParent, kindTimeEstimate:
		// Text only, read from the stored string or number below.
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	switch v := v.(type) {
	case string:
		return changeValue{text: strPtr(v)}
	case float64:
		return changeValue{text: strPtr(strconv.FormatFloat(v, 'f', -1, 64))}
	}
	return changeValue{}
}

// addChange records items as one changelog entry by author at now; no
// items record nothing.
func (s *jiraSite) addChange(e *Engine, is *JiraIssue, author string, now time.Time, items []JiraChangeItem) {
	if len(items) == 0 {
		return
	}
	is.Changelog = append(is.Changelog, JiraChange{ID: strconv.FormatInt(e.nextID(), 10), Author: author, Created: now, Items: items})
}

// changelogJSON is an entry as the per-issue changelog reads it; the bulk
// fetch writes created as epoch milliseconds instead (measured).
func (s *jiraSite) changelogJSON(c JiraChange, bulk bool) map[string]any {
	out := map[string]any{"id": c.ID, "author": s.userJSON(c.Author), "items": c.Items, "created": jiraTime(c.Created)}
	if bulk {
		out["created"] = c.Created.UnixMilli()
	}
	return out
}
