package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"agent-overflow/internal/tracker/adf"
)

// CreateResult is how far a create got: the issue once it exists, and
// the images attached to it.
type CreateResult struct {
	ID       string
	Key      string
	Uploaded []Attachment
}

// ApplyCreate sends a create plan. Without images it is one request.
// With images the issue is created with each image written as a
// paragraph naming its file, the images are attached to it, and the rich
// text that embeds them is written again in full. A failure is a
// *StepError; the result says what exists.
func (c *Client) ApplyCreate(ctx context.Context, p *CreatePlan) (CreateResult, error) {
	var res CreateResult
	if err := p.claim(c); err != nil {
		return res, err
	}
	fields := map[string]json.RawMessage{
		"project":   projectRef(p.Project),
		"issuetype": mustJSON(idRef{p.IssueType.ID}),
	}
	update := map[string]json.RawMessage{}
	for _, v := range p.Values {
		if len(v.Images) > 0 {
			stand, err := withoutPending(v.doc, v.Images)
			if err != nil {
				return res, &StepError{Step: StepCreate, Err: notSentErr(err)}
			}
			fields[v.FieldID] = mustJSON(stand)
			continue
		}
		v.addTo(fields, update)
	}
	body := map[string]any{"fields": fields}
	if len(update) > 0 {
		body["update"] = update
	}
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := c.send(ctx, http.MethodPost, "issue", nil, body, &created); err != nil {
		return res, &StepError{Step: StepCreate, Err: fmt.Errorf("jira: create in %s: %w", p.Project, err)}
	}
	res.ID, res.Key = created.ID, created.Key
	if len(p.Images) == 0 {
		return res, nil
	}
	ids, err := c.uploadImages(ctx, created.ID, p.stage, p.Images, &res.Uploaded)
	if err != nil {
		return res, &StepError{Step: StepUpload, Err: err}
	}
	finish, err := patchedFields(p.Values, ids, true)
	if err != nil {
		return res, &StepError{Step: StepFinish, Err: notSentErr(err)}
	}
	if err := c.send(ctx, http.MethodPut, "issue/"+url.PathEscape(created.ID), nil, map[string]any{"fields": finish}, nil); err != nil {
		return res, &StepError{Step: StepFinish, Err: fmt.Errorf("jira: write the images into %s: %w", created.Key, err)}
	}
	return res, nil
}

// projectRef names a project by ID or key.
func projectRef(ref string) json.RawMessage {
	if numericRE.MatchString(ref) {
		return mustJSON(idRef{ref})
	}
	return mustJSON(map[string]string{"key": ref})
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("jira: encode %T: %v", v, err))
	}
	return raw
}

// addTo puts v under fields or update.
func (v Value) addTo(fields, update map[string]json.RawMessage) {
	if v.Update != nil {
		update[v.FieldID] = v.Update
		return
	}
	fields[v.FieldID] = v.Fields
}

// patchedFields are the rich text values that embed images, with the
// uploaded media IDs in place of their placeholders; onlyImages leaves
// the other values out.
func patchedFields(vs []Value, ids map[string]string, onlyImages bool) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, v := range vs {
		if len(v.Images) == 0 {
			if !onlyImages && v.Update == nil {
				out[v.FieldID] = v.Fields
			}
			continue
		}
		doc, err := copyDoc(v.doc)
		if err != nil {
			return nil, err
		}
		adf.ReplaceMediaIDs(doc, ids)
		out[v.FieldID] = mustJSON(doc)
	}
	return out, nil
}

// uploadImages attaches imgs to the issue and returns their media IDs by
// placeholder, adding each attachment to done as it is stored.
func (c *Client) uploadImages(ctx context.Context, issueID string, stage *imageStage, imgs []*Image, done *[]Attachment) (map[string]string, error) {
	ids := map[string]string{}
	for _, img := range imgs {
		content, err := stage.content(ctx, img)
		if err != nil {
			if se, ok := errors.AsType[*StaleError](err); ok {
				se.Issue = issueID
			}
			return ids, notSentErr(err)
		}
		stored, err := c.Attach(ctx, issueID, File{Name: img.Name, Content: bytes.NewReader(content)})
		if err != nil {
			return ids, err
		}
		if len(stored) != 1 {
			return ids, fmt.Errorf("jira: attaching %s stored %d attachments", img.Name, len(stored))
		}
		*done = append(*done, stored[0])
		mediaID, err := c.AttachmentMediaID(ctx, stored[0].ID)
		if err != nil {
			return ids, err
		}
		ids[img.placeholder] = mediaID
	}
	return ids, nil
}

// EditResult is how far an edit got: the images attached, and the issue
// as Jira stored it once the fields are written.
type EditResult struct {
	Uploaded []Attachment
	// Issue holds the fields the edit wrote, as Jira returned them; nil
	// when the plan changed nothing and nothing was sent.
	Issue *Issue
}

// ApplyEdit sends an edit plan: it reads the issue's rich text fields
// again and refuses with a *StaleError when one changed since the plan,
// attaches the images, then writes the fields. A failure is a
// *StepError; the result says what was done.
func (c *Client) ApplyEdit(ctx context.Context, p *EditPlan) (EditResult, error) {
	var res EditResult
	if err := p.claim(c); err != nil {
		return res, err
	}
	if len(p.Values) == 0 {
		return res, nil
	}
	if err := c.recheck(ctx, p); err != nil {
		return res, &StepError{Step: StepCheck, Err: err}
	}
	var ids map[string]string
	if len(p.Images) > 0 {
		var err error
		if ids, err = c.uploadImages(ctx, p.IssueID, p.stage, p.Images, &res.Uploaded); err != nil {
			return res, &StepError{Step: StepUpload, Err: err}
		}
	}
	fields, err := patchedFields(p.Values, ids, false)
	if err != nil {
		return res, &StepError{Step: StepEdit, Err: notSentErr(err)}
	}
	update := map[string]json.RawMessage{}
	var names []string
	for _, v := range p.Values {
		if v.Update != nil {
			update[v.FieldID] = v.Update
		}
		names = append(names, v.FieldID)
	}
	body := map[string]any{"fields": fields}
	if len(update) > 0 {
		body["update"] = update
	}
	q := url.Values{"returnIssue": {"true"}, "fields": {strings.Join(names, ",")}}
	var stored issueJSON
	if err := c.send(ctx, http.MethodPut, "issue/"+url.PathEscape(p.IssueID), q, body, &stored); err != nil {
		return res, &StepError{Step: StepEdit, Err: fmt.Errorf("jira: edit %s: %w", p.IssueKey, err)}
	}
	if stored.ID != "" {
		res.Issue = stored.decode()
	}
	return res, nil
}

// recheck reads the stored documents p's rich text edits were computed
// from and refuses when one changed.
func (c *Client) recheck(ctx context.Context, p *EditPlan) error {
	var rich []Value
	for _, v := range p.Values {
		if v.doc != nil {
			rich = append(rich, v)
		}
	}
	if len(rich) == 0 {
		return nil
	}
	ids := make([]string, len(rich))
	for i, v := range rich {
		ids[i] = v.FieldID
	}
	is, err := c.GetIssue(ctx, p.IssueID, ids...)
	if err != nil {
		return err
	}
	for _, v := range rich {
		now, err := storedDoc(is, FieldMeta{ID: v.FieldID, Name: v.Name})
		if err != nil {
			return notSentErr(err)
		}
		if !adf.Equal(now, v.base) {
			return notSentErr(&StaleError{Issue: p.IssueKey, What: v.Name})
		}
	}
	return nil
}
