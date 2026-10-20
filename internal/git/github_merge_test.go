package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/forgeapi"
)

// githubMergePull is an open, clean PR's detail selection; a test
// overrides the keys its case is about.
func githubMergePull(overrides map[string]any) map[string]any {
	pull := map[string]any{
		"number": 7, "title": "t", "body": "", "state": "OPEN", "isDraft": false,
		"baseRefName": "main", "baseRefOid": "base123", "headRefName": "feature", "headRefOid": "abc1234", "url": "https://github.com/o/r/pull/7",
		"additions": 1, "deletions": 0, "changedFiles": 1,
		"mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": nil,
		"author":        map[string]any{"login": "author"},
		"latestReviews": map[string]any{"nodes": []any{}},
		"merged":        false, "mergedAt": nil, "mergedBy": nil, "closedAt": nil,
		"viewerDidAuthor": false, "isCrossRepository": false,
		"viewerCanEnableAutoMerge": false, "viewerCanDisableAutoMerge": false, "autoMergeRequest": nil,
		"latestOpinionatedReviews": map[string]any{"nodes": []any{}},
		"threadStates":             map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": []any{}},
		"baseRef":                  map[string]any{"refUpdateRule": nil, "rules": map[string]any{"nodes": []any{}}},
		"commits":                  map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"id": "C_1", "statusCheckRollup": nil}}}},
	}
	maps.Copy(pull, overrides)
	return pull
}

func githubMergeRepo(overrides map[string]any) map[string]any {
	repo := map[string]any{
		"mergeCommitAllowed": true, "squashMergeAllowed": true, "rebaseMergeAllowed": true,
		"deleteBranchOnMerge": false, "autoMergeAllowed": true, "viewerPermission": "WRITE", "viewerDefaultMergeMethod": "MERGE",
	}
	maps.Copy(repo, overrides)
	return repo
}

func githubTickJSON(t *testing.T, repo, pull map[string]any) string {
	t.Helper()
	data := map[string]any{"viewer": map[string]any{"login": "viewer"}, "repository": maps.Clone(repo)}
	data["repository"].(map[string]any)["pullRequest"] = pull
	out, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func githubRollup(contexts ...map[string]any) map[string]any {
	return map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"id": "C_1", "statusCheckRollup": map[string]any{
		"contexts": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": contexts},
	}}}}}
}

func githubRun(name, status, conclusion string, required bool) map[string]any {
	return map[string]any{"__typename": "CheckRun", "name": name, "status": status, "conclusion": conclusion, "isRequired": required,
		"detailsUrl": "https://github.com/o/r/runs/" + name, "databaseId": 1}
}

func githubOpinion(id, login, name, state string, writer, viewer bool) map[string]any {
	return map[string]any{"id": id, "state": state, "viewerDidAuthor": viewer, "authorCanPushToRepository": writer, "author": map[string]any{"login": login, "name": name}}
}

func githubRules(nodes ...map[string]any) map[string]any {
	return map[string]any{"refUpdateRule": nil, "rules": map[string]any{"nodes": nodes}}
}

// readGitHubDetail reads the detail of the PR the tick describes, failing
// on any request but the one PRTick.
func readGitHubDetail(t *testing.T, repo, pull map[string]any) PRDetail {
	t.Helper()
	answer := githubTickJSON(t, repo, pull)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PRTick" {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: answer}
	})
	got, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Detail: true}, nil, nil)
	if err != nil {
		t.Fatalf("ReadPR: %v", err)
	}
	if n := len(calls.all()); n != 1 {
		t.Fatalf("requests = %v, want the one PRTick", calls.ops())
	}
	return got.Detail
}

func blockerKinds(rows []MergeBlocker) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Kind)
	}
	return out
}

func findRow(rows []MergeBlocker, kind string) (MergeBlocker, bool) {
	for _, row := range rows {
		if row.Kind == kind {
			return row, true
		}
	}
	return MergeBlocker{}, false
}

// Each mergeStateStatus maps to its readiness state; the states that are
// not blocked list no blockers.
func TestGitHubReadinessFromMergeStateStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		pull     map[string]any
		state    string
		blockers []string
	}{
		{"clean", map[string]any{"mergeStateStatus": "CLEAN"}, MergeStateReady, nil},
		{"has hooks", map[string]any{"mergeStateStatus": "HAS_HOOKS"}, MergeStateReady, nil},
		{"unknown", map[string]any{"mergeStateStatus": "UNKNOWN", "mergeable": "UNKNOWN"}, MergeStateChecking, nil},
		{"absent", map[string]any{"mergeStateStatus": nil}, MergeStateChecking, nil},
		{"draft flag", map[string]any{"mergeStateStatus": "BLOCKED", "isDraft": true, "reviewDecision": "REVIEW_REQUIRED"}, MergeStateDraft, []string{BlockerDraft, BlockerApprovals}},
		{"deprecated DRAFT", map[string]any{"mergeStateStatus": "DRAFT"}, MergeStateDraft, []string{BlockerDraft}},
		{"dirty", map[string]any{"mergeStateStatus": "DIRTY", "mergeable": "CONFLICTING"}, MergeStateBlocked, []string{BlockerConflicts}},
		{"behind", map[string]any{"mergeStateStatus": "BEHIND"}, MergeStateBlocked, []string{BlockerRebase}},
		{"blocked, nothing explains it", map[string]any{"mergeStateStatus": "BLOCKED"}, MergeStateBlocked, []string{BlockerRules}},
		{"unstable", map[string]any{"mergeStateStatus": "UNSTABLE"}, MergeStateReady, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(tc.pull)).Readiness
			if r.State != tc.state || !slices.Equal(blockerKinds(r.Blockers), tc.blockers) {
				t.Fatalf("readiness = %+v, want %s with %v", r, tc.state, tc.blockers)
			}
			if r.Blockers == nil || r.Settled == nil {
				t.Fatalf("readiness lists are nil: %+v", r)
			}
		})
	}
	t.Run("conflict and rebase texts name the base", func(t *testing.T) {
		t.Parallel()
		dirty := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"mergeStateStatus": "DIRTY", "mergeable": "CONFLICTING"})).Readiness
		if dirty.Blockers[0].Text != "This branch has conflicts with main" {
			t.Fatalf("conflicts = %+v", dirty.Blockers[0])
		}
		if _, ok := findRow(dirty.Settled, BlockerConflicts); ok {
			t.Fatalf("a conflicting PR lists no conflicts as settled: %+v", dirty.Settled)
		}
		behind := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"mergeStateStatus": "BEHIND"})).Readiness
		if behind.Blockers[0].Text != "This branch is out of date with main" {
			t.Fatalf("rebase = %+v", behind.Blockers[0])
		}
	})
}

// A ready PR lists what holds: the approvals, the checks and no
// conflicts.
func TestGitHubReadyListsSettledRows(t *testing.T) {
	t.Parallel()
	d := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
		"reviewDecision": "APPROVED",
		"latestOpinionatedReviews": map[string]any{"nodes": []any{
			githubOpinion("PRR_1", "ann", "Ann", "APPROVED", true, false),
			githubOpinion("PRR_2", "ben", "", "APPROVED", false, false),
		}},
		"commits": githubRollup(githubRun("build", "COMPLETED", "SUCCESS", true), githubRun("lint", "COMPLETED", "SKIPPED", false)),
	}))
	r := d.Readiness
	if r.State != MergeStateReady || len(r.Blockers) != 0 {
		t.Fatalf("readiness = %+v", r)
	}
	want := []MergeBlocker{
		{Kind: BlockerApprovals, Text: "Approved by 2 reviewers", Detail: "Ann, ben"},
		{Kind: BlockerChecks, Text: "Checks passed"},
		{Kind: BlockerConflicts, Text: "No conflicts with main"},
	}
	if !slices.Equal(r.Settled, want) {
		t.Fatalf("settled = %+v, want %+v", r.Settled, want)
	}
}

// BLOCKED and UNSTABLE expand into the reasons GitHub withholds the
// merge: reviews, required checks, unresolved conversations.
func TestGitHubBlockedExpansion(t *testing.T) {
	t.Parallel()
	two := 2
	t.Run("approvals from branch protection", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "BLOCKED", "reviewDecision": "REVIEW_REQUIRED",
			"latestOpinionatedReviews": map[string]any{"nodes": []any{
				githubOpinion("PRR_1", "ann", "Ann", "APPROVED", true, false),
				githubOpinion("PRR_2", "guest", "", "APPROVED", false, false),
			}},
			"baseRef": map[string]any{"refUpdateRule": map[string]any{"requiredApprovingReviewCount": two, "requiredStatusCheckContexts": []string{}, "requiresCodeOwnerReviews": false, "requiresConversationResolution": false}, "rules": map[string]any{"nodes": []any{}}},
		})).Readiness
		want := []MergeBlocker{{Kind: BlockerApprovals, Text: "Needs 1 more approval", Detail: "Ann approved · 2 required"}}
		if r.State != MergeStateBlocked || !slices.Equal(r.Blockers, want) {
			t.Fatalf("readiness = %+v, want %+v (a reader without push access does not count)", r, want)
		}
	})
	t.Run("approvals from a ruleset with code owners", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "BLOCKED", "reviewDecision": "REVIEW_REQUIRED",
			"latestOpinionatedReviews": map[string]any{"nodes": []any{githubOpinion("PRR_1", "ann", "Ann", "APPROVED", true, false)}},
			"baseRef":                  githubRules(map[string]any{"type": "PULL_REQUEST", "parameters": map[string]any{"__typename": "PullRequestParameters", "requiredApprovingReviewCount": 1, "requireCodeOwnerReview": true, "requiredReviewThreadResolution": false, "allowedMergeMethods": []string{"MERGE", "SQUASH", "REBASE"}}}),
		})).Readiness
		if row, ok := findRow(r.Blockers, BlockerApprovals); !ok || row.Text != "Needs approval from a code owner" || row.Detail != "Ann approved · 1 required" {
			t.Fatalf("blockers = %+v", r.Blockers)
		}
	})
	t.Run("required count unreadable", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"mergeStateStatus": "BLOCKED", "reviewDecision": "REVIEW_REQUIRED"})).Readiness
		if !slices.Equal(r.Blockers, []MergeBlocker{{Kind: BlockerApprovals, Text: "Needs approval"}}) {
			t.Fatalf("blockers = %+v", r.Blockers)
		}
	})
	t.Run("changes requested", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "BLOCKED", "reviewDecision": "CHANGES_REQUESTED",
			"latestOpinionatedReviews": map[string]any{"nodes": []any{githubOpinion("PRR_1", "bob", "Bob", "CHANGES_REQUESTED", true, false)}},
		})).Readiness
		if !slices.Equal(r.Blockers, []MergeBlocker{{Kind: BlockerChangesRequested, Text: "Changes requested", Detail: "by Bob"}}) {
			t.Fatalf("blockers = %+v", r.Blockers)
		}
	})
	t.Run("required checks failed, pending and expected", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "BLOCKED",
			"commits": githubRollup(
				githubRun("build", "COMPLETED", "FAILURE", true),
				githubRun("test", "IN_PROGRESS", "", true),
				githubRun("lint", "COMPLETED", "FAILURE", false),
				githubRun("docs", "COMPLETED", "SUCCESS", true),
				map[string]any{"__typename": "StatusContext", "context": "ci/legacy", "state": "PENDING", "targetUrl": "https://ci", "isRequired": false},
			),
			"baseRef": map[string]any{"refUpdateRule": map[string]any{"requiredApprovingReviewCount": nil, "requiredStatusCheckContexts": []string{"build", "deploy"}, "requiresCodeOwnerReviews": false, "requiresConversationResolution": false},
				"rules": map[string]any{"nodes": []any{map[string]any{"type": "REQUIRED_STATUS_CHECKS", "parameters": map[string]any{"__typename": "RequiredStatusChecksParameters", "requiredStatusChecks": []any{map[string]any{"context": "e2e"}, map[string]any{"context": "test"}}}}}}},
		})).Readiness
		want := []MergeBlocker{{Kind: BlockerChecks, Text: "1 required check failed, 3 pending", Detail: "build, test, deploy, e2e", URL: "https://github.com/o/r/runs/build"}}
		if r.State != MergeStateBlocked || !slices.Equal(r.Blockers, want) {
			t.Fatalf("readiness = %+v, want %+v", r, want)
		}
	})
	t.Run("required checks pending alone", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "BLOCKED", "commits": githubRollup(githubRun("build", "QUEUED", "", true)),
		})).Readiness
		if row, _ := findRow(r.Blockers, BlockerChecks); row.Text != "1 required check pending" || row.Detail != "build" {
			t.Fatalf("blockers = %+v", r.Blockers)
		}
	})
	t.Run("unresolved threads when resolution is required", func(t *testing.T) {
		t.Parallel()
		states := map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{map[string]any{"isResolved": true}, map[string]any{"isResolved": false}, map[string]any{"isResolved": false}}}
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "BLOCKED", "threadStates": states,
			"baseRef": githubRules(map[string]any{"type": "REQUIRED_REVIEW_THREAD_RESOLUTION", "parameters": nil}),
		})).Readiness
		if !slices.Equal(r.Blockers, []MergeBlocker{{Kind: BlockerThreads, Text: "2 unresolved conversations"}}) {
			t.Fatalf("blockers = %+v", r.Blockers)
		}
		// Not required: unresolved threads are no condition at all.
		r = readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"mergeStateStatus": "CLEAN", "threadStates": states})).Readiness
		if _, ok := findRow(append(r.Blockers, r.Settled...), BlockerThreads); ok {
			t.Fatalf("threads row without the requirement: %+v", r)
		}
	})
	t.Run("every blocker in the merge box order", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "DIRTY", "mergeable": "CONFLICTING", "reviewDecision": "REVIEW_REQUIRED",
			"commits":      githubRollup(githubRun("build", "COMPLETED", "FAILURE", true)),
			"threadStates": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{map[string]any{"isResolved": false}}},
			"baseRef":      githubRules(map[string]any{"type": "PULL_REQUEST", "parameters": map[string]any{"requiredApprovingReviewCount": 1, "requiredReviewThreadResolution": true}}),
		})).Readiness
		if got := blockerKinds(r.Blockers); !slices.Equal(got, []string{BlockerApprovals, BlockerChecks, BlockerThreads, BlockerConflicts}) {
			t.Fatalf("blockers = %v", got)
		}
	})
	t.Run("unstable lists checks that are not required", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "UNSTABLE",
			"commits":          githubRollup(githubRun("build", "COMPLETED", "SUCCESS", true), githubRun("lint", "COMPLETED", "FAILURE", false)),
		})).Readiness
		if r.State != MergeStateReady {
			t.Fatalf("state = %s", r.State)
		}
		if row, _ := findRow(r.Settled, BlockerChecks); row.Text != "Required checks passed" || row.Detail != "1 failed not required" {
			t.Fatalf("settled = %+v", r.Settled)
		}
	})
	t.Run("blocked with every condition met is rules", func(t *testing.T) {
		t.Parallel()
		r := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "BLOCKED", "reviewDecision": "APPROVED",
			"latestOpinionatedReviews": map[string]any{"nodes": []any{githubOpinion("PRR_1", "ann", "Ann", "APPROVED", true, false)}},
			"commits":                  githubRollup(githubRun("build", "COMPLETED", "SUCCESS", true)),
		})).Readiness
		if !slices.Equal(r.Blockers, []MergeBlocker{{Kind: BlockerRules, Text: "Merging is blocked by the rules on main"}}) || len(r.Settled) != 3 {
			t.Fatalf("readiness = %+v", r)
		}
	})
}

// Thread states past their first page are read only when readiness needs
// them: resolution required and no unresolved thread on the first page.
func TestGitHubThreadStatesPage(t *testing.T) {
	t.Parallel()
	resolved := make([]any, 100)
	for i := range resolved {
		resolved[i] = map[string]any{"isResolved": true}
	}
	pull := githubMergePull(map[string]any{
		"mergeStateStatus": "BLOCKED",
		"threadStates":     map[string]any{"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "ts-1"}, "nodes": resolved},
		"baseRef":          githubRules(map[string]any{"type": "PULL_REQUEST", "parameters": map[string]any{"requiredReviewThreadResolution": true}}),
	})
	tick := githubTickJSON(t, githubMergeRepo(nil), pull)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Op {
		case "PRTick":
			return forgeAPIAnswer{Body: tick}
		case "PRThreadStatesPage":
			if call.Vars["after"] != "ts-1" || call.Vars["number"] != float64(7) || call.Vars["owner"] != "o" {
				t.Errorf("PRThreadStatesPage variables = %v", call.Vars)
			}
			return githubData(`{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":"ts-2"},"nodes":[{"isResolved":false}]}}}}`)
		}
		return forgeUnexpected(t, call)
	})
	got, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Detail: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ops := calls.ops(); fmt.Sprint(ops) != "[PRTick PRThreadStatesPage]" {
		t.Fatalf("requests = %v", ops)
	}
	if !slices.Equal(got.Detail.Readiness.Blockers, []MergeBlocker{{Kind: BlockerThreads, Text: "1 unresolved conversation"}}) {
		t.Fatalf("blockers = %+v", got.Detail.Readiness.Blockers)
	}

	// Without the requirement the second page is never read.
	pull["baseRef"] = githubRules()
	tick = githubTickJSON(t, githubMergeRepo(nil), pull)
	core, calls = newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PRTick" {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: tick}
	})
	if _, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Detail: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(calls.all()); n != 1 {
		t.Fatalf("requests = %v, want PRTick alone", calls.ops())
	}
}

// A merged or closed PR is its end state, with when and by whom.
func TestGitHubMergedAndClosed(t *testing.T) {
	t.Parallel()
	merged := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
		"state": "MERGED", "merged": true, "mergedAt": "2026-10-20T04:29:47Z", "mergedBy": map[string]any{"login": "ann", "name": "Ann"},
		"closedAt": "2026-10-20T04:29:47Z", "mergeStateStatus": "UNKNOWN",
	}))
	if merged.Readiness.State != MergeStateMerged || len(merged.Readiness.Blockers) != 0 || merged.MergedAt != "2026-10-20T04:29:47Z" || merged.MergedBy != "ann" || merged.ClosedAt == "" {
		t.Fatalf("merged = %+v", merged)
	}
	if m := merged.Merge; m.ViewerCanMerge || m.ViewerCanApprove || m.DeleteBranchOffered || m.AutoMergeAvailable || m.CannotMergeReason != "" || m.CannotApproveReason != "" {
		t.Fatalf("a merged PR offers actions: %+v", m)
	}
	closed := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"state": "CLOSED", "closedAt": "2026-10-19T00:00:00Z", "mergeStateStatus": "DIRTY"}))
	if closed.Readiness.State != MergeStateClosed || len(closed.Readiness.Blockers) != 0 || closed.ClosedAt != "2026-10-19T00:00:00Z" || closed.MergedAt != "" || closed.MergedBy != "" {
		t.Fatalf("closed = %+v", closed)
	}
	reopened := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"closedAt": "2026-10-19T00:00:00Z"}))
	if reopened.ClosedAt != "" {
		t.Fatalf("an open PR carries closedAt %q", reopened.ClosedAt)
	}
}

// The merge methods, the branch delete toggle and auto-merge follow the
// repository's settings and the base's rulesets.
func TestGitHubMergeOptions(t *testing.T) {
	t.Parallel()
	methods := []struct {
		name  string
		repo  map[string]any
		rules map[string]any
		want  []string
	}{
		{"all allowed", nil, nil, []string{MergeMethodMerge, MergeMethodSquash, MergeMethodRebase}},
		{"viewer default first", map[string]any{"viewerDefaultMergeMethod": "REBASE"}, nil, []string{MergeMethodRebase, MergeMethodMerge, MergeMethodSquash}},
		{"squash disabled", map[string]any{"squashMergeAllowed": false, "viewerDefaultMergeMethod": "SQUASH"}, nil, []string{MergeMethodMerge, MergeMethodRebase}},
		{"ruleset narrows", map[string]any{"viewerDefaultMergeMethod": "SQUASH"},
			githubRules(map[string]any{"type": "PULL_REQUEST", "parameters": map[string]any{"allowedMergeMethods": []string{"SQUASH", "REBASE"}}}, map[string]any{"type": "PULL_REQUEST", "parameters": map[string]any{"allowedMergeMethods": []string{"SQUASH"}}}),
			[]string{MergeMethodSquash}},
	}
	for _, tc := range methods {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pull := githubMergePull(nil)
			if tc.rules != nil {
				pull["baseRef"] = tc.rules
			}
			if got := readGitHubDetail(t, githubMergeRepo(tc.repo), pull).Merge.Methods; !slices.Equal(got, tc.want) {
				t.Fatalf("methods = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("branch delete", func(t *testing.T) {
		t.Parallel()
		own := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(nil)).Merge
		fork := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"isCrossRepository": true})).Merge
		auto := readGitHubDetail(t, githubMergeRepo(map[string]any{"deleteBranchOnMerge": true}), githubMergePull(nil)).Merge
		if !own.DeleteBranchOffered || own.DeleteBranchDefault || fork.DeleteBranchOffered || auto.DeleteBranchOffered || !auto.DeleteBranchDefault {
			t.Fatalf("own %+v, fork %+v, repository deletes %+v", own, fork, auto)
		}
	})
	t.Run("auto-merge", func(t *testing.T) {
		t.Parallel()
		offered := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"viewerCanEnableAutoMerge": true})).Merge
		disallowed := readGitHubDetail(t, githubMergeRepo(map[string]any{"autoMergeAllowed": false}), githubMergePull(map[string]any{"viewerCanEnableAutoMerge": true})).Merge
		viewerCannot := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(nil)).Merge
		if !offered.AutoMergeAvailable || offered.AutoMerge != nil || disallowed.AutoMergeAvailable || viewerCannot.AutoMergeAvailable {
			t.Fatalf("offered %+v, disallowed %+v, viewer cannot %+v", offered, disallowed, viewerCannot)
		}
		armed := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
			"mergeStateStatus": "BLOCKED", "viewerCanDisableAutoMerge": true,
			"commits":          githubRollup(githubRun("build", "IN_PROGRESS", "", true)),
			"autoMergeRequest": map[string]any{"enabledAt": "2026-10-20T01:00:00Z", "mergeMethod": "SQUASH", "enabledBy": map[string]any{"login": "ann", "name": "Ann"}},
		})).Merge
		want := AutoMergeRequest{Method: MergeMethodSquash, ByLogin: "ann", ByName: "Ann", At: "2026-10-20T01:00:00Z", WaitsFor: "required checks", CanCancel: true}
		if !armed.AutoMergeAvailable || armed.AutoMerge == nil || *armed.AutoMerge != want {
			t.Fatalf("armed = %+v, auto-merge %+v", armed, armed.AutoMerge)
		}
	})
	t.Run("viewer permission", func(t *testing.T) {
		t.Parallel()
		for perm, can := range map[string]bool{"READ": false, "TRIAGE": false, "WRITE": true, "MAINTAIN": true, "ADMIN": true} {
			m := readGitHubDetail(t, githubMergeRepo(map[string]any{"viewerPermission": perm}), githubMergePull(nil)).Merge
			if m.ViewerCanMerge != can || (m.CannotMergeReason == "") != can {
				t.Fatalf("%s: %+v", perm, m)
			}
		}
		if m := readGitHubDetail(t, githubMergeRepo(map[string]any{"viewerPermission": nil}), githubMergePull(nil)).Merge; !m.ViewerCanMerge {
			t.Fatalf("an unreported permission greys merge: %+v", m)
		}
	})
}

// The viewer's standing approval is its latest opinionated review: a later
// comment, which latestReviews would show instead, does not withdraw it.
func TestGitHubViewerApproval(t *testing.T) {
	t.Parallel()
	d := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
		"latestReviews": map[string]any{"nodes": []any{map[string]any{"author": map[string]any{"login": "viewer"}, "state": "COMMENTED", "body": "", "submittedAt": "2026-10-20T02:00:00Z", "commit": map[string]any{"oid": "abc1234"}}}},
		"latestOpinionatedReviews": map[string]any{"nodes": []any{
			githubOpinion("PRR_other", "ann", "Ann", "APPROVED", true, false),
			githubOpinion("PRR_mine", "viewer", "", "APPROVED", true, true),
		}},
	}))
	if m := d.Merge; !m.ViewerApproved || m.ViewerApprovalID != "PRR_mine" || !m.ViewerCanApprove || m.CannotApproveReason != "" {
		t.Fatalf("merge options = %+v", m)
	}
	if raw, _ := json.Marshal(d.Merge); strings.Contains(string(raw), "PRR_mine") {
		t.Fatalf("the approval id reaches the wire: %s", raw)
	}
	notApproved := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{
		"latestOpinionatedReviews": map[string]any{"nodes": []any{githubOpinion("PRR_mine", "viewer", "", "CHANGES_REQUESTED", true, true)}},
	})).Merge
	if notApproved.ViewerApproved || notApproved.ViewerApprovalID != "" {
		t.Fatalf("a change request reads as approval: %+v", notApproved)
	}
	author := readGitHubDetail(t, githubMergeRepo(nil), githubMergePull(map[string]any{"viewerDidAuthor": true})).Merge
	if author.ViewerCanApprove || author.CannotApproveReason != "You cannot approve your own pull request." {
		t.Fatalf("author = %+v", author)
	}
}

const githubTimelineNodes = `[
{"__typename":"ReviewRequestedEvent","id":"RRE_1","createdAt":"2026-10-01T00:00:01Z","actor":{"login":"author","name":"Au"},"requestedReviewer":{"__typename":"User","login":"ann"}},
{"__typename":"ReviewRequestedEvent","id":"RRE_2","createdAt":"2026-10-01T00:00:02Z","actor":{"login":"author"},"requestedReviewer":{"__typename":"Team","combinedSlug":"o/core"}},
{"__typename":"ReviewRequestedEvent","id":"RRE_3","createdAt":"2026-10-01T00:00:03Z","actor":{"login":"author"},"requestedReviewer":null},
{"__typename":"PullRequestReview","id":"PRR_c","state":"COMMENTED","submittedAt":"2026-10-01T00:00:04Z","author":{"login":"ann"},"commit":{"oid":"aaa"}},
{"__typename":"PullRequestReview","id":"PRR_a","state":"APPROVED","submittedAt":"2026-10-01T00:00:05Z","author":{"login":"ann","name":"Ann"},"commit":{"oid":"bbb"}},
{"__typename":"ReviewDismissedEvent","id":"RDE_1","createdAt":"2026-10-01T00:00:06Z","actor":{"login":"ann"},"dismissalMessage":"Approval withdrawn","review":{"author":{"login":"ann"}}},
{"__typename":"ConvertToDraftEvent","id":"CTD_1","createdAt":"2026-10-01T00:00:07Z","actor":{"login":"author"}},
{"__typename":"ReadyForReviewEvent","id":"RFR_1","createdAt":"2026-10-01T00:00:08Z","actor":{"login":"author"}},
{"__typename":"HeadRefForcePushedEvent","id":"HRFP_1","createdAt":"2026-10-01T00:00:09Z","actor":{"login":"author"},"afterCommit":{"oid":"ccc"}},
{"__typename":"BaseRefChangedEvent","id":"BRC_1","createdAt":"2026-10-01T00:00:10Z","actor":{"login":"author"},"currentRefName":"release"},
{"__typename":"ClosedEvent","id":"CE_1","createdAt":"2026-10-01T00:00:11Z","actor":{"login":"author"}},
{"__typename":"ReopenedEvent","id":"RE_1","createdAt":"2026-10-01T00:00:12Z","actor":{"login":"author"}},
{"__typename":"AutoMergeEnabledEvent","id":"AME_1","createdAt":"2026-10-01T00:00:13Z","actor":{"login":"ann"}},
{"__typename":"AutoMergeDisabledEvent","id":"AMD_1","createdAt":"2026-10-01T00:00:14Z","actor":{"login":"ann"},"reason":"Base branch was changed"},
{"__typename":"AutoSquashEnabledEvent","id":"ASE_1","createdAt":"2026-10-01T00:00:15Z","actor":{"login":"ann"}},
{"__typename":"AutoRebaseEnabledEvent","id":"ARE_1","createdAt":"2026-10-01T00:00:16Z","actor":{"login":"ann"}},
{"__typename":"MergedEvent","id":"ME_1","createdAt":"2026-10-01T00:00:17Z","actor":{"login":"ann","name":"Ann"},"commit":{"oid":"ddd"}},
{"__typename":"ClosedEvent","id":"CE_2","createdAt":"2026-10-01T00:00:17Z","actor":{"login":"ann"}},
{"__typename":"LabeledEvent","id":"LE_1","createdAt":"2026-10-01T00:00:18Z"}
]`

// Every timeline item type the tick selects maps to its event kind, in
// timeline order; a comment review, the close that accompanies a merge and
// a type the feed does not show are not events.
func TestGitHubEventsFromTheTimeline(t *testing.T) {
	t.Parallel()
	pull := map[string]any{"number": 7, "timelineItems": map[string]any{"pageInfo": map[string]any{"hasPreviousPage": false, "startCursor": "x"}, "nodes": json.RawMessage(githubTimelineNodes)}}
	tick := githubTickJSON(t, map[string]any{}, pull)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PRTick" {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: tick}
	})
	got, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Events: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sent := calls.all(); len(sent) != 1 || sent[0].Vars["wantEvents"] != true || sent[0].Vars["wantDetail"] != false || sent[0].Vars["wantThreads"] != false {
		t.Fatalf("requests = %+v", sent)
	}
	want := []ReviewEvent{
		{ID: "RRE_1", Kind: EventReviewRequested, ActorLogin: "author", ActorName: "Au", At: "2026-10-01T00:00:01Z", Ref: "ann"},
		{ID: "RRE_2", Kind: EventReviewRequested, ActorLogin: "author", At: "2026-10-01T00:00:02Z", Ref: "o/core"},
		{ID: "RRE_3", Kind: EventReviewRequested, ActorLogin: "author", At: "2026-10-01T00:00:03Z"},
		{ID: "PRR_a", Kind: EventApproved, ActorLogin: "ann", ActorName: "Ann", At: "2026-10-01T00:00:05Z", Ref: "bbb"},
		{ID: "RDE_1", Kind: EventReviewDismissed, ActorLogin: "ann", At: "2026-10-01T00:00:06Z", Text: "Approval withdrawn", Ref: "ann"},
		{ID: "CTD_1", Kind: EventMarkedDraft, ActorLogin: "author", At: "2026-10-01T00:00:07Z"},
		{ID: "RFR_1", Kind: EventMarkedReady, ActorLogin: "author", At: "2026-10-01T00:00:08Z"},
		{ID: "HRFP_1", Kind: EventForcePushed, ActorLogin: "author", At: "2026-10-01T00:00:09Z", Ref: "ccc"},
		{ID: "BRC_1", Kind: EventBaseChanged, ActorLogin: "author", At: "2026-10-01T00:00:10Z", Ref: "release"},
		{ID: "CE_1", Kind: EventClosed, ActorLogin: "author", At: "2026-10-01T00:00:11Z"},
		{ID: "RE_1", Kind: EventReopened, ActorLogin: "author", At: "2026-10-01T00:00:12Z"},
		{ID: "AME_1", Kind: EventAutoMergeSet, ActorLogin: "ann", At: "2026-10-01T00:00:13Z", Text: MergeMethodMerge},
		{ID: "AMD_1", Kind: EventAutoMergeCancel, ActorLogin: "ann", At: "2026-10-01T00:00:14Z", Text: "Base branch was changed"},
		{ID: "ASE_1", Kind: EventAutoMergeSet, ActorLogin: "ann", At: "2026-10-01T00:00:15Z", Text: MergeMethodSquash},
		{ID: "ARE_1", Kind: EventAutoMergeSet, ActorLogin: "ann", At: "2026-10-01T00:00:16Z", Text: MergeMethodRebase},
		{ID: "ME_1", Kind: EventMerged, ActorLogin: "ann", ActorName: "Ann", At: "2026-10-01T00:00:17Z", Ref: "ddd"},
	}
	if !slices.Equal(got.Events, want) {
		t.Fatalf("events =\n%+v\nwant\n%+v", got.Events, want)
	}
}

// A timeline longer than one page is read backwards from the tick's
// newest page and returned oldest first.
func TestGitHubEventsPageBackwards(t *testing.T) {
	t.Parallel()
	event := func(id string) string {
		return fmt.Sprintf(`{"__typename":"ReadyForReviewEvent","id":%q,"createdAt":"2026-10-01T00:00:00Z","actor":{"login":"a"}}`, id)
	}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Op {
		case "PRTick":
			return githubData(`{"repository":{"pullRequest":{"number":7,"timelineItems":{"pageInfo":{"hasPreviousPage":true,"startCursor":"tl-2"},"nodes":[` + event("E3") + `]}}}}`)
		case "PRTimelinePage":
			switch call.Vars["before"] {
			case "tl-2":
				return githubData(`{"repository":{"pullRequest":{"timelineItems":{"pageInfo":{"hasPreviousPage":true,"startCursor":"tl-1"},"nodes":[` + event("E2") + `]}}}}`)
			case "tl-1":
				return githubData(`{"repository":{"pullRequest":{"timelineItems":{"pageInfo":{"hasPreviousPage":false,"startCursor":"tl-0"},"nodes":[` + event("E1") + `]}}}}`)
			}
		}
		return forgeUnexpected(t, call)
	})
	got, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Events: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ops := calls.ops(); fmt.Sprint(ops) != "[PRTick PRTimelinePage PRTimelinePage]" {
		t.Fatalf("requests = %v", ops)
	}
	var ids []string
	for _, e := range got.Events {
		ids = append(ids, e.ID)
	}
	if fmt.Sprint(ids) != "[E1 E2 E3]" {
		t.Fatalf("events = %v", ids)
	}
}

// The pump's read (detail, threads and events) is one request, its
// variables naming each part, and the document gates the new parts.
func TestGitHubPumpReadIsOneRequest(t *testing.T) {
	t.Parallel()
	pull := githubMergePull(map[string]any{
		"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{}},
		"comments":      map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{}},
		"timelineItems": map[string]any{"pageInfo": map[string]any{"hasPreviousPage": false}, "nodes": json.RawMessage(githubTimelineNodes)},
	})
	tick := githubTickJSON(t, githubMergeRepo(nil), pull)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PRTick" {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: tick}
	})
	got, err := core.ReadPR(t.Context(), githubTestRef(7), PRReadParts{Detail: true, Threads: true, Events: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sent := calls.all()
	if len(sent) != 1 {
		t.Fatalf("requests = %v, want one PRTick", calls.ops())
	}
	vars := sent[0].Vars
	if vars["wantDetail"] != true || vars["wantThreads"] != true || vars["wantEvents"] != true || vars["wantChecks"] != true {
		t.Fatalf("variables = %v", vars)
	}
	if len(got.Events) == 0 || got.Detail.Readiness.State != MergeStateReady || len(got.Detail.Merge.Methods) != 3 {
		t.Fatalf("read = events %d, readiness %+v, merge %+v", len(got.Events), got.Detail.Readiness, got.Detail.Merge)
	}
	for _, gate := range []string{
		"$wantEvents: Boolean!",
		"repository(owner: $owner, name: $name) {\n    ... @include(if: $wantDetail) { mergeCommitAllowed",
		"@include(if: $wantEvents)",
		"timelineItems(last: 100, itemTypes: [PULL_REQUEST_REVIEW,",
		"threadStates: reviewThreads(first: 100)",
		"isRequired(pullRequestNumber: $number)",
		"latestOpinionatedReviews(first: 100)",
	} {
		if !strings.Contains(sent[0].Query, gate) {
			t.Fatalf("PRTick lacks %q", gate)
		}
	}
}

// githubMergeFake answers the merge actions: PRMergeTarget, and each
// mutation from answers by operation; REST requests by "METHOD path".
type githubMergeFake struct {
	mu      sync.Mutex
	answers map[string]forgeAPIAnswer
}

func (f *githubMergeFake) set(key string, answer forgeAPIAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[key] = answer
}

func newGitHubMergeCore(t *testing.T) (*Core, *githubMergeFake, *forgeAPICalls) {
	t.Helper()
	fake := &githubMergeFake{answers: map[string]forgeAPIAnswer{
		"PRMergeTarget": githubData(`{"repository":{"deleteBranchOnMerge":false,"pullRequest":{"id":"PR_node","headRefName":"feature/x","isCrossRepository":false}}}`),
	}}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		key := call.Op
		if key == "" {
			key = call.Method + " " + call.Path
		}
		fake.mu.Lock()
		answer, ok := fake.answers[key]
		fake.mu.Unlock()
		if !ok {
			return forgeUnexpected(t, call)
		}
		return answer
	})
	return core, fake, calls
}

const testHead = "abc1234def"

// Approve is an approving REST review pinned to the head the caller saw.
func TestGitHubApprove(t *testing.T) {
	t.Parallel()
	core, fake, calls := newGitHubMergeCore(t)
	fake.set("POST repos/o/r/pulls/5/reviews", forgeAPIAnswer{Body: `{"id":1,"state":"APPROVED"}`})
	if err := core.ApprovePR(t.Context(), githubTestRef(5), testHead); err != nil {
		t.Fatal(err)
	}
	sent := calls.all()
	if len(sent) != 1 || string(sent[0].Body) != `{"event":"APPROVE","commit_id":"abc1234def"}` {
		t.Fatalf("requests = %v, body %s", calls.ops(), sent[0].Body)
	}
	if err := core.ApprovePR(t.Context(), githubTestRef(5), "not-a-sha"); err == nil || len(calls.all()) != 1 {
		t.Fatalf("approve without a head = %v after %v", err, calls.ops())
	}
	fake.set("POST repos/o/r/pulls/5/reviews", forgeAPIAnswer{Status: http.StatusUnprocessableEntity, Body: `{"message":"Unprocessable Entity","errors":["Can not approve your own pull request"]}`})
	err := core.ApprovePR(t.Context(), githubTestRef(5), testHead)
	if status, ok := errors.AsType[*forgeapi.StatusError](err); !ok || status.Status != http.StatusUnprocessableEntity {
		t.Fatalf("refused approve = %v, want the 422", err)
	}
}

// Unapprove dismisses the approving review and reads its state back.
func TestGitHubUnapprove(t *testing.T) {
	t.Parallel()
	core, fake, calls := newGitHubMergeCore(t)
	fake.set("DismissReview", githubData(`{"dismissPullRequestReview":{"pullRequestReview":{"state":"DISMISSED"}}}`))
	if err := core.UnapprovePR(t.Context(), githubTestRef(5), "PRR_mine"); err != nil {
		t.Fatal(err)
	}
	sent := calls.all()
	if len(sent) != 1 || sent[0].Vars["reviewID"] != "PRR_mine" || sent[0].Vars["message"] != "Approval withdrawn" ||
		!strings.Contains(sent[0].Query, "dismissPullRequestReview(input: {pullRequestReviewId: $reviewID, message: $message})") {
		t.Fatalf("dismiss = %+v", sent)
	}
	fake.set("DismissReview", githubData(`{"dismissPullRequestReview":{"pullRequestReview":{"state":"APPROVED"}}}`))
	if err := core.UnapprovePR(t.Context(), githubTestRef(5), "PRR_mine"); err == nil || !strings.Contains(err.Error(), "APPROVED") {
		t.Fatalf("contradicting answer = %v", err)
	}
}

// Unapprove without a review id dismisses the viewer's own standing
// approval, found in latestOpinionatedReviews; with none it refuses and
// dismisses nothing.
func TestGitHubUnapproveFindsTheViewersApproval(t *testing.T) {
	t.Parallel()
	core, fake, calls := newGitHubMergeCore(t)
	fake.set("ViewerApproval", githubData(`{"repository":{"pullRequest":{"latestOpinionatedReviews":{"nodes":[
		{"id":"PRR_other","state":"APPROVED","viewerDidAuthor":false},
		{"id":"PRR_mine","state":"APPROVED","viewerDidAuthor":true}]}}}}`))
	fake.set("DismissReview", githubData(`{"dismissPullRequestReview":{"pullRequestReview":{"state":"DISMISSED"}}}`))
	if err := core.UnapprovePR(t.Context(), githubTestRef(5), " "); err != nil {
		t.Fatal(err)
	}
	sent := calls.all()
	if fmt.Sprint(calls.ops()) != "[ViewerApproval DismissReview]" {
		t.Fatalf("requests = %v", calls.ops())
	}
	if v := sent[0].Vars; v["owner"] != "o" || v["name"] != "r" || v["number"] != float64(5) ||
		!strings.Contains(sent[0].Query, "latestOpinionatedReviews(first: 100) { nodes { id state viewerDidAuthor } }") {
		t.Fatalf("approval read = %+v", sent[0])
	}
	if sent[1].Vars["reviewID"] != "PRR_mine" || sent[1].Vars["message"] != "Approval withdrawn" {
		t.Fatalf("dismiss variables = %v", sent[1].Vars)
	}

	fake.set("ViewerApproval", githubData(`{"repository":{"pullRequest":{"latestOpinionatedReviews":{"nodes":[
		{"id":"PRR_other","state":"APPROVED","viewerDidAuthor":false},
		{"id":"PRR_mine","state":"CHANGES_REQUESTED","viewerDidAuthor":true}]}}}}`))
	err := core.UnapprovePR(t.Context(), githubTestRef(5), "")
	explained, ok := errors.AsType[*ForgeActionError](err)
	if !ok || explained.Kind != ForgeErrorRefused || explained.Message != "You have no approval to withdraw." {
		t.Fatalf("no approval = %v", err)
	}
	if again, _ := errors.AsType[*ForgeActionError](ExplainForgeError(githubTestRef(5), ActionUnapprove, err)); again == nil || again.Message != explained.Message {
		t.Fatalf("explained = %v", again)
	}
	if fmt.Sprint(calls.ops()) != "[ViewerApproval DismissReview ViewerApproval]" {
		t.Fatalf("a dismiss was sent without an approval: %v", calls.ops())
	}
}

// Merge sends the method and the head it must still be, answers the merge
// commit, and deletes the branch after when asked and the repository does
// not.
func TestGitHubMerge(t *testing.T) {
	t.Parallel()
	merged := githubData(`{"mergePullRequest":{"pullRequest":{"merged":true,"mergeCommit":{"oid":"m3rg3d"}}}}`)
	t.Run("merge and delete the branch", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("MergePR", merged)
		fake.set("DELETE repos/o/r/git/refs/heads/feature/x", forgeAPIAnswer{Status: http.StatusNoContent})
		out, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodSquash, HeadSHA: testHead, DeleteBranch: true})
		if err != nil || out != (MergeOutcome{Merged: true, SHA: "m3rg3d"}) {
			t.Fatalf("merge = %+v, %v", out, err)
		}
		sent := calls.all()
		if fmt.Sprint(calls.ops()) != "[PRMergeTarget MergePR DELETE repos/o/r/git/refs/heads/feature/x]" {
			t.Fatalf("requests = %v", calls.ops())
		}
		if v := sent[0].Vars; v["owner"] != "o" || v["name"] != "r" || v["number"] != float64(5) {
			t.Fatalf("target variables = %v", v)
		}
		if v := sent[1].Vars; v["pullRequestID"] != "PR_node" || v["method"] != "SQUASH" || v["expectedHeadOid"] != testHead {
			t.Fatalf("merge variables = %v", v)
		}
		if !strings.Contains(sent[1].Query, "mergePullRequest(input: {pullRequestId: $pullRequestID, mergeMethod: $method, expectedHeadOid: $expectedHeadOid})") {
			t.Fatalf("merge document = %s", sent[1].Query)
		}
	})
	t.Run("the repository deletes it", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("PRMergeTarget", githubData(`{"repository":{"deleteBranchOnMerge":true,"pullRequest":{"id":"PR_node","headRefName":"feature/x","isCrossRepository":false}}}`))
		fake.set("MergePR", merged)
		if _, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge, HeadSHA: testHead, DeleteBranch: true}); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls.ops()) != "[PRMergeTarget MergePR]" {
			t.Fatalf("requests = %v", calls.ops())
		}
	})
	t.Run("not asked", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("MergePR", merged)
		if _, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodRebase, HeadSHA: testHead}); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(calls.ops()) != "[PRMergeTarget MergePR]" || calls.all()[1].Vars["method"] != "REBASE" {
			t.Fatalf("requests = %v", calls.all())
		}
	})
	t.Run("the delete fails after the merge", func(t *testing.T) {
		t.Parallel()
		core, fake, _ := newGitHubMergeCore(t)
		fake.set("MergePR", merged)
		fake.set("DELETE repos/o/r/git/refs/heads/feature/x", forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"Cannot delete this protected branch"}`})
		out, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge, HeadSHA: testHead, DeleteBranch: true})
		kept, ok := errors.AsType[*BranchKeptError](err)
		if !ok || kept.Outcome != (MergeOutcome{Merged: true, SHA: "m3rg3d"}) || kept.Branch != "feature/x" || out != kept.Outcome {
			t.Fatalf("merge = %+v, %v", out, err)
		}
		if _, ok := errors.AsType[*forgeapi.ForbiddenError](err); !ok {
			t.Fatalf("the delete's forge error is lost: %v", err)
		}
		explained, ok := errors.AsType[*ForgeActionError](ExplainForgeError(githubTestRef(5), ActionMerge, err))
		if !ok || explained.Message != "Merged o/r#5, but GitHub kept the branch feature/x: Cannot delete this protected branch" {
			t.Fatalf("explained = %+v", explained)
		}
	})
	t.Run("a branch already gone is deleted", func(t *testing.T) {
		t.Parallel()
		core, fake, _ := newGitHubMergeCore(t)
		fake.set("MergePR", merged)
		fake.set("DELETE repos/o/r/git/refs/heads/feature/x", forgeAPIAnswer{Status: http.StatusUnprocessableEntity, Body: `{"message":"Reference does not exist"}`})
		if _, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge, HeadSHA: testHead, DeleteBranch: true}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a fork's branch is refused before merging", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("PRMergeTarget", githubData(`{"repository":{"deleteBranchOnMerge":false,"pullRequest":{"id":"PR_node","headRefName":"feature/x","isCrossRepository":true}}}`))
		if _, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge, HeadSHA: testHead, DeleteBranch: true}); err == nil {
			t.Fatal("a fork's branch delete was accepted")
		}
		if fmt.Sprint(calls.ops()) != "[PRMergeTarget]" {
			t.Fatalf("requests = %v", calls.ops())
		}
	})
	t.Run("answered not merged", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("MergePR", githubData(`{"mergePullRequest":{"pullRequest":{"merged":false,"mergeCommit":null}}}`))
		out, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge, HeadSHA: testHead, DeleteBranch: true})
		if err != nil || out != (MergeOutcome{}) || fmt.Sprint(calls.ops()) != "[PRMergeTarget MergePR]" {
			t.Fatalf("merge = %+v, %v after %v", out, err, calls.ops())
		}
	})
	t.Run("auto-merge", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("EnableAutoMerge", githubData(`{"enablePullRequestAutoMerge":{"pullRequest":{"autoMergeRequest":{"enabledAt":"2026-10-20T00:00:00Z"}}}}`))
		out, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodSquash, HeadSHA: testHead, DeleteBranch: true, AutoMerge: true})
		if err != nil || out != (MergeOutcome{AutoMerge: true}) {
			t.Fatalf("auto-merge = %+v, %v", out, err)
		}
		sent := calls.all()
		if fmt.Sprint(calls.ops()) != "[PRMergeTarget EnableAutoMerge]" {
			t.Fatalf("requests = %v", calls.ops())
		}
		if v := sent[1].Vars; v["pullRequestID"] != "PR_node" || v["method"] != "SQUASH" || v["expectedHeadOid"] != testHead ||
			!strings.Contains(sent[1].Query, "enablePullRequestAutoMerge(input: {pullRequestId: $pullRequestID, mergeMethod: $method, expectedHeadOid: $expectedHeadOid})") {
			t.Fatalf("auto-merge = %+v", sent[1])
		}
		fake.set("EnableAutoMerge", githubData(`{"enablePullRequestAutoMerge":{"pullRequest":{"autoMergeRequest":null}}}`))
		if _, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodSquash, HeadSHA: testHead, AutoMerge: true}); err == nil {
			t.Fatal("an auto-merge GitHub did not arm succeeded")
		}
	})
	t.Run("forge refusals reach the caller as their types", func(t *testing.T) {
		t.Parallel()
		core, fake, _ := newGitHubMergeCore(t)
		fake.set("MergePR", forgeAPIAnswer{Body: `{"data":{"mergePullRequest":null},"errors":[{"type":"UNPROCESSABLE","message":"Head branch was modified. Review and try the merge again."}]}`})
		_, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge, HeadSHA: testHead})
		if gql, ok := errors.AsType[*forgeapi.GraphQLError](err); !ok || gql.Problems[0].Type != "UNPROCESSABLE" {
			t.Fatalf("GraphQL refusal = %v", err)
		}
		if explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(githubTestRef(5), ActionMerge, err)); explained == nil || explained.Kind != ForgeErrorStaleHead ||
			explained.Message != "The branch moved after you loaded this pull request. Reload and try again." {
			t.Fatalf("head moved explained = %+v", explained)
		}
		other := &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Message: "Pull request is not mergeable"}}}
		if explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(githubTestRef(5), ActionMerge, other)); explained == nil || explained.Kind != ForgeErrorRefused {
			t.Fatalf("refusal explained = %+v", explained)
		}
		fake.set("MergePR", forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"Resource not accessible by integration"}`})
		_, err = core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge, HeadSHA: testHead})
		if forbidden, ok := errors.AsType[*forgeapi.ForbiddenError](err); !ok || forbidden.Status.Status != http.StatusForbidden {
			t.Fatalf("403 = %v", err)
		}
		fake.set("PRMergeTarget", forgeAPIAnswer{Body: `{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a PullRequest with the number of 5."}]}`})
		_, err = core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge, HeadSHA: testHead})
		if _, ok := errors.AsType[*forgeapi.GraphQLError](err); !ok {
			t.Fatalf("missing PR = %v", err)
		}
	})
	t.Run("a request without the head is refused", func(t *testing.T) {
		t.Parallel()
		core, _, calls := newGitHubMergeCore(t)
		if _, err := core.MergePR(t.Context(), githubTestRef(5), MergeRequest{Method: MergeMethodMerge}); err == nil || len(calls.all()) != 0 {
			t.Fatalf("merge without a head = %v after %v", err, calls.ops())
		}
		var gh githubForge
		gh.core = core
		if _, err := gh.Merge(t.Context(), githubTestRef(5), MergeRequest{Method: "octopus", HeadSHA: testHead}); err == nil || len(calls.all()) != 0 {
			t.Fatalf("the forge accepted an unknown method: %v", err)
		}
	})
}

// CancelAutoMerge, UpdateBranch and MarkReady address the PR's node id,
// send the head where GitHub takes one, and read the result back.
func TestGitHubPRStateMutations(t *testing.T) {
	t.Parallel()
	t.Run("cancel auto-merge", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("DisableAutoMerge", githubData(`{"disablePullRequestAutoMerge":{"pullRequest":{"autoMergeRequest":null}}}`))
		if err := core.CancelPRAutoMerge(t.Context(), githubTestRef(5)); err != nil {
			t.Fatal(err)
		}
		if sent := calls.all(); fmt.Sprint(calls.ops()) != "[PRMergeTarget DisableAutoMerge]" || sent[1].Vars["pullRequestID"] != "PR_node" ||
			!strings.Contains(sent[1].Query, "disablePullRequestAutoMerge(input: {pullRequestId: $pullRequestID})") {
			t.Fatalf("requests = %+v", sent)
		}
		fake.set("DisableAutoMerge", githubData(`{"disablePullRequestAutoMerge":{"pullRequest":{"autoMergeRequest":{"enabledAt":"x"}}}}`))
		if err := core.CancelPRAutoMerge(t.Context(), githubTestRef(5)); err == nil {
			t.Fatal("an auto-merge still armed read as canceled")
		}
	})
	t.Run("update branch", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("UpdatePRBranch", githubData(`{"updatePullRequestBranch":{"pullRequest":{"id":"PR_node"}}}`))
		if err := core.UpdatePRBranch(t.Context(), githubTestRef(5), testHead); err != nil {
			t.Fatal(err)
		}
		sent := calls.all()
		if fmt.Sprint(calls.ops()) != "[PRMergeTarget UpdatePRBranch]" || sent[1].Vars["pullRequestID"] != "PR_node" || sent[1].Vars["expectedHeadOid"] != testHead ||
			!strings.Contains(sent[1].Query, "updatePullRequestBranch(input: {pullRequestId: $pullRequestID, expectedHeadOid: $expectedHeadOid})") {
			t.Fatalf("requests = %+v", sent)
		}
		if err := core.UpdatePRBranch(t.Context(), githubTestRef(5), ""); err == nil || len(calls.all()) != 2 {
			t.Fatalf("update without a head = %v", err)
		}
		fake.set("UpdatePRBranch", forgeAPIAnswer{Body: `{"data":{"updatePullRequestBranch":null},"errors":[{"type":"UNPROCESSABLE","message":"expected head sha didn't match current head ref."}]}`})
		if _, ok := errors.AsType[*forgeapi.GraphQLError](core.UpdatePRBranch(t.Context(), githubTestRef(5), testHead)); !ok {
			t.Fatal("the GraphQL refusal lost its type")
		}
	})
	t.Run("mark ready", func(t *testing.T) {
		t.Parallel()
		core, fake, calls := newGitHubMergeCore(t)
		fake.set("MarkPRReady", githubData(`{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}`))
		if err := core.MarkPRReady(t.Context(), githubTestRef(5)); err != nil {
			t.Fatal(err)
		}
		if sent := calls.all(); fmt.Sprint(calls.ops()) != "[PRMergeTarget MarkPRReady]" || sent[1].Vars["pullRequestID"] != "PR_node" ||
			!strings.Contains(sent[1].Query, "markPullRequestReadyForReview(input: {pullRequestId: $pullRequestID})") {
			t.Fatalf("requests = %+v", sent)
		}
		fake.set("MarkPRReady", githubData(`{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":true}}}`))
		if err := core.MarkPRReady(t.Context(), githubTestRef(5)); err == nil {
			t.Fatal("a PR still draft read as ready")
		}
		fake.set("PRMergeTarget", forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"Resource not accessible by personal access token"}`})
		if _, ok := errors.AsType[*forgeapi.ForbiddenError](core.MarkPRReady(t.Context(), githubTestRef(5))); !ok {
			t.Fatal("the 403 lost its type")
		}
	})
}
