package jiraprovider

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/adf"
	"agent-overflow/internal/tracker/jira"
)

// fieldInputs are edits as the jira client's field inputs, by Jira field
// ID. The form decides the input: a clear is Clear; picked IDs and typed
// text are Text, or Texts for a list field, which the client resolves
// against the field's values (an exact ID first, then a name), refusing
// text that matches none or several; Markdown is Text for a rich text
// field. A Markdown edit of an existing issue is checked against the
// field as stored now: the stored document must still render to the
// Markdown the edit was written from, else the edit is stale, and the
// document is handed to the client, which refuses the write when it
// changes again before it is sent. issue is empty for a create, whose
// Markdown is written from nothing.
//
// An edit of a field the catalog does not list, a Markdown edit of a
// field that is not rich text, a picked or typed one of a rich text
// field, an edit of a read-only field and several values for a field
// that takes one are refused before anything is sent.
func (p *Provider) fieldInputs(ctx context.Context, issue tracker.IssueID, edits []tracker.FieldEdit) ([]jira.FieldInput, error) {
	s, err := p.site(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]jira.FieldInput, len(edits))
	var rich []int // the Markdown edits of an existing issue
	for i, e := range edits {
		id := jiraField(e.Field)
		f, ok := s.field(id)
		if !ok {
			return nil, invalidf("there is no field %s", e.Field)
		}
		kind, list := kindOf(f.Schema)
		out[i].Field = id
		switch form := e.Value.Form(); {
		case kind == tracker.KindReadOnly:
			return nil, invalidf("%s is not editable here", f.Name)
		case form == tracker.EditMarkdown:
			if kind != tracker.KindRichText {
				return nil, invalidf("%s is not rich text; give its value by ID or as typed text", f.Name)
			}
			md, _ := e.Value.Markdown()
			out[i].Value = jira.Text(md)
			if issue != "" {
				rich = append(rich, i)
			}
		case form == tracker.EditClear:
			out[i].Value = jira.Clear()
		case kind == tracker.KindRichText:
			return nil, invalidf("%s is rich text; edit it as Markdown", f.Name)
		default:
			vals := e.Value.Values()
			switch {
			case list:
				out[i].Value = jira.Texts(vals...)
			case len(vals) == 1:
				out[i].Value = jira.Text(vals[0])
			default:
				return nil, invalidf("%s takes one value, not %d", f.Name, len(vals))
			}
		}
	}
	if len(rich) == 0 {
		return out, nil
	}
	return out, p.checkMarkdown(ctx, s, issue, edits, out, rich)
}

// checkMarkdown checks the Markdown edits at rich against the stored
// fields and records in out the documents they were written from.
func (p *Provider) checkMarkdown(ctx context.Context, s site, issue tracker.IssueID, edits []tracker.FieldEdit, out []jira.FieldInput, rich []int) error {
	ids := make([]string, len(rich))
	for i, at := range rich {
		ids[i] = out[at].Field
	}
	is, err := p.jira.GetIssue(ctx, string(issue), ids...)
	if err != nil {
		return fmt.Errorf("jiraprovider: read the rich text of %s: %w", issue, err)
	}
	for _, at := range rich {
		doc, err := storedDoc(is, out[at].Field)
		if err != nil {
			return err
		}
		md, from := edits[at].Value.Markdown()
		if adf.ToMarkdown(doc) != from {
			f, _ := s.field(out[at].Field)
			return stale(is.Key, f.Name)
		}
		if doc == nil {
			// An empty field: the client refuses the write if it is
			// filled meanwhile, as it does for a changed document.
			doc = adf.Doc()
		}
		out[at].Value = jira.Text(md).EditedFrom(doc)
	}
	return nil
}

// storedDoc is the document field id holds on is; nil when it is empty.
func storedDoc(is *jira.Issue, id string) (*adf.Node, error) {
	if id == "description" {
		for _, pr := range is.Problems {
			if pr.Field == id {
				return nil, fmt.Errorf("jiraprovider: the description of %s is not a document: %w", is.Key, pr.Err)
			}
		}
		return is.Description, nil
	}
	doc, err := is.Field(id).RichText()
	switch {
	case errors.Is(err, jira.ErrNoValue):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("jiraprovider: field %s of %s is not a document: %w", id, is.Key, err)
	}
	return doc, nil
}
