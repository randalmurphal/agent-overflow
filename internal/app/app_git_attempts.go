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
) (result gitops.GitActionResult, err error) {
	if attemptID == "" {
		return write()
	}
	key := gitAttemptKey(action, ws)
	attempt, repeated := a.gitActionAttempts.begin(attemptID, key)
	if repeated {
		if attempt.key != key {
			return gitops.GitActionResult{}, fmt.Errorf("%s: attempt %s was another git action or in another workspace", action, attemptID)
		}
		if attempt.forgotten {
			return gitops.GitActionResult{}, gitAttemptForgotten(action)
		}
		return attempt.wait(a.lifeCtx())
	}
	defer func() {
		// A panic still settles the attempt, so nothing waits on it forever.
		if recovered := recover(); recovered != nil {
			a.gitActionAttempts.finish(attempt, gitops.GitActionResult{}, fmt.Errorf("%s: failed unexpectedly", action))
			panic(recovered)
		}
		a.gitActionAttempts.finish(attempt, result, err)
	}()
	return write()
}

// gitAttemptForgotten answers a repeated attempt whose outcome this process
// no longer keeps.
func gitAttemptForgotten(action string) error {
	return fmt.Errorf("%s: this attempt ran before and its outcome is no longer kept; check the workspace before trying again", action)
}

// GitActionOutcome is what a git attempt did: "succeeded" (Result),
// "failed" (Message: it wrote nothing it reports), "unknown" (this process
// never received it), or "forgotten" (it may have received it and no longer
// keeps the outcome).
type GitActionOutcome struct {
	State   string                  `json:"state"`
	Result  *gitops.GitActionResult `json:"result,omitempty"`
	Message string                  `json:"message,omitempty"`
}

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
	attempt, forgotten := a.gitActionAttempts.find(attemptID, gitAttemptKey(action, ws))
	if forgotten {
		return GitActionOutcome{State: "forgotten"}, nil
	}
	if attempt == nil {
		return GitActionOutcome{State: "unknown"}, nil
	}
	result, err := attempt.wait(ctx)
	if ctx.Err() != nil {
		return GitActionOutcome{}, ctx.Err()
	}
	if err != nil {
		return GitActionOutcome{State: "failed", Message: err.Error()}, nil
	}
	return GitActionOutcome{State: "succeeded", Result: &result}, nil
}
