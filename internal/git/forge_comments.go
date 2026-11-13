package git

import (
	"agent-overflow/internal/forgeapi"
	"context"
	"errors"
	"fmt"
	"strings"
)

// ForgeCommenter is the comment half of Forge: a comment posted now, on
// its own, and an edit or delete of one already sent. Every method takes
// an interactive context (the caller marks it) and the PR's reference;
// the forge's own answer is the authority on who may do what.
type ForgeCommenter interface {
	// PostLineComment posts one comment on a line, a range (comment.Start)
	// or a file (Side "file") of the PR's diff at headSHA, the head the
	// caller's diff shows. GitHub pins the comment to that commit; GitLab
	// positions it on the merge request's current diff and refuses with a
	// *HeadMovedError when the head is not headSHA any more.
	PostLineComment(ctx context.Context, ref PRReference, headSHA string, comment ReviewLineComment) error
	// PostPRComment posts a comment on the PR itself: a GitHub issue
	// comment, a GitLab merge request note.
	PostPRComment(ctx context.Context, ref PRReference, body string) error
	// EditComment replaces the body of a sent comment.
	EditComment(ctx context.Context, ref PRReference, target CommentTarget, body string) error
	// DeleteComment deletes a sent comment. Deleting a thread's only
	// comment deletes the thread on both forges.
	DeleteComment(ctx context.Context, ref PRReference, target CommentTarget) error
}

// CommentTarget names one sent comment: ThreadID is its thread's
// ReviewThread.ID and CommentID its ReviewComment.ID. A GitHub
// conversation comment's thread ID is its own ID (ReviewThread).
type CommentTarget struct {
	ThreadID  string `json:"threadId"`
	CommentID string `json:"commentId"`
}

// Validate checks that the target names a thread and a comment.
func (t CommentTarget) Validate() error {
	if strings.TrimSpace(t.ThreadID) == "" || strings.TrimSpace(t.CommentID) == "" {
		return errors.New("a comment is named by its thread id and its comment id")
	}
	return nil
}

// UnconfirmedCommentError is a comment or reply posted now whose answer
// did not say whether the forge took it (forgeapi.WriteNotApplied could
// not prove it did not): it may be on the PR, so it is not posted again.
type UnconfirmedCommentError struct {
	Err error
}

func (e *UnconfirmedCommentError) Error() string {
	return "the forge did not confirm the comment: " + e.Err.Error()
}

func (e *UnconfirmedCommentError) Unwrap() error { return e.Err }

// commentWriteError is the error of the one request that creates a
// comment: nil, a write the forge did not apply as it is, any other an
// *UnconfirmedCommentError.
func commentWriteError(err error) error {
	if err == nil || forgeapi.WriteNotApplied(err) {
		return err
	}
	return &UnconfirmedCommentError{Err: err}
}

// CommentNotOnPRError is an edit or delete refused before it reached the
// forge because the comment it names is not on the PR the caller named.
type CommentNotOnPRError struct {
	CommentID string
}

func (e *CommentNotOnPRError) Error() string {
	return fmt.Sprintf("comment %s is not on this pull request", e.CommentID)
}

// HeadMovedError is a write refused before it reached the forge because the
// PR's head is no longer the one the caller's diff shows.
type HeadMovedError struct {
	Want, Have string
}

func (e *HeadMovedError) Error() string {
	return fmt.Sprintf("the head moved from %s to %s", e.Want, e.Have)
}

func (nullForge) PostLineComment(context.Context, PRReference, string, ReviewLineComment) error {
	return ErrUnsupportedForge
}
func (nullForge) PostPRComment(context.Context, PRReference, string) error {
	return ErrUnsupportedForge
}
func (nullForge) EditComment(context.Context, PRReference, CommentTarget, string) error {
	return ErrUnsupportedForge
}
func (nullForge) DeleteComment(context.Context, PRReference, CommentTarget) error {
	return ErrUnsupportedForge
}

// requireCommentBody refuses an empty body before any request.
func requireCommentBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("comment body is required")
	}
	return nil
}

// PostPRLineComment posts one line, range or file comment now (see
// ForgeCommenter.PostLineComment).
func (c *Core) PostPRLineComment(ctx context.Context, ref PRReference, headSHA string, comment ReviewLineComment) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if !mergeHeadSHAPattern.MatchString(strings.TrimSpace(headSHA)) {
		return errors.New("a comment on the diff requires the head commit sha the caller saw")
	}
	if comment.InReplyTo != nil {
		return errors.New("a reply is posted to its thread, not as a line comment")
	}
	if err := requireCommentBody(comment.Body); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).PostLineComment(forgeWrite(ctx), ref, strings.TrimSpace(headSHA), comment)
}

// PostPRComment posts a comment on the PR itself (see
// ForgeCommenter.PostPRComment).
func (c *Core) PostPRComment(ctx context.Context, ref PRReference, body string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := requireCommentBody(body); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).PostPRComment(forgeWrite(ctx), ref, body)
}

// EditPRComment replaces a sent comment's body (see
// ForgeCommenter.EditComment).
func (c *Core) EditPRComment(ctx context.Context, ref PRReference, target CommentTarget, body string) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if err := requireCommentBody(body); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).EditComment(forgeWrite(ctx), ref, target, body)
}

// DeletePRComment deletes a sent comment (see ForgeCommenter.DeleteComment).
func (c *Core) DeletePRComment(ctx context.Context, ref PRReference, target CommentTarget) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).DeleteComment(forgeWrite(ctx), ref, target)
}
