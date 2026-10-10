package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return string(data)
}

// decodeTestJSON decodes a recorded forge answer into the forge's raw
// shape.
func decodeTestJSON[T any](t *testing.T, raw string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %T: %v", out, err)
	}
	return out
}

func TestParseGitLabPRDetailAndApprovalsFixtures(t *testing.T) {
	t.Parallel()
	approvals := gitlabApprovalVerdicts(decodeTestJSON[gitlabApprovalsRaw](t, readTestdata(t, "gitlab-approvals.json")))
	detail := gitlabPRDetail(decodeTestJSON[gitlabMRRaw](t, readTestdata(t, "gitlab-mr-detail.json")), approvals)
	if detail.Number != 241785 || detail.AuthorLogin != "hbakergitlab" || detail.AuthorName != "Hannah Baker" {
		t.Fatalf("detail basics = %+v", detail)
	}
	approvers := map[string]string{}
	for _, review := range detail.LatestReviews {
		approvers[review.AuthorLogin] = review.AuthorName
	}
	if approvers["pshutsin"] != "Pavel Shutsin" || approvers["uokeadu"] != "Ugo Nnanna Okeadu" {
		t.Fatalf("approval names = %v", approvers)
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
	t.Parallel()
	detail := gitlabPRDetail(decodeTestJSON[gitlabMRRaw](t, readTestdata(t, "gitlab-mr-detail.json")), nil)
	threads := gitlabReviewThreads(decodeTestJSON[[]gitlabDiscussionRaw](t, readTestdata(t, "gitlab-discussions-all.json")), detail.HeadSHA)
	if len(threads) == 0 {
		t.Fatal("expected positioned GitLab threads")
	}
	var sawOutdated, sawFileLevel, sawReplyGroup, sawConversation, sawAuthorName bool
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			if comment.AuthorLogin == "hbakergitlab" {
				if comment.AuthorName != "Hannah Baker" {
					t.Fatalf("note author name = %q, want Hannah Baker", comment.AuthorName)
				}
				sawAuthorName = true
			}
		}
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
	if !sawAuthorName {
		t.Fatal("expected notes by hbakergitlab in fixture")
	}
}

func TestParseGitLabMergeableConflictFixture(t *testing.T) {
	t.Parallel()
	detail := gitlabPRDetail(decodeTestJSON[gitlabMRRaw](t, readTestdata(t, "gitlab-mergeable-conflict.json")), nil)
	if detail.Mergeability != MergeabilityConflicts {
		t.Fatalf("Mergeability = %q, want conflicts", detail.Mergeability)
	}
}

func commentAuthors(threads []ReviewThread) [][2]string {
	var out [][2]string
	for _, thread := range threads {
		for _, comment := range thread.Comments {
			out = append(out, [2]string{comment.AuthorLogin, comment.AuthorName})
		}
	}
	return out
}

func TestParseForgeAuthorWithoutName(t *testing.T) {
	t.Parallel()
	approvals := gitlabApprovalVerdicts(decodeTestJSON[gitlabApprovalsRaw](t, `{"approved_by":[{"user":{"username":"erin"},"approved_at":"t"}]}`))
	if len(approvals) != 1 || approvals[0].AuthorLogin != "erin" || approvals[0].AuthorName != "" {
		t.Fatalf("GitLab approvals without name = %+v", approvals)
	}
	gitlab := gitlabPRDetail(decodeTestJSON[gitlabMRRaw](t, `{"iid":1,"author":{"username":"dave"}}`), approvals)
	if gitlab.AuthorLogin != "dave" || gitlab.AuthorName != "" {
		t.Fatalf("GitLab detail without name = %+v", gitlab)
	}
	threads := gitlabReviewThreads(decodeTestJSON[[]gitlabDiscussionRaw](t, `[{"id":"d1","notes":[{"id":1,"body":"b","author":{"username":"dave"}}]}]`), "")
	if fmt.Sprint(commentAuthors(threads)) != fmt.Sprint([][2]string{{"dave", ""}}) {
		t.Fatalf("GitLab note without name = %+v", threads)
	}
}

// A missing thread id is refused before any request: the Core has no
// forge API transport, so a call that reached for one would fail
// differently.
func TestSetThreadResolvedRequiresAThreadID(t *testing.T) {
	t.Parallel()
	core := NewCore()
	for _, forge := range []string{"github", "gitlab"} {
		if err := core.ForgeByID(forge).SetThreadResolved(t.Context(), testPRRef("owner/repo", 9), "  ", true); err == nil || errors.Is(err, ErrNoForgeAPI) {
			t.Fatalf("%s: SetThreadResolved returned nil for an empty thread id", forge)
		}
	}
}

func TestUnsupportedForgeRefusesThreadResolution(t *testing.T) {
	t.Parallel()
	core := NewCore()
	err := core.SetThreadResolved(t.Context(), PRReference{Forge: "bitbucket", Host: "bitbucket.org", Namespace: "owner", Repo: "repo", Number: 9}, "abc", true)
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
