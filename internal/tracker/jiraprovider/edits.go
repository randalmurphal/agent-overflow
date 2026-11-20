package jiraprovider

import (
	"context"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/jira"
)

// fieldInputs are edits as the jira client's field inputs, by Jira field
// ID. The form decides the input: a clear is Clear; picked IDs and typed
// text are Text, or Texts for a list field, which the client resolves
// against the field's values (an exact ID first, then a name), refusing
// text that matches none or several; Markdown is Text for a rich text
// field. A Markdown edit of an existing issue carries the Markdown it was
// written from (EditedFrom): the client's plan reads the stored document
// with the edit screen and refuses the edit as stale when the document no
// longer renders to it, and again when it changes before the write is
// sent. issue is empty for a create, whose Markdown is written from
// nothing.
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
			md, from := e.Value.Markdown()
			out[i].Value = jira.Text(md)
			if issue != "" {
				out[i].Value = out[i].Value.EditedFrom(from)
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
	return out, nil
}
