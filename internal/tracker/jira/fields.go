package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"agent-overflow/internal/tracker/trackerapi"
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

// Field is one entry of the site's field catalog. It holds no reference
// type, so a copy shares nothing with the catalog it came from.
type Field struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Custom bool   `json:"custom"`
	Schema Schema `json:"schema"`
}

// Catalog is the site's field catalog as one read returned it. It cannot
// be changed: its fields are reached only through methods that return
// copies, so the one value the client caches is handed to every caller
// as it is. Field finds an entry by ID through an index built once.
type Catalog struct {
	fields []Field
	byID   map[string]int
	// firstCustom is the first field of each custom type, by type key.
	firstCustom map[string]int
}

// NewCatalog is a catalog of a copy of fields, for a caller that builds
// one (a test of code that reads the catalog); the client builds its own
// from Jira's answer.
func NewCatalog(fields []Field) *Catalog { return newCatalog(slices.Clone(fields)) }

// newCatalog is a catalog that owns fields.
func newCatalog(fields []Field) *Catalog {
	c := &Catalog{fields: fields, byID: make(map[string]int, len(fields)), firstCustom: map[string]int{}}
	for i, f := range fields {
		if _, dup := c.byID[f.ID]; !dup {
			c.byID[f.ID] = i
		}
		if t := f.Schema.Custom; t != "" {
			if _, seen := c.firstCustom[t]; !seen {
				c.firstCustom[t] = i
			}
		}
	}
	return c
}

// Len is the number of fields.
func (c *Catalog) Len() int { return len(c.fields) }

// All yields every field in Jira's order.
func (c *Catalog) All() iter.Seq[Field] {
	return func(yield func(Field) bool) {
		for _, f := range c.fields {
			if !yield(f) {
				return
			}
		}
	}
}

// Field is the field with exactly id.
func (c *Catalog) Field(id string) (Field, bool) {
	i, ok := c.byID[id]
	if !ok {
		return Field{}, false
	}
	return c.fields[i], true
}

// OfCustomType is the first field, in Jira's order, of the custom field
// type custom (its full key, such as
// com.pyxis.greenhopper.jira:gh-sprint).
func (c *Catalog) OfCustomType(custom string) (Field, bool) {
	i, ok := c.firstCustom[custom]
	if !ok {
		return Field{}, false
	}
	return c.fields[i], true
}

// Fields returns the field catalog, every field the user can see with its
// schema, from the client's cache once read. Callers that miss the cache
// together each read it. Every caller shares the one Catalog, which no
// caller can change.
func (c *Client) Fields(ctx context.Context) (*Catalog, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if cat, ok := c.catalog.get(struct{}{}); ok {
		return cat, nil
	}
	epoch := c.catalog.epoch()
	var fields []Field
	resp, err := c.call(ctx, trackerapi.Request{Path: apiPath + "field"}, &fields)
	if err != nil {
		return nil, fmt.Errorf("jira: field catalog: %w", err)
	}
	cat := newCatalog(fields)
	c.catalog.put(epoch, resp.CredentialMark(), struct{}{}, cat)
	return cat, nil
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
	err := createMetaPages(ctx, c, path, func() { out = nil }, func(p createMetaPage) int {
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
	err := createMetaPages(ctx, c, path, func() { s = Screen{} }, func(p createMetaPage) int {
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

// createMetaPages reads every page of path as one walk (walk.go),
// handing each to take, which returns how many items it held; reset
// drops what take kept before the walk starts again.
func createMetaPages(ctx context.Context, c *Client, path string, reset func(), take func(createMetaPage) int) error {
	_, err := walk(ctx, func(ctx context.Context) (struct{}, error) {
		reset()
		return struct{}{}, createMetaWalk(ctx, c, path, take)
	})
	return err
}

// createMetaWalk is one walk of createMetaPages.
func createMetaWalk(ctx context.Context, c *Client, path string, take func(createMetaPage) int) error {
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
