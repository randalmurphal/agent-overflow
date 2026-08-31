package app

// The branch-prune wire surface: preview (GitListBranchPruneCandidates)
// and delete (GitPruneBranches). The preview dialog is the consent
// surface — nothing here deletes anything the user did not see listed
// at the exact tip they confirmed.

import "agent-overflow/internal/gitapp"

// BranchPruneCandidate is one local branch the prune preview offers for
// deletion. Safe=true rows pre-check in the dialog: either the tip is
// reachable from the default branch, or a merged PR/MR's head SHA
// matches the tip exactly (squash-merge case — everything local was
// pushed before the merge). Everything else surfaces unchecked with the
// reason, and nothing deletes without the user confirming the list.
type BranchPruneCandidate struct {
	Branch      string `json:"branch"`
	Tip         string `json:"tip"`
	Subject     string `json:"subject"`
	Safe        bool   `json:"safe"`
	Reason      string `json:"reason"`
	MergedPRURL string `json:"mergedPrUrl,omitempty"`
}

// BranchPruneCandidates is the preview payload. ForgeWarning carries a
// non-fatal merged-PR lookup failure (no forge, gh/glab missing or
// offline) — candidates then classify on the merged-into-default check
// alone and the dialog shows why squash detection was unavailable.
type BranchPruneCandidates struct {
	Candidates   []BranchPruneCandidate `json:"candidates"`
	ForgeWarning string                 `json:"forgeWarning,omitempty"`
}

// BranchPruneSelection pins one branch the user confirmed for deletion
// to the exact tip the preview showed. Consent is to (branch, tip), not
// to a name — commits that arrive between preview and confirm void it.
type BranchPruneSelection struct {
	Branch string `json:"branch"`
	Tip    string `json:"tip"`
}

// BranchPruneResult reports what GitPruneBranches actually did.
type BranchPruneResult struct {
	Deleted []string          `json:"deleted"`
	Failed  map[string]string `json:"failed,omitempty"`
}

// GitListBranchPruneCandidates refreshes remote-tracking refs
// (`git fetch --all --prune`, so upstream-gone status is current) and
// returns local branches whose upstream is gone, classified for the
// prune preview. Only gone-upstream branches are considered:
// never-pushed local work has no upstream and is structurally excluded,
// as are the default branch and anything checked out in a worktree.
//
// Same locking rationale as GitMaybeFetchRemotes — fetch touches
// refs/remotes/* only; classification is read-only.
//
//ao:scope git:operate
func (a *App) GitListBranchPruneCandidates(threadID string) (BranchPruneCandidates, error) {
	result, err := a.gitApplication().ListBranchPruneCandidates(threadID)
	if err != nil {
		return BranchPruneCandidates{}, err
	}
	candidates := make([]BranchPruneCandidate, len(result.Candidates))
	for index := range result.Candidates {
		candidates[index] = BranchPruneCandidate(result.Candidates[index])
	}
	return BranchPruneCandidates{Candidates: candidates, ForgeWarning: result.ForgeWarning}, nil
}

// GitPruneBranches deletes the selected local branches. Selections are
// re-validated against a freshly computed candidate set: a branch that
// is no longer gone-upstream/unattached (checked out meanwhile, default
// flipped), or whose tip no longer matches the one the preview showed,
// is refused rather than deleted — a stale preview can never
// force-delete work the user did not see. No thread lock is needed: git
// itself refuses to delete any checked-out branch, and every other
// precondition is enforced by the fresh candidate check at delete time.
//
//ao:scope git:operate
func (a *App) GitPruneBranches(threadID string, selections []BranchPruneSelection) (BranchPruneResult, error) {
	projected := make([]gitapp.BranchPruneSelection, len(selections))
	for index := range selections {
		projected[index] = gitapp.BranchPruneSelection(selections[index])
	}
	result, err := a.gitApplication().PruneBranches(threadID, projected)
	return BranchPruneResult(result), err
}
