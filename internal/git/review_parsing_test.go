package git

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return string(data)
}

func TestParseGitHubPRDetailFixture(t *testing.T) {
	detail, err := parseGitHubPRDetail(readTestdata(t, "github-pr-detail.json"))
	if err != nil {
		t.Fatalf("parseGitHubPRDetail: %v", err)
	}
	if detail.Number != 13780 || detail.Title == "" || detail.AuthorLogin != "niik" {
		t.Fatalf("detail basics = %+v", detail)
	}
	if detail.ReviewDecision != "APPROVED" {
		t.Fatalf("ReviewDecision = %q, want APPROVED", detail.ReviewDecision)
	}
	if detail.Mergeability != MergeabilityClean {
		t.Fatalf("Mergeability = %q, want clean", detail.Mergeability)
	}
	if detail.Checks.Total == 0 || detail.Checks.Success == 0 {
		t.Fatalf("check summary did not count successful checks: %+v", detail.Checks)
	}
	seen := map[string]bool{}
	for _, review := range detail.LatestReviews {
		if seen[review.AuthorLogin] {
			t.Fatalf("duplicate latest review for %s", review.AuthorLogin)
		}
		seen[review.AuthorLogin] = true
	}
	if !seen["babakks"] {
		t.Fatalf("latest reviews missing observed reviewer babakks: %+v", detail.LatestReviews)
	}
}

func TestParseGitHubReviewThreadsNullableAnchorsAndReplies(t *testing.T) {
	threads, pageInfo, err := parseGitHubReviewThreads(readTestdata(t, "github-review-threads.json"))
	if err != nil {
		t.Fatalf("parseGitHubReviewThreads: %v", err)
	}
	if pageInfo.HasNextPage {
		t.Fatalf("fixture should fit on one page")
	}
	var nilAnchors, replies int
	for _, thread := range threads {
		if thread.Line == nil {
			nilAnchors++
		}
		for _, comment := range thread.Comments {
			if comment.ReplyTo != nil && comment.ReplyTo.DatabaseID != 0 {
				replies++
			}
		}
	}
	if nilAnchors == 0 {
		t.Fatal("expected nullable GitHub thread anchors from fixture")
	}
	if replies == 0 {
		t.Fatal("expected GitHub replyTo database IDs from fixture")
	}
}

func TestParseGitHubStatusCheckRollupUnionAndZeroValues(t *testing.T) {
	for _, fixture := range []string{"github-statusrollup-mixed.json", "github-statusrollup-statuscontext.json"} {
		var raw struct {
			StatusCheckRollup []json.RawMessage `json:"statusCheckRollup"`
		}
		if err := json.Unmarshal([]byte(readTestdata(t, fixture)), &raw); err != nil {
			t.Fatalf("decode %s: %v", fixture, err)
		}
		summary := parseGitHubCheckSummary(raw.StatusCheckRollup)
		if summary.Total == 0 {
			t.Fatalf("%s produced empty check summary", fixture)
		}
		var sawKnownKind bool
		for _, check := range summary.Checks {
			if check.Kind == "CheckRun" || check.Kind == "StatusContext" {
				sawKnownKind = true
			}
			if strings.HasPrefix(check.CompletedAt, "0001-01-01T") {
				t.Fatalf("zero timestamp was not normalized away: %+v", check)
			}
		}
		if !sawKnownKind {
			t.Fatalf("%s did not parse either check union variant: %+v", fixture, summary.Checks)
		}
	}
}

func TestParseGitHubMergeableConflictFixture(t *testing.T) {
	detail, err := parseGitHubPRDetail(readTestdata(t, "github-mergeable-conflict.json"))
	if err != nil {
		t.Fatalf("parseGitHubPRDetail: %v", err)
	}
	if detail.Mergeability != MergeabilityConflicts {
		t.Fatalf("Mergeability = %q, want conflicts", detail.Mergeability)
	}
}

func TestParseGitLabPRDetailAndApprovalsFixtures(t *testing.T) {
	approvals, err := parseGitLabApprovals(readTestdata(t, "gitlab-approvals.json"))
	if err != nil {
		t.Fatalf("parseGitLabApprovals: %v", err)
	}
	detail, err := parseGitLabPRDetail(readTestdata(t, "gitlab-mr-detail.json"), approvals)
	if err != nil {
		t.Fatalf("parseGitLabPRDetail: %v", err)
	}
	if detail.Number != 241785 || detail.AuthorLogin != "hbakergitlab" {
		t.Fatalf("detail basics = %+v", detail)
	}
	if detail.HeadSHA != "55cd21150717bf37ceee8c8c39292179801b3dfb" {
		t.Fatalf("HeadSHA = %q", detail.HeadSHA)
	}
	if detail.Checks.Total != 1 || detail.Checks.Success != 1 {
		t.Fatalf("pipeline check summary = %+v, want one success", detail.Checks)
	}
	if len(detail.LatestReviews) == 0 || detail.ReviewDecision != "APPROVED" {
		t.Fatalf("approval verdicts not normalized: decision=%q reviews=%+v", detail.ReviewDecision, detail.LatestReviews)
	}
}

func TestParseGitLabReviewThreadsFiltersSystemGroupsAndStaleness(t *testing.T) {
	detail, err := parseGitLabPRDetail(readTestdata(t, "gitlab-mr-detail.json"), nil)
	if err != nil {
		t.Fatalf("parseGitLabPRDetail: %v", err)
	}
	threads, err := parseGitLabReviewThreads(readTestdata(t, "gitlab-discussions-all.json"), detail.HeadSHA)
	if err != nil {
		t.Fatalf("parseGitLabReviewThreads: %v", err)
	}
	if len(threads) == 0 {
		t.Fatal("expected positioned GitLab threads")
	}
	var sawOutdated, sawFileLevel, sawReplyGroup, sawConversation bool
	for _, thread := range threads {
		if thread.IsOutdated {
			sawOutdated = true
		}
		if thread.Side == "file" && thread.Line == nil {
			sawFileLevel = true
		}
		if len(thread.Comments) > 1 {
			sawReplyGroup = true
		}
		if thread.Path != "" && !thread.IsResolvable {
			t.Fatalf("positioned thread must be resolvable: %+v", thread)
		}
		if thread.Path == "" {
			// Position-less discussions are PR-level conversation threads —
			// kept, not dropped, so the comments overview can list them.
			sawConversation = true
			if thread.Line != nil || thread.IsOutdated {
				t.Fatalf("conversation thread carries diff anchors: %+v", thread)
			}
		}
		for _, comment := range thread.Comments {
			if strings.Contains(comment.Body, "changed this line in [version") || strings.Contains(comment.Body, "changed this file in [version") {
				t.Fatalf("system note was not filtered: %q", comment.Body)
			}
		}
	}
	if !sawOutdated {
		t.Fatal("expected GitLab staleness from position.head_sha mismatch")
	}
	if !sawFileLevel {
		t.Fatal("expected GitLab file-level positioned thread")
	}
	if !sawReplyGroup {
		t.Fatal("expected GitLab grouped replies")
	}
	if !sawConversation {
		t.Fatal("expected GitLab position-less conversation threads from fixture")
	}
}

func TestParseGitHubPRCommentsSkipsMinimized(t *testing.T) {
	threads, pageInfo, err := parseGitHubPRComments(readTestdata(t, "github-pr-comments.json"))
	if err != nil {
		t.Fatalf("parseGitHubPRComments: %v", err)
	}
	if pageInfo.HasNextPage {
		t.Fatal("fixture should fit on one page")
	}
	if len(threads) != 2 {
		t.Fatalf("threads = %d, want 2 (minimized comment dropped)", len(threads))
	}
	for _, thread := range threads {
		if thread.Path != "" || thread.Line != nil || thread.IsResolvable {
			t.Fatalf("conversation comment mapped with diff-thread fields: %+v", thread)
		}
		if len(thread.Comments) != 1 || thread.Comments[0].AuthorLogin == "" || thread.Comments[0].DatabaseID == 0 {
			t.Fatalf("conversation comment mapping = %+v", thread.Comments)
		}
	}
	if threads[0].Comments[0].Body != "First pass done — see inline notes." {
		t.Fatalf("body = %q", threads[0].Comments[0].Body)
	}
}

func TestParseGitLabMergeableConflictFixture(t *testing.T) {
	detail, err := parseGitLabPRDetail(readTestdata(t, "gitlab-mergeable-conflict.json"), nil)
	if err != nil {
		t.Fatalf("parseGitLabPRDetail: %v", err)
	}
	if detail.Mergeability != MergeabilityConflicts {
		t.Fatalf("Mergeability = %q, want conflicts", detail.Mergeability)
	}
}

func TestReviewSubmitConstructionGitHub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	binDir := t.TempDir()
	argsLog := filepath.Join(binDir, "args.log")
	stdinLog := filepath.Join(binDir, "stdin.json")
	ghPath := filepath.Join(binDir, "gh")
	script := `#!/bin/sh
echo "$@" > "$AO_ARGS_LOG"
cat > "$AO_STDIN_LOG"
echo '{}'
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_ARGS_LOG", argsLog)
	t.Setenv("AO_STDIN_LOG", stdinLog)
	line := 12
	core := NewCore()
	_, err := core.ForgeByID("github").SubmitReview("", "owner/repo", 9, SubmitReviewRequest{
		Verdict: ReviewVerdictApprove,
		Body:    "looks good",
		Comments: []ReviewLineComment{{
			Path: "main.go",
			Body: "nit",
			Line: &line,
			Side: "right",
		}},
	})
	if err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	args := strings.TrimSpace(readFile(t, argsLog))
	if args != "api repos/owner/repo/pulls/9/reviews -X POST --input -" {
		t.Fatalf("argv = %q", args)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(readFile(t, stdinLog)), &payload); err != nil {
		t.Fatalf("decode stdin: %v", err)
	}
	if payload["event"] != "APPROVE" || payload["body"] != "looks good" {
		t.Fatalf("payload = %+v", payload)
	}
	comments := payload["comments"].([]any)
	comment := comments[0].(map[string]any)
	if comment["path"] != "main.go" || comment["line"].(float64) != 12 || comment["side"] != "RIGHT" {
		t.Fatalf("comment payload = %+v", comment)
	}
}

func TestReviewSubmitConstructionGitLabUsesJSONInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}
	binDir := t.TempDir()
	argsLog := filepath.Join(binDir, "args.log")
	stdinLog := filepath.Join(binDir, "stdin.json")
	glabPath := filepath.Join(binDir, "glab")
	script := `#!/bin/sh
echo "$@" >> "$AO_ARGS_LOG"
case "$*" in
  *"/approvals"*) echo '{"approved_by":[]}' ;;
  *"/bulk_publish"*) echo '{}' ;;
  *"/draft_notes"*) cat > "$AO_STDIN_LOG"; echo '{"id":1}' ;;
  *) cat <<'JSON'
{"iid":1,"title":"MR","description":"","source_branch":"feature","target_branch":"main","sha":"head","web_url":"https://gitlab.com/group/repo/-/merge_requests/1","state":"opened","author":{"username":"alice"},"diff_refs":{"base_sha":"base","head_sha":"head","start_sha":"start"},"changes_count":"1","detailed_merge_status":"mergeable","has_conflicts":false}
JSON
  ;;
esac
`
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_ARGS_LOG", argsLog)
	t.Setenv("AO_STDIN_LOG", stdinLog)
	line := 7
	core := NewCore()
	_, err := core.ForgeByID("gitlab").SubmitReview("", "group/repo", 1, SubmitReviewRequest{
		Verdict: ReviewVerdictComment,
		Comments: []ReviewLineComment{{
			Path: "app.go",
			Body: "fix this",
			Line: &line,
			Side: "left",
		}},
	})
	if err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	args := readFile(t, argsLog)
	if !strings.Contains(args, "api projects/group%2Frepo/merge_requests/1/draft_notes -X POST -H Content-Type: application/json --input -") {
		t.Fatalf("argv log missing draft_notes JSON input call:\n%s", args)
	}
	if strings.Contains(args, "position[") {
		t.Fatalf("argv used bracket-form position fields:\n%s", args)
	}
	var payload struct {
		Note     string            `json:"note"`
		Position gitlabPositionRaw `json:"position"`
	}
	if err := json.Unmarshal([]byte(readFile(t, stdinLog)), &payload); err != nil {
		t.Fatalf("decode stdin: %v", err)
	}
	if payload.Note != "fix this" || payload.Position.PositionType != "text" || payload.Position.OldLine == nil || *payload.Position.OldLine != 7 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Position.BaseSHA != "base" || payload.Position.HeadSHA != "head" || payload.Position.StartSHA != "start" {
		t.Fatalf("position refs = %+v", payload.Position)
	}
}

func TestGitHubFileLevelPartialFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	script := `#!/bin/sh
case "$*" in
  "pr view --repo owner/repo 9 --json "*)
    cat <<'JSON'
{"number":9,"title":"PR","body":"","state":"OPEN","isDraft":false,"baseRefName":"main","headRefName":"feature","headRefOid":"abc","url":"https://github.com/owner/repo/pull/9","author":{"login":"alice"}}
JSON
    ;;
  "api user --jq .login")
    echo bob
    ;;
  "api repos/owner/repo/pulls/9/reviews -X POST --input -")
    cat >/dev/null
    echo '{}'
    ;;
  "api repos/owner/repo/pulls/9/comments -X POST --input -")
    cat >/dev/null
    echo 'boom' 1>&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	core := NewCore()
	_, err := core.ForgeByID("github").SubmitReview("", "owner/repo", 9, SubmitReviewRequest{
		Verdict: ReviewVerdictComment,
		Comments: []ReviewLineComment{{
			Path: "README.md",
			Body: "file note",
			Side: "file",
		}},
	})
	var partial *PartialSubmitError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want PartialSubmitError", err)
	}
	if !partial.PostedReview || partial.FailedPath != "README.md" {
		t.Fatalf("partial = %+v", partial)
	}
}

func TestGitLabApproveFailureAfterPublishIsPartial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}
	binDir := t.TempDir()
	glabPath := filepath.Join(binDir, "glab")
	script := `#!/bin/sh
case "$*" in
  *"/approve -X POST"*) echo 'approve rejected' 1>&2; exit 1 ;;
  *"/approvals"*) echo '{"approved_by":[]}' ;;
  *"/bulk_publish"*) echo '{}' ;;
  *"/draft_notes"*) cat >/dev/null; echo '{"id":1}' ;;
  *) cat <<'JSON'
{"iid":1,"title":"MR","description":"","source_branch":"feature","target_branch":"main","sha":"head","web_url":"https://gitlab.com/group/repo/-/merge_requests/1","state":"opened","author":{"username":"alice"},"diff_refs":{"base_sha":"base","head_sha":"head","start_sha":"start"},"changes_count":"1","detailed_merge_status":"mergeable","has_conflicts":false}
JSON
  ;;
esac
`
	if err := os.WriteFile(glabPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	line := 7
	core := NewCore()
	out, err := core.ForgeByID("gitlab").SubmitReview("", "group/repo", 1, SubmitReviewRequest{
		Verdict: ReviewVerdictApprove,
		Comments: []ReviewLineComment{{
			Path: "app.go",
			Body: "note",
			Line: &line,
			Side: "right",
		}},
	})
	var partial *PartialSubmitError
	if !errors.As(err, &partial) {
		t.Fatalf("error = %v, want PartialSubmitError: notes were published before approve failed", err)
	}
	if !partial.PostedReview || !out.PostedReview {
		t.Fatalf("PostedReview: partial=%v out=%v, want both true", partial.PostedReview, out.PostedReview)
	}
	if partial.FailedPath != "" {
		t.Fatalf("FailedPath = %q, want empty for an approve failure", partial.FailedPath)
	}
}

func TestGitHubViewerLoginMemoizedAcrossDetailFetches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	binDir := t.TempDir()
	userCallLog := filepath.Join(binDir, "user-calls.log")
	ghPath := filepath.Join(binDir, "gh")
	script := `#!/bin/sh
case "$*" in
  "api user --jq .login")
    echo call >> "$AO_USER_CALL_LOG"
    echo alice
    ;;
  *)
    cat <<'JSON'
{"number":9,"title":"PR","body":"","state":"OPEN","isDraft":false,"baseRefName":"main","headRefName":"feature","headRefOid":"abc","url":"https://github.com/owner/repo/pull/9","author":{"login":"alice"}}
JSON
    ;;
esac
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_USER_CALL_LOG", userCallLog)

	core := NewCore()
	now := time.Now()
	core.nowFn = func() time.Time { return now }
	forge := core.ForgeByID("github")

	for i := 0; i < 2; i++ {
		detail, err := forge.GetPRDetail("", "owner/repo", 9)
		if err != nil {
			t.Fatalf("GetPRDetail call %d: %v", i+1, err)
		}
		if !detail.ViewerIsAuthor {
			t.Fatalf("call %d: ViewerIsAuthor = false, viewer and author are both alice", i+1)
		}
	}
	if calls := strings.Count(readFile(t, userCallLog), "call"); calls != 1 {
		t.Fatalf("gh api user calls after two detail fetches = %d, want 1 (memoized)", calls)
	}

	now = now.Add(githubViewerLoginTTL + time.Second)
	if _, err := forge.GetPRDetail("", "owner/repo", 9); err != nil {
		t.Fatalf("GetPRDetail after TTL expiry: %v", err)
	}
	if calls := strings.Count(readFile(t, userCallLog), "call"); calls != 2 {
		t.Fatalf("gh api user calls after TTL expiry = %d, want 2 (re-fetched)", calls)
	}
}

// writeMockForgeCLI installs a shell-script stand-in for gh/glab on PATH
// and points it at a per-test argv log. Nothing here may reach a real
// forge CLI or the network.
func writeMockForgeCLI(t *testing.T, binary, script string) (binDir, argsLog string) {
	t.Helper()
	binDir = t.TempDir()
	argsLog = filepath.Join(binDir, "args.log")
	if err := os.WriteFile(filepath.Join(binDir, binary), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock %s: %v", binary, err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AO_ARGS_LOG", argsLog)
	return binDir, argsLog
}

func TestGitHubSetThreadResolvedUsesGraphQLMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	// The mock answers with the state the requested mutation produces, the
	// way the real API does.
	script := `#!/bin/sh
echo "$@" > "$AO_ARGS_LOG"
case "$*" in
  *"thread: unresolveReviewThread"*) echo '{"data":{"thread":{"thread":{"isResolved":false}}}}' ;;
  *) echo '{"data":{"thread":{"thread":{"isResolved":true}}}}' ;;
esac
`
	cases := []struct {
		name     string
		resolved bool
		field    string
	}{
		{name: "resolve", resolved: true, field: "resolveReviewThread"},
		{name: "unresolve", resolved: false, field: "unresolveReviewThread"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, argsLog := writeMockForgeCLI(t, "gh", script)
			core := NewCore()
			err := core.ForgeByID("github").SetThreadResolved("", "owner/repo", 9, "PRRT_thread1", tt.resolved)
			if err != nil {
				t.Fatalf("SetThreadResolved: %v", err)
			}
			args := readFile(t, argsLog)
			if !strings.HasPrefix(strings.TrimSpace(args), "api graphql -f query=mutation {") {
				t.Fatalf("argv = %q, want a gh api graphql mutation", args)
			}
			want := `thread: ` + tt.field + `(input: {threadId: "PRRT_thread1"})`
			if !strings.Contains(args, want) {
				t.Fatalf("argv = %q, want %q", args, want)
			}
			if tt.resolved && strings.Contains(args, "unresolveReviewThread") {
				t.Fatalf("argv = %q, resolve must not call the unresolve mutation", args)
			}
		})
	}
}

func TestGitHubSetThreadResolvedRejectsContradictingAnswer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	// A mutation that reports the thread still unresolved is not a success:
	// the pane must not show a state the forge does not hold.
	script := `#!/bin/sh
echo "$@" > "$AO_ARGS_LOG"
echo '{"data":{"thread":{"thread":{"isResolved":false}}}}'
`
	writeMockForgeCLI(t, "gh", script)
	core := NewCore()
	err := core.ForgeByID("github").SetThreadResolved("", "owner/repo", 9, "PRRT_thread1", true)
	if err == nil {
		t.Fatal("SetThreadResolved returned nil for a forge that reported the opposite state")
	}
	if !strings.Contains(err.Error(), "PRRT_thread1") {
		t.Fatalf("error = %v, want it to name the thread", err)
	}
}

func TestGitHubSetThreadResolvedSurfacesCLIFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	script := `#!/bin/sh
echo "$@" > "$AO_ARGS_LOG"
echo 'Could not resolve to a node' 1>&2
exit 1
`
	writeMockForgeCLI(t, "gh", script)
	core := NewCore()
	err := core.ForgeByID("github").SetThreadResolved("", "owner/repo", 9, "PRRT_thread1", true)
	if err == nil {
		t.Fatal("SetThreadResolved returned nil for a failing gh")
	}
	if !strings.Contains(err.Error(), "Could not resolve to a node") {
		t.Fatalf("error = %v, want the CLI failure text", err)
	}
}

func TestGitLabSetThreadResolvedUsesDiscussionEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}
	script := `#!/bin/sh
echo "$@" > "$AO_ARGS_LOG"
echo '{"id":"abc123","notes":[]}'
`
	cases := []struct {
		name     string
		resolved bool
		want     string
	}{
		{
			name:     "resolve",
			resolved: true,
			want:     "api projects/group%2Fsub%2Frepo/merge_requests/7/discussions/abc123?resolved=true -X PUT",
		},
		{
			name:     "unresolve",
			resolved: false,
			want:     "api projects/group%2Fsub%2Frepo/merge_requests/7/discussions/abc123?resolved=false -X PUT",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, argsLog := writeMockForgeCLI(t, "glab", script)
			core := NewCore()
			err := core.ForgeByID("gitlab").SetThreadResolved("", "group/sub/repo", 7, "abc123", tt.resolved)
			if err != nil {
				t.Fatalf("SetThreadResolved: %v", err)
			}
			if got := strings.TrimSpace(readFile(t, argsLog)); got != tt.want {
				t.Fatalf("argv = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitLabSetThreadResolvedSurfacesCLIFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}
	script := `#!/bin/sh
echo "$@" > "$AO_ARGS_LOG"
echo '404 Not Found' 1>&2
exit 1
`
	writeMockForgeCLI(t, "glab", script)
	core := NewCore()
	err := core.ForgeByID("gitlab").SetThreadResolved("", "group/repo", 7, "abc123", true)
	if err == nil {
		t.Fatal("SetThreadResolved returned nil for a failing glab")
	}
	if !strings.Contains(err.Error(), "404 Not Found") {
		t.Fatalf("error = %v, want the CLI failure text", err)
	}
}

// A missing thread id is refused before any subprocess: PATH holds no
// forge CLI here, so a call that shelled out would fail differently.
func TestSetThreadResolvedRequiresAThreadID(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	core := NewCore()
	for _, forge := range []string{"github", "gitlab"} {
		if err := core.ForgeByID(forge).SetThreadResolved("", "owner/repo", 9, "  ", true); err == nil {
			t.Fatalf("%s: SetThreadResolved returned nil for an empty thread id", forge)
		}
	}
}

func TestUnsupportedForgeRefusesThreadResolution(t *testing.T) {
	core := NewCore()
	err := core.SetThreadResolved("", PRReference{Forge: "bitbucket", Namespace: "owner", Repo: "repo", Number: 9}, "abc", true)
	if !errors.Is(err, ErrUnsupportedForge) {
		t.Fatalf("error = %v, want ErrUnsupportedForge", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
