package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/tracker/adf"
)

// Input is what a person or an agent supplies for one field: Clear, one
// Text, several Texts, the IDs of values picked from a list, or RawJSON.
// The field's schema decides what text means (an option name, a user, a
// date, Markdown); there are no in-band conventions.
type Input struct {
	kind  inputKind
	texts []string
	raw   json.RawMessage
	// from is the Markdown a rich text edit or clear was written from.
	from *string
	// replaces are the texts a plain text edit or clear was written from.
	replaces *[]string
}

type inputKind uint8

const (
	inputNone inputKind = iota
	inputClear
	inputText
	inputTexts
	inputIDs
	inputRaw
)

// Clear empties the field: null, an empty list, or no parent.
func Clear() Input { return Input{kind: inputClear} }

// Text is one value, resolved by the field's type: an option, priority,
// component or version by name or ID; a user by account ID, exact name
// or email; a parent issue by key or ID; a number; an ISO 8601 date; an
// RFC 3339 date-time; a label; plain text; Markdown for rich text; a
// sprint by ID or by the name of an open sprint on the project's scrum
// boards; a team ID.
func Text(s string) Input { return Input{kind: inputText, texts: []string{s}} }

// Texts is several values for a multi-value field (options, labels,
// users, components, versions), or a cascading select's parent and
// child. The list replaces the field's value.
func Texts(ss ...string) Input { return Input{kind: inputTexts, texts: slices.Clone(ss)} }

// IDs names one or more values by ID, as picked from a list the field
// offers: an option, priority, component or version by its ID, a user by
// account ID, a parent issue by its numeric ID, a sprint by its numeric
// ID, a team by its ID, a label by its text. An ID is matched exactly and
// never as a name: one that names none of the field's values now is a
// *MatchError with Picked set, so a value picked from a list that has
// changed since is refused rather than read as another value's name. A
// field whose values have no IDs (text, numbers, dates, rich text) takes
// no IDs. For a list field the IDs replace the list.
func IDs(ids ...string) Input { return Input{kind: inputIDs, texts: slices.Clone(ids)} }

// RawJSON is the API value itself, sent unchanged: the escape hatch for a
// field type this package does not build.
func RawJSON(v json.RawMessage) Input {
	return Input{kind: inputRaw, raw: slices.Clone(v)}
}

// EditedFrom records the Markdown the Markdown of a rich text Text, or a
// Clear of a rich text field, was written from: the field as the writer
// saw it. A plan refuses with a *StaleError when the stored document
// renders to other Markdown by then, an empty field rendering to the
// empty string, so the edit never undoes a change made meanwhile. Content
// Markdown cannot show is kept whatever it holds, since the edit is
// applied to the stored document.
func (in Input) EditedFrom(md string) Input {
	in.from = &md
	return in
}

// Replacing records the texts a Text, Texts or Clear of a plain text
// field (one text or a list of them) was written from: a plan refuses
// with a *StaleError when the field stores other texts by then, none
// for an empty field.
func (in Input) Replacing(texts ...string) Input {
	t := slices.Clone(texts)
	in.replaces = &t
	return in
}

// Value is the API value of one field, ready to send, and what a UI
// shows before it is sent.
type Value struct {
	FieldID string
	Name    string
	// Fields is the JSON sent under "fields"; Update the operations sent
	// under "update" instead (clearing a parent). One of them is set.
	Fields json.RawMessage
	Update json.RawMessage
	// Display is the value as a person reads it: names for IDs, the
	// Markdown a rich text value will store.
	Display string
	// Warnings are notes on the value: content a rich text edit reshapes,
	// a value Jira will check that this package could not.
	Warnings []string
	// Images are the local images a rich text value embeds, uploaded to
	// the issue when a plan is applied; Fields names them by placeholder
	// until then.
	Images []Image
	// Unchanged reports a rich text edit that stores the document already
	// there.
	Unchanged bool

	rich     bool          // a rich text field's value, a clear included
	replaces bool          // a raw value replacing a stored document with content
	doc      *adf.Node     // a rich text value's document; nil for a clear
	base     *adf.Node     // the stored document a rich text value replaces
	imgs     []*Image      // the staged images doc embeds
	edits    []adf.Warning // the conversion's warnings, for CheckLoss
}

// CheckLoss reports whether a rich text edit may be written: a
// *adf.LossError when it would lose content Markdown cannot show, or
// replaces a stored document that has content with a raw one, and the
// user has not accepted that.
func (v Value) CheckLoss(accepted bool) error {
	if v.replaces && !accepted {
		return &adf.LossError{Lost: []adf.Warning{{
			Msg: v.Name + ": a raw document replaces the stored one whole, and nothing shows what it drops",
		}}}
	}
	return adf.CheckLoss(v.edits, accepted)
}

// rawRich builds a raw value for a rich text field: a full replacement
// document, with the protections of any rich text write. It must be a
// valid document (null is a clear); the value records the stored
// document it replaces, which a plan rechecks; and replacing a stored
// document that has content needs the caller's loss acceptance.
func (b *builder) rawRich(f FieldMeta, raw json.RawMessage, v Value) (Value, error) {
	v.rich, v.base = true, b.opts.Base
	if isNull(raw) {
		if f.Required {
			return v, fmt.Errorf("jira: %q is required and cannot be cleared", f.Name)
		}
		v.Fields, v.Display = json.RawMessage(`null`), "(none)"
		v.Unchanged = v.base == nil
		v.replaces = !adf.Blank(v.base)
		return v, nil
	}
	var doc *adf.Node
	if err := json.Unmarshal(raw, &doc); err != nil {
		return v, fmt.Errorf("jira: %q: the raw value is not a document: %w", f.Name, err)
	}
	if err := adf.Validate(doc); err != nil {
		return v, fmt.Errorf("jira: %q: the raw value is not a valid document: %w", f.Name, err)
	}
	// Emptiness is the content's, as for Markdown (see markdown): a
	// document showing nothing empties a required field as a null would.
	if f.Required && adf.Blank(doc) {
		return v, fmt.Errorf("jira: %q is required and the raw document shows nothing", f.Name)
	}
	v.doc, v.Fields, v.Display = doc, raw, adf.ToMarkdown(doc)
	v.Unchanged = v.base != nil && adf.Equal(doc, v.base)
	v.replaces = !v.Unchanged && !adf.Blank(v.base)
	if v.replaces {
		v.Warnings = append(v.Warnings, f.Name+": the raw document replaces the stored one whole")
	}
	return v, nil
}

// ValueOptions says what a value is built for.
type ValueOptions struct {
	// Issue is the issue edited, empty when creating; Project the project
	// created in. Users for the assignee are searched among those who can
	// be assigned there.
	Issue   string
	Project string
	// ImageDir is the folder a Markdown body's local images are read
	// from, an absolute path; empty refuses them. See ImageDir on
	// EditInput.
	ImageDir string
	// Base is the stored document a rich text value edits, kept where the
	// Markdown leaves it unchanged; nil converts the Markdown afresh.
	Base *adf.Node
}

// Value builds the API value of field f from in. Every check that needs
// no write runs here: a name that matches several values or none is a
// *MatchError listing them, a required field cannot be cleared, a field
// whose type this package does not build takes RawJSON only. Local
// images are read and checked but not uploaded; a plan uploads them.
func (c *Client) Value(ctx context.Context, f FieldMeta, in Input, opts ValueOptions) (Value, error) {
	if err := c.admit(ctx); err != nil {
		return Value{}, err
	}
	b := &builder{c: c, opts: opts, stage: newImageStage(opts.ImageDir, c.maxImages)}
	v, err := b.build(ctx, f, in)
	return v, notSentErr(err)
}

// builder builds the values of one plan; its stage collects the images
// they embed, so an image named twice is uploaded once.
type builder struct {
	c     *Client
	opts  ValueOptions
	stage *imageStage
	// asked reports a conversion that may have read Jira: it resolved a
	// mention.
	asked bool
}

func (b *builder) build(ctx context.Context, f FieldMeta, in Input) (Value, error) {
	v := Value{FieldID: f.ID, Name: f.Name}
	k := kindOf(f.Schema)
	r := rules[k]
	if in.from != nil && (k != kindRich || in.kind != inputText && in.kind != inputClear) {
		return v, fmt.Errorf("jira: %q: EditedFrom applies to Markdown and clears of rich text", f.Name)
	}
	if in.replaces != nil && (k != kindText && k != kindStrings || in.kind != inputText && in.kind != inputTexts && in.kind != inputClear) {
		return v, fmt.Errorf("jira: %q: Replacing applies to typed text and clears of plain text", f.Name)
	}
	if len(f.Operations) > 0 && !slices.Contains(f.Operations, "set") {
		return v, fmt.Errorf("jira: %q cannot be set here (it takes %s)", f.Name, strings.Join(f.Operations, ", "))
	}
	switch in.kind {
	case inputNone:
		return v, fmt.Errorf("jira: %q: no value given", f.Name)
	case inputClear:
		if f.Required {
			return v, fmt.Errorf("jira: %q is required and cannot be cleared", f.Name)
		}
		v.Fields, v.Update, v.Display = clearValue(f, r)
		if k == kindRich {
			if in.from != nil && adf.ToMarkdown(b.opts.Base) != *in.from {
				return v, &StaleError{Issue: b.opts.Issue, What: f.Name}
			}
			// A clear replaces the stored document as an edit does, so a
			// plan rechecks it the same way, and CheckLoss refuses it until
			// its loss is accepted when the document holds content Markdown
			// shows only simplified.
			v.rich, v.base = true, b.opts.Base
			v.Unchanged = v.base == nil
			if v.base != nil {
				v.edits = adf.ClearLoss(v.base)
				for _, w := range v.edits {
					v.Warnings = append(v.Warnings, f.Name+": "+w.String())
				}
			}
		}
		return v, nil
	case inputRaw:
		if !json.Valid(in.raw) {
			return v, fmt.Errorf("jira: %q: the raw value is not JSON", f.Name)
		}
		if k == kindRich {
			return b.rawRich(f, in.raw, v)
		}
		if f.Required && emptyValue(in.raw) {
			return v, fmt.Errorf("jira: %q is required and cannot be cleared", f.Name)
		}
		v.Fields, v.Display = in.raw, string(in.raw)
		return v, nil
	case inputText, inputTexts, inputIDs:
		// Built from the texts by the field's kind, below.
	}
	texts := in.texts
	switch {
	case k == kindUnknown:
		return v, fmt.Errorf("jira: %q has type %s, which takes RawJSON only", f.Name, schemaName(f.Schema))
	case in.kind == inputIDs && !r.picked:
		return v, fmt.Errorf("jira: %q takes %s, which has no IDs to pick", f.Name, r.what)
	case len(texts) == 0:
		return v, fmt.Errorf("jira: %q: no values given; Clear empties it", f.Name)
	case r.max > 0 && len(texts) > r.max:
		return v, fmt.Errorf("jira: %q takes %s, not %d values", f.Name, r.what, len(texts))
	}
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			return v, fmt.Errorf("jira: %q: an empty value; Clear empties it", f.Name)
		}
	}
	out, err := r.build(ctx, b, f, in, &v)
	if err != nil {
		return v, err
	}
	if v.Fields == nil {
		if v.Fields, err = json.Marshal(out); err != nil {
			return v, fmt.Errorf("jira: %q: encode the value: %w", f.Name, err)
		}
	}
	return v, nil
}

func schemaName(s Schema) string {
	name := s.Type
	if s.Items != "" {
		name += " of " + s.Items
	}
	if s.Custom != "" {
		name += " (" + s.Custom + ")"
	}
	return name
}

// kind is how values of a field are built, decided by its schema.
type kind uint8

const (
	kindUnknown kind = iota
	kindText
	kindRich
	kindNumber
	kindDate
	kindDateTime
	kindUser
	kindUsers
	kindLabels
	kindStrings
	kindOption
	kindOptions
	kindCascade
	kindNamed
	kindNamedList
	kindIssue
	kindSprint
	kindTeam
)

// kindOf is the one place a schema picks its kind.
func kindOf(s Schema) kind {
	custom := s.customType()
	switch {
	case s.System == "description" || s.System == "environment" || custom == "textarea":
		return kindRich
	case custom == "gh-sprint":
		return kindSprint
	case s.System == "parent" || s.Type == "issuelink":
		return kindIssue
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
	case "priority", "resolution", "issuetype", "securitylevel", "version", "component":
		return kindNamed
	case "team":
		return kindTeam
	case "array":
		switch s.Items {
		case "user":
			return kindUsers
		case "string":
			if s.System == "labels" || custom == "labels" {
				return kindLabels
			}
			return kindStrings
		case "option":
			return kindOptions
		case "component", "version":
			return kindNamedList
		}
	}
	return kindUnknown
}

// rule is how one kind's values are built. build returns the value to
// encode, or sets v.Fields itself, and sets v.Display; it reads in.texts
// as IDs when in is IDs.
type rule struct {
	what   string // what the kind takes, for errors
	max    int    // the most Texts it takes; zero means any number
	list   bool   // its value is a list, so Clear sends []
	picked bool   // its values have IDs, so it takes IDs
	build  func(ctx context.Context, b *builder, f FieldMeta, in Input, v *Value) (any, error)
}

// clearValue is the JSON that empties f, a field of r's kind: a parent
// is removed through an update operation, a list is emptied, anything
// else is set to null.
func clearValue(f FieldMeta, r rule) (fields, update json.RawMessage, display string) {
	switch {
	case f.Schema.System == "parent":
		return nil, json.RawMessage(`[{"set":{"none":true}}]`), "(none)"
	case r.list:
		return json.RawMessage(`[]`), nil, "(none)"
	}
	return json.RawMessage(`null`), nil, "(none)"
}

// rules holds one rule per kind, indexed by it; values_build.go defines
// the builders. An array, so linking the package builds nothing at start.
var rules = [...]rule{
	kindUnknown:   {what: "RawJSON only"},
	kindText:      {what: "one text", max: 1, build: buildText},
	kindRich:      {what: "one Markdown text", max: 1, build: buildRich},
	kindNumber:    {what: "one number", max: 1, build: buildNumber},
	kindDate:      {what: "one date", max: 1, build: buildDate},
	kindDateTime:  {what: "one date-time", max: 1, build: buildDateTime},
	kindUser:      {what: "one user", max: 1, picked: true, build: buildUser},
	kindUsers:     {what: "users", list: true, picked: true, build: buildUsers},
	kindLabels:    {what: "labels", list: true, picked: true, build: buildLabels},
	kindStrings:   {what: "texts", list: true, build: buildStrings},
	kindOption:    {what: "one option", max: 1, picked: true, build: buildOption},
	kindOptions:   {what: "options", list: true, picked: true, build: buildOptions},
	kindCascade:   {what: "an option and a child option", max: 2, picked: true, build: buildCascade},
	kindNamed:     {what: "one value", max: 1, picked: true, build: buildNamed},
	kindNamedList: {what: "values", list: true, picked: true, build: buildNamedList},
	kindIssue:     {what: "one issue", max: 1, picked: true, build: buildIssue},
	kindSprint:    {what: "one sprint ID", max: 1, picked: true, build: buildSprint},
	kindTeam:      {what: "one team ID", max: 1, picked: true, build: buildTeam},
}
