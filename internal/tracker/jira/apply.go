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
	"agent-overflow/internal/tracker/trackerapi"
)

// CreateResult is how far a create got: the issue once it exists, and
// the images attached to it.
type CreateResult struct {
	ID       string
	Key      string
	Uploaded []Attachment
}

// ApplyCreate sends a create plan. Every image is read again and checked
// against the plan before anything is sent. Without images the create is
// one request. With images the issue is created with each image written
// as a paragraph naming its file, the images are attached to it, and the
// rich text that embeds them is written again in full. A failure is a
// *StepError; the result says what exists.
func (c *Client) ApplyCreate(ctx context.Context, p *CreatePlan) (CreateResult, error) {
	var res CreateResult
	if err := c.admit(ctx); err != nil {
		return res, err
	}
	if p == nil {
		return res, notSent("no plan")
	}
	if err := p.claim(c); err != nil {
		return res, err
	}
	held, err := p.state.stage.hold(ctx, p.images)
	if err != nil {
		return res, &StepError{Step: StepCheck, Err: err}
	}
	fields := map[string]json.RawMessage{
		"project":   projectRef(p.project),
		"issuetype": mustJSON(idRef{p.issueType.ID}),
	}
	update := map[string]json.RawMessage{}
	for _, v := range p.values {
		if len(v.imgs) > 0 {
			stand, err := withoutPending(v.doc, v.imgs)
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
	created, err := c.write(ctx, trackerapi.Request{Method: http.MethodPost, Path: apiPath + "issue", Body: body}, nil)
	if numericRE.MatchString(created.id) {
		res.ID = created.id
	}
	if issueKeyRE.MatchString(created.key) {
		res.Key = created.key
	}
	if err != nil {
		return res, &StepError{Step: StepCreate, Err: fmt.Errorf("jira: create in %s: %w", p.project, err)}
	}
	if res.ID == "" || res.Key == "" {
		return res, &StepError{Step: StepCreate, Err: &AnswerError{Message: "the create's answer does not name the issue's ID and key"}}
	}
	if len(p.images) == 0 {
		return res, nil
	}
	ids, err := c.uploadImages(ctx, res.ID, p.images, held, &res.Uploaded)
	if err != nil {
		return res, &StepError{Step: StepUpload, Err: err}
	}
	finish, err := patchedFields(p.values, ids, true)
	if err != nil {
		return res, &StepError{Step: StepFinish, Err: notSentErr(err)}
	}
	if err := c.send(ctx, http.MethodPut, "issue/"+url.PathEscape(res.ID), nil, map[string]any{"fields": finish}, nil); err != nil {
		return res, &StepError{Step: StepFinish, Err: fmt.Errorf("jira: write the images into %s: %w", firstNonEmpty(res.Key, res.ID), err)}
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
		if len(v.imgs) == 0 {
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

// uploadImages attaches imgs, whose content held has, to the issue and
// returns their media IDs by placeholder, adding each attachment to done
// as it is stored.
func (c *Client) uploadImages(ctx context.Context, issue string, imgs []*Image, held map[*Image][]byte, done *[]Attachment) (map[string]string, error) {
	ids := map[string]string{}
	for _, img := range imgs {
		stored, err := c.attach(ctx, issue, File{Name: img.Name, Content: bytes.NewReader(held[img])})
		*done = append(*done, stored...)
		if err != nil {
			return ids, err
		}
		if len(stored) != 1 {
			return ids, &AnswerError{Message: fmt.Sprintf("attaching %s stored %d attachments", img.Name, len(stored))}
		}
		mediaID, err := c.mediaID(ctx, stored[0].ID)
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

// ApplyEdit sends an edit plan: it reads again every rich text field the
// plan writes and every image it embeds, and refuses with a *StaleError
// when one changed since the plan, before anything is sent; then it
// attaches the images and writes the fields. A failure is a *StepError;
// the result says what was done.
func (c *Client) ApplyEdit(ctx context.Context, p *EditPlan) (EditResult, error) {
	var res EditResult
	if err := c.admit(ctx); err != nil {
		return res, err
	}
	if p == nil {
		return res, notSent("no plan")
	}
	if err := p.claim(c); err != nil {
		return res, err
	}
	if len(p.values) == 0 {
		return res, nil
	}
	if err := c.recheck(ctx, p); err != nil {
		return res, &StepError{Step: StepCheck, Err: notSentErr(err)}
	}
	held, err := p.state.stage.hold(ctx, p.images)
	if err != nil {
		var se *StaleError
		if errors.As(err, &se) {
			se.Issue = p.issueKey
		}
		return res, &StepError{Step: StepCheck, Err: err}
	}
	var ids map[string]string
	if len(p.images) > 0 {
		if ids, err = c.uploadImages(ctx, p.issueID, p.images, held, &res.Uploaded); err != nil {
			return res, &StepError{Step: StepUpload, Err: err}
		}
	}
	fields, err := patchedFields(p.values, ids, false)
	if err != nil {
		return res, &StepError{Step: StepEdit, Err: notSentErr(err)}
	}
	update := map[string]json.RawMessage{}
	var names []string
	for _, v := range p.values {
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
	if err := c.send(ctx, http.MethodPut, "issue/"+url.PathEscape(p.issueID), q, body, &stored); err != nil {
		return res, &StepError{Step: StepEdit, Err: fmt.Errorf("jira: edit %s: %w", p.issueKey, err)}
	}
	if stored.ID != "" {
		res.Issue = stored.decode()
	}
	return res, nil
}

// recheck reads the stored document of every rich text field p writes,
// a clear included, and refuses when one is no longer the document the
// plan was made against.
func (c *Client) recheck(ctx context.Context, p *EditPlan) error {
	var rich []Value
	for _, v := range p.values {
		if v.rich {
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
	is, err := c.GetIssue(ctx, p.issueID, ids...)
	if err != nil {
		return err
	}
	for _, v := range rich {
		now, err := storedDoc(is, FieldMeta{ID: v.FieldID, Name: v.Name})
		if err != nil {
			return notSentErr(err)
		}
		if !adf.Equal(now, v.base) {
			return notSentErr(&StaleError{Issue: p.issueKey, What: v.Name})
		}
	}
	return nil
}
