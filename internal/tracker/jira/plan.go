package jira

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

	"agent-overflow/internal/tracker/adf"
)

// FieldInput sets one field, named by its exact name or ID.
type FieldInput struct {
	Field string
	Value Input
}

// CreateInput is an issue to create. Summary and the other fields are
// FieldInputs; the project and issue type are set here, not as fields.
type CreateInput struct {
	Project   string // key or ID
	IssueType string // name or ID
	Fields    []FieldInput
	// ImageDir is the folder Markdown bodies' local images are read from;
	// see EditInput.
	ImageDir string
}

// CreatePlan is a checked create: every field resolved, every Markdown
// body converted, every image read. What it sends is fixed when it is
// made and cannot be changed; Preview shows it. It is applied once, by
// the Client that made it.
type CreatePlan struct {
	project   string
	issueType IssueType
	values    []Value
	images    []*Image
	warnings  []string

	owned
}

// FieldPreview is one field a plan writes, as a person reads it.
type FieldPreview struct {
	FieldID  string
	Name     string
	Display  string
	Warnings []string
}

// CreatePreview is what a create plan sends. It is a copy: changing it
// changes nothing the plan sends.
type CreatePreview struct {
	Project   string
	IssueType IssueType
	Fields    []FieldPreview
	// Images are uploaded to the new issue after it is created.
	Images   []Image
	Warnings []string
}

// Preview returns a copy of what p sends.
func (p *CreatePlan) Preview() CreatePreview {
	return CreatePreview{
		Project: p.project, IssueType: p.issueType, Fields: fieldPreviews(p.values),
		Images: imageCopies(p.images), Warnings: slices.Clone(p.warnings),
	}
}

func fieldPreviews(vs []Value) []FieldPreview {
	out := make([]FieldPreview, len(vs))
	for i, v := range vs {
		out[i] = FieldPreview{FieldID: v.FieldID, Name: v.Name, Display: v.Display, Warnings: slices.Clone(v.Warnings)}
	}
	return out
}

// owned ties a plan to the Client that made it and lets it be applied
// once: a write whose outcome is unknown is never sent again. It points
// at the plan's state, so a copy of a plan is the same plan, and
// applying either uses both up.
type owned struct {
	state *planState
}

type planState struct {
	client *Client
	stage  *imageStage
	used   atomic.Bool
}

func newOwned(c *Client, imageDir string) owned {
	return owned{state: &planState{client: c, stage: newImageStage(imageDir, c.maxImages)}}
}

// claim refuses a plan made by another Client or by no PlanCreate or
// PlanEdit, or applied already.
func (o owned) claim(c *Client) error {
	switch {
	case o.state == nil || o.state.client != c:
		return notSent("the plan was not made by this client")
	case !o.state.used.CompareAndSwap(false, true):
		return notSent("the plan was applied already; plan again after reading the issue")
	}
	return nil
}

// reserved are the fields a create input sets outside its field list.
var reserved = []string{"project", "issuetype"}

// PlanCreate resolves and checks a create without writing anything. A
// required field left unset is a *MissingFieldsError.
func (c *Client) PlanCreate(ctx context.Context, in CreateInput) (*CreatePlan, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if err := checkProject(in.Project); err != nil {
		return nil, err
	}
	p := &CreatePlan{project: in.Project}
	var err error
	if p.issueType, err = c.createIssueType(ctx, in.Project, in.IssueType); err != nil {
		return nil, err
	}
	screen, err := c.CreateMeta(ctx, in.Project, p.issueType.ID)
	if err != nil {
		return nil, err
	}
	if p.issueType.Name == "" {
		p.issueType = screenIssueType(screen, p.issueType)
	}
	p.owned = newOwned(c, in.ImageDir)
	b := &builder{c: c, opts: ValueOptions{Project: in.Project, ImageDir: in.ImageDir}, stage: p.state.stage}
	set := map[string]bool{}
	for _, fi := range in.Fields {
		f, err := screen.Field(fi.Field)
		if err != nil {
			return nil, notSentErr(err)
		}
		if slices.Contains(reserved, f.ID) {
			return nil, notSent("%s is set by the create input, not as a field", f.Name)
		}
		if set[f.ID] {
			return nil, notSent("%s is set twice", f.Name)
		}
		set[f.ID] = true
		v, err := b.build(ctx, f, fi.Value)
		if err != nil {
			return nil, notSentErr(err)
		}
		p.values = append(p.values, v)
		p.warnings = append(p.warnings, v.Warnings...)
	}
	var missing []FieldMeta
	for _, f := range screen.Fields {
		if f.Demanded() && !set[f.ID] && !slices.Contains(reserved, f.ID) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return nil, notSentErr(&MissingFieldsError{Fields: missing})
	}
	p.images = imagesOf(p.values)
	return p, nil
}

// createIssueType is the issue type ref names in project. A numeric ref
// is an issue type ID, taken as it is: the create screen read next
// refuses one the project cannot create, so no listing is read for it
// and its Name is left to screenIssueType. Any other ref is matched by
// name against the types the user can create there.
func (c *Client) createIssueType(ctx context.Context, project, ref string) (IssueType, error) {
	if isNumeric(ref) {
		return IssueType{ID: ref}, nil
	}
	types, err := c.CreateIssueTypes(ctx, project)
	if err != nil {
		return IssueType{}, err
	}
	cands := make([]Candidate, len(types))
	for i, t := range types {
		cands[i] = Candidate{ID: t.ID, Name: t.Name}
	}
	hit, err := match("issue type of "+project, ref, cands)
	if err != nil {
		return IssueType{}, notSentErr(err)
	}
	return types[slices.IndexFunc(types, func(t IssueType) bool { return t.ID == hit.ID })], nil
}

// screenIssueType is t named as the create screen's issue type field
// lists it, or t as it is when the screen does not list it.
func screenIssueType(screen Screen, t IssueType) IssueType {
	for _, f := range screen.Fields {
		if f.ID != "issuetype" {
			continue
		}
		for _, a := range f.Allowed {
			if a.ID == t.ID {
				t.Name = a.Name
			}
		}
	}
	return t
}

// imagesOf is every image the values embed, each once.
func imagesOf(vs []Value) []*Image {
	var out []*Image
	for _, v := range vs {
		for _, img := range v.imgs {
			if !slices.Contains(out, img) {
				out = append(out, img)
			}
		}
	}
	return out
}

// EditInput is a change to an issue's fields.
type EditInput struct {
	Issue  string // key or ID
	Fields []FieldInput
	// ImageDir is the folder Markdown bodies' local images are read from,
	// an absolute path; empty refuses local images. A reference is a name
	// inside it, one leading "/" meaning the folder itself; a name that
	// leaves it, through ".." or a symbolic link, is refused.
	ImageDir string
	// AcceptLoss is the user's explicit acceptance that a rich text edit
	// loses content Markdown cannot show. Without it such an edit is a
	// *adf.LossError naming what would be lost.
	AcceptLoss bool
}

// EditPlan is a checked edit of one issue. It records the issue's ID and
// the stored document every rich text field it writes was planned
// against, a clear included; applying it reads them again and refuses
// with a *StaleError when one changed. What it sends is fixed when it is
// made; Preview shows it.
type EditPlan struct {
	issueID   string
	issueKey  string
	values    []Value
	unchanged []string
	images    []*Image
	warnings  []string

	owned
}

// EditPreview is what an edit plan sends. It is a copy: changing it
// changes nothing the plan sends.
type EditPreview struct {
	IssueID  string
	IssueKey string
	// Fields are the fields the edit changes.
	Fields []FieldPreview
	// Unchanged names the rich text fields whose edit stores what is
	// there already; they are not sent.
	Unchanged []string
	Images    []Image
	Warnings  []string
}

// Preview returns a copy of what p sends.
func (p *EditPlan) Preview() EditPreview {
	return EditPreview{
		IssueID: p.issueID, IssueKey: p.issueKey, Fields: fieldPreviews(p.values),
		Unchanged: slices.Clone(p.unchanged), Images: imageCopies(p.images), Warnings: slices.Clone(p.warnings),
	}
}

// PlanEdit resolves and checks an edit without writing anything. A rich
// text value edits the stored document, so what Markdown cannot show
// survives where the Markdown left it unchanged; an edit that would lose
// such content fails unless in.AcceptLoss.
func (c *Client) PlanEdit(ctx context.Context, in EditInput) (*EditPlan, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if err := checkIssue(in.Issue); err != nil {
		return nil, err
	}
	if len(in.Fields) == 0 {
		return nil, notSent("edit %s: no fields", in.Issue)
	}
	is, screen, asked, err := c.editRead(ctx, in.Issue, in.Fields)
	if err != nil {
		return nil, err
	}
	metas := make([]FieldMeta, len(in.Fields))
	for i, fi := range in.Fields {
		if metas[i], err = screen.Field(fi.Field); err != nil {
			return nil, notSentErr(err)
		}
		if slices.ContainsFunc(metas[:i], func(m FieldMeta) bool { return m.ID == metas[i].ID }) {
			return nil, notSent("%s is set twice", metas[i].Name)
		}
	}
	if richNotAsked(asked, metas) {
		// A rich text field named other than by its ID: its document
		// was not asked for, so the documents are read again.
		if is, err = c.GetIssue(ctx, in.Issue, richFieldIDs(metas)...); err != nil {
			return nil, err
		}
	}
	p := &EditPlan{issueID: is.ID, issueKey: is.Key}
	p.owned = newOwned(c, in.ImageDir)
	for i, fi := range in.Fields {
		f := metas[i]
		opts := ValueOptions{Issue: is.Key, ImageDir: in.ImageDir}
		if kindOf(f.Schema) == kindRich {
			if opts.Base, err = storedDoc(is, f); err != nil {
				return nil, notSentErr(err)
			}
		}
		b := &builder{c: c, opts: opts, stage: p.state.stage}
		v, err := b.build(ctx, f, fi.Value)
		if err != nil {
			return nil, notSentErr(err)
		}
		if err := v.CheckLoss(in.AcceptLoss); err != nil {
			return nil, notSentErr(fmt.Errorf("jira: %s: %w", f.Name, err))
		}
		if v.Unchanged {
			p.unchanged = append(p.unchanged, f.Name)
			continue
		}
		p.values = append(p.values, v)
		p.warnings = append(p.warnings, v.Warnings...)
	}
	p.images = imagesOf(p.values)
	return p, nil
}

// editRead reads, in one request, the issue's edit screen and the
// fields an edit names by ID, which hold the documents its rich text
// edits start from, with the summary so the read names something
// whatever the edit names; asked are the field IDs it asked for.
func (c *Client) editRead(ctx context.Context, issue string, fields []FieldInput) (is *Issue, screen Screen, asked []string, err error) {
	path, err := issuePath(issue)
	if err != nil {
		return nil, Screen{}, nil, err
	}
	asked = []string{"summary"}
	for _, f := range fields {
		// A field ID is a system field's name or customfield_<n>; a field
		// named any other way is named by its name.
		if isKeyName(f.Field) && !slices.Contains(asked, f.Field) {
			asked = append(asked, f.Field)
		}
	}
	var v struct {
		ID       string      `json:"id"`
		Key      string      `json:"key"`
		Fields   issueFields `json:"fields"`
		EditMeta struct {
			Fields map[string]fieldMetaJSON `json:"fields"`
		} `json:"editmeta"`
	}
	q := url.Values{"fields": {strings.Join(asked, ",")}, "expand": {"editmeta"}}
	if err := c.get(ctx, path, q, &v); err != nil {
		return nil, Screen{}, nil, fmt.Errorf("jira: edit screen of %s: %w", issue, err)
	}
	return issueJSON{ID: v.ID, Key: v.Key, Fields: v.Fields}.decode(), screenOf(v.EditMeta.Fields), asked, nil
}

// richNotAsked reports a rich text field among metas that asked does not
// name.
func richNotAsked(asked []string, metas []FieldMeta) bool {
	return slices.ContainsFunc(metas, func(f FieldMeta) bool { return kindOf(f.Schema) == kindRich && !slices.Contains(asked, f.ID) })
}

// richFieldIDs are the IDs of the rich text fields among metas, or
// "summary" when there are none, so reading the issue stays small.
func richFieldIDs(metas []FieldMeta) []string {
	var ids []string
	for _, f := range metas {
		if kindOf(f.Schema) == kindRich {
			ids = append(ids, f.ID)
		}
	}
	if len(ids) == 0 {
		return []string{"summary"}
	}
	return ids
}

// storedDoc is the document field f holds on is; nil when it is empty.
func storedDoc(is *Issue, f FieldMeta) (*adf.Node, error) {
	if f.ID == "description" {
		if err := problemIn(is.Problems, f.ID); err != nil {
			return nil, fmt.Errorf("jira: the stored %s is not a document, so it cannot be edited as Markdown: %w", f.Name, err)
		}
		return is.Description, nil
	}
	doc, err := is.Field(f.ID).RichText()
	switch {
	case errors.Is(err, ErrNoValue):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("jira: the stored %s is not a document, so it cannot be edited as Markdown: %w", f.Name, err)
	}
	return doc, nil
}
