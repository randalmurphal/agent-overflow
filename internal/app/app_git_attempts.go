package app

import (
	"context"
	"fmt"

	gitops "agent-overflow/internal/git"
)

// The git writes a client can lose the answer of, by GitActionResult.Action.
// Each takes an attempt id, fresh for each press, and records its outcome in
// gitActionAttempts: GetGitActionOutcome answers it after a lost answer, and
// a request repeating the id gets the recorded outcome and writes nothing.
const (
	gitActionCommit = "commit"
	gitActionPush   = "push"
	gitActionPull   = "pull"
	gitActionPR     = "pr"
)

// gitAttemptKey names what a git attempt wrote to: its action in one
// workspace, so an id reused for another action or checkout is not answered
// with this one's outcome.
func gitAttemptKey(action string, ws WorkspaceRef) string {
	return action + "\x00" + ws.ProjectID + "\x00" + ws.WorkspacePath
}

// attemptGitAction runs write as attempt attemptID of action in ws. An empty
// id runs it unrecorded (an in-process caller that cannot lose the answer).
// A repeated id waits for and answers with the first arrival's outcome.
func (a *App) attemptGitAction(
	action string, ws WorkspaceRef, attemptID string, write func() (gitops.GitActionResult, error),
) (gitops.GitActionResult, error) {
	if attemptID == "" {
		return write()
	}
	return a.gitActionAttempts.run(a.lifeCtx(), attemptID, gitAttemptKey(action, ws), attemptRefusals{
		other:     fmt.Errorf("%s: attempt %s was another git action or in another workspace", action, attemptID),
		forgotten: fmt.Errorf("%s: this attempt ran before and its outcome is no longer kept; check the workspace before trying again", action),
		panicked:  fmt.Errorf("%s: failed unexpectedly", action),
	}, write)
}

// GitActionOutcome is what a git attempt did: "succeeded" (Result),
// "failed" (Code and Message, the public ones: it wrote nothing it
// reports), "unknown" (this process never received it), or "forgotten" (it
// may have received it and no longer keeps the outcome).
type GitActionOutcome struct {
	State   string                  `json:"state"`
	Result  *gitops.GitActionResult `json:"result,omitempty"`
	Code    string                  `json:"code,omitempty"`
	Message string                  `json:"message,omitempty"`
}

// gitActionFailedCode is the code of a failed git attempt whose error has
// no public code of its own.
const gitActionFailedCode = "git_action_failed"

// GetGitActionOutcome answers what attempt attemptID of action ("commit",
// "push", "pull" or "pr") in ws did, waiting for one still running; its
// budget is the longest of those actions' for that reason.
//
//ao:scope git:operate
//ao:callerbound
//ao:budget 12m
func (a *App) GetGitActionOutcome(ctx context.Context, ws WorkspaceRef, action, attemptID string) (GitActionOutcome, error) {
	switch action {
	case gitActionCommit, gitActionPush, gitActionPull, gitActionPR:
	default:
		return GitActionOutcome{}, fmt.Errorf("git action outcome: unknown action %q", action)
	}
	outcome, err := a.gitActionAttempts.outcome(ctx, attemptID, gitAttemptKey(action, ws), attemptFailure{
		code: gitActionFailedCode, message: gitActionFailedMessages[action],
	})
	if err != nil {
		return GitActionOutcome{}, err
	}
	switch outcome.state {
	case attemptDone:
		return GitActionOutcome{State: "succeeded", Result: &outcome.result}, nil
	case attemptFailed:
		return GitActionOutcome{State: "failed", Code: outcome.code, Message: outcome.message}, nil
	}
	return GitActionOutcome{State: outcome.state}, nil
}

// gitActionFailedMessages are the outcome sentences of a failed git attempt
// whose error has no public message: git's or the forge CLI's own output is
// in the computer's log.
var gitActionFailedMessages = map[string]string{
	gitActionCommit: "The commit failed. The computer's log has git's output.",
	gitActionPush:   "The push failed. The computer's log has git's output.",
	gitActionPull:   "The pull failed. The computer's log has git's output.",
	gitActionPR:     "Creating the pull request failed. The computer's log has the forge's output.",
}
