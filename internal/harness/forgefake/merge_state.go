package forgefake

// Each forge's merge verdict for a pull, derived from the fixture the same
// way for every reader: the app's PR reads, the CLI's views and a merge
// call's refusal. The fake treats every check as required and any
// CHANGES_REQUESTED review as blocking, which is the strict branch
// protection both forges offer.

// ciBlocks reports whether the pull's CI keeps it from merging now:
// running, pending or failed.
func (p *Pull) ciBlocks() bool {
	return p.CI != nil && p.CI.Status != "success" && p.CI.Status != "skipped" && p.CI.Status != "canceled"
}

func (p *Pull) changesRequested() bool {
	return githubReviewDecision(p.Reviews) == "CHANGES_REQUESTED"
}

// githubMergeStateStatus is GitHub's mergeStateStatus.
func githubMergeStateStatus(p *Pull) string {
	switch {
	case p.State != "open":
		return "UNKNOWN"
	case p.Mergeable == "conflicts":
		return "DIRTY"
	case p.Mergeable == "checking":
		return "UNKNOWN"
	case p.Draft:
		return "DRAFT"
	case p.Mergeable == "behind":
		return "BEHIND"
	case p.ciBlocks() || p.changesRequested():
		return "BLOCKED"
	}
	return "CLEAN"
}

// githubMergeable is GitHub's mergeable: whether the merge commit can be
// created, independent of checks and reviews.
func githubMergeable(p *Pull) string {
	switch p.Mergeable {
	case "conflicts":
		return "CONFLICTING"
	case "checking":
		return "UNKNOWN"
	}
	return "MERGEABLE"
}

// gitlabDetailedMergeStatus is GitLab's detailed_merge_status.
func gitlabDetailedMergeStatus(p *Pull) string {
	switch {
	case p.State != "open":
		return "not_open"
	case p.Mergeable == "checking":
		return "checking"
	case p.Mergeable == "conflicts":
		return "conflict"
	case p.Draft:
		return "draft_status"
	case p.Mergeable == "behind":
		return "need_rebase"
	case p.CI != nil && (p.CI.Status == "running" || p.CI.Status == "pending"):
		return "ci_still_running"
	case p.ciBlocks():
		return "ci_must_pass"
	case p.changesRequested():
		return "requested_changes"
	}
	return "mergeable"
}
