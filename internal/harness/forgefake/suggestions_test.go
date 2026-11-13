package forgefake

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

const suggestionsFixture = `{
  "viewer": "octo",
  "repos": [
    {"forge": "github", "project": "acme/s", "pulls": [
      {"number": 1, "title": "S", "author": "someone", "headSha": "1111111111111111111111111111111111111111",
       "files": {"a.go": "one\ntwo\nthree\n", "run.sh": "echo\n", "link": "a.go"},
       "fileModes": {"run.sh": "100755", "link": "120000", "vendor": "160000", "dir": "040000"},
       "threads": [{"id": "PRRT_s", "path": "a.go", "line": 2, "side": "right", "comments": [{"id": 50, "author": "someone", "authorName": "Some One", "body": "` + "```suggestion\\nTWO\\n```" + `",
         "reactions": [{"name": "heart", "user": "someone"}, {"name": "heart", "user": "octo"}]}]}],
       "comments": [{"id": 51, "author": "someone", "body": "note"}]},
      {"number": 2, "title": "Fork", "headRef": "f", "headRepo": "fork/s", "headSha": "2222222222222222222222222222222222222222", "files": {"a.go": "x\n"}}]},
    {"forge": "gitlab", "project": "grp/s", "pulls": [
      {"number": 3, "title": "S", "author": "someone", "headSha": "3333333333333333333333333333333333333333",
       "files": {"a.go": "one\ntwo\nthree\n"},
       "threads": [{"id": "d1", "path": "a.go", "line": 2, "side": "right", "comments": [{"id": 70, "author": "someone", "body": "` + "```suggestion:-1+0\\nONE\\nTWO\\n```" + `",
         "reactions": [{"name": "party_parrot", "user": "someone", "id": 900}]}]}],
       "comments": [{"id": 71, "author": "someone", "body": "note"}]}]}
  ]
}`

func suggestionsEngine(t *testing.T) (*Engine, func(method, host, path, body string, header http.Header) httpAnswer) {
	t.Helper()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, suggestionsFixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	return e, func(method, host, path, body string, header http.Header) httpAnswer {
		if header == nil {
			header = http.Header{}
		}
		if host == "gitlab.com" {
			header.Set("Private-Token", testAPIToken)
			if !strings.HasPrefix(path, "/") {
				path = "/gitlab/api/v4/" + path
			}
		} else {
			header.Set("Authorization", "Bearer "+testAPIToken)
			if !strings.HasPrefix(path, "/") {
				path = "/github/rest/" + path
			}
		}
		return httpRequest(t, srv, method, host, path, body, header)
	}
}

func gql(t *testing.T, kind, op, decl string, vars map[string]any) string {
	t.Helper()
	return mustJSON(t, map[string]any{"query": kind + " " + op + decl + " { x }", "operationName": op, "variables": vars})
}

// GitHub: the contents route serves the head's file in the raw media type
// only; the commit refuses a moved head, a viewer who may not push and a
// fork head, and otherwise lands on the branch with a new head and the
// thread on the changed line outdated.
func TestGitHubSuggestionRoutes(t *testing.T) {
	t.Parallel()
	e, send := suggestionsEngine(t)
	raw := http.Header{"Accept": {"application/vnd.github.raw+json"}}
	head := "1111111111111111111111111111111111111111"
	entry := func(repo, oid, path string) string {
		owner, name, _ := strings.Cut(repo, "/")
		return send("POST", "github.com", "/github/graphql", gql(t, "query", "SuggestionFile", "($owner: String!, $name: String!, $oid: GitObjectID!, $path: String!)",
			map[string]any{"owner": owner, "name": name, "oid": oid, "path": path}), nil).body
	}
	blob := gitBlobID("one\ntwo\nthree\n")
	if got := entry("acme/s", head, "a.go"); !strings.Contains(got, `"file":{"mode":33188,"object":{"__typename":"Blob","byteSize":14,"isBinary":false},"oid":"`+blob+`","type":"blob"}`) {
		t.Fatalf("entry = %s", got)
	}
	for path, want := range map[string]string{
		"run.sh": `"mode":33261,"object":{"__typename":"Blob"`,
		"link":   `"mode":40960,"object":{"__typename":"Blob"`,
		"vendor": `"mode":57344,"object":{"__typename":"Commit"},"oid":"`,
		"dir":    `"mode":16384,"object":{"__typename":"Tree"},"oid":"`,
		"gone":   `"file":null`,
	} {
		if got := entry("acme/s", head, path); !strings.Contains(got, want) {
			t.Fatalf("entry %s = %s", path, got)
		}
	}
	if got := entry("acme/s", strings.Repeat("9", 40), "a.go"); !strings.Contains(got, `"object":null`) {
		t.Fatalf("entry at another commit = %s", got)
	}
	if got := entry("nobody/s", head, "a.go"); !strings.Contains(got, "NOT_FOUND") {
		t.Fatalf("entry in an unknown repository = %s", got)
	}
	if got := send("GET", "github.com", "repos/acme/s/git/blobs/"+blob, "", raw); got.status != 200 || got.body != "one\ntwo\nthree\n" {
		t.Fatalf("blob = %+v", got)
	}
	if got := send("GET", "github.com", "repos/acme/s/git/blobs/"+strings.Repeat("a", 40), "", raw); got.status != http.StatusNotFound {
		t.Fatalf("unknown blob = %+v", got)
	}
	if got := send("GET", "github.com", "repos/acme/s/git/blobs/"+blob, "", nil); got.status != http.StatusNotFound {
		t.Fatalf("blob without the raw media type = %+v, want unhandled", got)
	}
	if inv := e.Invocations(0).Invocations; !inv[len(inv)-1].Unhandled {
		t.Fatalf("blob without the raw media type was handled: %+v", inv[len(inv)-1])
	}
	if got := send("GET", "github.com", "repos/fork/s/git/blobs/"+gitBlobID("x\n"), "", raw); got.status != 200 || got.body != "x\n" {
		t.Fatalf("a fork head's blob = %+v", got)
	}
	decl := "($repository: String!, $branch: String!, $expectedHeadOid: GitObjectID!, $headline: String!, $body: String, $path: String!, $contents: Base64String!)"
	commit := func(repo, branch, head string) string {
		return gql(t, "mutation", "SuggestionCommit", decl, map[string]any{"repository": repo, "branch": branch, "expectedHeadOid": head,
			"headline": "Apply suggestion from code review", "path": "a.go", "contents": base64.StdEncoding.EncodeToString([]byte("one\nTWO\nthree\n"))})
	}
	if got := send("POST", "github.com", "/github/graphql", commit("acme/s", "feature", "0000000000000000000000000000000000000000"), nil); !strings.Contains(got.body, "Expected branch to point to") {
		t.Fatalf("commit on a moved head = %s", got.body)
	}
	if got := send("POST", "github.com", "/github/graphql", commit("fork/s", "f", "2222222222222222222222222222222222222222"), nil); !strings.Contains(got.body, "FORBIDDEN") {
		t.Fatalf("commit on a fork head = %s", got.body)
	}
	e.mu.Lock()
	p := e.repoOn("github", "github.com", "acme/s").pull(1)
	if p.HeadSHA != "1111111111111111111111111111111111111111" || p.Files["a.go"] != "one\ntwo\nthree\n" || p.Threads[0].Outdated {
		t.Fatalf("a refused commit changed the pull: %+v", p)
	}
	e.mu.Unlock()
	got := send("POST", "github.com", "/github/graphql", commit("acme/s", "feature", "1111111111111111111111111111111111111111"), nil)
	var answer struct {
		Data struct {
			CreateCommitOnBranch struct {
				Commit struct{ OID string }
			}
		}
	}
	if err := json.Unmarshal([]byte(got.body), &answer); err != nil || answer.Data.CreateCommitOnBranch.Commit.OID == "" {
		t.Fatalf("commit = %s", got.body)
	}
	e.mu.Lock()
	if p.HeadSHA != answer.Data.CreateCommitOnBranch.Commit.OID || p.Files["a.go"] != "one\nTWO\nthree\n" || !p.Threads[0].Outdated {
		t.Fatalf("after the commit: head %s, file %q, outdated %v", p.HeadSHA, p.Files["a.go"], p.Threads[0].Outdated)
	}
	e.mu.Unlock()
	// The viewer may not push below WRITE.
	e.mu.Lock()
	e.repoOn("github", "github.com", "acme/s").Merge.Permission = "TRIAGE"
	head = p.HeadSHA
	e.mu.Unlock()
	if got := send("POST", "github.com", "/github/graphql", commit("acme/s", "feature", head), nil); !strings.Contains(got.body, "FORBIDDEN") {
		t.Fatalf("commit below write = %s", got.body)
	}
}

// GitHub reactions: every content in reactionGroups, and the add and
// remove mutations change the viewer's reaction alone.
func TestGitHubReactionRoutes(t *testing.T) {
	t.Parallel()
	e, send := suggestionsEngine(t)
	target := func(id string) string {
		return send("POST", "github.com", "/github/graphql", gql(t, "query", "ReactionTarget", "($id: ID!)", map[string]any{"id": id}), nil).body
	}
	if body := target("PRRC_ao50"); !strings.Contains(body, `"content":"HEART","reactors":{"totalCount":2},"viewerHasReacted":true`) || !strings.Contains(body, `"nameWithOwner":"acme/s"`) {
		t.Fatalf("target = %s", body)
	}
	react := func(op, id, content string) string {
		return send("POST", "github.com", "/github/graphql", gql(t, "mutation", op, "($subjectID: ID!, $content: ReactionContent!)", map[string]any{"subjectID": id, "content": content}), nil).body
	}
	react("AddReaction", "IC_ao51", "ROCKET")
	react("AddReaction", "IC_ao51", "ROCKET")
	react("RemoveReaction", "PRRC_ao50", "HEART")
	react("RemoveReaction", "PRRC_ao50", "EYES")
	if body := react("AddReaction", "IC_ao999", "ROCKET"); !strings.Contains(body, "NOT_FOUND") {
		t.Fatalf("react to an unknown comment = %s", body)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.repoOn("github", "github.com", "acme/s").pull(1)
	if r := p.Comments[0].Reactions; len(r) != 1 || r[0].Name != "rocket" || r[0].User != "octo" {
		t.Fatalf("issue comment reactions = %+v", r)
	}
	if r := p.Threads[0].Comments[0].Reactions; len(r) != 1 || r[0].User != "someone" {
		t.Fatalf("review comment reactions = %+v", r)
	}
}

// GitLab: the discussions carry each note's suggestions as GitLab derives
// them (a -N+M header widens the lines), the apply refuses a viewer
// below developer and one not appliable, and otherwise rewrites the file,
// moves the head and reads applied.
func TestGitLabSuggestionRoutes(t *testing.T) {
	t.Parallel()
	e, send := suggestionsEngine(t)
	discussions := send("GET", "gitlab.com", "projects/grp%2Fs/merge_requests/3/discussions?per_page=100", "", nil).body
	id := "70000"
	if !strings.Contains(discussions, `"suggestions":[{"appliable":true,"applied":false,"from_content":"one\ntwo\n","from_line":1,"id":`+id+`,"to_content":"ONE\nTWO\n","to_line":2}]`) {
		t.Fatalf("discussions = %s", discussions)
	}
	e.mu.Lock()
	e.repoOn("gitlab", "gitlab.com", "grp/s").Merge.Permission = "reporter"
	e.mu.Unlock()
	if got := send("PUT", "gitlab.com", "suggestions/"+id+"/apply", "", nil); got.status != http.StatusForbidden {
		t.Fatalf("apply as a reporter = %+v", got)
	}
	e.mu.Lock()
	e.repoOn("gitlab", "gitlab.com", "grp/s").Merge.Permission = "developer"
	e.mu.Unlock()
	if got := send("PUT", "gitlab.com", "suggestions/70001/apply", "", nil); got.status != http.StatusNotFound {
		t.Fatalf("apply of an unknown suggestion = %+v", got)
	}
	if got := send("PUT", "gitlab.com", "suggestions/"+id+"/apply", "", nil); got.status != 200 || !strings.Contains(got.body, `"applied":true`) {
		t.Fatalf("apply = %+v", got)
	}
	e.mu.Lock()
	p := e.repoOn("gitlab", "gitlab.com", "grp/s").pull(3)
	if p.Files["a.go"] != "ONE\nTWO\nthree\n" || p.HeadSHA == "3333333333333333333333333333333333333333" || !p.Threads[0].Outdated {
		t.Fatalf("after the apply: %+v", p)
	}
	e.mu.Unlock()
	if got := send("PUT", "gitlab.com", "suggestions/"+id+"/apply", "", nil); got.status != http.StatusBadRequest {
		t.Fatalf("a second apply = %+v", got)
	}
}

// GitLab reactions: the GraphQL read pages every note with its awards,
// and the REST routes add, list and remove the viewer's award, refusing
// a duplicate, a blank name, another user's award and a note of another
// merge request.
func TestGitLabReactionRoutes(t *testing.T) {
	t.Parallel()
	e, send := suggestionsEngine(t)
	read := send("POST", "gitlab.com", "/gitlab/api/graphql", gql(t, "query", "MRNoteReactions", "($fullPath: ID!, $iid: String!, $after: String)", map[string]any{"fullPath": "grp/s", "iid": "3"}), nil).body
	if !strings.Contains(read, `{"awardEmoji":{"nodes":[{"name":"party_parrot","user":{"username":"someone"}}],"pageInfo":{"endCursor":"cursor:1","hasNextPage":false}},"id":"gid://gitlab/DiffNote/70"}`) ||
		!strings.Contains(read, `"id":"gid://gitlab/Note/71"`) {
		t.Fatalf("reactions read = %s", read)
	}
	notes := "projects/grp%2Fs/merge_requests/3/notes/"
	if got := send("POST", "gitlab.com", notes+"70/award_emoji", `{"name":"thumbsup"}`, nil); got.status != http.StatusCreated {
		t.Fatalf("award = %+v", got)
	}
	if got := send("POST", "gitlab.com", notes+"70/award_emoji", `{"name":"thumbsup"}`, nil); got.status != http.StatusNotFound || !strings.Contains(got.body, "has already been taken") {
		t.Fatalf("a second award = %+v", got)
	}
	if got := send("POST", "gitlab.com", notes+"70/award_emoji", `{"name":""}`, nil); got.status != http.StatusBadRequest {
		t.Fatalf("a blank award = %+v", got)
	}
	if got := send("POST", "gitlab.com", "projects/grp%2Fs/merge_requests/3/notes/12345/award_emoji", `{"name":"eyes"}`, nil); got.status != http.StatusNotFound {
		t.Fatalf("an award on a note of no merge request = %+v", got)
	}
	if got := send("DELETE", "gitlab.com", notes+"70/award_emoji/900", "", nil); got.status != http.StatusForbidden {
		t.Fatalf("removing another user's award = %+v", got)
	}
	list := send("GET", "gitlab.com", notes+"70/award_emoji?per_page=1", "", nil)
	if list.header.Get("X-Next-Page") != "2" || !strings.Contains(list.body, `"party_parrot"`) {
		t.Fatalf("first award page = %+v", list)
	}
	second := send("GET", "gitlab.com", notes+"70/award_emoji?per_page=1&page=2", "", nil)
	var awards []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(second.body), &awards); err != nil || len(awards) != 1 || awards[0].Name != "thumbsup" {
		t.Fatalf("second award page = %s", second.body)
	}
	if got := send("DELETE", "gitlab.com", notes+"70/award_emoji/"+strconv.FormatInt(awards[0].ID, 10), "", nil); got.status != http.StatusNoContent {
		t.Fatalf("removing the viewer's award = %+v", got)
	}
	e.mu.Lock()
	if r := e.repoOn("gitlab", "gitlab.com", "grp/s").pull(3).Threads[0].Comments[0].Reactions; len(r) != 1 || r[0].ID != 900 {
		t.Fatalf("reactions = %+v", r)
	}
	e.repoOn("gitlab", "gitlab.com", "grp/s").ServerLacks = []string{lacksAwardEmoji}
	e.mu.Unlock()
	older := send("POST", "gitlab.com", "/gitlab/api/graphql", gql(t, "query", "MRNoteReactions", "($fullPath: ID!, $iid: String!, $after: String)", map[string]any{"fullPath": "grp/s", "iid": "3"}), nil).body
	if !strings.Contains(older, "Field 'awardEmoji' doesn't exist on type 'Note'") || strings.Contains(older, `"data"`) {
		t.Fatalf("reactions read on an older server = %s", older)
	}
}

// fileModes name a GitHub mode the fake models, on a path whose kind
// matches what files holds.
func TestFileModesAreChecked(t *testing.T) {
	t.Parallel()
	for name, pull := range map[string]string{
		"unknown mode":         `"files": {"a": "x"}, "fileModes": {"a": "100666"}`,
		"directory with bytes": `"files": {"a": "x"}, "fileModes": {"a": "040000"}`,
		"link without target":  `"fileModes": {"a": "120000"}`,
	} {
		fixture := `{"repos": [{"forge": "github", "project": "o/r", "pulls": [{"number": 1, "title": "T", ` + pull + `}]}]}`
		if _, err := seedJSON(New(Options{APIToken: testAPIToken}), fixture); err == nil {
			t.Errorf("%s: seeded", name)
		}
	}
	gitlab := `{"repos": [{"forge": "gitlab", "project": "g/r", "pulls": [{"number": 1, "title": "T", "files": {"a": "x"}, "fileModes": {"a": "100755"}}]}]}`
	if _, err := seedJSON(New(Options{APIToken: testAPIToken}), gitlab); err == nil {
		t.Error("GitLab fileModes seeded")
	}
}
