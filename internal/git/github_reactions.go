package git

import (
	"context"
	"errors"
	"fmt"

	"agent-overflow/internal/forgeapi"
)

// GitHub's reaction writes: one ReactionTarget read (the comment's owner,
// as CommentOwner reads it for an edit, and its reaction groups), then
// addReaction or removeReaction when the viewer's reaction is not
// already as asked.

var githubReactionTargetQuery = forgeapi.NewDocument(forgeapi.ForgeGitHub, `query ReactionTarget($id: ID!) {
  node(id: $id) { `+githubCommentOwnerFields+` ... on Reactable { viewerCanReact `+githubReactionFields+` } }
}`)

var (
	githubAddReactionMutation = forgeapi.NewDocument(forgeapi.ForgeGitHub, `mutation AddReaction($subjectID: ID!, $content: ReactionContent!) {
  addReaction(input: {subjectId: $subjectID, content: $content}) { reaction { content } }
}`)
	githubRemoveReactionMutation = forgeapi.NewDocument(forgeapi.ForgeGitHub, `mutation RemoveReaction($subjectID: ID!, $content: ReactionContent!) {
  removeReaction(input: {subjectId: $subjectID, content: $content}) { reaction { content } }
}`)
)

// SetCommentReaction refuses a comment that is not of the kind target
// names on ref's pull request (*CommentNotOnPRError) or that GitHub says
// the viewer may not react to (*ViewerCannotError) before any write, and
// answers a reaction already as asked without one.
func (f *githubForge) SetCommentReaction(ctx context.Context, ref PRReference, target CommentTarget, name string, reacted bool) error {
	content := githubReactionContent[name]
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	var raw struct {
		Node *struct {
			githubCommentOwnerRaw
			ViewerCanReact bool                     `json:"viewerCanReact"`
			ReactionGroups []githubReactionGroupRaw `json:"reactionGroups"`
		} `json:"node"`
	}
	if _, err := client.GraphQL(ctx, githubReactionTargetQuery, map[string]any{"id": target.CommentID}, &raw); err != nil {
		var gql *forgeapi.GraphQLError
		if errors.As(err, &gql) && graphQLNotFound(gql) {
			return &CommentNotOnPRError{CommentID: target.CommentID}
		}
		return err
	}
	node := raw.Node
	if node == nil || !node.onPR(ref, target) {
		return &CommentNotOnPRError{CommentID: target.CommentID}
	}
	if !node.ViewerCanReact {
		return &ViewerCannotError{Reason: githubCannotReactReason}
	}
	for _, group := range node.ReactionGroups {
		if group.Content == content && group.ViewerHasReacted == reacted {
			return nil
		}
	}
	document, field := githubAddReactionMutation, "addReaction"
	if !reacted {
		document, field = githubRemoveReactionMutation, "removeReaction"
	}
	var answer map[string]*struct {
		Reaction *struct {
			Content string `json:"content"`
		} `json:"reaction"`
	}
	if _, err := client.GraphQL(ctx, document, map[string]any{"subjectID": target.CommentID, "content": content}, &answer); err != nil {
		return writeOutcome(err)
	}
	if answer[field] == nil {
		return &UnconfirmedWriteError{Err: fmt.Errorf("GitHub answered %s with no result", field)}
	}
	return nil
}
