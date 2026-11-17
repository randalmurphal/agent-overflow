package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"agent-overflow/internal/tracker/adf"
)

// Comment is a comment on an issue.
type Comment struct {
	ID           string
	Author       *User
	UpdateAuthor *User
	Body         *adf.Node
	Created      time.Time
	Updated      time.Time
	// Internal reports a service desk comment only agents see.
	Internal bool
	// Visibility is a role or group the comment is restricted to; nil
	// when everyone who can see the issue sees it.
	Visibility json.RawMessage
}

type commentJSON struct {
	ID           string          `json:"id"`
	Author       *userJSON       `json:"author"`
	UpdateAuthor *userJSON       `json:"updateAuthor"`
	Body         json.RawMessage `json:"body"`
	Created      string          `json:"created"`
	Updated      string          `json:"updated"`
	JSDPublic    *bool           `json:"jsdPublic"`
	Visibility   json.RawMessage `json:"visibility"`
}

// comment decodes a comment. A body that is not a document is an error,
// since the comment cannot be shown or edited; the comment returned with
// it holds everything else.
func (j commentJSON) comment() (Comment, error) {
	out := Comment{ID: j.ID, Author: j.Author.userPtr(), UpdateAuthor: j.UpdateAuthor.userPtr()}
	out.Created, _ = parseTime(j.Created)
	out.Updated, _ = parseTime(j.Updated)
	out.Internal = j.JSDPublic != nil && !*j.JSDPublic
	if !isNull(j.Visibility) {
		out.Visibility = j.Visibility
	}
	if !isNull(j.Body) {
		if err := decodeDoc(j.Body, &out.Body); err != nil {
			return out, fmt.Errorf("comment %s: %w", j.ID, err)
		}
	}
	return out, nil
}

// CommentPage is one page of an issue's comments, oldest first.
type CommentPage struct {
	Comments []Comment
	StartAt  int
	Total    int
}

// Comments returns the page of an issue's comments from startAt, at most
// max of them (zero means 100).
func (c *Client) Comments(ctx context.Context, issue string, startAt, max int) (CommentPage, error) {
	if err := c.admit(ctx); err != nil {
		return CommentPage{}, err
	}
	path, err := issuePath(issue)
	if err != nil {
		return CommentPage{}, err
	}
	if startAt < 0 || max < 0 {
		return CommentPage{}, notSent("comments of %s: a negative page", issue)
	}
	q := url.Values{"startAt": {strconv.Itoa(startAt)}, "maxResults": {strconv.Itoa(pageSize(max, 100, 5000))}, "orderBy": {"created"}}
	var v struct {
		Comments []commentJSON `json:"comments"`
		StartAt  int           `json:"startAt"`
		Total    int           `json:"total"`
	}
	if err := c.get(ctx, path+"/comment", q, &v); err != nil {
		return CommentPage{}, fmt.Errorf("jira: comments of %s: %w", issue, err)
	}
	page := CommentPage{StartAt: v.StartAt, Total: v.Total, Comments: make([]Comment, 0, len(v.Comments))}
	for _, j := range v.Comments {
		cm, err := j.comment()
		if err != nil {
			return page, fmt.Errorf("jira: comments of %s: %w", issue, err)
		}
		page.Comments = append(page.Comments, cm)
	}
	return page, nil
}

// Comment returns one comment.
func (c *Client) Comment(ctx context.Context, issue, id string) (Comment, error) {
	if err := c.admit(ctx); err != nil {
		return Comment{}, err
	}
	return c.comment(ctx, issue, id)
}

func (c *Client) comment(ctx context.Context, issue, id string) (Comment, error) {
	path, err := commentPath(issue, id)
	if err != nil {
		return Comment{}, err
	}
	var j commentJSON
	if err := c.get(ctx, path, nil, &j); err != nil {
		return Comment{}, fmt.Errorf("jira: comment %s on %s: %w", id, issue, err)
	}
	cm, err := j.comment()
	if err != nil {
		return Comment{}, fmt.Errorf("jira: comment %s on %s: %w", id, issue, err)
	}
	return cm, nil
}

func commentPath(issue, id string) (string, error) {
	path, err := issuePath(issue)
	if err != nil {
		return "", err
	}
	if err := checkID("comment", id); err != nil {
		return "", err
	}
	return path + "/comment/" + url.PathEscape(id), nil
}

// Audience is who a new comment reaches on a Jira Service Management
// project. It must be chosen there, since a public comment is emailed to
// the customer; elsewhere every comment is public. A value outside the
// constants below is refused before anything is sent.
type Audience uint8

const (
	// AudienceUnset is refused on a service desk project.
	AudienceUnset Audience = iota
	// AudiencePublic reaches everyone who can see the issue, the
	// customer included.
	AudiencePublic
	// AudienceInternal is a service desk note only agents see.
	AudienceInternal
)

// internalProperty is the comment property that makes a service desk
// comment internal. The platform spec does not document it; a site
// measured stores an internal comment with it and jsdPublic false
// (docs/references/tracker-api-measurements.md). A public comment
// carries sd.allow.public.comment, which Jira Service Management sets
// itself, so nothing is sent for one.
var internalProperty = []map[string]any{{"key": "sd.public.comment", "value": map[string]bool{"internal": true}}}

// commentAudience checks aud against the issue's project and returns the
// properties a new comment carries, and whether the project is a
// service desk.
func (c *Client) commentAudience(ctx context.Context, issue string, aud Audience) ([]map[string]any, bool, error) {
	switch aud {
	case AudienceUnset, AudiencePublic, AudienceInternal:
	default:
		return nil, false, notSent("comment on %s: %d is not an Audience", issue, aud)
	}
	is, err := c.GetIssue(ctx, issue, "project")
	if err != nil {
		return nil, false, err
	}
	if is.Project == nil {
		return nil, false, notSent("cannot tell which project %s is in, so not who a comment reaches", issue)
	}
	p := *is.Project
	if p.TypeKey == "" {
		if p, err = c.Project(ctx, p.Key); err != nil {
			return nil, false, err
		}
	}
	switch {
	case p.ServiceDesk() && aud == AudienceUnset:
		return nil, true, notSent("%s is a service desk project: choose AudienceInternal, or AudiencePublic to email the customer", p.Key)
	case !p.ServiceDesk() && aud == AudienceInternal:
		return nil, false, notSent("%s is not a service desk project, so a comment cannot be internal", p.Key)
	case aud == AudienceInternal:
		return internalProperty, true, nil
	}
	return nil, p.ServiceDesk(), nil
}

// CommentInput is a new comment.
type CommentInput struct {
	Issue    string
	Markdown string
	Audience Audience
	// ImageDir is the folder the Markdown's local images are read from;
	// see EditInput.
	ImageDir string
}

// CommentResult is how far a comment write got: the images attached, and
// the comment as Jira stored it.
type CommentResult struct {
	Uploaded []Attachment
	Comment  *Comment
}

// AddComment checks the audience, converts the Markdown and reads its
// images, then attaches the images and adds the comment. A failure after
// the first write is a *StepError; a comment stored whose answer did not
// decode is one whose Applied reports it, with the comment's ID in the
// result.
func (c *Client) AddComment(ctx context.Context, in CommentInput) (CommentResult, error) {
	var res CommentResult
	if err := c.admit(ctx); err != nil {
		return res, err
	}
	path, err := issuePath(in.Issue)
	if err != nil {
		return res, err
	}
	props, _, err := c.commentAudience(ctx, in.Issue, in.Audience)
	if err != nil {
		return res, fmt.Errorf("jira: comment on %s: %w", in.Issue, err)
	}
	b := &builder{c: c, opts: ValueOptions{Issue: in.Issue, ImageDir: in.ImageDir}, stage: newImageStage(in.ImageDir, c.maxUpload)}
	doc, _, err := b.markdown(ctx, in.Markdown, nil)
	if err != nil {
		return res, notSentErr(fmt.Errorf("jira: comment on %s: %w", in.Issue, err))
	}
	imgs := b.stage.in(doc)
	held, err := b.stage.hold(ctx, imgs)
	if err != nil {
		return res, err
	}
	if doc, err = c.uploadInto(ctx, in.Issue, doc, imgs, held, &res.Uploaded); err != nil {
		return res, err
	}
	body := map[string]any{"body": doc}
	if props != nil {
		body["properties"] = props
	}
	res.Comment, err = c.writeComment(ctx, http.MethodPost, path+"/comment", body)
	if err != nil {
		return res, fmt.Errorf("jira: comment on %s: %w", in.Issue, err)
	}
	return res, nil
}

// writeComment sends a comment write and decodes the comment it stored.
// A failure to send is a StepComment *StepError; so is an answer that
// does not decode, which Applied reports, with whatever of the comment
// decoded (its ID at least, when the answer named one) returned.
func (c *Client) writeComment(ctx context.Context, method, path string, body any) (*Comment, error) {
	var stored commentJSON
	if err := c.send(ctx, method, path, nil, body, &stored); err != nil {
		return nil, &StepError{Step: StepComment, Err: err}
	}
	cm, err := stored.comment()
	if err != nil {
		return &cm, &StepError{Step: StepComment, Err: &AnswerError{Message: "the comment was stored, but its answer did not decode: " + err.Error()}}
	}
	return &cm, nil
}

// uploadInto attaches imgs, whose content held has, to issue and returns
// a copy of doc naming them by media ID; doc itself without images.
func (c *Client) uploadInto(ctx context.Context, issue string, doc *adf.Node, imgs []*Image, held map[*Image][]byte, done *[]Attachment) (*adf.Node, error) {
	if len(imgs) == 0 {
		return doc, nil
	}
	ids, err := c.uploadImages(ctx, issue, imgs, held, done)
	if err != nil {
		return nil, &StepError{Step: StepUpload, Err: err}
	}
	out, err := copyDoc(doc)
	if err != nil {
		return nil, &StepError{Step: StepUpload, Err: notSentErr(err)}
	}
	adf.ReplaceMediaIDs(out, ids)
	return out, nil
}

// CommentEdit replaces a comment's text.
type CommentEdit struct {
	Issue    string
	ID       string
	Markdown string
	// From is the stored body the Markdown was written from; when set, a
	// comment that holds another body by now is a *StaleError.
	From *adf.Node
	// AcceptLoss is the user's explicit acceptance that the edit loses
	// content Markdown cannot show; without it such an edit is a
	// *adf.LossError.
	AcceptLoss bool
	// ImageDir is the folder the Markdown's local images are read from;
	// see EditInput.
	ImageDir string
}

// EditComment edits the stored body with the Markdown, keeping what
// Markdown cannot show where the text is unchanged, and keeps the
// comment's visibility. An edit that stores the same body sends nothing
// and returns the comment as read.
func (c *Client) EditComment(ctx context.Context, in CommentEdit) (CommentResult, error) {
	var res CommentResult
	if err := c.admit(ctx); err != nil {
		return res, err
	}
	path, err := commentPath(in.Issue, in.ID)
	if err != nil {
		return res, err
	}
	cur, err := c.comment(ctx, in.Issue, in.ID)
	if err != nil {
		return res, err
	}
	if in.From != nil && !adf.Equal(in.From, cur.Body) {
		return res, notSentErr(&StaleError{Issue: in.Issue, What: "comment " + in.ID})
	}
	b := &builder{c: c, opts: ValueOptions{Issue: in.Issue, ImageDir: in.ImageDir}, stage: newImageStage(in.ImageDir, c.maxUpload)}
	doc, ws, err := b.markdown(ctx, in.Markdown, cur.Body)
	if err == nil {
		err = adf.CheckLoss(ws, in.AcceptLoss)
	}
	if err != nil {
		return res, notSentErr(fmt.Errorf("jira: edit comment %s on %s: %w", in.ID, in.Issue, err))
	}
	if adf.Equal(doc, cur.Body) {
		res.Comment = &cur
		return res, nil
	}
	imgs := b.stage.in(doc)
	held, err := b.stage.hold(ctx, imgs)
	if err != nil {
		return res, err
	}
	if doc, err = c.uploadInto(ctx, in.Issue, doc, imgs, held, &res.Uploaded); err != nil {
		return res, err
	}
	body := map[string]any{"body": doc}
	if cur.Visibility != nil {
		// A PUT without it would show a restricted comment to everyone.
		body["visibility"] = cur.Visibility
	}
	res.Comment, err = c.writeComment(ctx, http.MethodPut, path, body)
	if err != nil {
		return res, fmt.Errorf("jira: edit comment %s on %s: %w", in.ID, in.Issue, err)
	}
	return res, nil
}

// DeleteComment deletes a comment.
func (c *Client) DeleteComment(ctx context.Context, issue, id string) error {
	if err := c.admit(ctx); err != nil {
		return err
	}
	path, err := commentPath(issue, id)
	if err != nil {
		return err
	}
	if err := c.send(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
		return fmt.Errorf("jira: delete comment %s on %s: %w", id, issue, err)
	}
	return nil
}
