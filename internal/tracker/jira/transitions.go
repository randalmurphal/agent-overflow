package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"agent-overflow/internal/tracker/adf"
)

// Transition is a workflow move an issue can make, with the fields its
// screen takes.
type Transition struct {
	ID          string
	Name        string
	To          Status
	HasScreen   bool
	Available   bool
	Conditional bool
	Global      bool
	Fields      Screen
}

// Transitions returns the moves the user can make on an issue now, with
// their screens' fields.
func (c *Client) Transitions(ctx context.Context, issue string) ([]Transition, error) {
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	return c.transitions(ctx, issue)
}

func (c *Client) transitions(ctx context.Context, issue string) ([]Transition, error) {
	path, err := issuePath(issue)
	if err != nil {
		return nil, err
	}
	var v struct {
		Transitions []struct {
			ID          string                   `json:"id"`
			Name        string                   `json:"name"`
			To          statusJSON               `json:"to"`
			HasScreen   bool                     `json:"hasScreen"`
			Available   *bool                    `json:"isAvailable"`
			Conditional bool                     `json:"isConditional"`
			Global      bool                     `json:"isGlobal"`
			Fields      map[string]fieldMetaJSON `json:"fields"`
		} `json:"transitions"`
	}
	if err := c.get(ctx, path+"/transitions", url.Values{"expand": {"transitions.fields"}}, &v); err != nil {
		return nil, fmt.Errorf("jira: transitions of %s: %w", issue, err)
	}
	out := make([]Transition, len(v.Transitions))
	for i, t := range v.Transitions {
		out[i] = Transition{
			ID: t.ID, Name: t.Name, To: t.To.status(), HasScreen: t.HasScreen,
			Available: t.Available == nil || *t.Available, Conditional: t.Conditional, Global: t.Global,
			Fields: screenOf(t.Fields),
		}
	}
	return out, nil
}

// TransitionInput moves an issue.
type TransitionInput struct {
	Issue string
	// Transition is the transition's name or ID.
	Transition string
	// Fields set fields on the transition's screen.
	Fields []FieldInput
	// Comment is Markdown for a comment made with the move; empty adds
	// none. Audience is checked as AddComment checks it.
	Comment  string
	Audience Audience
	// ImageDir is the folder Markdown bodies' local images are read from;
	// see EditInput.
	ImageDir string
	// AcceptLoss accepts that a rich text field's edit loses content
	// Markdown cannot show.
	AcceptLoss bool
}

// TransitionResult is how far a transition got.
type TransitionResult struct {
	Uploaded []Attachment
	// Moved reports that the transition was applied.
	Moved bool
	// Comment is the comment added after the move on a service desk
	// project; elsewhere the comment travels with the move and is nil.
	Comment *Comment
}

// transitionPlan is a transition checked in full: everything a move
// sends, and the images read for it.
type transitionPlan struct {
	path        string
	t           Transition
	values      []Value
	comment     *adf.Node
	props       []map[string]any
	serviceDesk bool
	imgs        []*Image
	held        map[*Image][]byte
}

// Transition runs every check that needs no write first: the transition,
// every field value, the screen's required fields, the comment and its
// audience, and the images, read whole; then, as its last read, it reads
// every rich text field it writes again and refuses with a *StaleError
// when one changed meanwhile. Then it attaches the images and moves the
// issue. On a service desk project the comment is added after
// the move through the comment call, where its audience is set; elsewhere
// it is part of the move. A failure after the first write is a
// *StepError.
func (c *Client) Transition(ctx context.Context, in TransitionInput) (TransitionResult, error) {
	var res TransitionResult
	if err := c.admit(ctx); err != nil {
		return res, err
	}
	tp, err := c.checkTransition(ctx, in)
	if err == nil {
		// The last read before the first write.
		err = c.recheckRich(ctx, in.Issue, in.Issue, tp.values)
	}
	if err != nil {
		return res, notSentErr(err)
	}
	var ids map[string]string
	if len(tp.imgs) > 0 {
		if ids, err = c.uploadImages(ctx, in.Issue, tp.imgs, tp.held, &res.Uploaded); err != nil {
			return res, &StepError{Step: StepUpload, Err: err}
		}
		if tp.comment != nil {
			if tp.comment, err = copyDoc(tp.comment); err != nil {
				return res, &StepError{Step: StepTransition, Err: notSentErr(err)}
			}
			adf.ReplaceMediaIDs(tp.comment, ids)
		}
	}
	fields, err := patchedFields(tp.values, ids, false)
	if err != nil {
		return res, &StepError{Step: StepTransition, Err: notSentErr(err)}
	}
	update := map[string]any{}
	for _, v := range tp.values {
		if v.Update != nil {
			update[v.FieldID] = v.Update
		}
	}
	if tp.comment != nil && !tp.serviceDesk {
		update["comment"] = []map[string]any{{"add": map[string]any{"body": tp.comment}}}
	}
	body := map[string]any{"transition": idRef{tp.t.ID}}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	if len(update) > 0 {
		body["update"] = update
	}
	// Documented: 204.
	if err := c.sendWrite(ctx, http.MethodPost, tp.path+"/transitions", nil, body, emptyAnswer(http.StatusNoContent)); err != nil {
		return res, &StepError{Step: StepTransition, Err: fmt.Errorf("jira: transition %s by %s: %w", in.Issue, tp.t.Name, err)}
	}
	res.Moved = true
	if tp.comment == nil || !tp.serviceDesk {
		return res, nil
	}
	cbody := map[string]any{"body": tp.comment}
	if tp.props != nil {
		cbody["properties"] = tp.props
	}
	res.Comment, err = c.writeComment(ctx, http.MethodPost, tp.path+"/comment", cbody)
	if err != nil {
		return res, fmt.Errorf("jira: comment on %s after its move: %w", in.Issue, err)
	}
	return res, nil
}

// checkTransition runs the checks of Transition that need no write.
func (c *Client) checkTransition(ctx context.Context, in TransitionInput) (*transitionPlan, error) {
	path, err := issuePath(in.Issue)
	if err != nil {
		return nil, err
	}
	tp := &transitionPlan{path: path}
	if tp.t, err = c.pickTransition(ctx, in.Issue, in.Transition); err != nil {
		return nil, err
	}
	b := &builder{c: c, opts: ValueOptions{Issue: in.Issue, ImageDir: in.ImageDir}, stage: newImageStage(in.ImageDir, c.maxUpload)}
	if tp.values, err = c.screenValues(ctx, b, in.Issue, tp.t.Fields, in.Fields, in.AcceptLoss); err != nil {
		return nil, err
	}
	if err := c.checkRequired(ctx, in.Issue, tp.t, tp.values); err != nil {
		return nil, err
	}
	if in.Comment != "" {
		if tp.props, tp.serviceDesk, err = c.commentAudience(ctx, in.Issue, in.Audience); err != nil {
			return nil, fmt.Errorf("jira: transition %s: %w", in.Issue, err)
		}
		b.opts.Base = nil
		if tp.comment, _, err = b.markdown(ctx, in.Comment, nil); err != nil {
			return nil, notSentErr(fmt.Errorf("jira: transition %s: comment: %w", in.Issue, err))
		}
	}
	tp.imgs = b.stage.list
	if tp.held, err = b.stage.hold(ctx, tp.imgs); err != nil {
		return nil, err
	}
	return tp, nil
}

// pickTransition finds the transition named by name or ID among those the
// issue can make now.
func (c *Client) pickTransition(ctx context.Context, issue, want string) (Transition, error) {
	ts, err := c.transitions(ctx, issue)
	if err != nil {
		return Transition{}, err
	}
	cands := make([]Candidate, len(ts))
	for i, t := range ts {
		cands[i] = Candidate{ID: t.ID, Name: t.Name}
	}
	hit, err := match("transition of "+issue, want, cands)
	if err != nil {
		return Transition{}, notSentErr(err)
	}
	i := slices.IndexFunc(ts, func(t Transition) bool { return t.ID == hit.ID })
	if !ts[i].Available {
		return Transition{}, notSent("%s cannot make the %s transition now", issue, ts[i].Name)
	}
	return ts[i], nil
}

// checkRequired refuses a move that leaves a required field of its
// screen provably unset: required, without a default, not given, and
// empty on the issue. A field the issue holds a value for is let through,
// since the transition metadata does not say whether that value meets
// the requirement; Jira decides.
func (c *Client) checkRequired(ctx context.Context, issue string, t Transition, values []Value) error {
	var open []FieldMeta
	for _, f := range t.Fields.Fields {
		given := slices.ContainsFunc(values, func(v Value) bool { return v.FieldID == f.ID })
		if f.Required && !f.HasDefault && !given {
			open = append(open, f)
		}
	}
	if len(open) == 0 {
		return nil
	}
	path, err := issuePath(issue)
	if err != nil {
		return err
	}
	ids := make([]string, len(open))
	for i, f := range open {
		ids[i] = f.ID
	}
	var is issueJSON
	if err := c.get(ctx, path, url.Values{"fields": {strings.Join(ids, ",")}}, &is); err != nil {
		return fmt.Errorf("jira: transition %s: read its required fields: %w", issue, err)
	}
	var missing []FieldMeta
	for _, f := range open {
		if emptyValue(is.Fields[f.ID]) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return notSentErr(&MissingFieldsError{Fields: missing})
	}
	return nil
}

// emptyValue reports a field value that holds nothing: absent, null, an
// empty string, list or object.
func emptyValue(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	switch string(s) {
	case "", "null", `""`, "[]", "{}":
		return true
	}
	return false
}

// screenValues builds the values of inputs against screen for issue. A
// rich text value edits the stored document, read first.
func (c *Client) screenValues(ctx context.Context, b *builder, issue string, screen Screen, inputs []FieldInput, acceptLoss bool) ([]Value, error) {
	metas := make([]FieldMeta, len(inputs))
	for i, fi := range inputs {
		f, err := screen.Field(fi.Field)
		if err != nil {
			return nil, notSentErr(err)
		}
		if slices.ContainsFunc(metas[:i], func(m FieldMeta) bool { return m.ID == f.ID }) {
			return nil, notSent("%s is set twice", f.Name)
		}
		metas[i] = f
	}
	var stored *Issue
	if slices.ContainsFunc(metas, func(f FieldMeta) bool { return kindOf(f.Schema) == kindRich }) {
		var err error
		if stored, err = c.GetIssue(ctx, issue, richFieldIDs(metas)...); err != nil {
			return nil, err
		}
	}
	out := make([]Value, 0, len(inputs))
	for i, fi := range inputs {
		b.opts.Base = nil
		if stored != nil && kindOf(metas[i].Schema) == kindRich {
			base, err := storedDoc(stored, metas[i])
			if err != nil {
				return nil, notSentErr(err)
			}
			b.opts.Base = base
		}
		v, err := b.build(ctx, metas[i], fi.Value)
		if err == nil {
			err = v.CheckLoss(acceptLoss)
		}
		if err != nil {
			return nil, notSentErr(err)
		}
		out = append(out, v)
	}
	return out, nil
}

// Assign assigns an issue to the user with accountID.
func (c *Client) Assign(ctx context.Context, issue, accountID string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	if accountID == "" {
		return notSent("assign %s: an account ID is required; Unassign removes the assignee", issue)
	}
	return c.assign(ctx, issue, accountID)
}

// Unassign leaves an issue without an assignee.
func (c *Client) Unassign(ctx context.Context, issue string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	return c.assign(ctx, issue, nil)
}

func (c *Client) assign(ctx context.Context, issue string, accountID any) error {
	path, err := issuePath(issue)
	if err != nil {
		return err
	}
	body := map[string]any{"accountId": accountID}
	if err := c.sendWrite(ctx, http.MethodPut, path+"/assignee", nil, body, emptyAnswer(http.StatusNoContent)); err != nil {
		return fmt.Errorf("jira: assign %s: %w", issue, err)
	}
	return nil
}
