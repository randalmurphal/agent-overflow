package trackerfake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// notOnScreen is Jira's refusal of a field a write may not set.
func notOnScreen(id string) string {
	return fmt.Sprintf("Field '%s' cannot be set. It is not on the appropriate screen, or unknown.", id)
}

// fieldWrite is the outcome of writing fields to an issue: the changelog
// items, Jira's refusals by field, or why the request is unhandled.
type fieldWrite struct {
	items     []JiraChangeItem
	refused   map[string]string
	unhandled string
}

// writeFields applies a create's or edit's fields and update members to
// is, all or nothing. screen is the fields the write may set.
func (s *jiraSite) writeFields(is *JiraIssue, screen []JiraScreenField, fields, update map[string]json.RawMessage) fieldWrite {
	next := cloneIssue(*is)
	var out fieldWrite
	refuse := func(id, msg string) {
		if out.refused == nil {
			out.refused = map[string]string{}
		}
		out.refused[id] = msg
	}
	ids := slices.Sorted(maps.Keys(fields))
	for _, id := range slices.Sorted(maps.Keys(update)) {
		if _, both := fields[id]; both {
			refuse(id, fmt.Sprintf("Field %s appears in both fields and update.", id))
			continue
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		f := s.field(id)
		if f == nil || !slices.ContainsFunc(screen, func(sf JiraScreenField) bool { return sf.Field == id }) {
			refuse(id, notOnScreen(id))
			continue
		}
		raw, ok := fields[id]
		if !ok {
			var why string
			if raw, why = updateSet(f, update[id]); why != "" {
				return fieldWrite{unhandled: why}
			}
		}
		msg, why := s.setValue(&next, f, raw)
		if why != "" {
			return fieldWrite{unhandled: fmt.Sprintf("field %s: %s", id, why)}
		}
		if msg != "" {
			refuse(id, msg)
		}
	}
	if out.refused != nil {
		return out
	}
	for _, id := range ids {
		out.items = append(out.items, s.changeItems(s.field(id), is, &next)...)
	}
	*is = next
	return out
}

// updateSet reads the one update operation implemented, set: the value it
// sets, or null for the parent's {"none": true}.
func updateSet(f *fieldDef, raw json.RawMessage) (json.RawMessage, string) {
	var ops []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ops); err != nil || len(ops) != 1 || len(ops[0]) != 1 || ops[0]["set"] == nil {
		return nil, fmt.Sprintf("update of %s is not one set operation: %s", f.id, raw)
	}
	v := ops[0]["set"]
	if f.kind == kindParent && bytes.Equal(bytes.Join(bytes.Fields(v), nil), []byte(`{"none":true}`)) {
		return json.RawMessage(`null`), ""
	}
	return v, ""
}

// jiraRef is an object naming a value: by ID, key, name, option value or
// account ID, and a cascading select's child.
type jiraRef struct {
	ID        string   `json:"id"`
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Value     string   `json:"value"`
	AccountID string   `json:"accountId"`
	Child     *jiraRef `json:"child"`
}

// readRef decodes a reference object whose members are among allowed.
func readRef(raw json.RawMessage, allowed ...string) (jiraRef, string) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return jiraRef{}, fmt.Sprintf("the value %s is not an object", raw)
	}
	for k := range members {
		if !slices.Contains(allowed, k) {
			return jiraRef{}, fmt.Sprintf("member %q of %s is not implemented", k, raw)
		}
	}
	var r jiraRef
	if err := json.Unmarshal(raw, &r); err != nil {
		return jiraRef{}, fmt.Sprintf("the value %s: %v", raw, err)
	}
	return r, ""
}

// readRefs decodes a list of reference objects.
func readRefs(raw json.RawMessage, allowed ...string) ([]jiraRef, string) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Sprintf("the value %s is not a list", raw)
	}
	out := make([]jiraRef, 0, len(list))
	for _, item := range list {
		r, why := readRef(item, allowed...)
		if why != "" {
			return nil, why
		}
		out = append(out, r)
	}
	return out, ""
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// setValue writes raw to field f of is: a refusal Jira answers with, or
// why the value is unhandled.
func (s *jiraSite) setValue(is *JiraIssue, f *fieldDef, raw json.RawMessage) (refusal, unhandled string) {
	null := isNull(raw)
	invalid := "Specify a valid value for " + f.name
	switch f.kind {
	case kindText:
		var v string
		if !null && json.Unmarshal(raw, &v) != nil {
			return "", "not a string"
		}
		if f.id == "summary" {
			if strings.TrimSpace(v) == "" {
				return "You must specify a summary of the issue.", ""
			}
			is.Summary = v
			return "", ""
		}
		return s.store(is, f, null || v == "", raw)
	case kindDoc:
		if !null && !validDoc(raw) {
			return "Operation value must be an Atlassian Document (see the Atlassian Document Format)", ""
		}
		if f.id == "description" {
			is.Description = nil
			if !null {
				is.Description = slices.Clone(raw)
			}
			return "", ""
		}
		return s.store(is, f, null, raw)
	case kindNumber:
		var v float64
		if !null && json.Unmarshal(raw, &v) != nil {
			return "Operation value must be a number", ""
		}
		return s.store(is, f, null, raw)
	case kindDate:
		var v string
		if !null && (json.Unmarshal(raw, &v) != nil || !isDate(v)) {
			return "Error parsing date string: " + string(raw), ""
		}
		if f.id == "duedate" {
			is.DueDate = v
			return "", ""
		}
		return s.store(is, f, null, raw)
	case kindDateTime:
		var v string
		if null {
			return s.store(is, f, true, nil)
		}
		t, err := time.Parse("2006-01-02T15:04:05.000-0700", strings.Trim(string(raw), `"`))
		if json.Unmarshal(raw, &v) != nil || err != nil {
			return "Error parsing time string: " + string(raw), ""
		}
		return s.store(is, f, false, mustJSON(jiraTime(t)))
	case kindLabels:
		var v []string
		if !null && json.Unmarshal(raw, &v) != nil {
			return "", "not a list of labels"
		}
		for _, l := range v {
			if strings.ContainsFunc(l, unicode.IsSpace) {
				return "The label '" + l + "' contains spaces which is invalid.", ""
			}
		}
		if f.id == "labels" {
			is.Labels = v
			return "", ""
		}
		return s.store(is, f, len(v) == 0, mustJSON(orEmpty(v)))
	case kindUser:
		return s.setUser(is, f, raw, null, invalid)
	case kindUsers:
		return s.setUsers(is, f, raw, null, invalid)
	case kindOption, kindCascade:
		if null {
			return s.store(is, f, true, nil)
		}
		v, msg, why := s.optionValue(f, raw)
		if why != "" || msg != "" {
			return msg, why
		}
		return s.store(is, f, false, mustJSON(v))
	case kindOptions:
		return s.setOptions(is, f, raw, null, invalid)
	case kindPriority:
		return s.setPriority(is, raw, null, invalid)
	case kindIssueType:
		return s.setIssueType(is, raw)
	case kindComponents:
		return s.setComponents(is, raw, null)
	case kindParent:
		return s.setParent(is, raw, null)
	case kindSprint:
		return s.setSprintField(is, raw, null)
	case kindUnsupported, kindReadOnly, kindRank, kindTimeEstimate:
	}
	return f.name + " cannot be set.", ""
}

// setUser writes a user field: the assignee, the reporter or a custom
// user field.
func (s *jiraSite) setUser(is *JiraIssue, f *fieldDef, raw json.RawMessage, null bool, invalid string) (refusal, unhandled string) {
	account := ""
	if !null {
		r, why := readRef(raw, "accountId")
		if why != "" {
			return "", why
		}
		if u := s.user(r.AccountID); u == nil || u.Inactive {
			return invalid, ""
		}
		account = r.AccountID
	}
	switch f.id {
	case "assignee":
		if account != "" && !s.assignable(s.project(is.Project), account) {
			return "User '" + account + "' cannot be assigned issues.", ""
		}
		is.Assignee = account
	case "reporter":
		is.Reporter = account
	default:
		return s.store(is, f, null, mustJSON(s.userJSON(account)))
	}
	return "", ""
}

func (s *jiraSite) setUsers(is *JiraIssue, f *fieldDef, raw json.RawMessage, null bool, invalid string) (refusal, unhandled string) {
	refs, why := readRefs(raw, "accountId")
	if why != "" && !null {
		return "", why
	}
	users := []any{}
	for _, r := range refs {
		if u := s.user(r.AccountID); u == nil {
			return invalid, ""
		}
		users = append(users, s.userJSON(r.AccountID))
	}
	return s.store(is, f, len(users) == 0, mustJSON(users))
}

func (s *jiraSite) setOptions(is *JiraIssue, f *fieldDef, raw json.RawMessage, null bool, invalid string) (refusal, unhandled string) {
	refs, why := readRefs(raw, "id", "value")
	if why != "" && !null {
		return "", why
	}
	values := []any{}
	for _, r := range refs {
		o := findOption(f.options, r)
		if o == nil {
			return invalid, ""
		}
		values = append(values, s.optionJSON(*o))
	}
	return s.store(is, f, len(values) == 0, mustJSON(values))
}

func (s *jiraSite) setPriority(is *JiraIssue, raw json.RawMessage, null bool, invalid string) (refusal, unhandled string) {
	if null {
		is.Priority = ""
		return "", ""
	}
	r, why := readRef(raw, "id", "name")
	if why != "" {
		return "", why
	}
	for _, pr := range s.Priorities {
		if pr.ID == r.ID || (r.ID == "" && pr.Name == r.Name) {
			is.Priority = pr.ID
			return "", ""
		}
	}
	return invalid, ""
}

func (s *jiraSite) setIssueType(is *JiraIssue, raw json.RawMessage) (refusal, unhandled string) {
	p := s.project(is.Project)
	r, why := readRef(raw, "id")
	if why != "" {
		return "", why
	}
	t := p.issueType(r.ID)
	if t == nil || t.Subtask != p.issueType(is.Type).Subtask {
		return "Specify a valid issue type", ""
	}
	is.Type = t.ID
	return "", ""
}

func (s *jiraSite) setComponents(is *JiraIssue, raw json.RawMessage, null bool) (refusal, unhandled string) {
	p := s.project(is.Project)
	refs, why := readRefs(raw, "id", "name")
	if why != "" && !null {
		return "", why
	}
	var ids []string
	for _, r := range refs {
		i := slices.IndexFunc(p.Components, func(c JiraNamed) bool { return c.ID == r.ID || (r.ID == "" && c.Name == r.Name) })
		if i < 0 {
			return "Component name '" + r.Name + r.ID + "' is not valid", ""
		}
		ids = append(ids, p.Components[i].ID)
	}
	is.Components = ids
	return "", ""
}

func (s *jiraSite) setParent(is *JiraIssue, raw json.RawMessage, null bool) (refusal, unhandled string) {
	if null {
		is.Parent = ""
		return "", ""
	}
	r, why := readRef(raw, "id", "key")
	if why != "" {
		return "", why
	}
	parent := s.issue(r.ID + r.Key)
	if parent == nil || parent.ID == is.ID {
		return "Could not find issue by id or key.", ""
	}
	is.Parent = parent.ID
	return "", ""
}

// setSprintField moves is into the sprint raw names, or out of its open
// sprints when null. A closed sprint is refused.
func (s *jiraSite) setSprintField(is *JiraIssue, raw json.RawMessage, null bool) (refusal, unhandled string) {
	var id int64
	if !null {
		if err := json.Unmarshal(raw, &id); err != nil {
			return "", "the sprint value is not a sprint ID"
		}
		sp := s.sprint(id)
		if sp == nil {
			return "Sprint with id " + strconv.FormatInt(id, 10) + " does not exist", ""
		}
		if sp.State == "closed" {
			return "Issue can be assigned only active or future sprints.", ""
		}
	}
	is.Sprints = slices.DeleteFunc(is.Sprints, s.openSprint)
	if id != 0 && !slices.Contains(is.Sprints, id) {
		is.Sprints = append(is.Sprints, id)
	}
	return "", ""
}

// store keeps a custom field's value as Jira reads it back, or removes it.
func (s *jiraSite) store(is *JiraIssue, f *fieldDef, empty bool, read json.RawMessage) (refusal, unhandled string) {
	if !f.custom {
		return "", "the fake keeps no value of system field " + f.id
	}
	if empty {
		delete(is.Values, f.id)
		return "", ""
	}
	if is.Values == nil {
		is.Values = map[string]json.RawMessage{}
	}
	is.Values[f.id] = slices.Clone(read)
	return "", ""
}

// optionValue is a select's or cascading select's value as Jira reads it.
func (s *jiraSite) optionValue(f *fieldDef, raw json.RawMessage) (value map[string]any, refusal, unhandled string) {
	allowed := []string{"id", "value"}
	if f.kind == kindCascade {
		allowed = append(allowed, "child")
	}
	r, why := readRef(raw, allowed...)
	if why != "" {
		return nil, "", why
	}
	o := findOption(f.options, r)
	if o == nil {
		return nil, "Option value '" + r.Value + r.ID + "' is not valid", ""
	}
	value = s.optionJSON(*o)
	if r.Child != nil {
		c := findOption(o.Children, *r.Child)
		if c == nil {
			return nil, "Option value '" + r.Child.Value + r.Child.ID + "' is not valid", ""
		}
		value["child"] = s.optionJSON(*c)
	}
	return value, "", ""
}

func findOption(opts []JiraOption, r jiraRef) *JiraOption {
	for i := range opts {
		if o := &opts[i]; !o.Disabled && (o.ID == r.ID || (r.ID == "" && o.Value == r.Value)) {
			return o
		}
	}
	return nil
}

// assignable reports whether account can be assigned p's issues.
func (s *jiraSite) assignable(p *JiraProject, account string) bool {
	if len(p.Assignable) > 0 {
		return slices.Contains(p.Assignable, account)
	}
	u := s.user(account)
	return u != nil && !u.Inactive && u.AccountType == "atlassian"
}

func isDate(v string) bool {
	_, err := time.Parse(time.DateOnly, v)
	return err == nil
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("trackerfake: encode %T: %v", v, err))
	}
	return raw
}

// cloneIssue is a deep copy of is.
func cloneIssue(is JiraIssue) JiraIssue {
	var out JiraIssue
	if err := json.Unmarshal(mustJSON(is), &out); err != nil {
		panic(fmt.Sprintf("trackerfake: copy an issue: %v", err))
	}
	return out
}
