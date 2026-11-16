package jira

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
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
	// Images is where Markdown bodies' local images are read.
	Images fs.FS
}

// CreatePlan is a checked create: every field resolved, every Markdown
// body converted, every image read. It is applied once, by the Client
// that made it.
type CreatePlan struct {
	Project   string
	IssueType IssueType
	Values    []Value
	// Images are uploaded to the new issue after it is created.
	Images []*Image
	// Warnings gathers the values' warnings.
	Warnings []string

	owned
}

// owned ties a plan to the Client that made it and lets it be applied
// once: a write whose outcome is unknown is never sent again.
type owned struct {
	client *Client
	stage  *imageStage
	used   atomic.Bool
}

// claim refuses a plan made by another Client, or applied already.
func (o *owned) claim(c *Client) error {
	switch {
	case o.client != c:
		return notSent("the plan was made by another client")
	case !o.used.CompareAndSwap(false, true):
		return notSent("the plan was applied already; plan again after reading the issue")
	}
	return nil
}

// reserved are the fields a create input sets outside its field list.
var reserved = []string{"project", "issuetype"}

// PlanCreate resolves and checks a create without writing anything. A
// required field left unset is a *MissingFieldsError.
func (c *Client) PlanCreate(ctx context.Context, in CreateInput) (*CreatePlan, error) {
	if err := checkProject(in.Project); err != nil {
		return nil, err
	}
	types, err := c.CreateIssueTypes(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	cands := make([]Candidate, len(types))
	for i, t := range types {
		cands[i] = Candidate{ID: t.ID, Name: t.Name}
	}
	hit, err := match("issue type of "+in.Project, in.IssueType, cands)
	if err != nil {
		return nil, notSentErr(err)
	}
	p := &CreatePlan{Project: in.Project}
	for _, t := range types {
		if t.ID == hit.ID {
			p.IssueType = t
		}
	}
	screen, err := c.CreateMeta(ctx, in.Project, hit.ID)
	if err != nil {
		return nil, err
	}
	p.client, p.stage = c, newImageStage(in.Images, c.maxUpload)
	b := &builder{c: c, opts: ValueOptions{Project: in.Project, Images: in.Images}, stage: p.stage}
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
		p.Values = append(p.Values, v)
		p.Warnings = append(p.Warnings, v.Warnings...)
	}
	var missing []FieldMeta
	for _, f := range screen.Fields {
		if f.Required && !f.HasDefault && !set[f.ID] && !slices.Contains(reserved, f.ID) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return nil, notSentErr(&MissingFieldsError{Fields: missing})
	}
	p.Images = imagesOf(p.Values)
	return p, nil
}

// imagesOf is every image the values embed, each once.
func imagesOf(vs []Value) []*Image {
	var out []*Image
	for _, v := range vs {
		for _, img := range v.Images {
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
	// Images is where Markdown bodies' local images are read.
	Images fs.FS
	// AcceptLoss is the user's explicit acceptance that a rich text edit
	// loses content Markdown cannot show. Without it such an edit is a
	// *adf.LossError naming what would be lost.
	AcceptLoss bool
}

// EditPlan is a checked edit of one issue. It records the issue's ID and
// the stored documents its rich text edits were computed from; applying
// it reads them again and refuses with a *StaleError when one changed.
type EditPlan struct {
	IssueID  string
	IssueKey string
	// Values are the fields the edit changes.
	Values []Value
	// Unchanged names the rich text fields whose edit stores what is
	// there already; they are not sent.
	Unchanged []string
	Images    []*Image
	Warnings  []string

	owned
}

// PlanEdit resolves and checks an edit without writing anything. A rich
// text value edits the stored document, so what Markdown cannot show
// survives where the Markdown left it unchanged; an edit that would lose
// such content fails unless in.AcceptLoss.
func (c *Client) PlanEdit(ctx context.Context, in EditInput) (*EditPlan, error) {
	if err := checkIssue(in.Issue); err != nil {
		return nil, err
	}
	if len(in.Fields) == 0 {
		return nil, notSent("edit %s: no fields", in.Issue)
	}
	screen, err := c.EditMeta(ctx, in.Issue)
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
	is, err := c.GetIssue(ctx, in.Issue, richFieldIDs(metas)...)
	if err != nil {
		return nil, err
	}
	p := &EditPlan{IssueID: is.ID, IssueKey: is.Key}
	p.client, p.stage = c, newImageStage(in.Images, c.maxUpload)
	for i, fi := range in.Fields {
		f := metas[i]
		opts := ValueOptions{Issue: is.Key, Images: in.Images}
		if kindOf(f.Schema) == kindRich {
			if opts.Base, err = storedDoc(is, f); err != nil {
				return nil, notSentErr(err)
			}
		}
		b := &builder{c: c, opts: opts, stage: p.stage}
		v, err := b.build(ctx, f, fi.Value)
		if err != nil {
			return nil, notSentErr(err)
		}
		if err := v.CheckLoss(in.AcceptLoss); err != nil {
			return nil, notSentErr(fmt.Errorf("jira: %s: %w", f.Name, err))
		}
		if v.Unchanged {
			p.Unchanged = append(p.Unchanged, f.Name)
			continue
		}
		p.Values = append(p.Values, v)
		p.Warnings = append(p.Warnings, v.Warnings...)
	}
	p.Images = imagesOf(p.Values)
	return p, nil
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
		if err := problemOf(is, f.ID); err != nil {
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
