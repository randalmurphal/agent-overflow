package jira

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"slices"

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
	// Images is where Markdown bodies' local images are read.
	Images fs.FS
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

// Transition checks every field value and the comment, attaches the
// images they embed, then moves the issue. On a service desk project the
// comment is added after the move through the comment call, where its
// audience is set; elsewhere it is part of the move. A failure after the
// first request is a *StepError.
func (c *Client) Transition(ctx context.Context, in TransitionInput) (TransitionResult, error) {
	var res TransitionResult
	path, err := issuePath(in.Issue)
	if err != nil {
		return res, err
	}
	t, err := c.pickTransition(ctx, in.Issue, in.Transition)
	if err != nil {
		return res, err
	}
	b := &builder{c: c, opts: ValueOptions{Issue: in.Issue, Images: in.Images}, stage: newImageStage(in.Images, c.maxUpload)}
	values, err := c.screenValues(ctx, b, in.Issue, t.Fields, in.Fields, in.AcceptLoss)
	if err != nil {
		return res, err
	}
	var comment *adf.Node
	var props []map[string]any
	var serviceDesk bool
	if in.Comment != "" {
		if props, serviceDesk, err = c.commentAudience(ctx, in.Issue, in.Audience); err != nil {
			return res, fmt.Errorf("jira: transition %s: %w", in.Issue, err)
		}
		b.opts.Base = nil
		if comment, _, err = b.markdown(ctx, in.Comment, nil); err != nil {
			return res, notSentErr(fmt.Errorf("jira: transition %s: comment: %w", in.Issue, err))
		}
	}
	var ids map[string]string
	if imgs := b.stage.list; len(imgs) > 0 {
		if ids, err = c.uploadImages(ctx, in.Issue, b.stage, imgs, &res.Uploaded); err != nil {
			return res, &StepError{Step: StepUpload, Err: err}
		}
		if comment != nil {
			if comment, err = copyDoc(comment); err != nil {
				return res, &StepError{Step: StepTransition, Err: notSentErr(err)}
			}
			adf.ReplaceMediaIDs(comment, ids)
		}
	}
	fields, err := patchedFields(values, ids, false)
	if err != nil {
		return res, &StepError{Step: StepTransition, Err: notSentErr(err)}
	}
	update := map[string]any{}
	for _, v := range values {
		if v.Update != nil {
			update[v.FieldID] = v.Update
		}
	}
	if comment != nil && !serviceDesk {
		update["comment"] = []map[string]any{{"add": map[string]any{"body": comment}}}
	}
	body := map[string]any{"transition": idRef{t.ID}}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	if len(update) > 0 {
		body["update"] = update
	}
	if err := c.send(ctx, http.MethodPost, path+"/transitions", nil, body, nil); err != nil {
		return res, &StepError{Step: StepTransition, Err: fmt.Errorf("jira: transition %s by %s: %w", in.Issue, t.Name, err)}
	}
	res.Moved = true
	if comment == nil || !serviceDesk {
		return res, nil
	}
	cbody := map[string]any{"body": comment}
	if props != nil {
		cbody["properties"] = props
	}
	var stored commentJSON
	if err := c.send(ctx, http.MethodPost, path+"/comment", nil, cbody, &stored); err != nil {
		return res, &StepError{Step: StepComment, Err: fmt.Errorf("jira: comment on %s after its move: %w", in.Issue, err)}
	}
	cres := CommentResult{}
	err = c.storedComment(stored, &cres)
	res.Comment = cres.Comment
	return res, err
}

// pickTransition finds the transition named by name or ID among those the
// issue can make now.
func (c *Client) pickTransition(ctx context.Context, issue, want string) (Transition, error) {
	ts, err := c.Transitions(ctx, issue)
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

// screenValues builds the values of inputs against screen for issue. A
// rich text value edits the stored document, read first.
func (c *Client) screenValues(ctx context.Context, b *builder, issue string, screen Screen, inputs []FieldInput, acceptLoss bool) ([]Value, error) {
	metas := make([]FieldMeta, len(inputs))
	for i, fi := range inputs {
		f, err := screen.Field(fi.Field)
		if err != nil {
			return nil, notSentErr(err)
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
	if accountID == "" {
		return notSent("assign %s: an account ID is required; Unassign removes the assignee", issue)
	}
	return c.assign(ctx, issue, accountID)
}

// Unassign leaves an issue without an assignee.
func (c *Client) Unassign(ctx context.Context, issue string) error { return c.assign(ctx, issue, nil) }

func (c *Client) assign(ctx context.Context, issue string, accountID any) error {
	path, err := issuePath(issue)
	if err != nil {
		return err
	}
	body := map[string]any{"accountId": accountID}
	if err := c.send(ctx, http.MethodPut, path+"/assignee", nil, body, nil); err != nil {
		return fmt.Errorf("jira: assign %s: %w", issue, err)
	}
	return nil
}
