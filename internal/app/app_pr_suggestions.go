package app

import (
	"context"
	"fmt"

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

// commentBodyMaxBytes bounds a body ParseCommentSuggestions reads: past
// either forge's comment limit.
const commentBodyMaxBytes = 1 << 20

// ParseCommentSuggestions reads the suggestion blocks of a comment body as
// the read does (mdsuggest.Parse), for an editor's preview. It
// reads nothing else and changes nothing.
//
//ao:scope git:operate
//ao:route home
func (a *App) ParseCommentSuggestions(body string) ([]mdsuggest.Block, error) {
	if len(body) > commentBodyMaxBytes {
		return nil, fmt.Errorf("a comment body is at most %d bytes", commentBodyMaxBytes)
	}
	out := mdsuggest.Parse(body)
	if out == nil {
		out = []mdsuggest.Block{}
	}
	return out, nil
}
