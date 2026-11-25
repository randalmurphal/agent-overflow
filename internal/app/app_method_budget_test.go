package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/appupdate"
	"agent-overflow/internal/commitmsg"
	"agent-overflow/internal/dictation"
	"agent-overflow/internal/errorsx"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/mcpapp"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/providerupdate"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflowapp"
)

// methodServerBounds is how long each budgeted method's server side may
// run, added up from the constants that bound each step of it, in the
// packages that own them. A step that is one git, gh or glab command or
// one forge request counts transport.DefaultMethodBudget, the bound those
// share; one that runs a user's hook counts hooked, the bound
// gitops.HookTimeout gives it. A method that bounds its whole run with
// withinMethodBudget is its budget by construction.
//
// Not covered: the wait for a lock another run holds (the per-thread action
// lock, a provider check already running is counted where it is the
// method's own). Such a wait is as long as the holder's run, which no
// constant here names.
var methodServerBounds = map[string]time.Duration{
	// The commit's hooks, their stop, and the snapshot and index steps.
	"GitCommit": hooked + transport.DefaultMethodBudget,
	// The pre-push hook and transfer, after the branch and remote reads.
	"GitPush": hooked + transport.DefaultMethodBudget,
	// The transfer and post-merge hook.
	"GitPull": hooked + transport.DefaultMethodBudget,
	// A pull of the current branch, or a fetch of another.
	"GitSyncBranch": hooked + transport.DefaultMethodBudget,
	// The checkout's post-checkout hook, then the status read.
	"GitCheckout": hooked + transport.DefaultMethodBudget,
	// Away from the current ref: a checkout of the base, then of the new
	// branch, each with its post-checkout hook.
	"GitCreateBranchFrom": 2*hooked + transport.DefaultMethodBudget,
	// Waits for the git write it reports.
	"GetGitActionOutcome": max(transport.MethodBudget("GitCommit"), transport.MethodBudget("GitPush"),
		transport.MethodBudget("GitPull"), transport.MethodBudget("GitCreatePR")),
	// A worktree add, whose post-checkout hook runs inside it, between the
	// reads and the branch and lock steps around it.
	"AttachThreadWorktree":  hooked + transport.DefaultMethodBudget,
	"PrepareThreadWorktree": hooked + transport.DefaultMethodBudget,
	// The merge and its hooks, then the worktree cleanup.
	"WorkflowMergeItem": hooked + 2*transport.DefaultMethodBudget,
	// The push's pre-push hook, `gh pr create` or `glab mr create` (whose
	// own push runs it again), the head read and the open-PR lookup.
	"WorkflowCreateItemPR": 2*hooked + 2*transport.DefaultMethodBudget,
	// The worktree name's origin fetch, then its add.
	"CreateThread": hooked + 2*transport.DefaultMethodBudget,
	// The pull request's head fetch, then its worktree's add.
	"OpenPRThread": hooked + 2*transport.DefaultMethodBudget,
	// The primary provider, then the fallback, after the diff reads.
	"GenerateCommitMessage": 2*commitmsg.Timeout + transport.DefaultMethodBudget,
	"FinishDictation":       dictation.TranscribeBudget,
	// A check already running for the provider finishes first.
	"CheckProviderUpdates":   2 * providerupdate.CheckTimeout,
	"GetProviderUpdateNotes": 2 * providerupdate.NotesTimeout,
	// A by-tag resolve or another check holds the updater for one check.
	"CheckForUpdate": 2 * appupdate.UpdaterCheckTimeout,
	// The spend and the reading after it, then the credential commit.
	"SpendProviderAccountReset":  provideraccountapp.ResetSendCutoff + codex.ResetSettleTimeout + 5*time.Second,
	"FinishProviderAccountReset": provideraccountapp.ResetSendCutoff + codex.ResetSettleTimeout + 5*time.Second,
	// `gh pr create` or `glab mr create`, which pushes and so runs the
	// pre-push hook, then the open-PR lookup.
	"GitCreatePR": hooked + transport.DefaultMethodBudget,
	// The Codex fork's cut, then the store and checkout steps.
	"ForkThread":            codexForkTimeout + transport.DefaultMethodBudget,
	"ForkThreadFromMessage": codexForkTimeout + transport.DefaultMethodBudget,
	"ForkSideChat":          codexForkTimeout + transport.DefaultMethodBudget,
	// The provider's OAuth round trip waits on a person in a browser.
	"TriggerMcpAuth":          mcpapp.AuthRoundTripTimeout + transport.DefaultMethodBudget/2,
	"TriggerWorkspaceMcpAuth": mcpapp.AuthRoundTripTimeout + transport.DefaultMethodBudget/2,
	"FetchForgeAttachment":    gitops.ForgeAttachmentTimeout + transport.DefaultMethodBudget,
	"SaveForgeAttachment":     gitops.ForgeAttachmentTimeout + transport.DefaultMethodBudget,
	// withinMethodBudget.
	"SaveCIArtifacts":          transport.MethodBudget("SaveCIArtifacts"),
	"SavePRCIJobLog":           transport.MethodBudget("SavePRCIJobLog"),
	"AgentRemoteFetchArtifact": transport.MethodBudget("AgentRemoteFetchArtifact"),
	"AgentRemoteFetchLog":      transport.MethodBudget("AgentRemoteFetchLog"),
	// A Claude interrupt's acknowledgement, which a burst of kill frames
	// delays, then the turn's settling.
	"InterruptTurn": claude.DefaultControlRequestCeiling + transport.DefaultMethodBudget/2,
	// A forwarded wait, then the page it renders.
	"ThreadToolQuery":    time.Duration(threadtools.MaxForwardedWaitSeconds)*time.Second + 5*time.Second,
	"ThreadOwnerChanges": ownerWaitBound,
	"ThreadToolWatch":    time.Duration(threadtools.MaxForwardedWaitSeconds)*time.Second + 5*time.Second,
	// A spawn may fork (the Codex cut), resolve its worktree's name against
	// origin and add the worktree.
	"ThreadToolCall": codexForkTimeout + hooked + 2*transport.DefaultMethodBudget,
	// withinMethodBudgetFinishing: the run starts no forge request
	// reviewSubmitFinish before its budget.
	"SubmitPRReview":           transport.MethodBudget("SubmitPRReview"),
	"GetPRReviewSubmitOutcome": transport.MethodBudget("SubmitPRReview"),
	// trackerRun: the write and its refresh, cut at the budget.
	"CreateTrackerIssue":        transport.MethodBudget("CreateTrackerIssue"),
	"EditTrackerIssue":          transport.MethodBudget("EditTrackerIssue"),
	"MoveTrackerIssue":          transport.MethodBudget("MoveTrackerIssue"),
	"SetTrackerEstimate":        transport.MethodBudget("SetTrackerEstimate"),
	"RankTrackerIssues":         transport.MethodBudget("RankTrackerIssues"),
	"MoveTrackerIssuesToSprint": transport.MethodBudget("MoveTrackerIssuesToSprint"),
	"CreateTrackerSprint":       transport.MethodBudget("CreateTrackerSprint"),
	"StartTrackerSprint":        transport.MethodBudget("StartTrackerSprint"),
	"CompleteTrackerSprint":     transport.MethodBudget("CompleteTrackerSprint"),
	"AddTrackerComment":         transport.MethodBudget("AddTrackerComment"),
	"EditTrackerComment":        transport.MethodBudget("EditTrackerComment"),
	"DeleteTrackerComment":      transport.MethodBudget("DeleteTrackerComment"),
	"LinkTrackerIssues":         transport.MethodBudget("LinkTrackerIssues"),
	"UnlinkTrackerIssues":       transport.MethodBudget("UnlinkTrackerIssues"),
	"AddTrackerWorklog":         transport.MethodBudget("AddTrackerWorklog"),
	"EditTrackerSprint":         transport.MethodBudget("EditTrackerSprint"),
	"EditTrackerWorklog":        transport.MethodBudget("EditTrackerWorklog"),
	"DeleteTrackerWorklog":      transport.MethodBudget("DeleteTrackerWorklog"),
	"VoteTrackerPoker":          transport.MethodBudget("VoteTrackerPoker"),
	"RevealTrackerPoker":        transport.MethodBudget("RevealTrackerPoker"),
	"ClearTrackerPokerVotes":    transport.MethodBudget("ClearTrackerPokerVotes"),
}

// hooked is one git or forge CLI command that runs a user's hook, stopped.
const hooked = gitops.HookTimeout + gitops.HookStopGrace

// The tripwire for method budgets: no bound method's client waits less than
// its server side may run.
//
// Structure carries a declared budget to every client (methodgen writes it
// into both tables and every client derives its wait from them), but it
// cannot find the bound a declaration must cover: that is the sum of the
// waits along the method's code path, set by constants in other packages,
// and methodgen reads source syntax only. So every budget names its bound
// in methodServerBounds, and this fails when a constant grows past the
// declaration, when a budget appears that no bound justifies, or when an
// unbudgeted method's step outgrows the default. Which methods reach a
// long step at all is TestEveryMethodReachingALongStepDeclaresItsBudget's
// to find.
func TestMethodBudgetsCoverServerBounds(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{}
	for _, method := range transport.GeneratedMethods {
		if method.Budget == 0 {
			continue
		}
		declared[method.Name] = true
		bound, ok := methodServerBounds[method.Name]
		if !ok {
			t.Errorf("%s declares //ao:budget %s but methodServerBounds does not say what it covers", method.Name, method.Budget)
			continue
		}
		if method.Budget < bound {
			t.Errorf("%s's budget %s is under its server bound %s: its clients give up first", method.Name, method.Budget, bound)
		}
	}
	for name, bound := range methodServerBounds {
		if !declared[name] {
			t.Errorf("%s may run %s on the server but declares no //ao:budget, so its clients give up at %s",
				name, bound, transport.ClientWait(name))
		}
	}
	for name, bound := range map[string]time.Duration{
		"workflowapp.MaxWatchHold": workflowapp.MaxWatchHold,
		"readAloudTimeout":         readAloudTimeout,
		"codexReviewRPCTimeout":    codexReviewRPCTimeout,
	} {
		if bound > transport.DefaultMethodBudget {
			t.Errorf("%s (%s) bounds a method with no budget of its own and is past the default %s", name, bound, transport.DefaultMethodBudget)
		}
	}
	if wait := threadPeerCallWait("ThreadToolCall"); wait < transport.MethodBudget("ThreadToolCall")+transport.MethodAnswerMargin {
		t.Errorf("a forwarded thread tool call waits %s, under its destination's budget", wait)
	}
}

// A run cut at its method's budget answers with the budget's own sentence,
// not a context error.
func TestARunCutAtItsBudgetSaysSo(t *testing.T) {
	t.Parallel()
	ctx, cancel := withinMethodBudget(context.Background(), "SaveCIArtifacts", "Saving the CI artifacts")
	defer cancel()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > transport.MethodBudget("SaveCIArtifacts") {
		t.Fatalf("deadline %v, %v; want within the method's budget", deadline, ok)
	}
	if err := budgetError(ctx, errors.New("other")); err.Error() != "other" {
		t.Fatalf("a run that did not reach its budget = %v", err)
	}
	cut, stop := context.WithCancelCause(context.Background())
	stop(budgetExceeded{doing: "Saving the CI artifacts", budget: 30 * time.Minute})
	err := budgetError(cut, context.DeadlineExceeded)
	code, message, _ := errorsx.PublicDetails(err)
	if code != methodBudgetExceededCode || message != "Saving the CI artifacts took longer than 30m0s, so it was stopped." {
		t.Fatalf("budgetError = %q, %q", code, message)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the budget's error drops the run's own")
	}
}
