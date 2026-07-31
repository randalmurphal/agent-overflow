package main

// The branch-prune wire surface: preview (GitListBranchPruneCandidates)
// and delete (GitPruneBranches). The preview dialog is the consent
// surface — nothing here deletes anything the user did not see listed
// at the exact tip they confirmed.

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
)

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

// mergedPRLookupWindow bounds the bulk merged-PR list backing squash
// detection (both forges honor it; GitLab pages its 100-cap). Branches
// merged further back than this many PRs surface as "no merged PR
// found" — unchecked, never wrongly deleted.
const mergedPRLookupWindow = 200

// GitListBranchPruneCandidates refreshes remote-tracking refs
// (`git fetch --all --prune`, so upstream-gone status is current) and
// returns local branches whose upstream is gone, classified for the
// prune preview. Only gone-upstream branches are considered:
// never-pushed local work has no upstream and is structurally excluded,
// as are the default branch and anything checked out in a worktree.
//
// Same locking rationale as GitMaybeFetchRemotes — fetch touches
// refs/remotes/* only; classification is read-only.
func (a *App) GitListBranchPruneCandidates(threadID string) (BranchPruneCandidates, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return BranchPruneCandidates{}, err
	}
	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return BranchPruneCandidates{}, err
	}
	core := a.gitCore()
	if err := core.PruneRemotes(project); err != nil {
		return BranchPruneCandidates{}, err
	}
	candidates, err := core.ListPruneCandidates(project)
	if err != nil {
		return BranchPruneCandidates{}, err
	}
	return a.classifyPruneCandidates(core, project, candidates), nil
}

// pruneMatchKey joins a branch name and tip oid with a NUL — legal in
// neither — for exact (name, tip) map lookups.
func pruneMatchKey(name, tip string) string {
	return name + "\x00" + tip
}

func (a *App) classifyPruneCandidates(
	core *gitops.Core,
	project string,
	candidates []gitops.PruneCandidate,
) BranchPruneCandidates {
	needForge := false
	for _, candidate := range candidates {
		if !candidate.MergedIntoDefault {
			needForge = true
			break
		}
	}

	prByTip := make(map[string]gitops.MergedPRHead)
	forgeWarning := ""
	if needForge {
		heads, err := core.ListMergedPRHeads(project, mergedPRLookupWindow)
		if err != nil {
			// Non-fatal by design: the preview still protects (unmerged
			// rows stay unchecked); the dialog shows why squash-merge
			// detection was unavailable.
			forgeWarning = fmt.Sprintf("merged PR lookup unavailable: %v", err)
		}
		for _, head := range heads {
			prByTip[pruneMatchKey(head.HeadRefName, head.HeadOid)] = head
		}
	}

	out := BranchPruneCandidates{
		Candidates:   make([]BranchPruneCandidate, 0, len(candidates)),
		ForgeWarning: forgeWarning,
	}
	for _, candidate := range candidates {
		row := BranchPruneCandidate{
			Branch:  candidate.Name,
			Tip:     candidate.Tip,
			Subject: candidate.Subject,
		}
		switch {
		case candidate.MergedIntoDefault:
			row.Safe = true
			row.Reason = "merged into the default branch"
		default:
			if head, ok := prByTip[pruneMatchKey(candidate.Name, candidate.Tip)]; ok {
				row.Safe = true
				row.Reason = "fully pushed and merged (squash)"
				row.MergedPRURL = head.URL
			} else {
				row.Safe = false
				row.Reason = "has commits not on the default branch and no matching merged PR — may hold unpushed work"
			}
		}
		out.Candidates = append(out.Candidates, row)
	}
	return out
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

// GitPruneBranches deletes the selected local branches. Selections are
// re-validated against a freshly computed candidate set: a branch that
// is no longer gone-upstream/unattached (checked out meanwhile, default
// flipped), or whose tip no longer matches the one the preview showed,
// is refused rather than deleted — a stale preview can never
// force-delete work the user did not see. No thread lock is needed: git
// itself refuses to delete any checked-out branch, and every other
// precondition is enforced by the fresh candidate check at delete time.
func (a *App) GitPruneBranches(threadID string, selections []BranchPruneSelection) (BranchPruneResult, error) {
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return BranchPruneResult{}, err
	}
	project, _, err := a.resolveGitPaths(thread)
	if err != nil {
		return BranchPruneResult{}, err
	}
	core := a.gitCore()
	candidates, err := core.ListPruneCandidates(project)
	if err != nil {
		return BranchPruneResult{}, err
	}
	eligibleTip := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		eligibleTip[candidate.Name] = candidate.Tip
	}

	result := BranchPruneResult{Deleted: []string{}}
	fail := func(branch, reason string) {
		if result.Failed == nil {
			result.Failed = make(map[string]string)
		}
		result.Failed[branch] = reason
	}
	processed := make(map[string]bool, len(selections))
	for _, selection := range selections {
		branch := strings.TrimSpace(selection.Branch)
		if branch == "" || processed[branch] {
			// A duplicate selection already has its outcome recorded;
			// re-running it would double-report the branch as both
			// deleted and failed.
			continue
		}
		processed[branch] = true
		tip, ok := eligibleTip[branch]
		if !ok {
			fail(branch, "no longer eligible for pruning; refresh the preview")
			continue
		}
		if tip != strings.TrimSpace(selection.Tip) {
			fail(branch, "branch changed since the preview; refresh and re-check")
			continue
		}
		if err := core.DeleteLocalBranch(project, branch); err != nil {
			fail(branch, err.Error())
			continue
		}
		result.Deleted = append(result.Deleted, branch)
	}
	return result, nil
}
