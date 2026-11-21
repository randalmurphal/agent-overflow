package linear

import (
	"context"
	"fmt"
)

// CreateCommentInput is a new comment on an issue.
type CreateCommentInput struct {
	IssueID string // an issue ID or identifier; required
	Body    string // Markdown; required, as Linear requires it
	// ParentID makes the comment a reply: the thread's root comment, even
	// when replying to a reply.
	ParentID string
}

var opCreateComment = newMutation("CreateComment", `mutation CreateComment($input: CommentCreateInput!) {
  result: commentCreate(input: $input) { success value: comment { ...CommentFields } }
}
`+commentFields)

type commentCreateInput struct {
	IssueID  string `json:"issueId"`
	Body     string `json:"body"`
	ParentID string `json:"parentId,omitempty"`
}

// CreateComment adds a comment and returns it.
func (c *Client) CreateComment(ctx context.Context, in CreateCommentInput) (*Comment, error) {
	if in.IssueID == "" || in.Body == "" {
		return nil, invalid(opCreateComment.name, "an issue ID and a body are required")
	}
	return mutateStored(ctx, c, opCreateComment, inputVars{commentCreateInput(in)}, "", wireComment.typed)
}

var opUpdateComment = newMutation("UpdateComment", `mutation UpdateComment($id: String!, $input: CommentUpdateInput!) {
  result: commentUpdate(id: $id, input: $input) { success value: comment { ...CommentFields } }
}
`+commentFields)

type commentUpdateInput struct {
	Body string `json:"body"`
}

// UpdateComment replaces a comment's Markdown body and returns it; an
// empty body clears it, as Linear documents.
func (c *Client) UpdateComment(ctx context.Context, id, body string) (*Comment, error) {
	if id == "" {
		return nil, invalid(opUpdateComment.name, "a comment ID is required")
	}
	return mutateStored(ctx, c, opUpdateComment, updateVars{id, commentUpdateInput{body}}, id, wireComment.typed)
}

var opDeleteComment = newMutation("DeleteComment", `mutation DeleteComment($id: String!) {
  result: commentDelete(id: $id) { success value: entityId }
}`)

// DeleteComment deletes a comment.
func (c *Client) DeleteComment(ctx context.Context, id string) error {
	return c.deleteByID(ctx, opDeleteComment, id)
}

// deleteByID deletes the entity id names with op, whose payload aliases
// the deleted entity's ID (DeletePayload.entityId) as its value; a
// success that names another, or none, leaves the outcome unknown.
func (c *Client) deleteByID(ctx context.Context, op operation, id string) error {
	if id == "" {
		return invalid(op.name, "an ID is required")
	}
	deleted, err := mutate[string](ctx, c, op, idVars{id})
	switch {
	case err != nil:
		return err
	case deleted == nil || *deleted != id:
		return fmt.Errorf("linear %s: the payload reported success without naming %s as deleted, so whether it was applied is unknown", op.name, id)
	}
	return nil
}

// CreateRelationInput relates two issues: IssueID blocks, duplicates,
// or relates to RelatedIssueID.
type CreateRelationInput struct {
	IssueID        string       `json:"issueId"`
	RelatedIssueID string       `json:"relatedIssueId"`
	Type           RelationType `json:"type"`
}

var opCreateRelation = newMutation("CreateRelation", `mutation CreateRelation($input: IssueRelationCreateInput!) {
  result: issueRelationCreate(input: $input) { success value: issueRelation { ...RelationFields } }
}
`+relationFields+issueRefFields)

// CreateRelation relates two issues and returns the relation.
func (c *Client) CreateRelation(ctx context.Context, in CreateRelationInput) (*Relation, error) {
	if in.IssueID == "" || in.RelatedIssueID == "" || in.Type == "" {
		return nil, invalid(opCreateRelation.name, "both issue IDs and a type are required")
	}
	if in.Type == RelationSimilar {
		return nil, invalid(opCreateRelation.name, "Linear documents the similar relation as deprecated; it can be read, not created")
	}
	return mutateStored(ctx, c, opCreateRelation, inputVars{in}, "", wireRelation.typed)
}

var opDeleteRelation = newMutation("DeleteRelation", `mutation DeleteRelation($id: String!) {
  result: issueRelationDelete(id: $id) { success value: entityId }
}`)

// DeleteRelation deletes a relation.
func (c *Client) DeleteRelation(ctx context.Context, id string) error {
	return c.deleteByID(ctx, opDeleteRelation, id)
}

// LinkInput is a link to record on an issue, such as a pull request.
type LinkInput struct {
	IssueID  string `json:"issueId"` // an issue ID or identifier
	URL      string `json:"url"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"` // such as the pull request's state
}

var opLinkURL = newMutation("LinkURL", `mutation LinkURL($input: AttachmentCreateInput!) {
  result: attachmentCreate(input: $input) { success value: attachment { ...AttachmentFields } }
}
`+attachmentFields)

// LinkURL records a link on an issue and returns the attachment. Linear
// keeps one attachment per URL on an issue: linking the same URL again
// updates its title and subtitle, so repeating the call after an
// unknown outcome is safe. No integration fetches anything for it.
func (c *Client) LinkURL(ctx context.Context, in LinkInput) (*Attachment, error) {
	if in.IssueID == "" || in.URL == "" || in.Title == "" {
		return nil, invalid(opLinkURL.name, "an issue ID, a URL and a title are required")
	}
	return mutateStored(ctx, c, opLinkURL, inputVars{in}, "", wireAttachment.typed)
}

var opStartUpcomingCycle = newMutation("StartUpcomingCycle", `mutation StartUpcomingCycle($id: String!) {
  result: cycleStartUpcomingCycleToday(id: $id) { success value: cycle { ...CycleFields } }
}
`+cycleFields)

// StartUpcomingCycle starts a team's next cycle as of midnight today and
// completes the current one if it has not ended; its unfinished issues
// roll over by the team's settings. id must be the upcoming cycle. This
// is the only cycle schedule change the client offers: Linear generates
// cycles and cannot create one through the API.
func (c *Client) StartUpcomingCycle(ctx context.Context, id string) (*Cycle, error) {
	if id == "" {
		return nil, invalid("StartUpcomingCycle", "a cycle ID is required")
	}
	return mutateStored(ctx, c, opStartUpcomingCycle, idVars{id}, id, wireCycle.typed)
}
