package git

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
)

// The GitLab Merge section: readiness, merge options and events read with
// the merge request, and the merge actions, over an httptest GitLab.

// gitlabTestProjectSettings is a project that merges with a merge commit,
// offers squash off by default, deletes the source branch by default and
// requires a passing pipeline and resolved threads.
const gitlabTestProjectSettings = `{"id":10,"merge_method":"merge","squash_option":"default_off","remove_source_branch_after_merge":true,
	"only_allow_merge_if_pipeline_succeeds":true,"only_allow_merge_if_all_discussions_are_resolved":true,
	"permissions":{"project_access":{"access_level":30},"group_access":null}}`

// gitlabMergeMR is an open merge request on project 10 with a passed head
// pipeline, as the single-MR endpoint answers, with fields replaced by
// over.
func gitlabMergeMR(t *testing.T, over map[string]any) string {
	t.Helper()
	mr := map[string]any{
		"iid": 7, "title": "MR", "description": "", "source_branch": "feature", "target_branch": "main",
		"sha": "abc1234", "web_url": "https://gitlab.example/grp/sub/app/-/merge_requests/7", "state": "opened",
		"draft": false, "work_in_progress": false, "changes_count": "1", "has_conflicts": false,
		"detailed_merge_status": "mergeable", "author": map[string]any{"username": "author", "name": "Author"},
		"diff_refs":     map[string]any{"base_sha": "base", "head_sha": "abc1234", "start_sha": "start"},
		"head_pipeline": map[string]any{"id": 77, "status": "success", "web_url": "https://gitlab.example/p/77"},
		"project_id":    10, "source_project_id": 10,
		"merged_at": nil, "merged_by": map[string]any{"username": nil, "name": nil}, "closed_at": nil, "closed_by": nil,
		"merge_user": map[string]any{"username": nil, "name": nil}, "merge_when_pipeline_succeeds": false,
		"squash": false, "force_remove_source_branch": nil, "blocking_discussions_resolved": true,
		"merge_commit_sha": nil, "user": map[string]any{"can_merge": true},
	}
	for k, v := range over {
		mr[k] = v
	}
	out, err := json.Marshal(mr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func gitlabTestApprovals(required, left int, approvers ...string) gitlabApprovalsRaw {
	out := gitlabApprovalsRaw{ApprovalsRequired: required, ApprovalsLeft: left, UserCanApprove: true}
	for _, login := range approvers {
		out.ApprovedBy = append(out.ApprovedBy, struct {
			ApprovedAt string          `json:"approved_at"`
			User       gitlabAuthorRaw `json:"user"`
		}{ApprovedAt: "t", User: gitlabAuthorRaw{Username: login, Name: strings.ToUpper(login)}})
	}
	return out
}

func decodeGitLabMR(t *testing.T, over map[string]any) gitlabMRRaw {
	t.Helper()
	var mr gitlabMRRaw
	if err := json.Unmarshal([]byte(gitlabMergeMR(t, over)), &mr); err != nil {
		t.Fatal(err)
	}
	return mr
}

func decodeGitLabSettings(t *testing.T, body string) gitlabProjectSettings {
	t.Helper()
	var settings gitlabProjectSettings
	if err := json.Unmarshal([]byte(body), &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

func gitlabBlockerKinds(blockers []MergeBlocker) string {
	kinds := make([]string, 0, len(blockers))
	for _, b := range blockers {
		kinds = append(kinds, b.Kind)
	}
	return strings.Join(kinds, ",")
}

// Every detailed_merge_status value maps to its readiness state and, when
// it blocks, one blocker of its kind with a sentence; an unknown value is
// other carrying the value.
func TestGitLabReadinessForEveryStatus(t *testing.T) {
	t.Parallel()
	settings := decodeGitLabSettings(t, `{"merge_method":"merge"}`)
	met := gitlabTestApprovals(1, 0, "lead")
	cases := []struct {
		status, state, kinds, text string
		approvals                  gitlabApprovalsRaw
	}{
		{"mergeable", MergeStateReady, "", "", met},
		{"checking", MergeStateChecking, "", "", met},
		{"unchecked", MergeStateChecking, "", "", met},
		{"preparing", MergeStateChecking, "", "", met},
		{"approvals_syncing", MergeStateChecking, "", "", met},
		{"not_approved", MergeStateBlocked, BlockerApprovals, "Needs 2 more approvals", gitlabTestApprovals(3, 2, "lead")},
		{"ci_must_pass", MergeStateBlocked, BlockerChecks, "The pipeline must succeed", met},
		{"ci_still_running", MergeStateBlocked, BlockerChecks, "The pipeline is still running", met},
		{"status_checks_must_pass", MergeStateBlocked, BlockerChecks, "External status checks must pass", met},
		{"commits_status", MergeStateBlocked, BlockerOther, "The source branch is missing or has no commits", met},
		{"discussions_not_resolved", MergeStateBlocked, BlockerThreads, "All threads must be resolved", met},
		{"conflict", MergeStateBlocked, BlockerConflicts, "Merge conflicts must be resolved", met},
		{"need_rebase", MergeStateBlocked, BlockerRebase, "The branch must be rebased onto main", met},
		{"draft_status", MergeStateDraft, BlockerDraft, "This merge request is a draft", met},
		{"requested_changes", MergeStateBlocked, BlockerChangesRequested, "A reviewer requested changes", met},
		{"merge_request_blocked", MergeStateBlocked, BlockerRules, "Blocked by a merge request it depends on", met},
		{"jira_association_missing", MergeStateBlocked, BlockerRules, "The title or description must reference a Jira issue", met},
		{"security_policy_violations", MergeStateBlocked, BlockerRules, "A security policy blocks this merge", met},
		{"security_policy_pipeline_check", MergeStateBlocked, BlockerRules, "A security policy blocks this merge", met},
		{"locked_paths", MergeStateBlocked, BlockerRules, "A path this merge request changes is locked", met},
		{"locked_lfs_files", MergeStateBlocked, BlockerRules, "An LFS file this merge request changes is locked", met},
		{"title_regex", MergeStateBlocked, BlockerRules, "The title does not match the project's required pattern", met},
		{"merge_time", MergeStateBlocked, BlockerRules, "Merging is scheduled for later", met},
		{"brand_new_check", MergeStateBlocked, BlockerOther, "brand_new_check", met},
	}
	for _, tc := range cases {
		mr := decodeGitLabMR(t, map[string]any{"detailed_merge_status": tc.status})
		got := gitlabReadiness(mr, tc.approvals, settings)
		if got.State != tc.state || gitlabBlockerKinds(got.Blockers) != tc.kinds {
			t.Fatalf("%s: readiness = %+v, want %s with %q", tc.status, got, tc.state, tc.kinds)
		}
		if tc.text != "" && got.Blockers[0].Text != tc.text {
			t.Fatalf("%s: blocker text = %q, want %q", tc.status, got.Blockers[0].Text, tc.text)
		}
	}
	// A draft is the draft state with its blocker first, whatever check
	// GitLab names.
	draft := gitlabReadiness(decodeGitLabMR(t, map[string]any{"draft": true, "detailed_merge_status": "not_approved"}), gitlabTestApprovals(1, 1), settings)
	if draft.State != MergeStateDraft || gitlabBlockerKinds(draft.Blockers) != "draft,approvals" {
		t.Fatalf("draft = %+v", draft)
	}
	// The pipeline blocker links the pipeline.
	ci := gitlabReadiness(decodeGitLabMR(t, map[string]any{"detailed_merge_status": "ci_must_pass", "head_pipeline": map[string]any{"id": 77, "status": "failed", "web_url": "https://gitlab.example/p/77"}}), met, settings)
	if ci.Blockers[0].URL != "https://gitlab.example/p/77" || ci.Blockers[0].Detail != "Pipeline failed" {
		t.Fatalf("pipeline blocker = %+v", ci.Blockers[0])
	}
	for state, want := range map[string]string{"merged": MergeStateMerged, "closed": MergeStateClosed, "locked": MergeStateChecking} {
		got := gitlabReadiness(decodeGitLabMR(t, map[string]any{"state": state, "detailed_merge_status": "not_open"}), met, settings)
		if got.State != want || len(got.Blockers) != 0 {
			t.Fatalf("%s: readiness = %+v, want %s", state, got, want)
		}
	}
}

// A ready merge request lists what passes as settled rows; a blocked one
// lists beside its status the blockers its own fields show, and settles
// the rest.
func TestGitLabReadinessSettledAndFieldBlockers(t *testing.T) {
	t.Parallel()
	settings := decodeGitLabSettings(t, gitlabTestProjectSettings)
	ready := gitlabReadiness(decodeGitLabMR(t, nil), gitlabTestApprovals(1, 0, "lead"), settings)
	if ready.State != MergeStateReady || len(ready.Blockers) != 0 || gitlabBlockerKinds(ready.Settled) != "approvals,checks,conflicts,threads" {
		t.Fatalf("ready = %+v", ready)
	}
	if s := ready.Settled; s[0].Text != "Approved 1 of 1" || s[0].Detail != "LEAD" || s[1].Text != "Pipeline passed" || s[1].URL == "" || s[2].Text != "No conflicts" || s[3].Text != "All threads resolved" {
		t.Fatalf("settled = %+v", s)
	}
	blocked := gitlabReadiness(decodeGitLabMR(t, map[string]any{
		"detailed_merge_status": "discussions_not_resolved", "blocking_discussions_resolved": false, "has_conflicts": true,
		"head_pipeline": map[string]any{"id": 77, "status": "failed", "web_url": "u"},
	}), gitlabTestApprovals(2, 1, "lead"), settings)
	if blocked.State != MergeStateBlocked || gitlabBlockerKinds(blocked.Blockers) != "threads,approvals,conflicts,checks" || len(blocked.Settled) != 0 {
		t.Fatalf("blocked = %+v", blocked)
	}
	if a := blocked.Blockers[1]; a.Text != "Needs 1 more approval" || a.Detail != "LEAD approved · 2 required" {
		t.Fatalf("approvals blocker = %+v", a)
	}
	// While GitLab checks, nothing is added.
	checking := gitlabReadiness(decodeGitLabMR(t, map[string]any{"detailed_merge_status": "checking", "has_conflicts": true}), gitlabTestApprovals(1, 1), settings)
	if checking.State != MergeStateChecking || len(checking.Blockers) != 0 {
		t.Fatalf("checking = %+v", checking)
	}
}

// GitLab Free requires no approval: the approvals row is a settled count,
// never a blocker.
func TestGitLabReadinessFreeTierApprovals(t *testing.T) {
	t.Parallel()
	settings := decodeGitLabSettings(t, `{"merge_method":"merge"}`)
	for n, want := range map[int]string{0: "No approvals", 1: "1 approval", 2: "2 approvals"} {
		logins := []string{"a", "b"}[:n]
		got := gitlabReadiness(decodeGitLabMR(t, map[string]any{"detailed_merge_status": "discussions_not_resolved"}), gitlabTestApprovals(0, 0, logins...), settings)
		if gitlabBlockerKinds(got.Blockers) != BlockerThreads || got.Settled[0].Kind != BlockerApprovals || got.Settled[0].Text != want {
			t.Fatalf("%d approvals: readiness = %+v, want a settled %q", n, got, want)
		}
	}
}

// The methods are the project's merge_method, default first, with squash
// per squash_option: first when the merge request squashes, alone when the
// project always squashes, absent when it never does.
func TestGitLabMergeMethods(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method, squashOption string
		squash               bool
		want                 string
	}{
		{"merge", "never", false, "merge"},
		{"merge", "always", false, "squash"},
		{"merge", "default_off", false, "merge,squash"},
		{"merge", "default_on", true, "squash,merge"},
		{"merge", "default_on", false, "merge,squash"},
		{"rebase_merge", "never", false, "merge,rebase"},
		{"rebase_merge", "default_off", true, "squash,merge,rebase"},
		{"ff", "never", false, "rebase"},
		{"ff", "default_off", false, "rebase,squash"},
		{"ff", "always", false, "squash"},
		{"", "default_on", true, ""},
	}
	for _, tc := range cases {
		settings := gitlabProjectSettings{MergeMethod: tc.method, SquashOption: tc.squashOption}
		if got := strings.Join(gitlabMethods(settings, tc.squash), ","); got != tc.want {
			t.Fatalf("%s/%s squash=%v: methods = %q, want %q", tc.method, tc.squashOption, tc.squash, got, tc.want)
		}
	}
}

// The merge options: the delete-branch toggle from the merge request's own
// choice else the project's, absent for a fork's branch; the viewer's
// rights and their reasons; an armed auto-merge from merge_user and its
// latest note.
func TestGitLabMergeOptions(t *testing.T) {
	t.Parallel()
	settings := decodeGitLabSettings(t, gitlabTestProjectSettings)
	ready := MergeReadiness{State: MergeStateReady}
	approvals := gitlabTestApprovals(1, 0)
	options := gitlabMergeOptions(decodeGitLabMR(t, nil), approvals, settings, ready, nil, false)
	if strings.Join(options.Methods, ",") != "merge,squash" || !options.DeleteBranchOffered || !options.DeleteBranchDefault || !options.ViewerCanMerge ||
		!options.ViewerCanApprove || options.ViewerApproved || options.AutoMerge != nil || !options.AutoMergeAvailable || options.CannotMergeReason != "" || options.CannotApproveReason != "" {
		t.Fatalf("options = %+v", options)
	}
	if o := gitlabMergeOptions(decodeGitLabMR(t, map[string]any{"force_remove_source_branch": false}), approvals, settings, ready, nil, false); !o.DeleteBranchOffered || o.DeleteBranchDefault {
		t.Fatalf("the merge request's own choice: %+v", o)
	}
	if o := gitlabMergeOptions(decodeGitLabMR(t, map[string]any{"source_project_id": 99, "force_remove_source_branch": true}), approvals, settings, ready, nil, false); o.DeleteBranchOffered || o.DeleteBranchDefault {
		t.Fatalf("fork: %+v", o)
	}
	denied := gitlabApprovalsRaw{UserCanApprove: false}
	o := gitlabMergeOptions(decodeGitLabMR(t, map[string]any{"user": map[string]any{"can_merge": false}}), denied, settings, ready, nil, false)
	if o.ViewerCanMerge || o.CannotMergeReason != "You do not have merge access to main." || o.ViewerCanApprove || o.CannotApproveReason != "You are not an eligible approver." {
		t.Fatalf("no rights: %+v", o)
	}
	reporter := decodeGitLabSettings(t, `{"merge_method":"merge","permissions":{"project_access":{"access_level":20}}}`)
	if o := gitlabMergeOptions(decodeGitLabMR(t, map[string]any{"user": map[string]any{"can_merge": false}}), approvals, reporter, ready, nil, false); o.CannotMergeReason != "Your role in this project cannot merge." {
		t.Fatalf("reporter: %+v", o)
	}
	// Approved already: no approve, no reason.
	if o := gitlabMergeOptions(decodeGitLabMR(t, nil), gitlabApprovalsRaw{UserHasApproved: true}, settings, ready, nil, false); !o.ViewerApproved || o.CannotApproveReason != "" {
		t.Fatalf("approved: %+v", o)
	}
	armed := decodeGitLabMR(t, map[string]any{"merge_when_pipeline_succeeds": true, "squash": true, "merge_user": map[string]any{"username": "lead", "name": "Lead"}})
	events := []ReviewEvent{
		{Kind: EventAutoMergeSet, At: "2026-01-01T00:00:00Z", Text: "enabled an automatic merge when all merge checks for 1111111 pass"},
		{Kind: EventAutoMergeSet, At: "2026-01-02T00:00:00Z", Text: "enabled an automatic merge when all merge checks for abc1234 pass"},
	}
	auto := gitlabMergeOptions(armed, approvals, settings, ready, events, false).AutoMerge
	if auto == nil || auto.Method != MergeMethodSquash || auto.ByLogin != "lead" || auto.ByName != "Lead" || auto.At != "2026-01-02T00:00:00Z" ||
		auto.WaitsFor != "all merge checks for abc1234 pass" || !auto.CanCancel {
		t.Fatalf("armed auto-merge = %+v", auto)
	}
	if auto := gitlabMergeOptions(armed, approvals, settings, ready, nil, false).AutoMerge; auto == nil || auto.At != "" || auto.WaitsFor != "all merge checks pass" {
		t.Fatalf("armed auto-merge without notes = %+v", auto)
	}
	// A merged merge request keeps merge_when_pipeline_succeeds; nothing is
	// armed and nothing is offered.
	merged := decodeGitLabMR(t, map[string]any{"state": "merged", "merge_when_pipeline_succeeds": true})
	if o := gitlabMergeOptions(merged, approvals, settings, MergeReadiness{State: MergeStateMerged}, events, false); o.AutoMerge != nil || o.AutoMergeAvailable || o.DeleteBranchOffered {
		t.Fatalf("merged: %+v", o)
	}
}

// Each GitLab system-note phrasing maps to its event; the notes that are
// not events (labels, assignees, pushes, mentions, descriptions) map to
// none.
func TestGitLabNoteEvents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		body, kind, ref, text string
	}{
		{"approved this merge request", EventApproved, "", ""},
		{"unapproved this merge request", EventUnapproved, "", ""},
		{"merged", EventMerged, "", ""},
		{"closed", EventClosed, "", ""},
		{"closed via abc1234", EventClosed, "", ""},
		{"reopened", EventReopened, "", ""},
		{"marked this merge request as **draft**", EventMarkedDraft, "", ""},
		{"marked this merge request as **draft** from abc1234", EventMarkedDraft, "", ""},
		{"marked this merge request as draft", EventMarkedDraft, "", ""},
		{"marked this merge request as **ready**", EventMarkedReady, "", ""},
		{"marked as a **Work In Progress**", EventMarkedDraft, "", ""},
		{"unmarked as a **Work In Progress**", EventMarkedReady, "", ""},
		{"enabled an automatic merge when all merge checks for 54eaa7e8721b pass", EventAutoMergeSet, "54eaa7e8721b", "enabled an automatic merge when all merge checks for 54eaa7e8721b pass"},
		{"enabled an automatic merge when the pipeline for 54eaa7e8 succeeds", EventAutoMergeSet, "54eaa7e8", "enabled an automatic merge when the pipeline for 54eaa7e8 succeeds"},
		{"canceled the automatic merge", EventAutoMergeCancel, "", ""},
		{"aborted the automatic merge because the source branch was updated", EventAutoMergeCancel, "", "aborted the automatic merge because the source branch was updated"},
		{"changed target branch from `main` to `release/2`", EventBaseChanged, "release/2", ""},
		{"deleted the `stage` branch. This merge request now targets the `main` branch", EventBaseChanged, "main", ""},
		{"requested review from @a, @b, and @c", EventReviewRequested, "a, b, c", ""},
		{"requested review from @a and @b", EventReviewRequested, "a, b", ""},
		{"requested review from @a and removed review request for @d", EventReviewRequested, "a", ""},
		{"requested review from @a and removed approval", EventReviewRequested, "a", ""},
		{"added 1 commit\n\n<ul><li>abc1234 - x</li></ul>\n\n[Compare with previous version](/p)", "", "", ""},
		{"added 3 commits\n\n<ul><li>a...b - 2 commits from branch <code>main</code></li></ul>", "", "", ""},
		{"assigned to @me", "", "", ""},
		{"added ~1 label", "", "", ""},
		{"changed milestone to %1", "", "", ""},
		{"mentioned in commit abc1234", "", "", ""},
		{"changed the description", "", "", ""},
		{"resolved all threads", "", "", ""},
		{"removed review request for @d", "", "", ""},
		{"requested changes", "", "", ""},
		{"changed source branch from `a` to `b`", "", "", ""},
	}
	for _, tc := range cases {
		kind, ref, text, ok := gitlabNoteEvent(tc.body)
		if ok != (tc.kind != "") || kind != tc.kind || ref != tc.ref || text != tc.text {
			t.Fatalf("%q = %q %q %q %v, want %q %q %q", tc.body, kind, ref, text, ok, tc.kind, tc.ref, tc.text)
		}
	}
}

// An events read maps the system notes of the discussions the threads read
// already fetched, keeps them out of the threads, and adds the merge the
// merge request reports, since GitLab records it as a state event.
func TestGitLabReadPREvents(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	discussions := `[
		{"id":"s1","notes":[{"id":101,"body":"approved this merge request","system":true,"created_at":"2026-01-01T00:00:01.000Z","author":{"username":"lead","name":"Lead"}}]},
		{"id":"d1","notes":[{"id":102,"body":"looks fine","system":false,"created_at":"2026-01-01T00:00:02.000Z","author":{"username":"rev","name":"Rev"}}]},
		{"id":"s2","notes":[{"id":103,"body":"added 1 commit\n\n<ul><li>abc</li></ul>","system":true,"created_at":"2026-01-01T00:00:03.000Z","author":{"username":"author","name":"Author"}}]},
		{"id":"s3","notes":[{"id":104,"body":"enabled an automatic merge when all merge checks for abc1234 pass","system":true,"created_at":"2026-01-01T00:00:04.000Z","author":{"username":"lead","name":"Lead"}}]}
	]`
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case mr:
			return forgeAPIAnswer{Body: gitlabMergeMR(t, map[string]any{
				"state": "merged", "detailed_merge_status": "not_open", "merged_at": "2026-01-01T00:00:05.000Z",
				"merged_by": map[string]any{"username": "lead", "name": "Lead"}, "merge_commit_sha": "mmm1111",
				"merge_when_pipeline_succeeds": true, "merge_user": map[string]any{"username": "lead", "name": "Lead"},
			})}
		case mr + "/discussions?per_page=100":
			return forgeAPIAnswer{Body: discussions}
		case mr + "/approvals":
			return forgeAPIAnswer{Body: `{"approvals_required":1,"approvals_left":0,"user_has_approved":true,"approved_by":[{"approved_at":"t","user":{"username":"lead","name":"Lead"}}]}`}
		case gitlabTestProject:
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		case "user":
			return forgeAPIAnswer{Body: `{"username":"viewer"}`}
		}
		return forgeUnexpected(t, call)
	})
	read, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{Detail: true, Threads: true, Events: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Threads) != 1 || read.Threads[0].ID != "d1" {
		t.Fatalf("threads = %+v, want the human discussion alone", read.Threads)
	}
	want := []ReviewEvent{
		{ID: "101", Kind: EventApproved, ActorLogin: "lead", ActorName: "Lead", At: "2026-01-01T00:00:01.000Z"},
		{ID: "104", Kind: EventAutoMergeSet, ActorLogin: "lead", ActorName: "Lead", At: "2026-01-01T00:00:04.000Z", Ref: "abc1234", Text: "enabled an automatic merge when all merge checks for abc1234 pass"},
		{ID: "merged", Kind: EventMerged, ActorLogin: "lead", ActorName: "Lead", At: "2026-01-01T00:00:05.000Z", Ref: "mmm1111"},
	}
	if len(read.Events) != len(want) {
		t.Fatalf("events = %+v", read.Events)
	}
	for i := range want {
		if read.Events[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, read.Events[i], want[i])
		}
	}
	d := read.Detail
	if d.Readiness.State != MergeStateMerged || d.MergedAt != "2026-01-01T00:00:05.000Z" || d.MergedBy != "lead" || d.ClosedAt != "" || d.Merge.AutoMerge != nil {
		t.Fatalf("merged detail = %+v", d)
	}
	if n := len(calls.all()); n != 5 {
		t.Fatalf("requests = %q, want the MR, one discussions page, the approvals, the project and the viewer", gitlabPaths(calls))
	}

	// A closed merge request's close comes from closed_at, its id from the
	// time so a later close is a new event.
	events := gitlabEvents(decodeGitLabMR(t, map[string]any{"state": "closed", "closed_at": "2026-02-01T00:00:00.000Z", "closed_by": map[string]any{"username": "x", "name": "X"}}), nil)
	if len(events) != 1 || events[0].Kind != EventClosed || events[0].ID != "closed:2026-02-01T00:00:00.000Z" || events[0].ActorLogin != "x" {
		t.Fatalf("closed events = %+v", events)
	}
	// A legacy "merged" note stands for the merge; none is added.
	legacy := gitlabEvents(decodeGitLabMR(t, map[string]any{"state": "merged", "merged_at": "2026-01-01T00:00:05.000Z"}), []gitlabNoteRaw{{ID: 9, Body: "merged", System: true, CreatedAt: "2026-01-01T00:00:05.000Z"}})
	if len(legacy) != 1 || legacy[0].ID != "9" {
		t.Fatalf("legacy merged events = %+v", legacy)
	}
}

// testClockCore is a mutable clock for a Core's nowFn.
type testClockCore struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClockCore) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClockCore) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// The project's settings are read once per project per TTL: a second read
// within it sends no project request, one after it goes out again, sent
// with the ETag the transport holds and answered 304 from the store, and
// the options still carry the settings.
func TestGitLabProjectSettingsCache(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case mr:
			return forgeAPIAnswer{Body: gitlabMergeMR(t, nil)}
		case mr + "/approvals":
			return forgeAPIAnswer{Body: `{"approvals_required":0}`}
		case gitlabTestProject:
			if call.Header.Get("If-None-Match") == `W/"p1"` {
				return forgeAPIAnswer{Status: http.StatusNotModified, Header: http.Header{"ETag": {`W/"p1"`}}}
			}
			return forgeAPIAnswer{Body: gitlabTestProjectSettings, Header: http.Header{"ETag": {`W/"p1"`}}}
		case "user":
			return forgeAPIAnswer{Body: `{"username":"viewer"}`}
		}
		return forgeUnexpected(t, call)
	})
	clock := &testClockCore{t: time.Unix(1_700_000_000, 0)}
	core.nowFn = clock.now
	projectReads := func() []forgeAPICall {
		var out []forgeAPICall
		for _, call := range calls.all() {
			if call.Path == gitlabTestProject {
				out = append(out, call)
			}
		}
		return out
	}
	read := func() PRDetail {
		t.Helper()
		detail, err := core.GetPRDetail(t.Context(), gitlabTestRef(7))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(detail.Merge.Methods, ",") != "merge,squash" {
			t.Fatalf("methods = %q", detail.Merge.Methods)
		}
		return detail
	}
	read()
	clock.advance(gitlabCacheTTL - time.Second)
	read()
	if n := len(projectReads()); n != 1 {
		t.Fatalf("project reads within the TTL = %d, want 1", n)
	}
	clock.advance(2 * time.Second)
	read()
	reads := projectReads()
	if len(reads) != 2 || reads[1].Header.Get("If-None-Match") != `W/"p1"` {
		t.Fatalf("project reads after the TTL = %d, second If-None-Match %q", len(reads), reads[len(reads)-1].Header.Get("If-None-Match"))
	}
}

// GitLab lets a merge request's author cancel its auto-merge without merge
// rights, and refuses the author's approval unless the project allows it.
// The viewer's login is read once per host, not per read.
func TestGitLabViewerIsAuthor(t *testing.T) {
	t.Parallel()
	mr := func(n int) string { return gitlabTestProject + "/merge_requests/" + strconv.Itoa(n) }
	armedBy := func(author string) map[string]any {
		return map[string]any{
			"author": map[string]any{"username": author, "name": author}, "user": map[string]any{"can_merge": false},
			"merge_when_pipeline_succeeds": true, "merge_user": map[string]any{"username": author, "name": author},
		}
	}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case "user":
			return forgeAPIAnswer{Body: `{"id":5,"username":"dev","name":"Dev"}`}
		case mr(7):
			return forgeAPIAnswer{Body: gitlabMergeMR(t, armedBy("Dev"))}
		case mr(8):
			return forgeAPIAnswer{Body: gitlabMergeMR(t, armedBy("other"))}
		case mr(7) + "/approvals", mr(8) + "/approvals":
			return forgeAPIAnswer{Body: `{"approvals_required":1,"approvals_left":1,"user_can_approve":false,"user_has_approved":false}`}
		case gitlabTestProject:
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		}
		return forgeUnexpected(t, call)
	})
	own, err := core.GetPRDetail(t.Context(), gitlabTestRef(7))
	if err != nil {
		t.Fatal(err)
	}
	if !own.ViewerIsAuthor || own.Merge.ViewerCanMerge || own.Merge.AutoMerge == nil || !own.Merge.AutoMerge.CanCancel ||
		own.Merge.CannotApproveReason != "You cannot approve your own merge request." {
		t.Fatalf("author's detail: author %v, merge %+v, auto-merge %+v", own.ViewerIsAuthor, own.Merge, own.Merge.AutoMerge)
	}
	other, err := core.GetPRDetail(t.Context(), gitlabTestRef(8))
	if err != nil {
		t.Fatal(err)
	}
	if other.ViewerIsAuthor || other.Merge.AutoMerge == nil || other.Merge.AutoMerge.CanCancel ||
		other.Merge.CannotApproveReason != "You are not an eligible approver." {
		t.Fatalf("other's detail: author %v, merge %+v, auto-merge %+v", other.ViewerIsAuthor, other.Merge, other.Merge.AutoMerge)
	}
	if n := countPrefix(gitlabPaths(calls), "GET user"); n != 1 {
		t.Fatalf("user reads over two detail reads = %d, want 1 (%q)", n, gitlabPaths(calls))
	}
	// A project that lets authors approve their own merge requests says
	// so with user_can_approve: no reason then.
	settings := decodeGitLabSettings(t, gitlabTestProjectSettings)
	if o := gitlabMergeOptions(decodeGitLabMR(t, nil), gitlabTestApprovals(1, 1), settings, MergeReadiness{State: MergeStateBlocked}, nil, true); !o.ViewerCanApprove || o.CannotApproveReason != "" {
		t.Fatalf("self-approval allowed: %+v", o)
	}
}

// A user read the forge refuses leaves the viewer unknown for the cache's
// TTL: the detail still reads, as nobody's, and the next read sends no
// user request until the TTL passes.
func TestGitLabViewerReadFailureLeavesTheViewerUnknown(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case "user":
			return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{"message":"500 Internal Server Error"}`}
		case mr:
			return forgeAPIAnswer{Body: gitlabMergeMR(t, map[string]any{"author": map[string]any{"username": "dev"}})}
		case mr + "/approvals":
			return forgeAPIAnswer{Body: `{"approvals_required":0,"user_can_approve":true}`}
		case gitlabTestProject:
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		}
		return forgeUnexpected(t, call)
	})
	clock := &testClockCore{t: time.Unix(1_700_000_000, 0)}
	core.nowFn = clock.now
	userReads := func() int { return countPrefix(gitlabPaths(calls), "GET user") }
	for range 2 {
		detail, err := core.GetPRDetail(t.Context(), gitlabTestRef(7))
		if err != nil || detail.ViewerIsAuthor {
			t.Fatalf("detail with the viewer unknown = %v, author %v", err, detail.ViewerIsAuthor)
		}
	}
	if n := userReads(); n != 1 {
		t.Fatalf("user reads within the TTL = %d, want 1", n)
	}
	clock.advance(gitlabCacheTTL)
	if _, err := core.GetPRDetail(t.Context(), gitlabTestRef(7)); err != nil || userReads() != 2 {
		t.Fatalf("after the TTL: %v, user reads %d, want 2", err, userReads())
	}
}

// swappableTokenSource answers the login set last, the way glab answers
// after `glab auth login` as another user. Its bases point at an httptest
// GitLab over plain HTTP.
type swappableTokenSource struct {
	token   atomic.Value // string
	apiHost string
}

func (s *swappableTokenSource) Token(context.Context, string, string) ([]byte, forgeapi.SourceInfo, error) {
	return []byte(s.token.Load().(string)), forgeapi.SourceInfo{Header: forgeapi.AuthPrivateToken, APIHost: s.apiHost, APIProtocol: "http"}, nil
}

// The viewer and the project's permissions are cached per login, not per
// host: once the login is replaced (here the old token is revoked, so the
// next request's 401 reads the new one), the next detail read asks GitLab
// who the viewer is and what it may do again, inside the cache's TTL.
func TestGitLabCachesFollowTheLogin(t *testing.T) {
	t.Parallel()
	logins := map[string]string{"token-alice": "alice", "token-bob": "bob"}
	var accepted atomic.Value
	accepted.Store("token-alice")
	calls := &forgeAPICalls{}
	mr := gitlabTestProject + "/merge_requests/7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := forgeAPICall{Method: r.Method, Path: strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/"), Header: r.Header.Clone()}
		calls.mu.Lock()
		calls.calls = append(calls.calls, call)
		calls.mu.Unlock()
		token := r.Header.Get("Private-Token")
		if token != accepted.Load().(string) {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch call.Path {
		case "user":
			_, _ = io.WriteString(w, `{"username":"`+logins[token]+`"}`)
		case mr:
			_, _ = io.WriteString(w, gitlabMergeMR(t, map[string]any{"author": map[string]any{"username": "bob"}}))
		case mr + "/approvals":
			_, _ = io.WriteString(w, `{"approvals_required":0}`)
		case gitlabTestProject:
			_, _ = io.WriteString(w, gitlabTestProjectSettings)
		default:
			t.Errorf("unexpected %s %s", call.Method, call.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	source := &swappableTokenSource{apiHost: strings.TrimPrefix(srv.URL, "http://")}
	source.token.Store("token-alice")
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", TokenSource: source})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	core := NewCore(WithForgeAPI(svc))
	clock := &testClockCore{t: time.Unix(1_700_000_000, 0)}
	core.nowFn = clock.now
	reads := func(path string) int { return countPrefix(gitlabPaths(calls), "GET "+path) }

	if detail, err := core.GetPRDetail(t.Context(), gitlabTestRef(7)); err != nil || detail.ViewerIsAuthor {
		t.Fatalf("as alice: %v, viewer is author %v", err, detail.ViewerIsAuthor)
	}
	source.token.Store("token-bob")
	accepted.Store("token-bob")
	clock.advance(time.Minute)
	detail, err := core.GetPRDetail(t.Context(), gitlabTestRef(7))
	if err != nil || !detail.ViewerIsAuthor {
		t.Fatalf("as bob: %v, viewer is author %v, want bob's own merge request", err, detail.ViewerIsAuthor)
	}
	if n := reads("user"); n != 2 {
		t.Fatalf("user reads = %d, want one per login (%q)", n, gitlabPaths(calls))
	}
	if n := reads(gitlabTestProject); n != 2 {
		t.Fatalf("project reads = %d, want one per login (%q)", n, gitlabPaths(calls))
	}
	// The same login inside the TTL still reads from the caches.
	if _, err := core.GetPRDetail(t.Context(), gitlabTestRef(7)); err != nil || reads("user") != 2 || reads(gitlabTestProject) != 2 {
		t.Fatalf("bob again: %v, user reads %d, project reads %d", err, reads("user"), reads(gitlabTestProject))
	}
}

// A cache entry is published under the login that answered, not the one
// the lookup saw: a 401 inside the read (the old token revoked) replaces
// the credential before GitLab answers, and the answer is the new login's.
func TestGitLabCachePublishesUnderTheAnsweringLogin(t *testing.T) {
	t.Parallel()
	settingsByToken := map[string]string{
		"token-alice": gitlabTestProjectSettings,
		"token-bob":   strings.Replace(gitlabTestProjectSettings, `"merge_method":"merge"`, `"merge_method":"ff"`, 1),
	}
	var accepted atomic.Value
	accepted.Store("token-alice")
	mr := gitlabTestProject + "/merge_requests/7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Private-Token")
		if token != accepted.Load().(string) {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v4/"); {
		case path == "user":
			_, _ = io.WriteString(w, `{"username":"`+strings.TrimPrefix(token, "token-")+`"}`)
		case path == mr && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, gitlabMergeMR(t, nil))
		case path == mr+"/approvals":
			_, _ = io.WriteString(w, `{"approvals_required":0}`)
		case path == gitlabTestProject:
			_, _ = io.WriteString(w, settingsByToken[token])
		case path == mr+"/merge" && r.Method == http.MethodPut:
			_, _ = io.WriteString(w, `{"state":"merged","sha":"abc1234"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	source := &swappableTokenSource{apiHost: strings.TrimPrefix(srv.URL, "http://")}
	source.token.Store("token-alice")
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", TokenSource: source})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	core := NewCore(WithForgeAPI(svc))
	ref := gitlabTestRef(7)
	forge := core.ForgeByID("gitlab").(*gitlabForge)
	client, err := forge.core.gitlabAPI(ref.Host)
	if err != nil {
		t.Fatal(err)
	}
	// The client reads alice's token; it is then revoked for bob's before
	// anything is cached. The merge's settings read looks the cache up as
	// alice (a miss), is refused, re-reads the credential and is answered
	// as bob: bob's permissions, under bob's credential.
	alice, err := client.CredentialFingerprint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source.token.Store("token-bob")
	accepted.Store("token-bob")
	if _, err := core.MergePR(t.Context(), ref, MergeRequest{Method: MergeMethodRebase, HeadSHA: "abc1234"}); err != nil {
		t.Fatalf("merge as bob: %v", err)
	}
	bob, err := client.CredentialFingerprint(t.Context())
	if err != nil || bob == alice {
		t.Fatalf("fingerprint after the swap = %q, %v (alice %q)", bob, err, alice)
	}
	entry := func(fp string) (string, bool) {
		forge.projects.mu.Lock()
		defer forge.projects.mu.Unlock()
		e, ok := forge.projects.entries[fp+" "+ref.Host+" "+ref.Project()]
		return e.value.MergeMethod, ok
	}
	if method, ok := entry(bob); !ok || method != "ff" {
		t.Fatalf("bob's settings = %q, %v; want ff under bob's credential", method, ok)
	}
	if method, ok := entry(alice); ok {
		t.Fatalf("alice's credential holds bob's answer: %q", method)
	}
}

// gitlabMergeForge is an httptest GitLab for the merge actions: each test
// sets the answer per method and path.
func gitlabMergeForge(t *testing.T, answer func(call forgeAPICall) (forgeAPIAnswer, bool)) (*Core, *forgeAPICalls) {
	t.Helper()
	return newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if reply, ok := answer(call); ok {
			return reply
		}
		if call.Method == http.MethodGet && call.Path == gitlabTestProject {
			return forgeAPIAnswer{Body: gitlabTestProjectSettings}
		}
		return forgeUnexpected(t, call)
	})
}

func jsonBody(t *testing.T, call forgeAPICall) map[string]any {
	t.Helper()
	var out map[string]any
	if len(call.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(call.Body, &out); err != nil {
		t.Fatalf("%s %s body %q: %v", call.Method, call.Path, call.Body, err)
	}
	return out
}

// Approve posts the head sha; unapprove posts nothing; a malformed sha is
// refused before anything is sent.
func TestGitLabApproveAndUnapprove(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := gitlabMergeForge(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		switch call.Method + " " + call.Path {
		case "POST " + mr + "/approve", "POST " + mr + "/unapprove":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{}`}, true
		}
		return forgeAPIAnswer{}, false
	})
	ref := gitlabTestRef(7)
	if err := core.ApprovePR(t.Context(), ref, "abc1234"); err != nil {
		t.Fatal(err)
	}
	if err := core.UnapprovePR(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := core.ApprovePR(t.Context(), ref, "not a sha"); err == nil {
		t.Fatal("an approval without a head sha was sent")
	}
	sent := calls.all()
	if len(sent) != 2 || sent[0].Method != http.MethodPost || sent[0].Path != mr+"/approve" || string(sent[0].Body) != `{"sha":"abc1234"}` ||
		sent[1].Path != mr+"/unapprove" || len(sent[1].Body) != 0 {
		t.Fatalf("requests = %q, approve body %s", gitlabPaths(calls), sent[0].Body)
	}
}

// Merge now PUTs the head sha, squash and the branch delete, and maps the
// answer: merged with its merge commit, or with the head for a
// fast-forward; still open is a merge GitLab runs asynchronously.
func TestGitLabMergeNow(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	var answer atomic.Value
	core, calls := gitlabMergeForge(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Method == http.MethodPut && call.Path == mr+"/merge" {
			return forgeAPIAnswer{Body: answer.Load().(string)}, true
		}
		return forgeAPIAnswer{}, false
	})
	ref := gitlabTestRef(7)
	cases := []struct {
		req    MergeRequest
		answer string
		want   MergeOutcome
		squash bool
	}{
		{MergeRequest{Method: MergeMethodSquash, HeadSHA: "abc1234", DeleteBranch: true}, `{"state":"merged","merge_commit_sha":"mmm1111","squash_commit_sha":"sss1111","sha":"abc1234"}`, MergeOutcome{Merged: true, SHA: "mmm1111"}, true},
		{MergeRequest{Method: MergeMethodMerge, HeadSHA: "abc1234"}, `{"state":"merged","merge_commit_sha":null,"sha":"abc1234"}`, MergeOutcome{Merged: true, SHA: "abc1234"}, false},
		{MergeRequest{Method: MergeMethodMerge, HeadSHA: "abc1234"}, `{"state":"opened","merge_when_pipeline_succeeds":false}`, MergeOutcome{}, false},
	}
	for i, tc := range cases {
		answer.Store(tc.answer)
		got, err := core.MergePR(t.Context(), ref, tc.req)
		if err != nil || got != tc.want {
			t.Fatalf("case %d: MergePR = %+v, %v; want %+v", i, got, err, tc.want)
		}
		var put forgeAPICall
		for _, call := range calls.all() {
			if call.Method == http.MethodPut {
				put = call
			}
		}
		body := jsonBody(t, put)
		if body["sha"] != "abc1234" || body["squash"] != tc.squash || body["should_remove_source_branch"] != tc.req.DeleteBranch || len(body) != 3 {
			t.Fatalf("case %d: merge body = %s", i, put.Body)
		}
	}
	// The project's settings were read once for the three merges.
	if n := countPrefix(gitlabPaths(calls), "GET "+gitlabTestProject); n != 1 {
		t.Fatalf("project reads = %d", n)
	}
}

// A method the project does not allow is refused before the merge is sent.
func TestGitLabMergeRefusesAMethodTheProjectDoesNotAllow(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path == gitlabTestProject {
			return forgeAPIAnswer{Body: `{"merge_method":"ff","squash_option":"never"}`}
		}
		return forgeUnexpected(t, call)
	})
	_, err := core.MergePR(t.Context(), gitlabTestRef(7), MergeRequest{Method: MergeMethodMerge, HeadSHA: "abc1234"})
	if err == nil || !strings.Contains(err.Error(), "does not allow the merge merge method") {
		t.Fatalf("MergePR = %v", err)
	}
	if paths := gitlabPaths(calls); len(paths) != 1 {
		t.Fatalf("requests = %q, want the project read alone", paths)
	}
	// A project answer with no merge settings allows merge alone.
	core2, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case gitlabTestProject:
			return forgeAPIAnswer{Body: `{"id":10}`}
		case gitlabTestProject + "/merge_requests/7/merge":
			return forgeAPIAnswer{Body: `{"state":"merged","merge_commit_sha":"m1"}`}
		}
		return forgeUnexpected(t, call)
	})
	if _, err := core2.MergePR(t.Context(), gitlabTestRef(7), MergeRequest{Method: MergeMethodSquash, HeadSHA: "abc1234"}); err == nil {
		t.Fatal("squash was sent to a project whose settings the read lacks")
	}
	if got, err := core2.MergePR(t.Context(), gitlabTestRef(7), MergeRequest{Method: MergeMethodMerge, HeadSHA: "abc1234"}); err != nil || !got.Merged {
		t.Fatalf("merge = %+v, %v", got, err)
	}
}

// Auto-merge sends auto_merge and merge_when_pipeline_succeeds in one
// request: GitLab 17.11+ reads either, and an older server ignores the
// auto_merge it does not know and arms on the second. An armed answer is
// AutoMerge.
func TestGitLabAutoMergeSendsBothParameters(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := gitlabMergeForge(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Method != http.MethodPut || call.Path != mr+"/merge" {
			return forgeAPIAnswer{}, false
		}
		// A server before 17.11: auto_merge is an undeclared parameter
		// Grape drops, so only merge_when_pipeline_succeeds arms.
		var body map[string]any
		_ = json.Unmarshal(call.Body, &body)
		if body["merge_when_pipeline_succeeds"] == true {
			return forgeAPIAnswer{Body: `{"state":"opened","merge_when_pipeline_succeeds":true}`}, true
		}
		return forgeAPIAnswer{Status: http.StatusMethodNotAllowed, Body: `{"message":"405 Method Not Allowed"}`}, true
	})
	req := MergeRequest{Method: MergeMethodMerge, HeadSHA: "abc1234", AutoMerge: true}
	for range 2 {
		got, err := core.MergePR(t.Context(), gitlabTestRef(7), req)
		if err != nil || got != (MergeOutcome{AutoMerge: true}) {
			t.Fatalf("auto-merge = %+v, %v", got, err)
		}
	}
	var puts []map[string]any
	for _, call := range calls.all() {
		if call.Method == http.MethodPut {
			puts = append(puts, jsonBody(t, call))
		}
	}
	if len(puts) != 2 {
		t.Fatalf("merge bodies = %+v, want one PUT per auto-merge", puts)
	}
	for _, body := range puts {
		if body["auto_merge"] != true || body["merge_when_pipeline_succeeds"] != true || body["sha"] != "abc1234" {
			t.Fatalf("merge body = %+v, want auto_merge and merge_when_pipeline_succeeds with the head", body)
		}
	}
	// A refusal is the caller's, with no second attempt.
	core2, calls2 := gitlabMergeForge(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Method == http.MethodPut {
			return forgeAPIAnswer{Status: http.StatusBadRequest, Body: `{"message":"SHA must be provided when merging"}`}, true
		}
		return forgeAPIAnswer{}, false
	})
	_, err := core2.MergePR(t.Context(), gitlabTestRef(7), req)
	if status, ok := errors.AsType[*forgeapi.StatusError](err); !ok || status.Status != http.StatusBadRequest {
		t.Fatalf("other 400 = %v", err)
	}
	if n := countPrefix(gitlabPaths(calls2), "PUT "+mr+"/merge"); n != 1 {
		t.Fatalf("PUTs = %d, want one", n)
	}
}

// Cancel uses cancel_auto_merge; a server without it (404) is asked the
// older route, remembered for the host once it answers. A 404 on both is
// the merge request gone, not remembered; the older route's error status
// is an error.
func TestGitLabCancelAutoMerge(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	var legacy atomic.Value
	legacy.Store(`{"status":"success"}`)
	var gone atomic.Bool
	core, calls := gitlabMergeForge(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		switch call.Method + " " + call.Path {
		case "POST " + mr + "/cancel_auto_merge":
			return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"error":"404 Not Found"}`}, true
		case "POST " + mr + "/cancel_merge_when_pipeline_succeeds":
			if gone.Load() {
				return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Merge Request Not Found"}`}, true
			}
			return forgeAPIAnswer{Status: http.StatusCreated, Body: legacy.Load().(string)}, true
		}
		return forgeAPIAnswer{}, false
	})
	ref := gitlabTestRef(7)
	gone.Store(true)
	if err := core.CancelPRAutoMerge(t.Context(), ref); !errors.Is(err, forgeapi.ErrNotFound) || !strings.Contains(err.Error(), "cancel_auto_merge") {
		t.Fatalf("both routes 404 = %v, want the first 404", err)
	}
	gone.Store(false)
	if err := core.CancelPRAutoMerge(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := core.CancelPRAutoMerge(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	legacy.Store(`{"status":"error","message":"none set"}`)
	if err := core.CancelPRAutoMerge(t.Context(), ref); err == nil || !strings.Contains(err.Error(), "none set") {
		t.Fatalf("nothing armed = %v", err)
	}
	want := []string{
		"POST " + mr + "/cancel_auto_merge", "POST " + mr + "/cancel_merge_when_pipeline_succeeds",
		"POST " + mr + "/cancel_auto_merge", "POST " + mr + "/cancel_merge_when_pipeline_succeeds",
		"POST " + mr + "/cancel_merge_when_pipeline_succeeds",
		"POST " + mr + "/cancel_merge_when_pipeline_succeeds",
	}
	if got := gitlabPaths(calls); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A current server answers the new route alone.
	core2, calls2 := gitlabMergeForge(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Method == http.MethodPost && call.Path == mr+"/cancel_auto_merge" {
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"iid":7}`}, true
		}
		return forgeAPIAnswer{}, false
	})
	if err := core2.CancelPRAutoMerge(t.Context(), ref); err != nil || len(calls2.all()) != 1 {
		t.Fatalf("cancel = %v, requests %q", err, gitlabPaths(calls2))
	}
}

// Update branch PUTs the rebase (202); mark ready reads the title and PUTs
// it without every draft prefix, and sends nothing for a title without
// one.
func TestGitLabUpdateBranchAndMarkReady(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	var title atomic.Value
	title.Store("Draft: [draft] (Draft)  Fix the thing")
	core, calls := gitlabMergeForge(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		switch call.Method + " " + call.Path {
		case "PUT " + mr + "/rebase":
			return forgeAPIAnswer{Status: http.StatusAccepted, Body: `{"rebase_in_progress":true}`}, true
		case "GET " + mr:
			return forgeAPIAnswer{Body: gitlabMergeMR(t, map[string]any{"title": title.Load().(string), "draft": true})}, true
		case "PUT " + mr:
			return forgeAPIAnswer{Body: `{"iid":7}`}, true
		}
		return forgeAPIAnswer{}, false
	})
	ref := gitlabTestRef(7)
	if err := core.UpdatePRBranch(t.Context(), ref, "abc1234"); err != nil {
		t.Fatal(err)
	}
	if err := core.MarkPRReady(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	title.Store("Fix the thing")
	if err := core.MarkPRReady(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	sent := calls.all()
	want := []string{"PUT " + mr + "/rebase", "GET " + mr, "PUT " + mr, "GET " + mr}
	if got := gitlabPaths(calls); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %q, want %q", got, want)
	}
	if string(sent[2].Body) != `{"title":"Fix the thing"}` {
		t.Fatalf("mark ready body = %s", sent[2].Body)
	}
	if got := gitlabDraftlessTitle("WIP: still a title"); got != "WIP: still a title" {
		t.Fatalf("WIP is not a draft prefix on current GitLab: %q", got)
	}
}

// The forge's refusals reach the caller as the transport's typed errors: a
// 403 as *ForbiddenError, a 409 (head moved) and a 405 (not mergeable) as
// *StatusError.
func TestGitLabMergeActionsReturnTypedRefusals(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	core, _ := gitlabMergeForge(t, func(call forgeAPICall) (forgeAPIAnswer, bool) {
		if call.Method == http.MethodGet {
			return forgeAPIAnswer{}, false
		}
		return forgeAPIAnswer{Status: int(status.Load()), Body: `{"message":"refused"}`}, true
	})
	ref := gitlabTestRef(7)
	actions := map[string]func() error{
		"approve":   func() error { return core.ApprovePR(t.Context(), ref, "abc1234") },
		"unapprove": func() error { return core.UnapprovePR(t.Context(), ref) },
		"merge": func() error {
			_, err := core.MergePR(t.Context(), ref, MergeRequest{Method: MergeMethodMerge, HeadSHA: "abc1234"})
			return err
		},
		"auto-merge": func() error {
			_, err := core.MergePR(t.Context(), ref, MergeRequest{Method: MergeMethodMerge, HeadSHA: "abc1234", AutoMerge: true})
			return err
		},
		"cancel": func() error { return core.CancelPRAutoMerge(t.Context(), ref) },
		"rebase": func() error { return core.UpdatePRBranch(t.Context(), ref, "abc1234") },
	}
	for name, action := range actions {
		status.Store(http.StatusForbidden)
		if _, ok := errors.AsType[*forgeapi.ForbiddenError](action()); !ok {
			t.Fatalf("%s 403: want *ForbiddenError", name)
		}
		for _, code := range []int{http.StatusConflict, http.StatusMethodNotAllowed} {
			status.Store(int32(code))
			err := action()
			if s, ok := errors.AsType[*forgeapi.StatusError](err); !ok || s.Status != code {
				t.Fatalf("%s %d = %v, want its *StatusError", name, code, err)
			}
		}
	}
}

// A viewer read refused after a 401 inside it replaced the login caches
// nothing: the refusal was the old login's, and the new login was never
// asked. A refusal of the login the lookup saw is cached as unknown.
func TestGitLabRefusedViewerIsCachedOnlyForTheRefusedLogin(t *testing.T) {
	t.Parallel()
	var accepted atomic.Value
	accepted.Store("token-alice")
	var userReads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Private-Token") != accepted.Load().(string) {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		userReads.Add(1)
		http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	source := &swappableTokenSource{apiHost: strings.TrimPrefix(srv.URL, "http://")}
	source.token.Store("token-alice")
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", TokenSource: source})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	core := NewCore(WithForgeAPI(svc))
	forge := core.ForgeByID("gitlab").(*gitlabForge)
	host := gitlabTestRef(7).Host
	client, err := forge.core.gitlabAPI(host)
	if err != nil {
		t.Fatal(err)
	}
	entries := func() int {
		forge.viewers.mu.Lock()
		defer forge.viewers.mu.Unlock()
		return len(forge.viewers.entries)
	}
	// The client reads alice's token; it is revoked for bob's before the
	// read, which is refused as bob after the 401 replaced the credential.
	if _, err := client.CredentialFingerprint(t.Context()); err != nil {
		t.Fatal(err)
	}
	source.token.Store("token-bob")
	accepted.Store("token-bob")
	if viewer := forge.viewerLogin(t.Context(), client, host); viewer != "" || userReads.Load() != 1 || entries() != 0 {
		t.Fatalf("viewer = %q, user reads %d, cached entries %d; want unknown, one read, nothing cached", viewer, userReads.Load(), entries())
	}
	// Refused again as bob, the login the lookup saw: cached as unknown
	// for the TTL, so the next read asks nothing.
	if viewer := forge.viewerLogin(t.Context(), client, host); viewer != "" || userReads.Load() != 2 || entries() != 1 {
		t.Fatalf("viewer = %q, user reads %d, cached entries %d; want unknown, a second read, one entry", viewer, userReads.Load(), entries())
	}
	if viewer := forge.viewerLogin(t.Context(), client, host); viewer != "" || userReads.Load() != 2 {
		t.Fatalf("viewer = %q, user reads %d; want the cached unknown", viewer, userReads.Load())
	}
}
