package git

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitHub's reaction writes: one ReactionTarget read (the comment's owner,
// as CommentOwner reads it for an edit, and its reaction groups), then
// addReaction or removeReaction when the viewer's reaction is not
// already as asked.

const githubReactionTargetQuery = `query ReactionTarget($id: ID!) {
  node(id: $id) {
    __typename
    ... on PullRequestReviewComment { pullRequest { number repository { nameWithOwner } } ` + githubReactionFields + ` }
    ... on IssueComment { pullRequest { number repository { nameWithOwner } } ` + githubReactionFields + ` }
  }
}`

const (
	githubAddReactionMutation = `mutation AddReaction($subjectID: ID!, $content: ReactionContent!) {
  addReaction(input: {subjectId: $subjectID, content: $content}) { reaction { content } }
}`
	githubRemoveReactionMutation = `mutation RemoveReaction($subjectID: ID!, $content: ReactionContent!) {
  removeReaction(input: {subjectId: $subjectID, content: $content}) { reaction { content } }
}`
)

// SetCommentReaction refuses a comment that is not of the kind target
// names on ref's pull request (*CommentNotOnPRError) before any write,
// and answers a reaction already as asked without one.
func (f *githubForge) SetCommentReaction(ctx context.Context, ref PRReference, target CommentTarget, name string, reacted bool) error {
	content := githubReactionContent[name]
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	var raw struct {
		Node *struct {
			Typename    string `json:"__typename"`
			PullRequest *struct {
				Number     int `json:"number"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"pullRequest"`
			ReactionGroups []githubReactionGroupRaw `json:"reactionGroups"`
		} `json:"node"`
	}
	if _, err := client.GraphQL(ctx, "ReactionTarget", githubReactionTargetQuery, map[string]any{"id": target.CommentID}, &raw); err != nil {
		var gql *forgeapi.GraphQLError
		if errors.As(err, &gql) && graphQLNotFound(gql) {
			return &CommentNotOnPRError{CommentID: target.CommentID}
		}
		return err
	}
	kind := "PullRequestReviewComment"
	if githubConversationTarget(target) {
		kind = "IssueComment"
	}
	node := raw.Node
	if node == nil || node.Typename != kind || node.PullRequest == nil || node.PullRequest.Number != ref.Number ||
		!strings.EqualFold(node.PullRequest.Repository.NameWithOwner, ref.Namespace+"/"+ref.Repo) {
		return &CommentNotOnPRError{CommentID: target.CommentID}
	}
	for _, group := range node.ReactionGroups {
		if group.Content == content && group.ViewerHasReacted == reacted {
			return nil
		}
	}
	operation, document, field := "AddReaction", githubAddReactionMutation, "addReaction"
	if !reacted {
		operation, document, field = "RemoveReaction", githubRemoveReactionMutation, "removeReaction"
	}
	var answer map[string]*struct {
		Reaction *struct {
			Content string `json:"content"`
		} `json:"reaction"`
	}
	if _, err := client.GraphQL(ctx, operation, document, map[string]any{"subjectID": target.CommentID, "content": content}, &answer); err != nil {
		return writeOutcome(err)
	}
	if answer[field] == nil {
		return &UnconfirmedWriteError{Err: fmt.Errorf("GitHub answered %s with no result", field)}
	}
	return nil
}
