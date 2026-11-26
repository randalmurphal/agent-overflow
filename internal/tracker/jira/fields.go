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
	"time"

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
	// absent are IDs a read naming them found missing (FieldsNaming),
	// carried from the catalog this one replaced.
	absent map[string]bool
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
	if cat, ok := c.catalog.get(ctx, struct{}{}); ok {
		return cat, nil
	}
	return c.readFields(ctx, nil, nil)
}

// FieldsNaming is Fields, read again when the cached catalog lacks one of
// ids that no read has found missing yet: a field added in Jira after the
// catalog was read is found by the first read or write that names it, at
// the cost of one request, and a read that names only fields the catalog
// holds sends none. An ID still missing after the read again is recorded
// on the new catalog, so naming it again sends nothing.
func (c *Client) FieldsNaming(ctx context.Context, ids ...string) (*Catalog, error) {
	cat, err := c.Fields(ctx)
	if err != nil {
		return nil, err
	}
	var lacking []string
	for _, id := range ids {
		if _, ok := cat.byID[id]; !ok && !cat.absent[id] && !slices.Contains(lacking, id) {
			lacking = append(lacking, id)
		}
	}
	if len(lacking) == 0 {
		return cat, nil
	}
	return c.readFields(ctx, cat, lacking)
}

// readFields reads the catalog and caches it. A catalog read again
// because it lacked the IDs lacking carries prior's absent IDs and
// records those of lacking it still lacks.
func (c *Client) readFields(ctx context.Context, prior *Catalog, lacking []string) (*Catalog, error) {
	epoch := c.catalog.epoch()
	var fields []Field
	resp, err := c.call(ctx, trackerapi.Request{Path: apiPath + "field"}, &fields)
	if err != nil {
		return nil, fmt.Errorf("jira: field catalog: %w", err)
	}
	cat := newCatalog(fields)
	if prior != nil {
		cat.absent = map[string]bool{}
		for id := range prior.absent {
			if _, ok := cat.byID[id]; !ok {
				cat.absent[id] = true
			}
		}
		for _, id := range lacking {
			if _, ok := cat.byID[id]; !ok {
				cat.absent[id] = true
			}
		}
	}
	c.catalog.put(epoch, resp.CredentialMark(), struct{}{}, cat)
	return cat, nil
}

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

// Demanded reports a field a create or a move must be given a value of:
// required by its screen, with no default Jira fills in. It is the one
// rule for what a person must supply (PlanCreate, a move's check, and
// tracker.ScreenField: Required and not Defaulted).
func (f FieldMeta) Demanded() bool { return f.Required && !f.HasDefault }

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
	_, err := createMetaPages(ctx, c, path, func() { out = nil }, func(p createMetaPage) int {
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
// A screen read is kept for createScreenAge, so a create planned after
// its form, or a tool, read the screen reads it once, until a create
// refuses a field on it (forgetCreateScreen). The value is the caller's
// own.
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
	key := [2]string{project, issueTypeID}
	if s, ok := c.createScreens.get(ctx, key); ok {
		return s, nil
	}
	epoch := c.createScreens.epoch()
	path := "issue/createmeta/" + url.PathEscape(project) + "/issuetypes/" + url.PathEscape(issueTypeID)
	var s Screen
	mark, err := createMetaPages(ctx, c, path, func() { s = Screen{} }, func(p createMetaPage) int {
		fields := append(p.Fields, p.Results...)
		for _, f := range fields {
			s.Fields = append(s.Fields, f.meta(""))
		}
		return len(fields)
	})
	if err != nil {
		return Screen{}, fmt.Errorf("jira: create screen of %s type %s: %w", project, issueTypeID, err)
	}
	c.createScreens.put(epoch, mark, key, s)
	return s, nil
}

// forgetCreateScreen drops the kept create screen of a project and issue
// type ID after a create that refused a field on it, which the screen
// may have changed under: the next read of it is Jira's.
func (c *Client) forgetCreateScreen(project, issueTypeID string) {
	c.createScreens.drop([2]string{project, issueTypeID})
}

// createScreenAge is how long a create screen is kept. Jira sends no
// signal when an administrator changes a screen, so it is kept no longer
// than a create's form or a tool call takes to plan the create after
// reading it.
const createScreenAge = time.Minute

// createScreenCacheSize is the most create screens kept, by project and
// issue type.
const createScreenCacheSize = 8

func cloneScreen(s Screen) Screen {
	s.Fields = slices.Clone(s.Fields)
	for i := range s.Fields {
		f := &s.Fields[i]
		f.Operations = slices.Clone(f.Operations)
		f.Allowed = cloneAllowed(f.Allowed)
	}
	return s
}

func cloneAllowed(vs []AllowedValue) []AllowedValue {
	vs = slices.Clone(vs)
	for i := range vs {
		vs[i].Raw = slices.Clone(vs[i].Raw)
		vs[i].Children = cloneAllowed(vs[i].Children)
	}
	return vs
}

// createMetaPages reads every page of path as one walk (walk.go),
// handing each to take, which returns how many items it held; reset
// drops what take kept before the walk starts again. It returns the
// credential read the pages were drawn under.
func createMetaPages(ctx context.Context, c *Client, path string, reset func(), take func(createMetaPage) int) (trackerapi.CredentialMark, error) {
	return trackerapi.Walk(ctx, func(ctx context.Context) (trackerapi.CredentialMark, error) {
		reset()
		return createMetaWalk(ctx, c, path, take)
	})
}

// createMetaWalk is one walk of createMetaPages.
func createMetaWalk(ctx context.Context, c *Client, path string, take func(createMetaPage) int) (trackerapi.CredentialMark, error) {
	var mark trackerapi.CredentialMark
	for start := 0; ; {
		q := url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {strconv.Itoa(createMetaPageSize)}}
		var page createMetaPage
		resp, err := c.call(ctx, trackerapi.Request{Path: apiPath + path, Query: q}, &page)
		if err != nil {
			return mark, err
		}
		mark = resp.CredentialMark()
		n := take(page)
		start += n
		if start >= page.Total {
			return mark, nil
		}
		if n == 0 {
			return mark, fmt.Errorf("an empty page at %d of %d", start, page.Total)
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
