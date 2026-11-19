package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Schema is a field's type as Jira reports it. Type is the JSON type
// ("string", "number", "array", "option", "user", ...), Items the type of
// an array's items, System the system field it is, and Custom the custom
// field type's key.
type Schema struct {
	Type     string `json:"type"`
	Items    string `json:"items,omitempty"`
	System   string `json:"system,omitempty"`
	Custom   string `json:"custom,omitempty"`
	CustomID int64  `json:"customId,omitempty"`
}

// customType is the custom type's short key: "select" for
// com.atlassian.jira.plugin.system.customfieldtypes:select.
func (s Schema) customType() string {
	if i := strings.LastIndex(s.Custom, ":"); i >= 0 {
		return s.Custom[i+1:]
	}
	return s.Custom
}

// RichText reports a field whose value is a document: the client writes
// a Text input to it as Markdown and FieldValue.RichText reads it. It is
// the rule the client's value builder follows (the description, the
// environment and textarea custom fields).
func (s Schema) RichText() bool { return kindOf(s) == kindRich }

// Field is one entry of the site's field catalog.
type Field struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Custom      bool     `json:"custom"`
	Schema      Schema   `json:"schema"`
	ClauseNames []string `json:"clauseNames"`
	Navigable   bool     `json:"navigable"`
	Searchable  bool     `json:"searchable"`
	Orderable   bool     `json:"orderable"`
}

// cloneFields is a copy of fs sharing no memory with it.
func cloneFields(fs []Field) []Field {
	out := slices.Clone(fs)
	for i := range out {
		out[i].ClauseNames = slices.Clone(out[i].ClauseNames)
	}
	return out
}

// Fields returns every field the user can see, with its schema, from the
// client's cache once read. Callers that miss the cache together each
// read it. The slice is the caller's own.
func (c *Client) Fields(ctx context.Context) ([]Field, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if fields, ok := c.catalog.get(struct{}{}); ok {
		return fields, nil
	}
	epoch := c.catalog.epoch()
	var fields []Field
	if err := c.get(ctx, "field", nil, &fields); err != nil {
		return nil, fmt.Errorf("jira: field catalog: %w", err)
	}
	c.catalog.put(epoch, struct{}{}, fields)
	return fields, nil
}

// ForgetFields drops the cached field catalog; a read in flight stores
// nothing.
func (c *Client) ForgetFields() { c.catalog.clear() }

// FieldMeta is a field as a screen offers it: on create, on edit, or on
// a transition.
type FieldMeta struct {
	ID       string
	Key      string
	Name     string
	Required bool
	Schema   Schema
	// Operations are what an update may do: set, add, remove, edit. Empty
	// when the metadata names none.
	Operations []string
	// Allowed lists the values the field takes; nil when the metadata
	// lists none (a free value, or a list too long to send).
	Allowed    []AllowedValue
	HasDefault bool
	// AutoCompleteURL is where Jira suggests values for the field.
	AutoCompleteURL string
}

// AllowedValue is one value a field takes: an option, priority,
// component, version, issue type or resolution. Raw is the value as Jira
// sent it, for the details this type does not name.
type AllowedValue struct {
	ID       string
	Name     string
	Disabled bool
	Children []AllowedValue // the child options of a cascading select
	Raw      json.RawMessage
}

type fieldMetaJSON struct {
	FieldID         string            `json:"fieldId"`
	Key             string            `json:"key"`
	Name            string            `json:"name"`
	Required        bool              `json:"required"`
	Schema          Schema            `json:"schema"`
	Operations      []string          `json:"operations"`
	AllowedValues   []json.RawMessage `json:"allowedValues"`
	HasDefaultValue bool              `json:"hasDefaultValue"`
	AutoCompleteURL string            `json:"autoCompleteUrl"`
}

func (m fieldMetaJSON) meta(id string) FieldMeta {
	if id == "" {
		id = firstNonEmpty(m.FieldID, m.Key)
	}
	out := FieldMeta{
		ID: id, Key: m.Key, Name: m.Name, Required: m.Required, Schema: m.Schema,
		Operations: m.Operations, HasDefault: m.HasDefaultValue, AutoCompleteURL: m.AutoCompleteURL,
	}
	if m.AllowedValues != nil {
		out.Allowed = make([]AllowedValue, 0, len(m.AllowedValues))
		for _, raw := range m.AllowedValues {
			out.Allowed = append(out.Allowed, allowedValue(raw))
		}
	}
	return out
}

// allowedValue reads one allowed value. Its ID is a string or, for some
// app fields, a number; its name is name, value or key by type.
func allowedValue(raw json.RawMessage) AllowedValue {
	var v struct {
		ID       json.RawMessage   `json:"id"`
		Name     string            `json:"name"`
		Value    string            `json:"value"`
		Key      string            `json:"key"`
		Disabled bool              `json:"disabled"`
		Children []json.RawMessage `json:"children"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return AllowedValue{Name: jsonText(raw), Raw: raw}
	}
	a := AllowedValue{ID: idText(v.ID), Name: firstNonEmpty(v.Name, v.Value, v.Key), Disabled: v.Disabled, Raw: raw}
	for _, child := range v.Children {
		a.Children = append(a.Children, allowedValue(child))
	}
	return a
}

// idText reads an ID sent as a string or a number.
func idText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// Screen is the fields one screen offers, in the order Jira listed them.
type Screen struct {
	Fields []FieldMeta
}

// Field finds the field named by nameOrID: its ID, or its name (case
// ignored, an exact spelling winning among several). No match or several
// is a *MatchError listing the screen's fields.
func (s Screen) Field(nameOrID string) (FieldMeta, error) {
	cands := make([]Candidate, len(s.Fields))
	for i, f := range s.Fields {
		cands[i] = Candidate{ID: f.ID, Name: f.Name}
	}
	hit, err := match("field on this screen", nameOrID, cands)
	if err != nil {
		return FieldMeta{}, err
	}
	for _, f := range s.Fields {
		if f.ID == hit.ID {
			return f, nil
		}
	}
	return FieldMeta{}, fmt.Errorf("jira: field %s vanished from its screen", hit.ID)
}

// screenOf turns a field map (edit and transition metadata) into a
// Screen ordered by field ID, the map having no order.
func screenOf(fields map[string]fieldMetaJSON) Screen {
	ids := make([]string, 0, len(fields))
	for id := range fields {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	s := Screen{Fields: make([]FieldMeta, 0, len(ids))}
	for _, id := range ids {
		s.Fields = append(s.Fields, fields[id].meta(id))
	}
	return s
}

// createMetaPage is a page of create metadata. The spec names both the
// list keys each page may use.
type createMetaPage struct {
	IssueTypes  []issueTypeJSON `json:"issueTypes"`
	IssueTypes2 []issueTypeJSON `json:"createMetaIssueType"`
	Fields      []fieldMetaJSON `json:"fields"`
	Results     []fieldMetaJSON `json:"results"`
	Total       int             `json:"total"`
}

// createMetaPageSize is the most create metadata a page holds.
const createMetaPageSize = 200

// CreateIssueTypes returns the issue types the user can create in a
// project.
func (c *Client) CreateIssueTypes(ctx context.Context, project string) ([]IssueType, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if err := checkProject(project); err != nil {
		return nil, err
	}
	path := "issue/createmeta/" + url.PathEscape(project) + "/issuetypes"
	var out []IssueType
	err := createMetaPages(ctx, c, path, func(p createMetaPage) int {
		types := append(p.IssueTypes, p.IssueTypes2...)
		for _, t := range types {
			out = append(out, t.issueType())
		}
		return len(types)
	})
	if err != nil {
		return out, fmt.Errorf("jira: issue types to create in %s: %w", project, err)
	}
	return out, nil
}

// CreateMeta returns the create screen of a project and issue type ID.
func (c *Client) CreateMeta(ctx context.Context, project, issueTypeID string) (Screen, error) {
	if err := c.admit(ctx); err != nil {
		return Screen{}, err
	}
	if err := checkProject(project); err != nil {
		return Screen{}, err
	}
	if err := checkID("issue type", issueTypeID); err != nil {
		return Screen{}, err
	}
	path := "issue/createmeta/" + url.PathEscape(project) + "/issuetypes/" + url.PathEscape(issueTypeID)
	var s Screen
	err := createMetaPages(ctx, c, path, func(p createMetaPage) int {
		fields := append(p.Fields, p.Results...)
		for _, f := range fields {
			s.Fields = append(s.Fields, f.meta(""))
		}
		return len(fields)
	})
	if err != nil {
		return Screen{}, fmt.Errorf("jira: create screen of %s type %s: %w", project, issueTypeID, err)
	}
	return s, nil
}

// createMetaPages reads every page of path, handing each to take, which
// returns how many items it held.
func createMetaPages(ctx context.Context, c *Client, path string, take func(createMetaPage) int) error {
	for start := 0; ; {
		q := url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {strconv.Itoa(createMetaPageSize)}}
		var page createMetaPage
		if err := c.get(ctx, path, q, &page); err != nil {
			return err
		}
		n := take(page)
		start += n
		if start >= page.Total {
			return nil
		}
		if n == 0 {
			return fmt.Errorf("an empty page at %d of %d", start, page.Total)
		}
	}
}

// EditMeta returns the edit screen of an issue: the fields the user may
// change on it.
func (c *Client) EditMeta(ctx context.Context, issue string) (Screen, error) {
	if err := c.admit(ctx); err != nil {
		return Screen{}, err
	}
	path, err := issuePath(issue)
	if err != nil {
		return Screen{}, err
	}
	var v struct {
		Fields map[string]fieldMetaJSON `json:"fields"`
	}
	if err := c.get(ctx, path+"/editmeta", nil, &v); err != nil {
		return Screen{}, fmt.Errorf("jira: edit screen of %s: %w", issue, err)
	}
	return screenOf(v.Fields), nil
}
