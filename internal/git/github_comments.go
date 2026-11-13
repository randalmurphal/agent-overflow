package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitHub's comment actions: a comment now is REST, as the review's own
// file comments are (`POST pulls/N/comments` pinned to commit_id, `POST
// issues/N/comments` for the PR itself); an edit or delete is GraphQL on
// the comment's node id, a review comment's or a conversation (issue)
// comment's mutation chosen by its thread (CommentTarget).

// githubPullCommentIn is the REST body of one review comment posted on its
// own: a line or range (Line, Side, StartLine, StartSide) or a file
// (SubjectType "file").
type githubPullCommentIn struct {
	Body        string `json:"body"`
	CommitID    string `json:"commit_id"`
	Path        string `json:"path"`
	Line        int    `json:"line,omitempty"`
	Side        string `json:"side,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	StartSide   string `json:"start_side,omitempty"`
	SubjectType string `json:"subject_type,omitempty"`
}

func githubPullCommentBody(comment ReviewLineComment, headSHA string) (githubPullCommentIn, error) {
	if strings.EqualFold(comment.Side, "file") {
		file, err := githubFileCommentRequestBody(comment, headSHA)
		if err != nil {
			return githubPullCommentIn{}, err
		}
		return githubPullCommentIn{Body: file.Body, CommitID: file.CommitID, Path: file.Path, SubjectType: file.SubjectType}, nil
	}
	line, err := githubReviewCommentBody(comment)
	if err != nil {
		return githubPullCommentIn{}, err
	}
	out := githubPullCommentIn{Body: line.Body, CommitID: headSHA, Path: line.Path, Line: line.Line, Side: line.Side, StartSide: line.StartSide}
	if line.StartLine != nil {
		out.StartLine = *line.StartLine
	}
	return out, nil
}

func (f *githubForge) PostLineComment(ctx context.Context, ref PRReference, headSHA string, comment ReviewLineComment) error {
	body, err := githubPullCommentBody(comment, headSHA)
	if err != nil {
		return err
	}
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: githubPullPath(ref) + "/comments", Body: body}, nil)
	return commentWriteError(err)
}

func (f *githubForge) PostPRComment(ctx context.Context, ref PRReference, body string) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/issues/%d/comments", githubRepoPath(ref.Repository()), ref.Number)
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: path, Body: map[string]string{"body": body}}, nil)
	return commentWriteError(err)
}

// githubConversationTarget reports whether target is a conversation (issue)
// comment: the read makes each one a thread of its own, under its own id.
func githubConversationTarget(target CommentTarget) bool {
	return target.ThreadID == target.CommentID
}

const (
	githubUpdateReviewCommentMutation = `mutation UpdateReviewComment($id: ID!, $body: String!) {
  updatePullRequestReviewComment(input: {pullRequestReviewCommentId: $id, body: $body}) { pullRequestReviewComment { id } }
}`
	githubUpdateIssueCommentMutation = `mutation UpdateIssueComment($id: ID!, $body: String!) {
  updateIssueComment(input: {id: $id, body: $body}) { issueComment { id } }
}`
	githubDeleteReviewCommentMutation = `mutation DeleteReviewComment($id: ID!) {
  deletePullRequestReviewComment(input: {id: $id}) { clientMutationId }
}`
	githubDeleteIssueCommentMutation = `mutation DeleteIssueComment($id: ID!) {
  deleteIssueComment(input: {id: $id}) { clientMutationId }
}`
)

// githubCommentOwnerQuery reads which pull request a comment's node id
// belongs to. A node id is global, so a mutation on one names nothing
// about the PR: an edit or delete reads the owner first and refuses a
// comment of another PR or repository.
const githubCommentOwnerQuery = `query CommentOwner($id: ID!) {
  node(id: $id) {
    __typename
    ... on PullRequestReviewComment { pullRequest { number repository { nameWithOwner } } }
    ... on IssueComment { pullRequest { number repository { nameWithOwner } } }
  }
}`

// requireCommentOnPR reads target's owner and refuses, before any write,
// a comment that is not of the kind target names or not on ref's pull
// request (*CommentNotOnPRError).
func requireCommentOnPR(ctx context.Context, client *forgeapi.Client, ref PRReference, target CommentTarget) error {
	var raw struct {
		Node *struct {
			Typename    string `json:"__typename"`
			PullRequest *struct {
				Number     int `json:"number"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"pullRequest"`
		} `json:"node"`
	}
	if _, err := client.GraphQL(ctx, "CommentOwner", githubCommentOwnerQuery, map[string]any{"id": target.CommentID}, &raw); err != nil {
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
	return nil
}

// EditComment runs the comment's update mutation and checks that the
// answer names the comment: a 200 that updated nothing must not read as
// an edit.
func (f *githubForge) EditComment(ctx context.Context, ref PRReference, target CommentTarget, body string) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	if err := requireCommentOnPR(ctx, client, ref, target); err != nil {
		return err
	}
	operation, document, field, node := "UpdateReviewComment", githubUpdateReviewCommentMutation, "updatePullRequestReviewComment", "pullRequestReviewComment"
	if githubConversationTarget(target) {
		operation, document, field, node = "UpdateIssueComment", githubUpdateIssueCommentMutation, "updateIssueComment", "issueComment"
	}
	var raw map[string]map[string]*struct {
		ID string `json:"id"`
	}
	if _, err := client.GraphQL(ctx, operation, document, map[string]any{"id": target.CommentID, "body": body}, &raw); err != nil {
		return err
	}
	if answer := raw[field][node]; answer == nil || answer.ID != target.CommentID {
		return fmt.Errorf("GitHub reported no edit of comment %s", target.CommentID)
	}
	return nil
}

func (f *githubForge) DeleteComment(ctx context.Context, ref PRReference, target CommentTarget) error {
	client, err := f.core.githubAPI(ref.Host)
	if err != nil {
		return err
	}
	if err := requireCommentOnPR(ctx, client, ref, target); err != nil {
		return err
	}
	operation, document, field := "DeleteReviewComment", githubDeleteReviewCommentMutation, "deletePullRequestReviewComment"
	if githubConversationTarget(target) {
		operation, document, field = "DeleteIssueComment", githubDeleteIssueCommentMutation, "deleteIssueComment"
	}
	var raw map[string]*struct {
		ClientMutationID *string `json:"clientMutationId"`
	}
	if _, err := client.GraphQL(ctx, operation, document, map[string]any{"id": target.CommentID}, &raw); err != nil {
		return err
	}
	if raw[field] == nil {
		return fmt.Errorf("GitHub reported no delete of comment %s", target.CommentID)
	}
	return nil
}

// postGitHubReply posts one reply under a review thread's first comment.
// A 404 is the thread gone (its root comment deleted).
func postGitHubReply(ctx context.Context, client *forgeapi.Client, ref PRReference, reply ReviewThreadReply, body string) error {
	if reply.RootDatabaseID <= 0 {
		return errors.New("GitHub review reply requires the root comment databaseID")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("reply body is required")
	}
	path := fmt.Sprintf("%s/comments/%d/replies", githubPullPath(ref), reply.RootDatabaseID)
	_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: path, Body: map[string]string{"body": body}}, nil)
	if errors.Is(err, forgeapi.ErrNotFound) {
		return &ReplyThreadGoneError{ThreadID: reply.ThreadID, Err: err}
	}
	return err
}
