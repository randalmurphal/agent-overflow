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
	// Documented: 201 with the issue's id and key.
	created, err := c.write(ctx, trackerapi.Request{Method: http.MethodPost, Path: apiPath + "issue", Body: body},
		bodyAnswer(namesIssue, http.StatusCreated), nil)
	if err != nil {
		return res, &StepError{Step: StepCreate, Err: fmt.Errorf("jira: create in %s: %w", p.project, err)}
	}
	res.ID, res.Key = created.id(), created.key()
	if len(p.images) == 0 {
		return res, nil
	}
	ids, err := c.uploadImages(ctx, res.ID, p.images, held, &res.Uploaded, StepFinish)
	if err != nil {
		return res, err
	}
	finish, err := patchedFields(p.values, ids, true)
	if err != nil {
		return res, unsent(StepFinish, err)
	}
	finishReq := trackerapi.Request{Method: http.MethodPut, Path: apiPath + "issue/" + url.PathEscape(res.ID), Body: map[string]any{"fields": finish}}
	if _, err := c.write(ctx, finishReq, emptyAnswer(http.StatusNoContent), nil); err != nil {
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

// uploadImages attaches imgs, whose content held has, to the issue, one
// attach request each, reads each one's media ID, and returns the IDs by
// placeholder. Every attachment Jira stored is added to done as soon as
// its answer names it, whatever fails after. A failure is a *StepError:
// StepUpload when an attach request failed, StepMediaID when a media ID
// read did, and next, the write the images are for, when an attach answer
// named other than one attachment; in the last two next was not sent. An
// attach whose answer names its attachment and did not decode in full is
// applied, and the upload goes on.
func (c *Client) uploadImages(ctx context.Context, issue string, imgs []*Image, held map[*Image][]byte, done *[]Attachment, next Step) (map[string]string, error) {
	ids := map[string]string{}
	for _, img := range imgs {
		stored, err := c.attach(ctx, issue, File{Name: img.Name, Content: bytes.NewReader(held[img])})
		*done = append(*done, stored...)
		if err != nil && !answerApplied(err) {
			return ids, &StepError{Step: StepUpload, Err: err}
		}
		if len(stored) != 1 {
			return ids, unsent(next, &AnswerError{Message: fmt.Sprintf("attaching %s stored %d attachments, so which one the document names is not known", img.Name, len(stored))})
		}
		mediaID, err := c.mediaID(ctx, stored[0].ID)
		if err != nil {
			return ids, unsent(StepMediaID, err)
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
	// when Jira answered without them (a 204) or nothing was sent.
	Issue *Issue
	// Unchanged reports a plan whose every value the issue already held,
	// so nothing was sent.
	Unchanged bool
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
		res.Unchanged = true
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
		if ids, err = c.uploadImages(ctx, p.issueID, p.images, held, &res.Uploaded, StepEdit); err != nil {
			return res, err
		}
	}
	fields, err := patchedFields(p.values, ids, false)
	if err != nil {
		return res, unsent(StepEdit, err)
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
	// Documented: 200 with the issue when returnIssue is true, or 204.
	var stored issueJSON
	req := trackerapi.Request{Method: http.MethodPut, Path: apiPath + "issue/" + url.PathEscape(p.issueID), Query: q, Body: body}
	w, err := c.write(ctx, req, writeAnswer{empty: []int{http.StatusNoContent}, body: []int{http.StatusOK}, names: namesThisID(p.issueID)}, &stored)
	if err != nil && !answerApplied(err) {
		return res, &StepError{Step: StepEdit, Err: fmt.Errorf("jira: edit %s: %w", p.issueKey, err)}
	}
	// A 200 names the issue; its ID is kept even when the typed decode
	// lost it (a number where a string is due).
	if id := w.id(); id != "" {
		stored.ID = id
		res.Issue = stored.decode()
	}
	if err != nil {
		return res, &StepError{Step: StepEdit, Err: fmt.Errorf("jira: edit %s: %w", p.issueKey, err)}
	}
	return res, nil
}

// recheck reads the stored document of every rich text field p writes,
// a clear included, and refuses when one is no longer the document the
// plan was made against.
func (c *Client) recheck(ctx context.Context, p *EditPlan) error {
	return c.recheckRich(ctx, p.issueID, p.issueKey, p.values)
}

// recheckRich reads the stored document of every rich text field values
// write on issue (named issueKey in errors) and refuses with a
// *StaleError when one is no longer the document its value was built
// against. A write calls it as its last read before its first write; it
// narrows the window for a lost update and does not close it, since Jira
// takes no precondition on the write.
func (c *Client) recheckRich(ctx context.Context, issue, issueKey string, values []Value) error {
	var rich []Value
	for _, v := range values {
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
	is, err := c.GetIssue(ctx, issue, ids...)
	if err != nil {
		return err
	}
	for _, v := range rich {
		now, err := storedDoc(is, FieldMeta{ID: v.FieldID, Name: v.Name})
		if err != nil {
			return notSentErr(err)
		}
		if !adf.Equal(now, v.base) {
			return notSentErr(&StaleError{Issue: issueKey, What: v.Name})
		}
	}
	return nil
}
