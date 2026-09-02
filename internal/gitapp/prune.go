package gitapp

import (
	"fmt"
	"strings"

	gitops "agent-overflow/internal/git"
)

const mergedPRLookupWindow = 200

// BranchPruneCandidate is one branch offered by the prune consent preview.
type BranchPruneCandidate struct {
	Branch      string `json:"branch"`
	Tip         string `json:"tip"`
	Subject     string `json:"subject"`
	Safe        bool   `json:"safe"`
	Reason      string `json:"reason"`
	MergedPRURL string `json:"mergedPrUrl,omitempty"`
}

// BranchPruneCandidates is the preview payload, including a non-fatal forge
// lookup warning when squash-merge classification was unavailable.
type BranchPruneCandidates struct {
	Candidates   []BranchPruneCandidate `json:"candidates"`
	ForgeWarning string                 `json:"forgeWarning,omitempty"`
}

// BranchPruneSelection pins consent to the exact branch tip shown in preview.
type BranchPruneSelection struct {
	Branch string `json:"branch"`
	Tip    string `json:"tip"`
}

// BranchPruneResult reports successful and refused branch deletions.
type BranchPruneResult struct {
	Deleted []string          `json:"deleted"`
	Failed  map[string]string `json:"failed,omitempty"`
}

// ListBranchPruneCandidates refreshes remote-tracking refs and builds the
// consent preview from gone-upstream branches only.
func (s *Service) ListBranchPruneCandidates(ref WorkspaceRef) (BranchPruneCandidates, error) {
	project, _, err := s.ResolveWorkspace(ref)
	if err != nil {
		return BranchPruneCandidates{}, err
	}
	if err := s.core.PruneRemotes(project); err != nil {
		return BranchPruneCandidates{}, err
	}
	candidates, err := s.core.ListPruneCandidates(project)
	if err != nil {
		return BranchPruneCandidates{}, err
	}
	return s.classifyPruneCandidates(project, candidates), nil
}

func pruneMatchKey(name, tip string) string { return name + "\x00" + tip }

func (s *Service) classifyPruneCandidates(project string, candidates []gitops.PruneCandidate) BranchPruneCandidates {
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
		heads, err := s.core.ListMergedPRHeads(project, mergedPRLookupWindow)
		if err != nil {
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
		row := BranchPruneCandidate{Branch: candidate.Name, Tip: candidate.Tip, Subject: candidate.Subject}
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
				row.Reason = "has commits not on the default branch and no matching merged PR — may hold unpushed work"
			}
		}
		out.Candidates = append(out.Candidates, row)
	}
	return out
}

// PruneBranches revalidates eligibility and exact tips before each deletion.
func (s *Service) PruneBranches(ref WorkspaceRef, selections []BranchPruneSelection) (BranchPruneResult, error) {
	project, _, err := s.ResolveWorkspace(ref)
	if err != nil {
		return BranchPruneResult{}, err
	}
	candidates, err := s.core.ListPruneCandidates(project)
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
		if err := s.core.DeleteLocalBranch(project, branch); err != nil {
			fail(branch, err.Error())
			continue
		}
		result.Deleted = append(result.Deleted, branch)
	}
	return result, nil
}
