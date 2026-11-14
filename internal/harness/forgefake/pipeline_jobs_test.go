package forgefake

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pipelineJobsAnswer is a decoded PipelineJobs answer.
type pipelineJobsAnswer struct {
	Data struct {
		Project *struct {
			Pipeline *struct {
				Stages struct {
					Nodes []map[string]any `json:"nodes"`
				} `json:"stages"`
				Jobs struct {
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []map[string]any `json:"nodes"`
				} `json:"jobs"`
			} `json:"pipeline"`
		} `json:"project"`
	} `json:"data"`
}

func (a pipelineJobsAnswer) nodes() []map[string]any {
	if a.Data.Project == nil || a.Data.Project.Pipeline == nil {
		return nil
	}
	return a.Data.Project.Pipeline.Jobs.Nodes
}

// pipelineJobsCall sends PipelineJobs for pipeline id of fullPath on
// host, after the cursor when one is given.
func pipelineJobsCall(t *testing.T, srv *httptest.Server, host, fullPath string, id int64, after *string) httpAnswer {
	t.Helper()
	vars := map[string]any{"fullPath": fullPath, "id": fmt.Sprintf("gid://gitlab/Ci::Pipeline/%d", id)}
	if after != nil {
		vars["after"] = *after
	}
	return httpRequest(t, srv, "POST", host, "/gitlab/api/graphql", mustJSON(t, map[string]any{
		"query":         testDocument("query", "PipelineJobs", "($fullPath: ID!, $id: CiPipelineID!, $after: String)"),
		"operationName": "PipelineJobs", "variables": vars,
	}), http.Header{"Private-Token": {testAPIToken}})
}

func pipelineJobsRead(t *testing.T, srv *httptest.Server, host, fullPath string, id int64, after *string) pipelineJobsAnswer {
	t.Helper()
	var got pipelineJobsAnswer
	decodeBody(t, pipelineJobsCall(t, srv, host, fullPath, id, after), &got)
	return got
}

// PipelineJobs answers a pipeline's builds, bridges and external commit
// statuses newest first, with its stages in fixture order, spelled as
// gitlab.com spells them: a build's gid, job page and trace; a bridge
// with no webPath, its downstream pipeline once it has one; a commit
// status with a job webPath and its target URL as detailsPath. A bridge
// or status has no trace.
func TestGitLabPipelineJobsAnswerEveryRowKind(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"repos":[{"forge":"gitlab","project":"grp/rows","pipelines":[
	  {"id":50,"source":"external","jobs":[
	    {"id":501,"name":"CodeRabbit","kind":"external","status":"success"},
	    {"id":502,"name":"appveyor","kind":"external","status":"failed","targetUrl":"https://ci.appveyor.com/b/1"}]},
	  {"id":60,"jobs":[
	    {"id":601,"name":"compile","stage":"build","status":"success","log":"ok\n"},
	    {"id":602,"name":"child","stage":"test","kind":"bridge","status":"running","downstream":"/grp/child/-/pipelines/61"},
	    {"id":603,"name":"perf","stage":"qa","kind":"bridge","status":"pending","allowFailure":true}]}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)

	external := pipelineJobsRead(t, srv, "gitlab.com", "grp/rows", 50, nil)
	if got := mustJSON(t, external.Data.Project.Pipeline.Stages.Nodes); got != `[{"name":"external"}]` {
		t.Errorf("external stages = %s", got)
	}
	rows := external.nodes()
	if len(rows) != 2 || rows[0]["id"] != "gid://gitlab/GenericCommitStatus/502" || rows[1]["id"] != "gid://gitlab/GenericCommitStatus/501" {
		t.Fatalf("external rows = %s", mustJSON(t, rows))
	}
	if rows[0]["status"] != "FAILED" || mustJSON(t, rows[0]["detailedStatus"]) != `{"detailsPath":"https://ci.appveyor.com/b/1"}` ||
		rows[0]["webPath"] != "/grp/rows/-/jobs/502" || rows[0]["downstreamPipeline"] != nil {
		t.Errorf("status with a target = %s", mustJSON(t, rows[0]))
	}
	if mustJSON(t, rows[1]["detailedStatus"]) != `{"detailsPath":null}` || mustJSON(t, rows[1]["stage"]) != `{"name":"external"}` {
		t.Errorf("status without a target = %s", mustJSON(t, rows[1]))
	}

	mixed := pipelineJobsRead(t, srv, "gitlab.com", "grp/rows", 60, nil)
	if got := mustJSON(t, mixed.Data.Project.Pipeline.Stages.Nodes); got != `[{"name":"build"},{"name":"test"},{"name":"qa"}]` {
		t.Errorf("stages = %s", got)
	}
	rows = mixed.nodes()
	if len(rows) != 3 {
		t.Fatalf("rows = %s", mustJSON(t, rows))
	}
	perf, child, compile := rows[0], rows[1], rows[2]
	if perf["id"] != "gid://gitlab/Ci::Bridge/603" || perf["webPath"] != nil || perf["downstreamPipeline"] != nil || perf["allowFailure"] != true ||
		perf["startedAt"] != nil || perf["duration"] != nil {
		t.Errorf("bridge without a child = %s", mustJSON(t, perf))
	}
	if child["status"] != "RUNNING" || mustJSON(t, child["downstreamPipeline"]) != `{"path":"/grp/child/-/pipelines/61"}` ||
		mustJSON(t, child["detailedStatus"]) != `{"detailsPath":"/grp/child/-/pipelines/61"}` {
		t.Errorf("bridge with a child = %s", mustJSON(t, child))
	}
	if compile["id"] != "gid://gitlab/Ci::Build/601" || compile["webPath"] != "/grp/rows/-/jobs/601" || compile["duration"] != float64(60) ||
		compile["startedAt"] != "2026-01-01T00:00:00Z" {
		t.Errorf("build = %s", mustJSON(t, compile))
	}
	for id, want := range map[int]int{601: 200, 602: 404, 501: 404} {
		if got := glabGET(t, srv, "gitlab.com", fmt.Sprintf("projects/grp%%2Frows/jobs/%d/trace", id)); got.status != want {
			t.Errorf("trace of %d = %d, want %d", id, got.status, want)
		}
	}
}

// The rows page by 100 with the connection's cursor; a pipeline or
// project the host does not have is null, as GitLab answers; a pipeline
// id that is not a pipeline gid is unhandled.
func TestGitLabPipelineJobsPageAndMiss(t *testing.T) {
	t.Parallel()
	jobs := make([]string, 130)
	for i := range jobs {
		jobs[i] = fmt.Sprintf(`{"id":%d,"name":"j%d","status":"success"}`, 7000+i, i)
	}
	e := New(Options{APIToken: testAPIToken})
	if _, err := seedJSON(e, `{"repos":[{"forge":"gitlab","project":"grp/many","pipelines":[{"id":70,"jobs":[`+strings.Join(jobs, ",")+`]}]}]}`); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t, e)
	first := pipelineJobsRead(t, srv, "gitlab.com", "grp/many", 70, nil)
	page := first.Data.Project.Pipeline.Jobs.PageInfo
	if len(first.nodes()) != 100 || !page.HasNextPage || page.EndCursor == nil || first.nodes()[0]["id"] != "gid://gitlab/Ci::Build/7129" {
		t.Fatalf("first page = %d rows, %+v", len(first.nodes()), page)
	}
	second := pipelineJobsRead(t, srv, "gitlab.com", "grp/many", 70, page.EndCursor)
	if len(second.nodes()) != 30 || second.Data.Project.Pipeline.Jobs.PageInfo.HasNextPage || second.nodes()[29]["id"] != "gid://gitlab/Ci::Build/7000" {
		t.Fatalf("second page = %d rows", len(second.nodes()))
	}
	if inv := lastInvocation(t, e); inv.Route != "glab graphql PipelineJobs" || inv.Unhandled {
		t.Fatalf("recorded as %+v", inv)
	}
	if missing := pipelineJobsRead(t, srv, "gitlab.com", "grp/many", 71, nil); missing.Data.Project == nil || missing.Data.Project.Pipeline != nil {
		t.Errorf("an unknown pipeline = %+v, want a null pipeline", missing.Data)
	}
	if other := pipelineJobsRead(t, srv, "gitlab.example.com", "grp/many", 70, nil); other.Data.Project != nil {
		t.Errorf("on another host = %+v, want a null project", other.Data)
	}
	bad := httpRequest(t, srv, "POST", "gitlab.com", "/gitlab/api/graphql", mustJSON(t, map[string]any{
		"query":         testDocument("query", "PipelineJobs", "($fullPath: ID!, $id: CiPipelineID!, $after: String)"),
		"operationName": "PipelineJobs", "variables": map[string]any{"fullPath": "grp/many", "id": "70"},
	}), http.Header{"Private-Token": {testAPIToken}})
	if bad.status != 404 || !lastInvocation(t, e).Unhandled {
		t.Errorf("a bare id = %+v, want unhandled", bad)
	}
}

// A row's kind and the fields only one kind has are validated at seed.
func TestSeedRejectsMalformedRowKinds(t *testing.T) {
	t.Parallel()
	for name, job := range map[string]string{
		"unknown kind":          `{"name":"x","status":"success","kind":"trigger"}`,
		"bridge with a log":     `{"name":"x","status":"success","kind":"bridge","log":"x"}`,
		"external with steps":   `{"name":"x","status":"success","kind":"external","steps":[{"name":"s","status":"success"}]}`,
		"build with downstream": `{"name":"x","status":"success","downstream":"/a/-/pipelines/1"}`,
		"relative downstream":   `{"name":"x","status":"success","kind":"bridge","downstream":"a/-/pipelines/1"}`,
		"bridge with a target":  `{"name":"x","status":"success","kind":"bridge","targetUrl":"https://x.example/1"}`,
		"relative target":       `{"name":"x","status":"success","kind":"external","targetUrl":"/builds/1"}`,
	} {
		if _, err := seedJSON(New(Options{}), `{"repos":[{"forge":"gitlab","project":"a/b","pipelines":[{"jobs":[`+job+`]}]}]}`); err == nil {
			t.Errorf("%s: seeded", name)
		}
	}
	if _, err := seedJSON(New(Options{}), `{"repos":[{"forge":"github","project":"a/b","pipelines":[{"jobs":[{"name":"x","status":"success","kind":"external"}]}]}]}`); err == nil {
		t.Error("a GitHub run with an external row seeded")
	}
}
