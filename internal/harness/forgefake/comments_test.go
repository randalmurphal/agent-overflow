package forgefake

import (
	"crypto/sha1"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const commentsFixture = `{
  "viewer": "octo",
  "repos": [
    {"forge": "github", "project": "acme/c", "pulls": [{"number": 1, "title": "C", "author": "someone", "headSha": "1111111111111111111111111111111111111111",
      "threads": [{"id": "PRRT_t", "path": "a.go", "line": 3, "side": "right", "comments": [{"id": 50, "author": "someone", "body": "theirs"}]}]}]},
    {"forge": "gitlab", "project": "grp/c", "pulls": [{"number": 2, "title": "C", "author": "someone", "headSha": "2222222222222222222222222222222222222222", "baseSha": "3333333333333333333333333333333333333333",
      "diff": "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,9 @@\n o1\n o2\n o3\n+n4\n+n5\n+n6\n+n7\n+n8\n+n9\ndiff --git a/old.go b/new.go\nsimilarity index 80%\nrename from old.go\nrename to new.go\n--- a/old.go\n+++ b/new.go\n@@ -1,3 +1,3 @@\n k\n-gone\n+came\n k2\n",
      "comments": [{"id": 60, "author": "octo", "body": "root", "replies": [{"id": 61, "author": "someone", "body": "answer"}]}]}]}
  ]
}`

func commentsEngine(t *testing.T) (*Engine, func(method, host, path, body string) httpAnswer) {
	t.Helper()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, commentsFixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	return e, func(method, host, path, body string) httpAnswer {
		header := http.Header{"Authorization": {"Bearer " + testAPIToken}}
		if host == "gitlab.com" {
			path, header = "/gitlab/api/v4/"+path, http.Header{"Private-Token": {testAPIToken}}
		} else if !strings.HasPrefix(path, "/github/graphql") {
			path = "/github/rest/" + path
		}
		return httpRequest(t, srv, method, host, path, body, header)
	}
}

// GitHub's comment routes refuse what GitHub refuses and change nothing:
// a commit other than the head, a reply under a comment that is not a
// thread's first, and an edit of another author's comment.
func TestGitHubCommentRoutesRefuseAsGitHubDoes(t *testing.T) {
	t.Parallel()
	e, send := commentsEngine(t)
	if got := send("POST", "github.com", "repos/acme/c/pulls/1/comments", `{"body":"x","commit_id":"0000000000000000000000000000000000000000","path":"a.go","line":2,"side":"RIGHT"}`); got.status != http.StatusUnprocessableEntity {
		t.Fatalf("comment on another commit = %+v, want 422", got)
	}
	if got := send("POST", "github.com", "repos/acme/c/pulls/1/comments", `{"body":"x","commit_id":"1111111111111111111111111111111111111111","path":"a.go","subject_type":"file","line":2}`); got.status != http.StatusUnprocessableEntity {
		t.Fatalf("file comment with a line = %+v, want 422", got)
	}
	if got := send("POST", "github.com", "repos/acme/c/pulls/1/comments/51/replies", `{"body":"x"}`); got.status != http.StatusNotFound {
		t.Fatalf("reply under no thread = %+v, want 404", got)
	}
	update := mustJSON(t, map[string]any{"query": "mutation UpdateReviewComment($id: ID!, $body: String!) { x }", "operationName": "UpdateReviewComment", "variables": map[string]any{"id": "PRRC_ao50", "body": "mine"}})
	if got := send("POST", "github.com", "/github/graphql", update); !strings.Contains(got.body, "FORBIDDEN") {
		t.Fatalf("edit of another author's comment = %+v", got)
	}
	issue := mustJSON(t, map[string]any{"query": "mutation UpdateIssueComment($id: ID!, $body: String!) { x }", "operationName": "UpdateIssueComment", "variables": map[string]any{"id": "PRRC_ao50", "body": "mine"}})
	if got := send("POST", "github.com", "/github/graphql", issue); !strings.Contains(got.body, "NOT_FOUND") {
		t.Fatalf("an issue comment edit naming a review comment = %+v", got)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.repoOn("github", "github.com", "acme/c").pull(1)
	if len(p.Threads) != 1 || len(p.Threads[0].Comments) != 1 || p.Threads[0].Comments[0].Body != "theirs" || p.Threads[0].Comments[0].EditedAt != "" {
		t.Fatalf("threads after the refusals = %+v", p.Threads)
	}
}

func lineCode(path string, oldLine, newLine int) string {
	return fmt.Sprintf("%x_%d_%d", sha1.Sum([]byte(path)), oldLine, newLine)
}

// GitLab's discussion routes: a position against another diff, or naming
// a file or a line the diff does not have, is GitLab's 400; a line range
// whose line codes are not the diff's cursors is drift; a renamed file is
// named by both its paths; a note's edit is its author's or a
// maintainer's; deleting a conversation's first note leaves the
// discussion, under its id, to its reply.
func TestGitLabCommentRoutesKeepDiscussionsAsGitLabDoes(t *testing.T) {
	t.Parallel()
	e, send := commentsEngine(t)
	const mr = "projects/grp%2Fc/merge_requests/2"
	position := func(head, code string) string {
		return fmt.Sprintf(`{"body":"r","position":{"base_sha":"3333333333333333333333333333333333333333","start_sha":"3333333333333333333333333333333333333333","head_sha":%q,"old_path":"a.go","new_path":"a.go","position_type":"text","new_line":9,
			"line_range":{"start":{"line_code":%q,"type":"new","new_line":4},"end":{"line_code":%q,"type":"new","new_line":9}}}}`, head, code, lineCode("a.go", 4, 9))
	}
	single := func(oldPath, newPath, lines string) string {
		return fmt.Sprintf(`{"body":"s","position":{"base_sha":"3333333333333333333333333333333333333333","start_sha":"3333333333333333333333333333333333333333","head_sha":%q,"old_path":%q,"new_path":%q,"position_type":"text",%s}}`, strings.Repeat("2", 40), oldPath, newPath, lines)
	}
	if got := send("POST", "gitlab.com", mr+"/discussions", position(strings.Repeat("9", 40), lineCode("a.go", 4, 4))); got.status != http.StatusBadRequest {
		t.Fatalf("a position on another head = %+v, want 400", got)
	}
	if got := send("POST", "gitlab.com", mr+"/discussions", position(strings.Repeat("2", 40), lineCode("b.go", 4, 4))); got.status != http.StatusNotFound || !lastInvocation(t, e).Unhandled {
		t.Fatalf("a line code of another file = %+v, want unhandled", got)
	}
	// An added line's old cursor is the next old line, not 0.
	if got := send("POST", "gitlab.com", mr+"/discussions", position(strings.Repeat("2", 40), lineCode("a.go", 0, 4))); got.status != http.StatusNotFound || !lastInvocation(t, e).Unhandled {
		t.Fatalf("a zero-filled line code = %+v, want unhandled", got)
	}
	for name, body := range map[string]string{
		"a file the diff lacks":          single("z.go", "z.go", `"new_line":1`),
		"a line the diff lacks":          single("a.go", "a.go", `"new_line":2`),
		"a renamed file by its new path": single("new.go", "new.go", `"new_line":2`),
		"an unchanged line off its pair": single("a.go", "a.go", `"old_line":1,"new_line":2`),
	} {
		if got := send("POST", "gitlab.com", mr+"/discussions", body); got.status != http.StatusBadRequest || lastInvocation(t, e).Unhandled {
			t.Fatalf("%s = %+v, want GitLab's 400", name, got)
		}
	}
	if got := send("POST", "gitlab.com", mr+"/discussions", position(strings.Repeat("2", 40), lineCode("a.go", 4, 4))); got.status != http.StatusCreated {
		t.Fatalf("a range = %+v", got)
	}
	for _, lines := range []string{`"old_line":2`, `"new_line":2`, `"old_line":3,"new_line":3`} {
		if got := send("POST", "gitlab.com", mr+"/discussions", single("old.go", "new.go", lines)); got.status != http.StatusCreated {
			t.Fatalf("a renamed file's line %s = %+v", lines, got)
		}
	}
	if read := send("GET", "gitlab.com", mr+"/discussions?per_page=100", ""); strings.Count(read.body, `"old_path":"old.go"`) != 3 || strings.Count(read.body, `"new_path":"new.go"`) != 3 {
		t.Fatalf("a renamed file's discussions = %s", read.body)
	}
	diffs := send("GET", "gitlab.com", mr+"/diffs?per_page=1&page=2", "")
	if diffs.status != http.StatusOK || !strings.Contains(diffs.body, `"old_path":"old.go"`) || !strings.Contains(diffs.body, `"new_path":"new.go"`) || !strings.Contains(diffs.body, `"renamed_file":true`) || strings.Contains(diffs.body, `"a.go"`) {
		t.Fatalf("the diffs' second page = %+v", diffs)
	}

	discussion := fmt.Sprintf("%040x", 60)
	if got := send("PUT", "gitlab.com", mr+"/discussions/"+discussion+"/notes/61", `{"body":"mine"}`); got.status != http.StatusForbidden {
		t.Fatalf("edit of another author's note = %+v, want 403", got)
	}
	if got := send("DELETE", "gitlab.com", mr+"/discussions/"+discussion+"/notes/61", ""); got.status != http.StatusForbidden {
		t.Fatalf("a developer's delete of another author's note = %+v, want 403", got)
	}
	if got := send("DELETE", "gitlab.com", mr+"/discussions/"+discussion+"/notes/60", ""); got.status != http.StatusNoContent {
		t.Fatalf("delete of the first note = %+v", got)
	}
	if got := send("POST", "gitlab.com", mr+"/discussions/"+discussion+"/notes", `{"body":"still here"}`); got.status != http.StatusCreated {
		t.Fatalf("a reply after the first note went = %+v", got)
	}
	read := send("GET", "gitlab.com", mr+"/discussions?per_page=100", "")
	if !strings.Contains(read.body, `"id":"`+discussion+`"`) || strings.Contains(read.body, `"body":"root"`) || !strings.Contains(read.body, `"body":"answer"`) || !strings.Contains(read.body, `"body":"still here"`) {
		t.Fatalf("discussions = %s", read.body)
	}
	e.mu.Lock()
	p := e.repoOn("gitlab", "gitlab.com", "grp/c").pull(2)
	if len(p.Threads) != 4 || p.Threads[0].StartLine == nil || *p.Threads[0].StartLine != 4 || *p.Threads[0].Line != 9 || p.Threads[1].OldPath != "old.go" || p.Threads[1].Path != "new.go" {
		t.Fatalf("threads = %+v", p.Threads)
	}
	e.mu.Unlock()

	if got := send("POST", "gitlab.com", mr+"/draft_notes", `{"note":"x","in_reply_to_discussion_id":"nope"}`); got.status != http.StatusBadRequest || lastInvocation(t, e).Unhandled {
		t.Fatalf("a draft reply to no discussion = %+v, want GitLab's 400", got)
	}
}
