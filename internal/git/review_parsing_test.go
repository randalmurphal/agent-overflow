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
	var sawOutdated, sawFileLevel, sawReplyGroup bool
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
