package app

import (
	"context"
	"errors"
	"strings"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
)

// The comment actions of the review pane: a comment posted now (on a line,
// a range, a file or the PR itself), a reply now, and an edit or delete of
// a sent comment. Each follows the merge actions' shape (app_forge_merge.go):
// admitted and validated here, run through git.Core, and on success the
// PR's pump polls at once and the answer carries its sequence after that
// poll (PRActionResult.Seq), so the pane closes the editor once the frame
// with the forge's answer is applied. A failure is the one-sentence
// ForgeActionError, and the pane keeps the editor and its text.

// PRLineCommentRequest is a comment posted now on the PR's diff, anchored
// as a draft is (store.DiffReviewCommentInput): Side "file" for a file,
// "old", "new" or "context" for a line, with StartOldLine and StartNewLine
// the first line of a range. HeadSHA is the head of the diff the caller
// shows; the forge refuses a comment once the head moved on GitLab, and
// GitHub pins the comment to that commit.
type PRLineCommentRequest struct {
	HeadSHA      string `json:"headSha"`
	FilePath     string `json:"filePath"`
	Side         string `json:"side"`
	OldLine      int    `json:"oldLine,omitempty"`
	NewLine      int    `json:"newLine,omitempty"`
	StartOldLine int    `json:"startOldLine,omitempty"`
	StartNewLine int    `json:"startNewLine,omitempty"`
	Body         string `json:"body"`
}

// lineComment is the request as the forge's comment, its anchor checked as
// a draft's is.
func (r PRLineCommentRequest) lineComment() (gitops.ReviewLineComment, error) {
	anchor := store.DiffReviewComment{
		Scope: string(store.DiffReviewScopePR), FilePath: r.FilePath, Side: r.Side,
		OldLine: r.OldLine, NewLine: r.NewLine, StartOldLine: r.StartOldLine, StartNewLine: r.StartNewLine,
		Body: r.Body,
	}
	if err := store.NormalizeDiffReviewLineAnchor(&anchor); err != nil {
		return gitops.ReviewLineComment{}, err
	}
	comment, ok := reviewLineCommentForDraft(anchor)
	if !ok {
		return gitops.ReviewLineComment{}, errInvalidLineAnchor
	}
	return comment, nil
}

var errInvalidLineAnchor = errors.New("the comment names no line of the diff")

// PostPRLineComment posts one comment on a line, range or file of pr's
// diff now.
//
//ao:scope git:operate
//ao:route selected
func (a *App) PostPRLineComment(ctx context.Context, pr gitops.PRReference, req PRLineCommentRequest) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	comment, err := req.lineComment()
	if err != nil {
		return PRActionResult{}, err
	}
	err = a.gitCore().PostPRLineComment(forgeapi.WithInteractive(ctx), pr, req.HeadSHA, comment)
	return a.settlePRAction(ctx, pr, gitops.ActionComment, err)
}

// PostPRComment posts a comment on pr itself, not on a line: a GitHub
// conversation comment, a GitLab merge request note.
//
//ao:scope git:operate
//ao:route selected
func (a *App) PostPRComment(ctx context.Context, pr gitops.PRReference, body string) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	commentID, err := a.gitCore().PostPRComment(forgeapi.WithInteractive(ctx), pr, body)
	result, err := a.settlePRAction(ctx, pr, gitops.ActionComment, err)
	if err == nil {
		result.CommentID = commentID
	}
	return result, err
}

// ReplyToPRThread posts a reply to one thread now. threadID is the thread's
// id and databaseID its first comment's (GitHub posts the reply under it).
// A thread that is gone is refused as kind not_found.
//
//ao:scope git:operate
//ao:route selected
func (a *App) ReplyToPRThread(ctx context.Context, pr gitops.PRReference, threadID string, databaseID int64, body string) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().ReplyToThread(forgeapi.WithInteractive(ctx), pr, strings.TrimSpace(threadID), databaseID, body)
	return a.settlePRAction(ctx, pr, gitops.ActionReply, err)
}

// EditPRComment replaces the body of a sent comment. The forge decides
// whether the viewer may (ReviewComment.ViewerCanEdit is its answer).
//
//ao:scope git:operate
//ao:route selected
func (a *App) EditPRComment(ctx context.Context, pr gitops.PRReference, target gitops.CommentTarget, body string) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().EditPRComment(forgeapi.WithInteractive(ctx), pr, target, body)
	return a.settlePRAction(ctx, pr, gitops.ActionEdit, err)
}

// DeletePRComment deletes a sent comment; a thread's only comment takes
// the thread with it on the forge, and the next frame shows it gone.
//
//ao:scope git:operate
//ao:route selected
func (a *App) DeletePRComment(ctx context.Context, pr gitops.PRReference, target gitops.CommentTarget) (PRActionResult, error) {
	if err := a.admitPRAction(pr); err != nil {
		return PRActionResult{}, err
	}
	err := a.gitCore().DeletePRComment(forgeapi.WithInteractive(ctx), pr, target)
	return a.settlePRAction(ctx, pr, gitops.ActionDelete, err)
}
