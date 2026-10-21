package git

import (
	"fmt"
	"slices"
	"strings"
)

// GitHub's merge readiness, merge options and forge events, derived from
// the PRTick detail and timeline (github_read.go). mergeStateStatus is the
// forge's verdict; the blockers that explain a BLOCKED, UNSTABLE, DIRTY or
// BEHIND verdict come from reviewDecision and the opinionated reviews,
// the rollup's required contexts, the thread resolution states and the
// base's protection and rulesets. See docs/specs/pull-requests.md.

// githubPullMergeRaw is the merge half of a PR's detail selection.
type githubPullMergeRaw struct {
	Merged                    bool            `json:"merged"`
	MergedAt                  string          `json:"mergedAt"`
	MergedBy                  *githubActorRaw `json:"mergedBy"`
	ClosedAt                  string          `json:"closedAt"`
	ViewerDidAuthor           bool            `json:"viewerDidAuthor"`
	IsCrossRepository         bool            `json:"isCrossRepository"`
	ViewerCanEnableAutoMerge  bool            `json:"viewerCanEnableAutoMerge"`
	ViewerCanDisableAutoMerge bool            `json:"viewerCanDisableAutoMerge"`
	AutoMergeRequest          *struct {
		EnabledAt   string          `json:"enabledAt"`
		MergeMethod string          `json:"mergeMethod"`
		EnabledBy   *githubActorRaw `json:"enabledBy"`
	} `json:"autoMergeRequest"`
	LatestOpinionatedReviews struct {
		Nodes []githubOpinionRaw `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	ThreadStates githubThreadStatesRaw `json:"threadStates"`
	BaseRef      *githubBaseRefRaw     `json:"baseRef"`
}

// githubOpinionRaw is one latestOpinionatedReviews node: a reviewer's
// latest approval or change request. authorCanPushToRepository says
// whether it counts toward the required approvals.
type githubOpinionRaw struct {
	ID                        string         `json:"id"`
	State                     string         `json:"state"`
	ViewerDidAuthor           bool           `json:"viewerDidAuthor"`
	AuthorCanPushToRepository bool           `json:"authorCanPushToRepository"`
	Author                    githubActorRaw `json:"author"`
}

type githubThreadStatesRaw struct {
	PageInfo githubPageInfo `json:"pageInfo"`
	Nodes    []struct {
		IsResolved bool `json:"isResolved"`
	} `json:"nodes"`
}

// githubBaseRefRaw is the base branch's protection (refUpdateRule, null
// when classic protection does not cover it) and its active ruleset rules.
type githubBaseRefRaw struct {
	RefUpdateRule *struct {
		RequiredApprovingReviewCount   *int     `json:"requiredApprovingReviewCount"`
		RequiredStatusCheckContexts    []string `json:"requiredStatusCheckContexts"`
		RequiresCodeOwnerReviews       bool     `json:"requiresCodeOwnerReviews"`
		RequiresConversationResolution bool     `json:"requiresConversationResolution"`
	} `json:"refUpdateRule"`
	Rules struct {
		Nodes []struct {
			Type       string `json:"type"`
			Parameters *struct {
				RequiredApprovingReviewCount   int      `json:"requiredApprovingReviewCount"`
				RequireCodeOwnerReview         bool     `json:"requireCodeOwnerReview"`
				RequiredReviewThreadResolution bool     `json:"requiredReviewThreadResolution"`
				AllowedMergeMethods            []string `json:"allowedMergeMethods"`
				RequiredStatusChecks           []struct {
					Context string `json:"context"`
				} `json:"requiredStatusChecks"`
			} `json:"parameters"`
		} `json:"nodes"`
	} `json:"rules"`
}

// githubRepositoryMergeRaw is the repository half of the detail.
// viewerPermission is null for a GitHub App token; viewerDefaultMergeMethod
// is the viewer's last used method or the repository's default.
type githubRepositoryMergeRaw struct {
	MergeCommitAllowed       bool   `json:"mergeCommitAllowed"`
	SquashMergeAllowed       bool   `json:"squashMergeAllowed"`
	RebaseMergeAllowed       bool   `json:"rebaseMergeAllowed"`
	DeleteBranchOnMerge      bool   `json:"deleteBranchOnMerge"`
	AutoMergeAllowed         bool   `json:"autoMergeAllowed"`
	ViewerPermission         string `json:"viewerPermission"`
	ViewerDefaultMergeMethod string `json:"viewerDefaultMergeMethod"`
}

type githubTimelineRaw struct {
	PageInfo struct {
		HasPreviousPage bool   `json:"hasPreviousPage"`
		StartCursor     string `json:"startCursor"`
	} `json:"pageInfo"`
	Nodes []githubTimelineItemRaw `json:"nodes"`
}

type githubOIDRaw struct {
	OID string `json:"oid"`
}

// githubTimelineItemRaw is one timeline item of the types PRTick selects,
// told apart by __typename. A review carries author and submittedAt, the
// events actor and createdAt.
type githubTimelineItemRaw struct {
	Typename         string          `json:"__typename"`
	ID               string          `json:"id"`
	CreatedAt        string          `json:"createdAt"`
	SubmittedAt      string          `json:"submittedAt"`
	State            string          `json:"state"`
	Author           *githubActorRaw `json:"author"`
	Actor            *githubActorRaw `json:"actor"`
	Commit           *githubOIDRaw   `json:"commit"`
	AfterCommit      *githubOIDRaw   `json:"afterCommit"`
	DismissalMessage string          `json:"dismissalMessage"`
	Reason           string          `json:"reason"`
	CurrentRefName   string          `json:"currentRefName"`
	Review           *struct {
		Author *githubActorRaw `json:"author"`
	} `json:"review"`
	RequestedReviewer *struct {
		Login        string `json:"login"`
		CombinedSlug string `json:"combinedSlug"`
	} `json:"requestedReviewer"`
}

// githubMergeInputs is what readiness and options are derived from.
type githubMergeInputs struct {
	pull     *githubPullRaw
	repo     githubRepositoryMergeRaw
	contexts []githubContextRaw
}

// githubBaseRules is the base branch's merge requirements, protection and
// rulesets combined: every one must hold, so counts take the maximum and
// flags the union. methods is nil when no rule restricts them.
type githubBaseRules struct {
	approvals      int
	codeOwners     bool
	resolveThreads bool
	requiredChecks []string
	methods        []string
}

func (in githubMergeInputs) baseRules() githubBaseRules {
	var out githubBaseRules
	base := in.pull.BaseRef
	if base == nil {
		return out
	}
	if rule := base.RefUpdateRule; rule != nil {
		if rule.RequiredApprovingReviewCount != nil {
			out.approvals = *rule.RequiredApprovingReviewCount
		}
		out.codeOwners = rule.RequiresCodeOwnerReviews
		out.resolveThreads = rule.RequiresConversationResolution
		out.requiredChecks = append(out.requiredChecks, rule.RequiredStatusCheckContexts...)
	}
	for _, rule := range base.Rules.Nodes {
		switch rule.Type {
		case "REQUIRED_REVIEW_THREAD_RESOLUTION":
			out.resolveThreads = true
		}
		params := rule.Parameters
		if params == nil {
			continue
		}
		switch rule.Type {
		case "PULL_REQUEST":
			out.approvals = max(out.approvals, params.RequiredApprovingReviewCount)
			out.codeOwners = out.codeOwners || params.RequireCodeOwnerReview
			out.resolveThreads = out.resolveThreads || params.RequiredReviewThreadResolution
			if len(params.AllowedMergeMethods) > 0 {
				allowed := make([]string, 0, len(params.AllowedMergeMethods))
				for _, method := range params.AllowedMergeMethods {
					if name := githubMergeMethodName(method); name != "" && (out.methods == nil || slices.Contains(out.methods, name)) {
						allowed = append(allowed, name)
					}
				}
				out.methods = allowed
			}
		case "REQUIRED_STATUS_CHECKS":
			for _, check := range params.RequiredStatusChecks {
				out.requiredChecks = append(out.requiredChecks, check.Context)
			}
		}
	}
	return out
}

func (in githubMergeInputs) open() bool { return strings.EqualFold(in.pull.State, "OPEN") }

// needsThreadStates reports whether readiness needs the thread states
// past their first page: the base requires resolution, the PR is open
// and no unresolved thread was in the first page.
func (in githubMergeInputs) needsThreadStates() bool {
	return in.open() && in.pull.ThreadStates.PageInfo.HasNextPage && in.unresolvedThreads() == 0 && in.baseRules().resolveThreads
}

// approvalCount is the approvals standing in latestOpinionatedReviews
// against the base's required count.
func (in githubMergeInputs) approvalCount() ApprovalCount {
	out := ApprovalCount{Required: in.baseRules().approvals}
	for _, review := range in.pull.LatestOpinionatedReviews.Nodes {
		if strings.EqualFold(review.State, "APPROVED") {
			out.Given++
		}
	}
	return out
}

func (in githubMergeInputs) unresolvedThreads() int {
	n := 0
	for _, node := range in.pull.ThreadStates.Nodes {
		if !node.IsResolved {
			n++
		}
	}
	return n
}

// readiness maps mergeStateStatus to the state and, when GitHub would not
// merge now, the blockers that explain why. A draft or checking PR lists
// no blockers (the state is the row); UNSTABLE is GitHub's "mergeable with
// non-passing checks that are not required", so it is ready unless the
// expansion finds a blocker. A BLOCKED verdict nothing explains is a rules
// blocker.
func (in githubMergeInputs) readiness() MergeReadiness {
	pull := in.pull
	out := MergeReadiness{Blockers: []MergeBlocker{}, Settled: []MergeBlocker{}, Approvals: in.approvalCount()}
	if !pull.ThreadStates.PageInfo.HasNextPage {
		unresolved := in.unresolvedThreads()
		out.Unresolved = &unresolved
	}
	switch {
	case pull.Merged || strings.EqualFold(pull.State, "MERGED"):
		out.State = MergeStateMerged
		return out
	case strings.EqualFold(pull.State, "CLOSED"):
		out.State = MergeStateClosed
		return out
	}
	status := strings.ToUpper(strings.TrimSpace(pull.MergeStateStatus))
	blockers, settled := in.conditions(status)
	out.Settled = settled
	switch {
	case pull.IsDraft || status == "DRAFT":
		// A draft leads with the draft itself (its fix is Mark ready), then
		// whatever else would block once it is ready.
		out.State = MergeStateDraft
		out.Blockers = append([]MergeBlocker{{Kind: BlockerDraft, Text: "This pull request is a draft", Detail: "Mark it ready for review to merge"}}, blockers...)
		return out
	case status == "UNKNOWN" || status == "":
		out.State = MergeStateChecking
		return out
	case status == "CLEAN" || status == "HAS_HOOKS":
		out.State = MergeStateReady
		return out
	}
	switch status {
	case "DIRTY":
		blockers = append(blockers, MergeBlocker{Kind: BlockerConflicts, Text: "This branch has conflicts with " + pull.BaseRefName})
	case "BEHIND":
		blockers = append(blockers, MergeBlocker{Kind: BlockerRebase, Text: "This branch is out of date with " + pull.BaseRefName, Detail: "The base branch requires branches to be up to date before merging"})
	}
	if status == "BLOCKED" && len(blockers) == 0 {
		blockers = append(blockers, MergeBlocker{Kind: BlockerRules, Text: "Merging is blocked by the rules on " + pull.BaseRefName})
	}
	if len(blockers) == 0 {
		out.State = MergeStateReady
		return out
	}
	out.State = MergeStateBlocked
	out.Blockers = blockers
	return out
}

// conditions expands reviews, required checks, threads and conflicts into
// the conditions that fail (blockers, in GitHub's merge box order) and
// the ones that hold (settled).
func (in githubMergeInputs) conditions(status string) (blockers, settled []MergeBlocker) {
	blockers, settled = []MergeBlocker{}, []MergeBlocker{}
	rules := in.baseRules()
	add := func(ok bool, row MergeBlocker) {
		if ok {
			settled = append(settled, row)
		} else {
			blockers = append(blockers, row)
		}
	}

	var counted, approvedBy, changesBy []string
	for _, review := range in.pull.LatestOpinionatedReviews.Nodes {
		name := firstNonEmpty(review.Author.Name, review.Author.Login)
		switch strings.ToUpper(review.State) {
		case "APPROVED":
			approvedBy = append(approvedBy, name)
			if review.AuthorCanPushToRepository {
				counted = append(counted, name)
			}
		case "CHANGES_REQUESTED":
			changesBy = append(changesBy, name)
		}
	}
	switch strings.ToUpper(in.pull.ReviewDecision) {
	case "CHANGES_REQUESTED":
		add(false, MergeBlocker{Kind: BlockerChangesRequested, Text: "Changes requested", Detail: "by " + nameList(changesBy)})
	case "REVIEW_REQUIRED":
		text := "Needs approval"
		if need := rules.approvals - len(counted); need > 0 {
			text = fmt.Sprintf("Needs %d more %s", need, plural(need, "approval", "approvals"))
		} else if rules.codeOwners {
			text = "Needs approval from a code owner"
		}
		add(false, MergeBlocker{Kind: BlockerApprovals, Text: text, Detail: approvalDetail(counted, rules.approvals)})
	default:
		if len(approvedBy) > 0 {
			add(true, MergeBlocker{Kind: BlockerApprovals, Text: fmt.Sprintf("Approved by %d %s", len(approvedBy), plural(len(approvedBy), "reviewer", "reviewers")), Detail: nameList(approvedBy)})
		}
	}

	if row, ok, present := in.checksCondition(rules); present {
		add(ok, row)
	}

	if rules.resolveThreads {
		if n := in.unresolvedThreads(); n > 0 {
			add(false, MergeBlocker{Kind: BlockerThreads, Text: fmt.Sprintf("%d unresolved %s", n, plural(n, "conversation", "conversations"))})
		} else {
			add(true, MergeBlocker{Kind: BlockerThreads, Text: "All conversations resolved"})
		}
	}

	if strings.EqualFold(in.pull.Mergeable, "MERGEABLE") && status != "DIRTY" {
		add(true, MergeBlocker{Kind: BlockerConflicts, Text: "No conflicts with " + in.pull.BaseRefName})
	}
	return blockers, settled
}

// checksCondition is the required checks' row: a blocker naming the
// required contexts that failed or have not passed yet (an expected one
// that has not reported counts as pending), else a settled row. present is
// false when the head has no checks and none are required.
func (in githubMergeInputs) checksCondition(rules githubBaseRules) (row MergeBlocker, ok, present bool) {
	var failed, pending []string
	var url string
	reported := map[string]bool{}
	var others CheckSummary
	for _, raw := range in.contexts {
		check, known := githubCheckStatus(raw)
		if !known {
			continue
		}
		reported[check.Name] = true
		if !raw.IsRequired {
			others.Total++
			addCheckBucket(&others, check)
			continue
		}
		var bucket CheckSummary
		addCheckBucket(&bucket, check)
		switch {
		case bucket.Failure > 0 || bucket.Canceled > 0:
			failed = append(failed, check.Name)
			if url == "" {
				url = check.DetailsURL
			}
		case bucket.Pending > 0:
			pending = append(pending, check.Name)
		}
	}
	for _, name := range rules.requiredChecks {
		if !reported[name] && !slices.Contains(pending, name) {
			pending = append(pending, name)
		}
	}
	if len(failed)+len(pending) > 0 {
		var text string
		switch {
		case len(failed) > 0 && len(pending) > 0:
			text = fmt.Sprintf("%d required %s failed, %d pending", len(failed), plural(len(failed), "check", "checks"), len(pending))
		case len(failed) > 0:
			text = fmt.Sprintf("%d required %s failed", len(failed), plural(len(failed), "check", "checks"))
		default:
			text = fmt.Sprintf("%d required %s pending", len(pending), plural(len(pending), "check", "checks"))
		}
		return MergeBlocker{Kind: BlockerChecks, Text: text, Detail: nameList(append(failed, pending...)), URL: url}, false, true
	}
	if len(in.contexts) == 0 {
		return MergeBlocker{}, false, false
	}
	row = MergeBlocker{Kind: BlockerChecks, Text: "Checks passed"}
	if notPassed := others.Failure + others.Canceled; notPassed > 0 || others.Pending > 0 {
		row.Text = "Required checks passed"
		var parts []string
		if notPassed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", notPassed))
		}
		if others.Pending > 0 {
			parts = append(parts, fmt.Sprintf("%d pending", others.Pending))
		}
		row.Detail = strings.Join(parts, ", ") + " not required"
	}
	return row, true, true
}

// options is what the repository allows and the viewer may do. Methods
// are the repository's allowed methods, narrowed by the base's rulesets,
// in the order merge, squash, rebase with the viewer's default first. A
// null viewerPermission (a GitHub App token) does not grey Merge: the
// forge refuses a merge it does not allow.
func (in githubMergeInputs) options(readiness MergeReadiness) MergeOptions {
	pull, repo := in.pull, in.repo
	open := in.open()
	out := MergeOptions{
		Methods:             in.methods(),
		DeleteBranchOffered: open && !pull.IsCrossRepository && !repo.DeleteBranchOnMerge,
		DeleteBranchDefault: repo.DeleteBranchOnMerge,
	}
	if request := pull.AutoMergeRequest; request != nil && open {
		armed := &AutoMergeRequest{
			Method:    githubMergeMethodName(request.MergeMethod),
			At:        request.EnabledAt,
			WaitsFor:  autoMergeWaitsFor(readiness),
			CanCancel: pull.ViewerCanDisableAutoMerge,
		}
		if by := request.EnabledBy; by != nil {
			armed.ByLogin, armed.ByName = by.Login, by.Name
		}
		out.AutoMerge = armed
	}
	out.AutoMergeAvailable = open && (repo.AutoMergeAllowed && pull.ViewerCanEnableAutoMerge || out.AutoMerge != nil)
	switch strings.ToUpper(repo.ViewerPermission) {
	case "", "WRITE", "MAINTAIN", "ADMIN":
		out.ViewerCanMerge = open
	default:
		if open {
			out.CannotMergeReason = "You need write access to this repository to merge."
		}
	}
	out.ViewerCanApprove = open && !pull.ViewerDidAuthor
	if open && pull.ViewerDidAuthor {
		out.CannotApproveReason = "You cannot approve your own pull request."
	}
	for _, review := range pull.LatestOpinionatedReviews.Nodes {
		if review.ViewerDidAuthor && strings.EqualFold(review.State, "APPROVED") {
			out.ViewerApproved = true
		}
	}
	return out
}

func (in githubMergeInputs) methods() []string {
	repo := in.repo
	allowed := in.baseRules().methods
	out := make([]string, 0, 3)
	for _, method := range []struct {
		name string
		ok   bool
	}{{MergeMethodMerge, repo.MergeCommitAllowed}, {MergeMethodSquash, repo.SquashMergeAllowed}, {MergeMethodRebase, repo.RebaseMergeAllowed}} {
		if method.ok && (allowed == nil || slices.Contains(allowed, method.name)) {
			out = append(out, method.name)
		}
	}
	if i := slices.Index(out, githubMergeMethodName(repo.ViewerDefaultMergeMethod)); i > 0 {
		first := out[i]
		out = append([]string{first}, slices.Delete(out, i, i+1)...)
	}
	return out
}

// githubMergeMethodName is the forge-neutral name of a PullRequestMergeMethod
// or PullRequestAllowedMergeMethods value, "" for any other.
func githubMergeMethodName(method string) string {
	switch strings.ToUpper(method) {
	case "MERGE":
		return MergeMethodMerge
	case "SQUASH":
		return MergeMethodSquash
	case "REBASE":
		return MergeMethodRebase
	}
	return ""
}

// githubMergeMethodEnum is the PullRequestMergeMethod value of a
// forge-neutral method.
func githubMergeMethodEnum(method string) string {
	return strings.ToUpper(method)
}

// autoMergeWaitsFor names what an armed auto-merge is waiting for, from
// the blockers.
func autoMergeWaitsFor(readiness MergeReadiness) string {
	var reviews, checks bool
	for _, blocker := range readiness.Blockers {
		switch blocker.Kind {
		case BlockerApprovals, BlockerChangesRequested:
			reviews = true
		case BlockerChecks:
			checks = true
		}
	}
	switch {
	case reviews && checks:
		return "required reviews and checks"
	case reviews:
		return "required reviews"
	case checks:
		return "required checks"
	}
	return "all requirements"
}

func approvalDetail(approvers []string, required int) string {
	var parts []string
	if len(approvers) > 0 {
		parts = append(parts, nameList(approvers)+" approved")
	}
	if required > 0 {
		parts = append(parts, fmt.Sprintf("%d required", required))
	}
	return strings.Join(parts, " · ")
}

// nameListMax bounds the names a row's detail lists.
const nameListMax = 5

func nameList(names []string) string {
	if len(names) <= nameListMax {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:nameListMax], ", "), len(names)-nameListMax)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// githubReviewEvents maps timeline items, oldest first, to forge events.
// A review is an event only when it approves; GitHub closes a PR in the
// same moment it merges it, so a ClosedEvent after the MergedEvent is the
// merge and is dropped. An item without an id is skipped.
func githubReviewEvents(items []githubTimelineItemRaw) []ReviewEvent {
	out := make([]ReviewEvent, 0, len(items))
	merged := false
	for _, item := range items {
		if item.ID == "" {
			continue
		}
		event := ReviewEvent{ID: item.ID, At: item.CreatedAt}
		actor := item.Actor
		switch item.Typename {
		case "PullRequestReview":
			if !strings.EqualFold(item.State, "APPROVED") {
				continue
			}
			event.Kind, event.At, actor = EventApproved, item.SubmittedAt, item.Author
			if item.Commit != nil {
				event.Ref = item.Commit.OID
			}
		case "ReviewDismissedEvent":
			event.Kind, event.Text = EventReviewDismissed, item.DismissalMessage
			if item.Review != nil && item.Review.Author != nil {
				event.Ref = item.Review.Author.Login
			}
		case "MergedEvent":
			event.Kind = EventMerged
			if item.Commit != nil {
				event.Ref = item.Commit.OID
			}
			merged = true
		case "ClosedEvent":
			if merged {
				continue
			}
			event.Kind = EventClosed
		case "ReopenedEvent":
			event.Kind = EventReopened
		case "ConvertToDraftEvent":
			event.Kind = EventMarkedDraft
		case "ReadyForReviewEvent":
			event.Kind = EventMarkedReady
		case "AutoMergeEnabledEvent":
			event.Kind, event.Text = EventAutoMergeSet, MergeMethodMerge
		case "AutoSquashEnabledEvent":
			event.Kind, event.Text = EventAutoMergeSet, MergeMethodSquash
		case "AutoRebaseEnabledEvent":
			event.Kind, event.Text = EventAutoMergeSet, MergeMethodRebase
		case "AutoMergeDisabledEvent":
			event.Kind, event.Text = EventAutoMergeCancel, item.Reason
		case "HeadRefForcePushedEvent":
			event.Kind = EventForcePushed
			if item.AfterCommit != nil {
				event.Ref = item.AfterCommit.OID
			}
		case "BaseRefChangedEvent":
			event.Kind, event.Ref = EventBaseChanged, item.CurrentRefName
		case "ReviewRequestedEvent":
			event.Kind = EventReviewRequested
			if reviewer := item.RequestedReviewer; reviewer != nil {
				event.Ref = firstNonEmpty(reviewer.Login, reviewer.CombinedSlug)
			}
		default:
			continue
		}
		if actor != nil {
			event.ActorLogin, event.ActorName = actor.Login, actor.Name
		}
		out = append(out, event)
	}
	return out
}
