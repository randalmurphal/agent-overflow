package app

import (
	"context"
	"fmt"
	"unicode/utf8"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/mdsuggest"
)

// The review pane's suggestion and reaction actions. Each runs as a
// management write does (runPRManage): admitted, the user's own
// (interactive), one at a time per PR under the PR's write lock (an apply
// has a precondition, the head, and a reaction write reads before it
// writes), and settled with the PR's pump polling at once
// (PRActionResult.Seq). An apply moves the head; the pump's next frame
// carries it and the pane's stale-head flow takes over. See
// docs/architecture/forge-transport.md, "Suggestions and reactions".

// ApplyPRSuggestion applies one suggestion of a thread comment: GitLab's
// apply, or on GitHub a commit of the rewritten file to the head branch
// followed by resolving the thread.
//
//ao:scope git:operate
//ao:route selected
func (a *App) ApplyPRSuggestion(ctx context.Context, pr gitops.PRReference, req gitops.SuggestionApply) (PRActionResult, error) {
	return a.runPRManage(ctx, pr, gitops.ActionApplySuggestion, func(ctx context.Context) error {
		return a.gitCore().ApplyPRSuggestion(ctx, pr, req)
	})
}

// SetPRCommentReaction adds (reacted true) or removes the viewer's
// reaction name (gitops.CommentReaction.Name) on one comment. The
// reaction already as asked answers success.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SetPRCommentReaction(ctx context.Context, pr gitops.PRReference, target gitops.CommentTarget, name string, reacted bool) (PRActionResult, error) {
	return a.runPRManage(ctx, pr, gitops.ActionReact, func(ctx context.Context) error {
		return a.gitCore().SetPRCommentReaction(ctx, pr, target, name, reacted)
	})
}

// commentBodyMaxChars bounds a body ParseCommentSuggestions reads: the
// larger forge comment limit, GitLab's 1,000,000 characters for a note
// (GitHub's is smaller). UTF-8 takes at most 4 bytes a character, so a
// body past 4 bytes a character is refused before it is counted.
const commentBodyMaxChars = 1_000_000

// ParseCommentSuggestions reads the suggestion blocks of a comment body as
// the read does (mdsuggest.Parse), each with its fence lines in the body
// (StartLine, EndLine), for an editor's preview. It reads nothing else
// and changes nothing.
//
//ao:scope git:operate
//ao:route home
func (a *App) ParseCommentSuggestions(body string) ([]mdsuggest.Block, error) {
	if len(body) > 4*commentBodyMaxChars || utf8.RuneCountInString(body) > commentBodyMaxChars {
		return nil, fmt.Errorf("a comment body is at most %d characters", commentBodyMaxChars)
	}
	out := mdsuggest.Parse(body)
	if out == nil {
		out = []mdsuggest.Block{}
	}
	return out, nil
}
