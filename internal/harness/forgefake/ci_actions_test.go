package forgefake

import (
	"encoding/base64"
	"net/http"
	"testing"
)

// ciActionsFixture is a GitHub repository and a GitLab project whose CI
// the actions move: a completed GitHub run with a failed job and
// artifacts, a running one, and a GitLab pipeline with a failed build, a
// manual one and one holding an archive, beside a running pipeline.
const ciActionsFixture = `{
  "repos": [
    {"forge": "github", "project": "acme/ci", "pipelines": [
      {"id": 100, "name": "CI", "ref": "main", "jobs": [{"id": 1001, "name": "unit", "status": "failed"}, {"id": 1002, "name": "lint", "status": "success"}],
       "artifacts": [{"id": 51, "name": "coverage", "text": "PK-coverage"}, {"id": 52, "name": "old", "text": "x", "expired": true}]},
      {"id": 200, "name": "CI", "ref": "feature", "jobs": [{"id": 2001, "name": "unit", "status": "running"}, {"id": 2002, "name": "e2e", "status": "pending"}]}
    ]},
    {"forge": "gitlab", "project": "grp/ci", "id": 7700, "pipelines": [
      {"id": 10, "ref": "main", "jobs": [
        {"id": 1101, "name": "unit", "stage": "test", "status": "failed"},
        {"id": 1102, "name": "build", "stage": "build", "status": "success", "artifacts": {"base64": "UEsDBA=="}},
        {"id": 1103, "name": "release", "stage": "deploy", "status": "manual"}
      ]},
      {"id": 20, "ref": "feature", "jobs": [{"id": 2101, "name": "unit", "stage": "test", "status": "running"}, {"id": 2102, "name": "lint", "stage": "test", "status": "pending"}]}
    ]}
  ]
}`

func ghRunJobIDs(t *testing.T, e *Engine, run int64) map[string]string {
	t.Helper()
	srv := testServer(t, e)
	var got struct {
		Jobs []map[string]any `json:"jobs"`
	}
	decodeBody(t, httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/ci/actions/runs/"+itoa(run)+"/jobs?per_page=100", "", nil), &got)
	out := map[string]string{}
	for _, job := range got.Jobs {
		out[job["name"].(string)] = itoa(int64(job["id"].(float64))) + " " + job["status"].(string)
	}
	return out
}

func itoa(n int64) string { return jsonInt(n) }

// A GitHub re-run replaces the job with a pending attempt under a new id,
// which the jobs list answers in its place; finishing it on demand moves
// the run; the run's failed jobs re-run the same way; a cancel ends a
// live run's jobs; and each refuses what GitHub refuses, changing
// nothing.
func TestGitHubCIActionsMoveTheRun(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, ciActionsFixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	post := func(path string) httpAnswer {
		return httpRequest(t, srv, "POST", "github.com", "/github/rest/repos/acme/ci/actions/"+path, "", nil)
	}
	if got := post("jobs/2001/rerun"); got.status != http.StatusForbidden {
		t.Fatalf("re-run of a job of a live run = %d", got.status)
	}
	if got := post("jobs/1001/rerun"); got.status != http.StatusCreated || lastInvocation(t, e).Route != "gh api job rerun" {
		t.Fatalf("re-run = %+v", got)
	}
	jobs := ghRunJobIDs(t, e, 100)
	if jobs["unit"] == "1001 completed" || jobs["unit"][len(jobs["unit"])-6:] != "queued" || jobs["lint"] != "1002 completed" {
		t.Fatalf("jobs after the re-run = %v, want unit as a new queued attempt", jobs)
	}
	job, err := e.AdvanceJob(JobAdvance{Forge: "github", Project: "acme/ci", Job: "unit", Status: "success", Log: new(string)})
	if err != nil || job.StartedAt == "" || job.CompletedAt == "" {
		t.Fatalf("AdvanceJob = %+v, %v", job, err)
	}
	if run := e.repos["github\x00acme/ci"].pipeline(100); run.Status != "success" {
		t.Fatalf("run status after the attempt passed = %q", run.Status)
	}
	if got := post("runs/100/rerun-failed-jobs"); got.status != http.StatusForbidden {
		t.Fatalf("re-run failed of a run with none = %d", got.status)
	}
	if got := post("runs/200/cancel"); got.status != http.StatusAccepted {
		t.Fatalf("cancel = %+v", got)
	}
	if jobs := ghRunJobIDs(t, e, 200); jobs["unit"] != "2001 completed" || jobs["e2e"] != "2002 completed" {
		t.Fatalf("jobs after the cancel = %v", jobs)
	}
	if run := e.repos["github\x00acme/ci"].pipeline(200); run.Status != "canceled" {
		t.Fatalf("canceled run status = %q", run.Status)
	}
	if got := post("runs/200/cancel"); got.status != http.StatusConflict {
		t.Fatalf("cancel of a completed run = %d, want 409", got.status)
	}
	if got := post("runs/200/rerun-failed-jobs"); got.status != http.StatusCreated {
		t.Fatalf("re-run of the canceled jobs = %d", got.status)
	}
	if jobs := ghRunJobIDs(t, e, 200); jobs["unit"][len(jobs["unit"])-6:] != "queued" {
		t.Fatalf("jobs after re-running the canceled ones = %v", jobs)
	}
	if got := httpRequest(t, srv, "POST", "github.com", "/github/rest/repos/acme/ci/actions/jobs/1002/rerun", `{"enable_debug_logging":true}`, nil); got.status != http.StatusNotFound || !lastInvocation(t, e).Unhandled {
		t.Fatalf("a parameter the fake does not implement = %+v", got)
	}

	// A reader is refused, and nothing moves.
	reader := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(reader, `{"repos":[{"forge":"github","project":"acme/ci","merge":{"permission":"READ","methods":["merge"]},"pipelines":[{"id":100,"jobs":[{"id":1001,"name":"unit","status":"failed"}]}]}]}`); err != nil {
		t.Fatal(err)
	}
	readerSrv := testServer(t, reader)
	if got := httpRequest(t, readerSrv, "POST", "github.com", "/github/rest/repos/acme/ci/actions/jobs/1001/rerun", "", nil); got.status != http.StatusForbidden {
		t.Fatalf("a reader's re-run = %d", got.status)
	}
	if jobs := ghRunJobIDs(t, reader, 100); jobs["unit"] != "1001 completed" {
		t.Fatalf("a refused re-run moved the job: %v", jobs)
	}
}

// A GitHub run's artifacts are listed with their sizes and expiry, and
// each served as its zip; an expired one is a 410.
func TestGitHubRunArtifacts(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, ciActionsFixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	var list struct {
		TotalCount int              `json:"total_count"`
		Artifacts  []map[string]any `json:"artifacts"`
	}
	decodeBody(t, httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/ci/actions/runs/100/artifacts?per_page=100", "", nil), &list)
	if list.TotalCount != 2 || list.Artifacts[0]["name"] != "coverage" || list.Artifacts[0]["size_in_bytes"] != float64(len("PK-coverage")) || list.Artifacts[1]["expired"] != true {
		t.Fatalf("artifacts = %+v", list)
	}
	if got := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/ci/actions/artifacts/51/zip", "", nil); got.status != 200 || got.body != "PK-coverage" {
		t.Fatalf("zip = %+v", got)
	}
	if got := httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/acme/ci/actions/artifacts/52/zip", "", nil); got.status != http.StatusGone {
		t.Fatalf("expired zip = %d", got.status)
	}
	if _, err := seedJSON(New(Options{APIToken: testAPIToken}), `{"repos":[{"forge":"github","project":"a/b","pipelines":[{"jobs":[{"name":"x","status":"manual"}]}]}]}`); err == nil {
		t.Fatal("a GitHub manual job was seeded")
	}
}

// glabNodes reads PipelineJobs' rows of pipeline id by name.
func glabNodes(t *testing.T, e *Engine, id int64) (map[string]map[string]any, map[string]any) {
	t.Helper()
	got := pipelineJobsRead(t, testServer(t, e), "gitlab.com", "grp/ci", id, nil)
	out := map[string]map[string]any{}
	for _, node := range got.nodes() {
		out[node["name"].(string)] = node
	}
	var raw struct {
		Data struct {
			Project struct {
				Pipeline map[string]any `json:"pipeline"`
			} `json:"project"`
		} `json:"data"`
	}
	decodeBody(t, pipelineJobsCall(t, testServer(t, e), "gitlab.com", "grp/ci", id, nil), &raw)
	pipeline := raw.Data.Project.Pipeline
	return out, pipeline
}

// GitLab's rows carry the action fields as GitLab answers them, and the
// actions move the pipeline: a retry adds the new pending job and drops
// the old one from the read, play starts a manual job, a cancel ends the
// running ones, and a job GitLab cannot retry or play is refused.
func TestGitLabCIActionsMoveThePipeline(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, ciActionsFixture); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	nodes, pipeline := glabNodes(t, e, 10)
	if unit := nodes["unit"]; unit["retryable"] != true || unit["cancelable"] != false || mustJSON(t, unit["userPermissions"]) != `{"cancelBuild":true,"readJobArtifacts":true,"updateBuild":true}` {
		t.Fatalf("failed build = %v", mustJSON(t, unit))
	}
	if build := nodes["build"]; mustJSON(t, build["artifacts"]) != `{"nodes":[{"fileType":"ARCHIVE"},{"fileType":"TRACE"}]}` {
		t.Fatalf("build artifacts = %v", mustJSON(t, build["artifacts"]))
	}
	if release := nodes["release"]; release["status"] != "MANUAL" || release["playable"] != true || release["canPlayJob"] != true || release["startedAt"] != nil {
		t.Fatalf("manual job = %v", mustJSON(t, release))
	}
	if pipeline["retryable"] != true || pipeline["cancelable"] != false {
		t.Fatalf("finished pipeline = %v", mustJSON(t, pipeline))
	}

	post := func(path string) httpAnswer {
		return httpRequest(t, srv, "POST", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fci/"+path, "", http.Header{"Private-Token": {testAPIToken}})
	}
	retried := post("jobs/1101/retry")
	var next map[string]any
	decodeBodyStatus(t, retried, http.StatusCreated, &next)
	if next["status"] != "pending" || next["name"] != "unit" || next["id"] == float64(1101) {
		t.Fatalf("retry answer = %v", next)
	}
	nodes, _ = glabNodes(t, e, 10)
	if unit := nodes["unit"]; unit["id"] == "gid://gitlab/Ci::Build/1101" || unit["status"] != "PENDING" || unit["cancelable"] != true {
		t.Fatalf("unit after the retry = %v", mustJSON(t, unit))
	}
	if got := post("jobs/1101/retry"); got.status != http.StatusForbidden {
		t.Fatalf("a retry of the replaced job = %d, want 403 not retryable", got.status)
	}
	if got := post("jobs/1103/play"); got.status != http.StatusOK {
		t.Fatalf("play = %+v", got)
	}
	if got := post("jobs/1103/play"); got.status != http.StatusBadRequest {
		t.Fatalf("play of a started job = %d, want 400 unplayable", got.status)
	}
	if got := post("pipelines/20/cancel"); got.status != http.StatusOK {
		t.Fatalf("pipeline cancel = %+v", got)
	}
	nodes, pipeline = glabNodes(t, e, 20)
	if nodes["unit"]["status"] != "CANCELED" || nodes["lint"]["status"] != "CANCELED" || pipeline["retryable"] != true {
		t.Fatalf("canceled pipeline = %v / %v", mustJSON(t, nodes), mustJSON(t, pipeline))
	}
	if got := post("pipelines/20/retry"); got.status != http.StatusCreated {
		t.Fatalf("pipeline retry = %+v", got)
	}
	if nodes, _ = glabNodes(t, e, 20); nodes["unit"]["status"] != "PENDING" || nodes["lint"]["status"] != "PENDING" {
		t.Fatalf("retried pipeline = %v", mustJSON(t, nodes))
	}
	if got := glabGET(t, srv, "gitlab.com", "projects/grp%2Fci/jobs/1102/artifacts"); got.status != 200 || got.body != string(mustDecode(t, "UEsDBA==")) {
		t.Fatalf("archive = %+v", got)
	}
	if got := glabGET(t, srv, "gitlab.com", "projects/grp%2Fci/jobs/1103/artifacts"); got.status != http.StatusNotFound {
		t.Fatalf("a job without an archive = %d", got.status)
	}

	// A reporter may do none of it.
	reporter := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(reporter, `{"repos":[{"forge":"gitlab","project":"grp/ci","merge":{"permission":"reporter"},"pipelines":[{"id":10,"jobs":[{"id":1101,"name":"unit","status":"failed"}]}]}]}`); err != nil {
		t.Fatal(err)
	}
	reporterSrv := testServer(t, reporter)
	if got := httpRequest(t, reporterSrv, "POST", "gitlab.com", "/gitlab/api/v4/projects/grp%2Fci/jobs/1101/retry", "", http.Header{"Private-Token": {testAPIToken}}); got.status != http.StatusForbidden {
		t.Fatalf("a reporter's retry = %d", got.status)
	}
	if nodes, _ := glabNodes(t, reporter, 10); nodes["unit"]["id"] != "gid://gitlab/Ci::Build/1101" || mustJSON(t, nodes["unit"]["userPermissions"]) != `{"cancelBuild":false,"readJobArtifacts":true,"updateBuild":false}` {
		t.Fatalf("a refused retry moved the job, or the permissions are wrong: %v", mustJSON(t, nodes["unit"]))
	}
}

// AdvanceJob names a job by name or id, refuses what the forge would not
// hold, and gives a GitLab build its archive.
func TestAdvanceJob(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, ciActionsFixture); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]JobAdvance{
		"no job":         {Forge: "gitlab", Project: "grp/ci", Status: "success"},
		"both":           {Forge: "gitlab", Project: "grp/ci", Job: "unit", ID: 1101, Status: "success"},
		"bad status":     {Forge: "gitlab", Project: "grp/ci", Job: "unit", Status: "done"},
		"unknown":        {Forge: "gitlab", Project: "grp/ci", Job: "nope", Status: "success"},
		"github manual":  {Forge: "github", Project: "acme/ci", Job: "lint", Status: "manual"},
		"github archive": {Forge: "github", Project: "acme/ci", Job: "lint", Status: "success", Artifacts: &Artifact{Text: "x"}},
	} {
		if _, err := e.AdvanceJob(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	job, err := e.AdvanceJob(JobAdvance{Forge: "gitlab", Project: "grp/ci", ID: 2101, Status: "success", Artifacts: &Artifact{Text: "PK"}})
	if err != nil || job.Status != "success" || job.CompletedAt == "" || job.Artifacts == nil {
		t.Fatalf("AdvanceJob = %+v, %v", job, err)
	}
	if got := glabGET(t, testServer(t, e), "gitlab.com", "projects/grp%2Fci/jobs/2101/artifacts"); got.body != "PK" {
		t.Fatalf("advanced archive = %+v", got)
	}
}

func decodeBodyStatus(t *testing.T, answer httpAnswer, status int, out any) {
	t.Helper()
	if answer.status != status {
		t.Fatalf("status %d, want %d: %s", answer.status, status, answer.body)
	}
	answer.status = 200
	decodeBody(t, answer, out)
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
