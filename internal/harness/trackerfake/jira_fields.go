package trackerfake

import (
	"strconv"
	"strings"
	"time"
)

// fieldKind is how a field's value is read and written. Each kind has one
// renderer (jiraRender) and one writer (jira_values.go).
type fieldKind uint8

const (
	kindUnsupported fieldKind = iota
	kindReadOnly
	kindText
	kindDoc
	kindNumber
	kindDate
	kindDateTime
	kindUser
	kindUsers
	kindLabels
	kindOption
	kindOptions
	kindCascade
	kindPriority
	kindResolution
	kindIssueType
	kindComponents
	kindParent
	kindSprint
	kindRank
	kindTimeEstimate
)

// fieldDef is one field of a site: the catalog entry and its kind.
type fieldDef struct {
	id     string
	name   string
	schema JiraSchema
	custom bool
	kind   fieldKind
	// options are a select's.
	options []JiraOption
}

// systemFields are the system fields the fake models, by ID: name,
// schema type and kind. A system field missing here is not a field of
// any site.
var systemFields = []fieldDef{
	{id: "summary", name: "Summary", schema: JiraSchema{Type: "string"}, kind: kindText},
	{id: "description", name: "Description", schema: JiraSchema{Type: "string"}, kind: kindDoc},
	{id: "status", name: "Status", schema: JiraSchema{Type: "status"}, kind: kindReadOnly},
	{id: "issuetype", name: "Issue Type", schema: JiraSchema{Type: "issuetype"}, kind: kindIssueType},
	{id: "project", name: "Project", schema: JiraSchema{Type: "project"}, kind: kindReadOnly},
	{id: "priority", name: "Priority", schema: JiraSchema{Type: "priority"}, kind: kindPriority},
	{id: "resolution", name: "Resolution", schema: JiraSchema{Type: "resolution"}, kind: kindResolution},
	{id: "assignee", name: "Assignee", schema: JiraSchema{Type: "user"}, kind: kindUser},
	{id: "reporter", name: "Reporter", schema: JiraSchema{Type: "user"}, kind: kindUser},
	{id: "labels", name: "Labels", schema: JiraSchema{Type: "array", Items: "string"}, kind: kindLabels},
	{id: "components", name: "Components", schema: JiraSchema{Type: "array", Items: "component"}, kind: kindComponents},
	{id: "parent", name: "Parent", schema: JiraSchema{Type: "issuelink"}, kind: kindParent},
	{id: "duedate", name: "Due date", schema: JiraSchema{Type: "date"}, kind: kindDate},
	{id: "created", name: "Created", schema: JiraSchema{Type: "datetime"}, kind: kindReadOnly},
	{id: "updated", name: "Updated", schema: JiraSchema{Type: "datetime"}, kind: kindReadOnly},
	{id: "resolutiondate", name: "Resolved", schema: JiraSchema{Type: "datetime"}, kind: kindReadOnly},
	{id: "timeoriginalestimate", name: "Original estimate", schema: JiraSchema{Type: "number"}, kind: kindTimeEstimate},
	{id: "timeestimate", name: "Remaining Estimate", schema: JiraSchema{Type: "number"}, kind: kindTimeEstimate},
	{id: "timespent", name: "Time Spent", schema: JiraSchema{Type: "number"}, kind: kindReadOnly},
	{id: "attachment", name: "Attachment", schema: JiraSchema{Type: "array", Items: "attachment"}, kind: kindReadOnly},
	{id: "issuelinks", name: "Linked Issues", schema: JiraSchema{Type: "array", Items: "issuelinks"}, kind: kindReadOnly},
	{id: "subtasks", name: "Sub-tasks", schema: JiraSchema{Type: "array", Items: "issuelinks"}, kind: kindReadOnly},
	{id: "comment", name: "Comment", schema: JiraSchema{Type: "comments-page"}, kind: kindReadOnly},
	{id: "worklog", name: "Log Work", schema: JiraSchema{Type: "array", Items: "worklog"}, kind: kindReadOnly},
	{id: "watches", name: "Watchers", schema: JiraSchema{Type: "watches"}, kind: kindReadOnly},
}

// navigable are the system fields *navigable selects.
var navigable = map[string]bool{
	"summary": true, "status": true, "issuetype": true, "project": true, "priority": true, "resolution": true,
	"assignee": true, "reporter": true, "labels": true, "components": true, "parent": true, "duedate": true,
	"created": true, "updated": true, "resolutiondate": true, "timeoriginalestimate": true, "timeestimate": true,
	"timespent": true, "issuelinks": true, "subtasks": true, "watches": true,
}

func systemField(id string) *fieldDef {
	for i := range systemFields {
		if systemFields[i].id == id {
			f := systemFields[i]
			f.schema.System = id
			return &f
		}
	}
	return nil
}

// customKind is the kind of a custom field by its schema.
func customKind(s JiraSchema) fieldKind {
	custom := s.Custom[strings.LastIndex(s.Custom, ":")+1:]
	switch {
	case s.Custom == sprintFieldType:
		return kindSprint
	case s.Custom == rankFieldType:
		return kindRank
	case custom == "textarea":
		return kindDoc
	}
	switch s.Type {
	case "string":
		return kindText
	case "number":
		return kindNumber
	case "date":
		return kindDate
	case "datetime":
		return kindDateTime
	case "user":
		return kindUser
	case "option":
		return kindOption
	case "option-with-child":
		return kindCascade
	case "array":
		switch {
		case s.Items == "user":
			return kindUsers
		case s.Items == "option":
			return kindOptions
		case s.Items == "string" && custom == "labels":
			return kindLabels
		case s.Items == "string":
			// A list of strings of another custom type is read and written
			// as labels are (unverified).
			return kindLabels
		}
	}
	return kindUnsupported
}

// field is the site's field with id: a system field or a custom one.
func (s *jiraSite) field(id string) *fieldDef {
	if f := systemField(id); f != nil {
		return f
	}
	cf := s.customField(id)
	if cf == nil {
		return nil
	}
	return &fieldDef{id: cf.ID, name: cf.Name, schema: cf.Schema, custom: true, kind: customKind(cf.Schema), options: cf.Options}
}

// fields are every field of the site, system fields first.
func (s *jiraSite) fields() []*fieldDef {
	out := make([]*fieldDef, 0, len(systemFields)+len(s.Fields))
	for _, f := range systemFields {
		out = append(out, systemField(f.id))
	}
	for _, cf := range s.Fields {
		out = append(out, s.field(cf.ID))
	}
	return out
}

// catalogEntry is a field as GET field lists it.
func (f *fieldDef) catalogEntry() map[string]any {
	out := map[string]any{
		"id": f.id, "key": f.id, "name": f.name, "custom": f.custom, "schema": f.schema,
		"navigable": true, "searchable": true, "orderable": f.kind != kindReadOnly,
		"clauseNames": []string{f.id},
	}
	if f.custom {
		out["clauseNames"] = []string{"cf[" + strings.TrimPrefix(f.id, "customfield_") + "]", f.name}
	}
	return out
}

// jiraTime is how Jira writes a timestamp.
func jiraTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000-0700") }

// self is a resource's address on the site.
func (s *jiraSite) self(path string) string { return "https://" + s.Site + "/rest/api/3/" + path }

func (s *jiraSite) userJSON(id string) any {
	u := s.user(id)
	if u == nil {
		return nil
	}
	avatar := "https://avatar.example.com/" + u.AccountID + "/48.png"
	out := map[string]any{
		"self": s.self("user?accountId=" + u.AccountID), "accountId": u.AccountID, "displayName": u.DisplayName,
		"active": !u.Inactive, "accountType": u.AccountType, "timeZone": u.TimeZone,
		"avatarUrls": map[string]string{"48x48": avatar, "24x24": avatar, "16x16": avatar, "32x32": avatar},
	}
	if u.Email != "" {
		out["emailAddress"] = u.Email
	}
	if u.HidesTimeZone {
		out["timeZone"] = nil
	}
	if u.OmitsAccountType {
		delete(out, "accountType")
	}
	return out
}

func (s *jiraSite) statusJSON(id string) any {
	st := s.status(id)
	if st == nil {
		return nil
	}
	cat := statusCategories[st.Category]
	return map[string]any{
		"self": s.self("status/" + st.ID), "id": st.ID, "name": st.Name, "description": "",
		"statusCategory": map[string]any{"self": s.self("statuscategory/" + strconv.Itoa(cat.id)), "id": cat.id, "key": st.Category, "name": cat.name, "colorName": cat.color},
	}
}

func (s *jiraSite) priorityJSON(id string) any {
	p := s.priority(id)
	if p == nil {
		return nil
	}
	return map[string]any{"self": s.self("priority/" + p.ID), "id": p.ID, "name": p.Name, "iconUrl": "https://" + s.Site + "/images/icons/priorities/" + strings.ToLower(p.Name) + ".svg"}
}

func (s *jiraSite) issueTypeJSON(p *JiraProject, id string) any {
	t := p.issueType(id)
	if t == nil {
		return nil
	}
	return map[string]any{"self": s.self("issuetype/" + t.ID), "id": t.ID, "name": t.Name, "description": "", "subtask": t.Subtask, "hierarchyLevel": t.HierarchyLevel}
}

func (s *jiraSite) projectJSON(p *JiraProject) map[string]any {
	// Jira sends four avatar sizes; the fake sends one, which the lists
	// rewrite (listURLs).
	return map[string]any{
		"self": s.self("project/" + p.ID), "id": p.ID, "key": p.Key, "name": p.Name, "projectTypeKey": p.TypeKey, "simplified": p.Simplified,
		"avatarUrls": map[string]string{"48x48": s.self("project/" + p.ID + "/avatar")},
	}
}

func (s *jiraSite) optionJSON(o JiraOption) map[string]any {
	return map[string]any{"self": s.self("customFieldOption/" + o.ID), "id": o.ID, "value": o.Value}
}

func (s *jiraSite) sprintJSON(sp *JiraSprint) map[string]any {
	out := map[string]any{"id": sp.ID, "name": sp.Name, "state": sp.State, "boardId": sp.Board, "goal": sp.Goal}
	putSprintDates(out, sp)
	return out
}

// sprintTime is how the agile API writes a sprint's dates.
func sprintTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

func putSprintDates(out map[string]any, sp *JiraSprint) {
	for key, t := range map[string]time.Time{"startDate": sp.Start, "endDate": sp.End, "completeDate": sp.Complete} {
		if !t.IsZero() {
			out[key] = sprintTime(t)
		}
	}
}

// issueRef is another issue as an issue's fields name it.
func (s *jiraSite) issueRef(is *JiraIssue) map[string]any {
	p := s.project(is.Project)
	return map[string]any{
		"id": is.ID, "key": is.Key, "self": s.self("issue/" + is.ID),
		"fields": map[string]any{"summary": is.Summary, "status": s.statusJSON(is.Status), "priority": s.priorityJSON(is.Priority), "issuetype": s.issueTypeJSON(p, is.Type)},
	}
}
