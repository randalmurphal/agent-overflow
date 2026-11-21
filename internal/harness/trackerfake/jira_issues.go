package trackerfake

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// issueWrite is the body of an issue create or edit.
type issueWrite struct {
	Fields map[string]json.RawMessage `json:"fields"`
	Update map[string]json.RawMessage `json:"update"`
}

// found is the issue {issue} names, or Jira's not-found answer.
func (c *jiraCall) found() (*JiraIssue, *answer) {
	if is := c.issue(); is != nil {
		return is, nil
	}
	a := jiraErrors(http.StatusNotFound, issueNotFound)
	return nil, &a
}

// changed stamps is updated and records items as the viewer's change,
// when there are any.
func (c *jiraCall) changed(is *JiraIssue, items []JiraChangeItem) {
	if len(items) == 0 {
		return
	}
	c.s.addChange(c.e, is, c.viewer, c.touch(is), items)
}

// fieldOutcome answers a field write that did not apply.
func fieldOutcome(w fieldWrite) answer {
	if w.unhandled != "" {
		return unhandledf("%s", w.unhandled)
	}
	return jiraFieldErrors(http.StatusBadRequest, w.refused)
}

func getIssue(c *jiraCall) answer {
	if err := c.noBody(); err != nil {
		return unhandledf("%v", err)
	}
	expand := map[string]bool{}
	for e := range strings.SplitSeq(c.query.Get("expand"), ",") {
		switch e = strings.TrimSpace(e); e {
		case "":
		case "editmeta", "changelog", "transitions", "transitions.fields":
			expand[e] = true
		default:
			return unhandledf("expand=%q", e)
		}
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	out := c.s.issueJSON(is, parseFields(c.query["fields"], selectAll), c.viewer)
	// Measured: each expansion embeds what its own read returns.
	if expand["editmeta"] {
		t := c.s.project(is.Project).issueType(is.Type)
		out["editmeta"] = map[string]any{"fields": c.s.screenMeta(is, t.Screen, false)}
	}
	if expand["transitions"] || expand["transitions.fields"] {
		out["transitions"] = c.s.transitionsJSON(is, expand["transitions.fields"])
	}
	if expand["changelog"] {
		out["changelog"] = c.s.embeddedChangelog(is)
	}
	return jsonAnswer(http.StatusOK, out)
}

// embeddedChangelogMost is how many changelog entries an issue read
// embeds (measured: 100, newest first, with the total).
const embeddedChangelogMost = 100

// embeddedChangelog is is's changelog as an issue read embeds it: the
// newest entries first. Its maxResults is the entries embedded (measured
// with fewer than 100: 20 of 20).
func (s *jiraSite) embeddedChangelog(is *JiraIssue) map[string]any {
	n := min(len(is.Changelog), embeddedChangelogMost)
	out := make([]any, 0, n)
	for i := len(is.Changelog) - 1; i >= len(is.Changelog)-n; i-- {
		out = append(out, s.changelogJSON(is.Changelog[i], false))
	}
	return map[string]any{"startAt": 0, "maxResults": n, "total": len(is.Changelog), "histories": out}
}

func createIssue(c *jiraCall) answer {
	var in issueWrite
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	var pref, tref jiraRef
	var why string
	if pref, why = readRef(in.Fields["project"], "id", "key"); why != "" {
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"project": "valid project is required"})
	}
	p := c.s.project(pref.ID + pref.Key)
	if p == nil {
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"project": "valid project is required"})
	}
	if tref, why = readRef(in.Fields["issuetype"], "id"); why != "" || p.issueType(tref.ID) == nil {
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"issuetype": "valid issue type is required"})
	}
	t := p.issueType(tref.ID)
	fields := maps.Clone(in.Fields)
	delete(fields, "project")
	delete(fields, "issuetype")
	missing := map[string]string{}
	for _, sf := range t.Screen {
		_, set := fields[sf.Field]
		_, updated := in.Update[sf.Field]
		if sf.Required && !sf.HasDefault && !set && !updated {
			missing[sf.Field] = c.s.field(sf.Field).name + " is required."
		}
	}
	if len(missing) > 0 {
		return jiraFieldErrors(http.StatusBadRequest, missing)
	}
	now := c.e.now()
	is := JiraIssue{
		ID: strconv.FormatInt(c.e.nextID(), 10), Project: p.Key, Type: t.ID, Status: p.Statuses[0],
		Reporter: c.viewer, Created: now, Updated: now, Rank: c.s.lastRank(),
	}
	if w := c.s.writeFields(&is, t.Screen, fields, in.Update); w.unhandled != "" || w.refused != nil {
		return fieldOutcome(w)
	}
	if t.Subtask && is.Parent == "" {
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"parent": "Given parent work item does not belong to appropriate hierarchy."})
	}
	if err := c.s.keyIssue(&is, p); err != nil {
		return unhandledf("%v", err)
	}
	c.s.Issues = append(c.s.Issues, is)
	return jsonAnswer(http.StatusCreated, map[string]any{"id": is.ID, "key": is.Key, "self": c.s.self("issue/" + is.ID)})
}

func editIssue(c *jiraCall) answer {
	var in issueWrite
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	ret := c.query.Get("returnIssue")
	if ret != "" && ret != "true" && ret != "false" {
		return unhandledf("returnIssue=%q", ret)
	}
	if ret != "true" && c.query.Has("fields") {
		return unhandledf("fields without returnIssue=true")
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	t := c.s.project(is.Project).issueType(is.Type)
	w := c.s.writeFields(is, t.Screen, in.Fields, in.Update)
	if w.unhandled != "" || w.refused != nil {
		return fieldOutcome(w)
	}
	c.changed(is, w.items)
	if ret != "true" {
		return emptyAnswer(http.StatusNoContent)
	}
	return jsonAnswer(http.StatusOK, c.s.issueJSON(is, parseFields(c.query["fields"], selectAll), c.viewer))
}

func assignIssue(c *jiraCall) answer {
	var in struct {
		AccountID *string `json:"accountId"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	raw := json.RawMessage(`null`)
	if in.AccountID != nil {
		raw = mustJSON(map[string]string{"accountId": *in.AccountID})
	}
	before := cloneIssue(*is)
	f := systemField("assignee")
	if msg, why := c.s.setValue(is, f, raw); why != "" || msg != "" {
		if why != "" {
			return unhandledf("%s", why)
		}
		return jiraFieldErrors(http.StatusBadRequest, map[string]string{"assignee": msg})
	}
	if item, ok := c.s.changeItem(f, &before, is); ok {
		c.changed(is, []JiraChangeItem{item})
	}
	return emptyAnswer(http.StatusNoContent)
}

// available are the transitions of is's workflow from its status.
func (s *jiraSite) available(is *JiraIssue) []JiraTransition {
	var out []JiraTransition
	for _, t := range s.project(is.Project).Transitions {
		if len(t.From) == 0 || slices.Contains(t.From, is.Status) {
			out = append(out, t)
		}
	}
	return out
}

func getTransitions(c *jiraCall) answer {
	if exp := c.query.Get("expand"); exp != "" && exp != "transitions.fields" {
		return unhandledf("expand=%q", exp)
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	return jsonAnswer(http.StatusOK, map[string]any{"expand": "transitions", "transitions": c.s.transitionsJSON(is, c.query.Get("expand") != "")})
}

// transitionsJSON are the transitions is can make, with each one's screen
// fields when fields is set.
func (s *jiraSite) transitionsJSON(is *JiraIssue, fields bool) []any {
	out := []any{}
	for _, t := range s.available(is) {
		tj := map[string]any{
			"id": t.ID, "name": t.Name, "to": s.statusJSON(t.To), "hasScreen": len(t.Screen) > 0,
			"isGlobal": len(t.From) == 0, "isInitial": false, "isAvailable": true, "isConditional": false, "isLooped": false,
		}
		if fields {
			tj["fields"] = s.screenMeta(is, t.Screen, false)
		}
		out = append(out, tj)
	}
	return out
}

func transitionIssue(c *jiraCall) answer {
	var in struct {
		Transition jiraRef                    `json:"transition"`
		Fields     map[string]json.RawMessage `json:"fields"`
		Update     map[string]json.RawMessage `json:"update"`
	}
	if err := c.decode(&in, false); err != nil {
		return unhandledf("%v", err)
	}
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	i := slices.IndexFunc(c.s.available(is), func(t JiraTransition) bool { return t.ID == in.Transition.ID })
	if i < 0 {
		return jiraErrors(http.StatusBadRequest, fmt.Sprintf("Transition id '%s' is not valid for this issue.", in.Transition.ID))
	}
	t := c.s.available(is)[i]
	update := maps.Clone(in.Update)
	var comment json.RawMessage
	if raw, ok := update["comment"]; ok {
		delete(update, "comment")
		var ops []struct {
			Add struct {
				Body json.RawMessage `json:"body"`
			} `json:"add"`
		}
		if err := decodeStrict(raw, &ops, false); err != nil || len(ops) != 1 || !validDoc(ops[0].Add.Body) {
			return unhandledf("update.comment is not one add of a document: %s", raw)
		}
		comment = ops[0].Add.Body
	}
	before := cloneIssue(*is)
	w := c.s.writeFields(is, t.Screen, in.Fields, update)
	if w.unhandled != "" || w.refused != nil {
		return fieldOutcome(w)
	}
	items := w.items
	c.s.moveStatus(is, t.To, c.e.now())
	for _, id := range []string{"status", "resolution"} {
		if item, ok := c.s.changeItem(systemField(id), &before, is); ok {
			items = append(items, item)
		}
	}
	c.changed(is, items)
	if comment != nil {
		c.s.addComment(c.e, is, c.viewer, comment, nil, false)
	}
	return emptyAnswer(http.StatusNoContent)
}

// moveStatus sets is's status; moving into a done status sets the first
// resolution and the resolved time, and moving out clears them. A done
// transition writing a resolution item is measured; which resolution a
// site's workflow sets, and the clearing, are unverified.
func (s *jiraSite) moveStatus(is *JiraIssue, to string, now time.Time) {
	wasDone := s.status(is.Status).Category == "done"
	is.Status = to
	switch done := s.status(to).Category == "done"; {
	case done && !wasDone:
		is.Resolution, is.Resolved = s.Resolutions[0].ID, now
	case !done:
		is.Resolution, is.Resolved = "", time.Time{}
	}
}

func editMeta(c *jiraCall) answer {
	is, nf := c.found()
	if nf != nil {
		return *nf
	}
	t := c.s.project(is.Project).issueType(is.Type)
	return jsonAnswer(http.StatusOK, map[string]any{"fields": c.s.screenMeta(is, t.Screen, false)})
}

// screenMeta is a screen's fields as edit and transition metadata list
// them, by field ID.
func (s *jiraSite) screenMeta(is *JiraIssue, screen []JiraScreenField, create bool) map[string]any {
	out := map[string]any{}
	for _, sf := range screen {
		out[sf.Field] = s.fieldMeta(s.project(is.Project), is, sf, create)
	}
	return out
}

// fieldMeta is one screen field's metadata. is is the issue edited, nil
// on create.
func (s *jiraSite) fieldMeta(p *JiraProject, is *JiraIssue, sf JiraScreenField, create bool) map[string]any {
	f := s.field(sf.Field)
	out := map[string]any{
		"required": sf.Required, "schema": f.schema, "name": f.name, "key": f.id,
		"operations": fieldOperations(f.kind), "hasDefaultValue": sf.HasDefault,
	}
	if create {
		out["fieldId"] = f.id
	}
	var allowed []any
	switch f.kind {
	case kindOption, kindOptions, kindCascade:
		for _, o := range f.options {
			v := s.optionJSON(o)
			v["disabled"] = o.Disabled
			if len(o.Children) > 0 {
				var kids []any
				for _, k := range o.Children {
					kids = append(kids, s.optionJSON(k))
				}
				v["children"] = kids
			}
			allowed = append(allowed, v)
		}
	case kindPriority:
		for _, pr := range s.Priorities {
			allowed = append(allowed, s.priorityJSON(pr.ID))
		}
	case kindComponents:
		allowed = s.componentsJSON(p, componentIDs(p)).([]any)
	case kindIssueType:
		for _, t := range p.IssueTypes {
			if is == nil || t.Subtask == p.issueType(is.Type).Subtask {
				allowed = append(allowed, s.issueTypeJSON(p, t.ID))
			}
		}
	case kindUser, kindUsers:
		where := "project=" + p.Key
		if is != nil {
			where = "issueKey=" + is.Key
		}
		out["autoCompleteUrl"] = s.self("user/assignable/search?" + where + "&query=")
	}
	if allowed != nil {
		out["allowedValues"] = allowed
	}
	return out
}

func componentIDs(p *JiraProject) []string {
	ids := make([]string, len(p.Components))
	for i, c := range p.Components {
		ids[i] = c.ID
	}
	return ids
}

// fieldOperations are what an update may do to a field of kind k.
func fieldOperations(k fieldKind) []string {
	switch k {
	case kindLabels, kindUsers, kindOptions, kindComponents:
		return []string{"add", "set", "remove"}
	case kindReadOnly, kindRank, kindTimeEstimate:
		return []string{}
	}
	return []string{"set"}
}

func createMetaTypes(c *jiraCall) answer {
	p := c.s.project(c.params["project"])
	if p == nil {
		return jiraErrors(http.StatusNotFound, "No project could be found with key '"+c.params["project"]+"'.")
	}
	start, size, err := c.pageWindow(50, 200)
	if err != nil {
		return unhandledf("%v", err)
	}
	var types []any
	for _, t := range p.IssueTypes {
		tj := c.s.issueTypeJSON(p, t.ID).(map[string]any)
		types = append(types, tj)
	}
	return jsonAnswer(http.StatusOK, listPage("issueTypes", types, start, size))
}

func createMetaFields(c *jiraCall) answer {
	p := c.s.project(c.params["project"])
	if p == nil {
		return jiraErrors(http.StatusNotFound, "No project could be found with key '"+c.params["project"]+"'.")
	}
	t := p.issueType(c.params["type"])
	if t == nil {
		return jiraErrors(http.StatusNotFound, "Issue type with id '"+c.params["type"]+"' does not exist or is not available in project "+p.Key+".")
	}
	start, size, err := c.pageWindow(50, 200)
	if err != nil {
		return unhandledf("%v", err)
	}
	var fields []any
	for _, sf := range t.Screen {
		fields = append(fields, c.s.fieldMeta(p, nil, sf, true))
	}
	return jsonAnswer(http.StatusOK, listPage("fields", fields, start, size))
}
