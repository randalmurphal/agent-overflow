package forgefake

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// pipelinesFixture is a GitHub repository and a GitLab project with a
// pull's CI beside pipelines on a branch, a tag and (GitLab) a merge
// request ref.
const pipelinesFixture = `{
  "repos": [
    {"forge": "github", "project": "acme/pipes", "pulls": [
      {"number": 5, "title": "Feature", "headRef": "feature", "headSha": "5555555555555555555555555555555555555555", "startSha": "6666666666666666666666666666666666666666",
       "ci": {"id": 300, "name": "CI", "jobs": [{"id": 3001, "name": "unit", "status": "running"}]}},
      {"number": 6, "title": "Fork", "headRef": "feature", "headRepo": "stranger/pipes"}
    ], "pipelines": [
      {"id": 100, "name": "CI", "ref": "main", "title": "Merge #4", "createdAt": "2026-10-01T00:00:00Z", "jobs": [{"id": 1001, "name": "unit", "status": "failed"}]},
      {"id": 200, "name": "Release", "refKind": "tag", "ref": "v1.0.0", "updatedAt": "2026-10-02T00:00:00Z", "jobs": [{"id": 2001, "name": "publish", "status": "success", "steps": [{"name": "push", "status": "success"}]}]}
    ]},
    {"forge": "gitlab", "project": "grp/pipes", "id": 6600, "pulls": [
      {"number": 9, "title": "Feature", "headRef": "feature", "ci": {"id": 30, "jobs": [{"id": 3101, "name": "unit", "stage": "test", "status": "pending"}]}}
    ], "pipelines": [
      {"id": 10, "ref": "main", "source": "schedule", "jobs": [{"id": 1101, "name": "nightly", "status": "success"}]},
      {"id": 20, "refKind": "tag", "ref": "v2.0.0", "jobs": [{"id": 2101, "name": "build", "stage": "build", "status": "success"}]}
    ]}
  ]
}`

func pipelinesEngine(t *testing.T) *Engine {
	t.Helper()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, pipelinesFixture); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestGitHubRunsListAnswersEveryPipelineNewestFirst(t *testing.T) {
	t.Parallel()
	srv := testServer(t, pipelinesEngine(t))
	var got struct {
		TotalCount   int              `json:"total_count"`
		WorkflowRuns []map[string]any `json:"workflow_runs"`
	}
	decodeBody(t, httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/runs?per_page=100", "", nil), &got)
	if got.TotalCount != 3 || len(got.WorkflowRuns) != 3 {
		t.Fatalf("runs = %+v", got)
	}
	pull, release, push := got.WorkflowRuns[0], got.WorkflowRuns[1], got.WorkflowRuns[2]
	if pull["id"] != float64(300) || pull["event"] != "pull_request" || pull["head_branch"] != "feature" || pull["status"] != "in_progress" ||
		pull["conclusion"] != nil || pull["head_sha"] != strings.Repeat("5", 40) ||
		mustJSON(t, pull["pull_requests"]) != `[{"base":{"ref":"main","sha":"`+strings.Repeat("6", 40)+`"},"head":{"ref":"feature","sha":"`+strings.Repeat("5", 40)+`"},"number":5}]` {
		t.Errorf("pull run = %v", mustJSON(t, pull))
	}
	if release["event"] != "release" || release["head_branch"] != "v1.0.0" || release["conclusion"] != "success" || release["display_title"] != "Release" ||
		release["updated_at"] != "2026-10-02T00:00:00Z" || release["html_url"] != "https://github.com/acme/pipes/actions/runs/200" {
		t.Errorf("release run = %v", mustJSON(t, release))
	}
	if push["event"] != "push" || push["display_title"] != "Merge #4" || push["conclusion"] != "failure" || push["created_at"] != "2026-10-01T00:00:00Z" {
		t.Errorf("push run = %v", mustJSON(t, push))
	}

	filtered := func(query string) []float64 {
		var page struct {
			WorkflowRuns []map[string]any `json:"workflow_runs"`
		}
		decodeBody(t, httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/runs?"+query, "", nil), &page)
		var ids []float64
		for _, run := range page.WorkflowRuns {
			ids = append(ids, run["id"].(float64))
		}
		return ids
	}
	for query, want := range map[string]string{
		"branch=main":        "[100]",
		"status=failure":     "[100]",
		"status=in_progress": "[300]",
		"status=completed":   "[200 100]",
		"branch=v1.0.0":      "[200]",
		"per_page=1&page=2":  "[200]",
		"branch=none":        "[]",
	} {
		if got := fmt.Sprint(filtered(query)); got != want {
			t.Errorf("runs?%s = %s, want %s", query, got, want)
		}
	}
	paged := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/runs?per_page=2", "", nil)
	if link := paged.header.Get("Link"); !strings.Contains(link, "/github/rest/repos/acme/pipes/actions/runs?page=2&per_page=2") {
		t.Errorf("Link = %q", link)
	}
	for _, query := range []string{"status=waiting", "event=push", "created=2026"} {
		if other := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/runs?"+query, "", nil); other.status != 404 {
			t.Errorf("runs?%s = %d, want unhandled", query, other.status)
		}
	}
	if missing := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/none/actions/runs", "", nil); missing.status != 404 || !strings.Contains(missing.body, "Not Found") {
		t.Errorf("unknown repository = %+v", missing)
	}
}

func TestGitHubRunAnswersOneRunAndRevalidates(t *testing.T) {
	t.Parallel()
	e := pipelinesEngine(t)
	srv := testServer(t, e)
	run := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/runs/200", "", nil)
	var got map[string]any
	decodeBody(t, run, &got)
	if got["id"] != float64(200) || got["name"] != "Release" || got["head_branch"] != "v1.0.0" || got["status"] != "completed" {
		t.Fatalf("run = %v", got)
	}
	etag := run.header.Get("ETag")
	if etag == "" {
		t.Fatal("the run answered no ETag")
	}
	again := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/runs/200", "", http.Header{"Authorization": {"Bearer " + testAPIToken}, "If-None-Match": {etag}})
	if again.status != http.StatusNotModified || lastInvocation(t, e).Route != "gh api run" {
		t.Fatalf("revalidated run = %d %s", again.status, lastInvocation(t, e).Route)
	}
	if missing := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/runs/999", "", nil); missing.status != 404 {
		t.Fatalf("unknown run = %d", missing.status)
	}
	// A repository pipeline's jobs and logs answer as a pull's do.
	var jobs struct {
		Jobs []map[string]any `json:"jobs"`
	}
	decodeBody(t, httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/runs/200/jobs?per_page=100", "", nil), &jobs)
	if len(jobs.Jobs) != 1 || jobs.Jobs[0]["id"] != float64(2001) || !strings.Contains(mustJSON(t, jobs.Jobs[0]["steps"]), `"name":"push"`) {
		t.Fatalf("run jobs = %v", jobs)
	}
	if log := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/pipes/actions/jobs/2001/logs", "", nil); log.status != 200 {
		t.Fatalf("repository pipeline job log = %d", log.status)
	}
}

func TestGitLabPipelinesAnswerEveryPipelineNewestFirst(t *testing.T) {
	t.Parallel()
	e := pipelinesEngine(t)
	srv := testServer(t, e)
	var rows []map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Fpipes/pipelines?per_page=100"), &rows)
	if len(rows) != 3 {
		t.Fatalf("pipelines = %v", rows)
	}
	mr, tag, branch := rows[0], rows[1], rows[2]
	if mr["id"] != float64(30) || mr["ref"] != "refs/merge-requests/9/head" || mr["source"] != "merge_request_event" || mr["status"] != "pending" ||
		mr["web_url"] != "https://gitlab.com/grp/pipes/-/pipelines/30" || mr["project_id"] != float64(6600) {
		t.Errorf("merge request pipeline = %v", mustJSON(t, mr))
	}
	if _, has := tag["tag"]; has || tag["ref"] != "v2.0.0" || tag["source"] != "push" {
		t.Errorf("tag pipeline in the list = %v", mustJSON(t, tag))
	}
	if branch["source"] != "schedule" || branch["ref"] != "main" {
		t.Errorf("branch pipeline = %v", mustJSON(t, branch))
	}
	var only []map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/6600/pipelines?ref=main&status=success"), &only)
	if len(only) != 1 || only[0]["id"] != float64(10) {
		t.Errorf("filtered = %v", only)
	}
	page := glabGET(t, srv, "gitlab.com", "projects/grp%2Fpipes/pipelines?per_page=2")
	if page.header.Get("X-Next-Page") != "2" {
		t.Errorf("X-Next-Page = %q", page.header.Get("X-Next-Page"))
	}
	if bad := glabGET(t, srv, "gitlab.com", "projects/grp%2Fpipes/pipelines?status=done"); bad.status != 404 {
		t.Errorf("status=done = %d, want unhandled", bad.status)
	}
	if bad := glabGET(t, srv, "gitlab.com", "projects/grp%2Fpipes/pipelines?scope=tags"); bad.status != 404 {
		t.Errorf("scope=tags = %d, want unhandled", bad.status)
	}

	var single map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Fpipes/pipelines/20"), &single)
	if single["tag"] != true || single["id"] != float64(20) || single["sha"] == "" {
		t.Errorf("tag pipeline = %v", single)
	}
	if missing := glabGET(t, srv, "gitlab.com", "projects/grp%2Fpipes/pipelines/99"); missing.status != 404 || !strings.Contains(missing.body, "Pipeline") {
		t.Errorf("unknown pipeline = %+v", missing)
	}
	var jobs []map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Fpipes/pipelines/20/jobs?per_page=100"), &jobs)
	if len(jobs) != 1 || jobs[0]["id"] != float64(2101) {
		t.Errorf("repository pipeline jobs = %v", jobs)
	}
	var mrJSON map[string]any
	decodeBody(t, glabGET(t, srv, "gitlab.com", "projects/grp%2Fpipes/merge_requests/9"), &mrJSON)
	if head := mustJSON(t, mrJSON["head_pipeline"]); !strings.Contains(head, `"ref":"refs/merge-requests/9/head"`) || !strings.Contains(head, `"id":30`) {
		t.Errorf("head pipeline = %s", head)
	}
}

func TestSeedRejectsMalformedPipelines(t *testing.T) {
	t.Parallel()
	for name, repo := range map[string]string{
		"unknown kind":        `{"forge":"github","project":"a/b","pipelines":[{"refKind":"note"}]}`,
		"tag without ref":     `{"forge":"github","project":"a/b","pipelines":[{"refKind":"tag"}]}`,
		"gitlab mr ref":       `{"forge":"gitlab","project":"a/b","pipelines":[{"refKind":"merge_request","ref":"feature"}]}`,
		"gitlab branch as mr": `{"forge":"gitlab","project":"a/b","pipelines":[{"ref":"refs/merge-requests/1/head"}]}`,
		"bad createdAt":       `{"forge":"github","project":"a/b","pipelines":[{"createdAt":"today"}]}`,
		"bad status":          `{"forge":"github","project":"a/b","pipelines":[{"status":"great"}]}`,
		"duplicate id":        `{"forge":"github","project":"a/b","pipelines":[{"id":4}],"pulls":[{"number":1,"title":"t","ci":{"id":4}}]}`,
	} {
		if _, err := seedJSON(New(Options{}), `{"repos":[`+repo+`]}`); err == nil {
			t.Errorf("%s: seeded", name)
		}
	}
	fixture, err := seedJSON(New(Options{}), `{"repos":[{"forge":"github","project":"a/b","pipelines":[{}],"pulls":[{"number":1,"title":"t","headRef":"topic","ci":{"refKind":"branch"}}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	repo := fixture.Repos[0]
	if p := repo.Pipelines[0]; p.Ref != "main" || p.RefKind != "branch" || p.Source != "push" || p.SHA == "" || p.Title != "CI" || p.UpdatedAt != p.CreatedAt {
		t.Errorf("repository pipeline defaults = %+v", p)
	}
	if ci := repo.Pulls[0].CI; ci.Ref != "topic" || ci.SHA != repo.Pulls[0].HeadSHA || ci.Source != "push" {
		t.Errorf("pull branch pipeline defaults = %+v", ci)
	}
}
