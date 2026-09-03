package app

import (
	"context"
	"errors"
	"fmt"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/transport"
)

// maxGitWatchHandles bounds the outstanding GitStatusSubscribe handles for
// the whole app. Each distinct cwd behind them costs a recursive fs watch
// (hundreds of inotify watchpoints on a real repo) and a git status cadence,
// so an unbounded handle map is a resource-exhaustion surface reachable by
// anything that can call the binding in a loop — a compromised renderer, a
// buggy caller that never unsubscribes. Panes are counted in single digits;
// this is three orders of magnitude above any real UI.
const maxGitWatchHandles = gitapp.MaxStatusHandles

// ErrTooManyGitStatusSubscriptions is returned once the handle cap is hit.
// Typed so the frontend can tell it apart from a workspace that failed to
// resolve — this one is never fixed by retrying the same call.
var ErrTooManyGitStatusSubscriptions = errors.New("gitwatch: too many active git status subscriptions")

// GitStatusSubscriptionResult is the wire shape returned by
// GitStatusSubscribe. ID is the per-caller handle used to call
// GitStatusUnsubscribe. Cwd is the canonical workspace path the stream is
// keyed on — the frontend maps it back to whatever local spelling it asked
// with, because "git:status" events are addressed by cwd.
type GitStatusSubscriptionResult struct {
	ID     string           `json:"id"`
	Cwd    string           `json:"cwd"`
	Status gitops.GitStatus `json:"status"`
}

// GitStatusEvent is the shape pushed on the "git:status" channel. One
// event per actual change per cwd — gitwatch dedups against the previous
// status before broadcasting, and the App forwards each cwd's stream
// exactly once regardless of how many callers subscribed to it.
//
// The event carries no subscription id on purpose. Git status is workspace
// state, so the wire key is the workspace: two panes on one worktree are
// the normal case, and addressing them by subscription made each hold a
// private copy that could disagree with the other's for minutes.
type GitStatusEvent struct {
	Cwd    string           `json:"cwd"`
	Status gitops.GitStatus `json:"status"`
}

// GitStatusSubscribe begins streaming git-status updates for the
// referenced workspace. Returns the initial status synchronously so the
// caller can render immediately, plus the canonical cwd the stream is
// keyed on and a handle used to call GitStatusUnsubscribe.
//
// It rides `git:operate`: workspace paths are local FS paths and gitwatch
// can spawn recursive fs watches plus continuous git invocations, so a
// session without that grant is refused rather than told where repos live
// or left free to enumerate threads by probing.
//
// The subscription is automatically released when the calling WS
// connection drops (via transport.ConnState cleanup). The frontend
// SHOULD still call GitStatusUnsubscribe on unmount; the
// connection-tied cleanup is the safety net for unclean disconnects.
//
//ao:scope git:operate
func (a *App) GitStatusSubscribe(ctx context.Context, ws WorkspaceRef) (GitStatusSubscriptionResult, error) {
	if a.shuttingDown.Load() {
		return GitStatusSubscriptionResult{}, ErrShuttingDown
	}
	result, err := a.gitApplication().Subscribe(ws)
	if err != nil {
		if errors.Is(err, gitapp.ErrTooManyStatusSubscriptions) {
			return GitStatusSubscriptionResult{}, ErrTooManyGitStatusSubscriptions
		}
		return GitStatusSubscriptionResult{}, err
	}
	if state := transport.ConnStateFromContext(ctx); state != nil {
		if !state.RegisterCleanup(func() { a.gitApplication().Unsubscribe(result.ID) }) {
			a.gitApplication().Unsubscribe(result.ID)
			return GitStatusSubscriptionResult{}, fmt.Errorf("gitwatch: connection closing")
		}
	}
	return GitStatusSubscriptionResult(result), nil
}

// GitStatusUnsubscribe releases a subscription previously created via
// GitStatusSubscribe. Idempotent — unknown ids and double-unsubscribes
// are no-ops because the connection-cleanup safety net may have run
// first on disconnect.
//
//ao:scope git:operate
//ao:route home
func (a *App) GitStatusUnsubscribe(subscriptionID string) error {
	a.gitApplication().Unsubscribe(subscriptionID)
	return nil
}
