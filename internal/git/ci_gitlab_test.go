package git

import (
	"fmt"
	"strings"
	"testing"
)

// gitlabJobNode is one PipelineJobs node: kind is the gid's type
// (Ci::Build, Ci::Bridge, GenericCommitStatus), extra the node's fields
// past its id, name, status and stage.
func gitlabJobNode(kind string, id int64, name, stage, status, extra string) string {
	node := fmt.Sprintf(`"id":"gid://gitlab/%s/%d","name":%q,"status":%q,"stage":{"name":%q},"allowFailure":false,"duration":null,"startedAt":null,"webPath":null,"detailedStatus":{"detailsPath":null},"downstreamPipeline":null`,
		kind, id, name, status, stage)
	if extra != "" {
		// A later key wins when the node decodes.
		node += "," + extra
	}
	return "{" + node + "}"
}

// gitlabJobsPage is a PipelineJobs answer: the pipeline's stages and one
// page of its jobs.
func gitlabJobsPage(stages []string, next string, nodes ...string) forgeAPIAnswer {
	stageNodes := make([]string, len(stages))
	for i, name := range stages {
		stageNodes[i] = fmt.Sprintf(`{"name":%q}`, name)
	}
	pageInfo := `{"hasNextPage":false,"endCursor":null}`
	if next != "" {
		pageInfo = fmt.Sprintf(`{"hasNextPage":true,"endCursor":%q}`, next)
	}
	return githubData(fmt.Sprintf(`{"project":{"pipeline":{"stages":{"nodes":[%s]},"jobs":{"pageInfo":%s,"nodes":[%s]}}}}`,
		strings.Join(stageNodes, ","), pageInfo, strings.Join(nodes, ",")))
}

// gitlabPipelineJobsCore serves pipeline 7002 of grp/sub/app (web URL on
// gitlab.example) and answers PipelineJobs with jobs.
func gitlabPipelineJobsCore(t *testing.T, jobs func(call forgeAPICall) forgeAPIAnswer) (*Core, *forgeAPICalls) {
	t.Helper()
	return newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == gitlabTestProject+"/pipelines/7002":
			return forgeAPIAnswer{Body: `{"id":7002,"status":"success","ref":"main","sha":"abc","source":"external","web_url":"https://gitlab.example/grp/sub/app/-/pipelines/7002"}`}
		case call.Op == "PipelineJobs":
			return jobs(call)
		}
		return forgeUnexpected(t, call)
	})
}

// A pipeline's rows are its builds, its bridges and its external
// statuses, read in one PipelineJobs request per 100 rows: a build keeps
// its numeric job id, its job page and its log once started; a bridge
// links to its downstream pipeline and an external status to its target
// URL, neither with an id or a log. Stages follow the pipeline's stage
// order, rows creation (ascending id) order within one.
func TestGitLabPipelineRows(t *testing.T) {
	t.Parallel()
	started := `"startedAt":"2026-11-11T01:36:24Z","duration":181`
	cases := []struct {
		name   string
		stages []string
		nodes  []string
		want   string
	}{
		{
			name:   "builds only",
			stages: []string{"build", "test"},
			nodes: []string{
				gitlabJobNode("Ci::Build", 12, "unit", "test", "FAILED", started+`,"allowFailure":true,"webPath":"/grp/sub/app/-/jobs/12"`),
				gitlabJobNode("Ci::Build", 11, "lint", "test", "SUCCESS", started+`,"webPath":"/grp/sub/app/-/jobs/11"`),
				gitlabJobNode("Ci::Build", 10, "compile", "build", "SUCCESS", started+`,"webPath":"/grp/sub/app/-/jobs/10"`),
				gitlabJobNode("Ci::Build", 13, "deploy", "test", "MANUAL", `"webPath":"/grp/sub/app/-/jobs/13"`),
			},
			want: `build success: 10 compile success 181s https://gitlab.example/grp/sub/app/-/jobs/10 log
test failed: 11 lint success 181s https://gitlab.example/grp/sub/app/-/jobs/11 log | 12 unit failed 181s https://gitlab.example/grp/sub/app/-/jobs/12 log allow-failure | 13 deploy manual 0s https://gitlab.example/grp/sub/app/-/jobs/13`,
		},
		{
			name:   "external only",
			stages: []string{"external"},
			nodes: []string{
				// GitLab gives a commit status a job webPath that is no page.
				gitlabJobNode("GenericCommitStatus", 17401881211, "CodeRabbit", "external", "SUCCESS", `"webPath":"/grp/sub/app/-/jobs/17401881211"`),
				gitlabJobNode("GenericCommitStatus", 17401881212, "appveyor", "external", "FAILED", `"detailedStatus":{"detailsPath":"https://ci.appveyor.com/project/a/b/builds/1"}`),
			},
			want: `external failed: - CodeRabbit success 0s - | - appveyor failed 0s https://ci.appveyor.com/project/a/b/builds/1`,
		},
		{
			name:   "bridge",
			stages: []string{"test", "qa"},
			nodes: []string{
				gitlabJobNode("Ci::Bridge", 21, "e2e:perf", "qa", "MANUAL", `"allowFailure":true`),
				gitlabJobNode("Ci::Bridge", 20, "rspec:trigger", "test", "RUNNING", started+`,"downstreamPipeline":{"path":"/grp/other/-/pipelines/9"}`),
			},
			want: `test running: - rspec:trigger running 181s https://gitlab.example/grp/other/-/pipelines/9
qa manual: - e2e:perf manual 0s - allow-failure`,
		},
		{
			name:   "mixed",
			stages: []string{"build", "test", "external"},
			nodes: []string{
				gitlabJobNode("GenericCommitStatus", 40, "CodeRabbit", "external", "SUCCESS", ""),
				gitlabJobNode("Ci::Bridge", 31, "child", "test", "SUCCESS", `"downstreamPipeline":{"path":"/grp/sub/app/-/pipelines/7003"}`),
				gitlabJobNode("Ci::Build", 30, "unit", "test", "RUNNING", started+`,"webPath":"/grp/sub/app/-/jobs/30"`),
				gitlabJobNode("Ci::Build", 20, "compile", "build", "SUCCESS", started+`,"webPath":"/grp/sub/app/-/jobs/20"`),
				// A stage the pipeline's stage list lacks comes last.
				gitlabJobNode("Ci::Build", 15, "late", "post", "PENDING", ""),
			},
			want: `build success: 20 compile success 181s https://gitlab.example/grp/sub/app/-/jobs/20 log
test running: 30 unit running 181s https://gitlab.example/grp/sub/app/-/jobs/30 log | - child success 0s https://gitlab.example/grp/sub/app/-/pipelines/7003
external success: - CodeRabbit success 0s -
post pending: 15 late pending 0s -`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := gitlabPipelineJobsCore(t, func(forgeAPICall) forgeAPIAnswer {
				return gitlabJobsPage(tc.stages, "", tc.nodes...)
			})
			pipeline, err := core.ReadPipeline(t.Context(), gitlabTestRepo(), "7002", nil, nil)
			if err != nil {
				t.Fatalf("ReadPipeline: %v", err)
			}
			if got := describeStages(pipeline.Stages); got != tc.want {
				t.Fatalf("stages =\n%s\nwant\n%s", got, tc.want)
			}
			sent := calls.all()
			if len(sent) != 2 || sent[1].Op != "PipelineJobs" {
				t.Fatalf("requests = %v, want the pipeline and one PipelineJobs", calls.ops())
			}
			if v := sent[1].Vars; v["fullPath"] != "grp/sub/app" || v["id"] != "gid://gitlab/Ci::Pipeline/7002" || v["after"] != nil {
				t.Fatalf("PipelineJobs variables = %v", v)
			}
			if !strings.Contains(sent[1].Query, "retried: false") {
				t.Fatalf("PipelineJobs reads retried jobs: %s", sent[1].Query)
			}
		})
	}
}

// describeStages renders stages one per line as "name status: rows",
// each row "id name status duration url [log] [allow-failure]", "-" for
// an empty id or URL.
func describeStages(stages []CIStage) string {
	lines := make([]string, 0, len(stages))
	for _, stage := range stages {
		rows := make([]string, 0, len(stage.Jobs))
		for _, job := range stage.Jobs {
			row := fmt.Sprintf("%s %s %s %gs %s", dash(job.ID), job.Name, job.Status, job.DurationSeconds, dash(job.URL))
			if job.LogsAvailable {
				row += " log"
			}
			if job.AllowFailure {
				row += " allow-failure"
			}
			rows = append(rows, row)
		}
		lines = append(lines, stage.Name+" "+stage.Status+": "+strings.Join(rows, " | "))
	}
	return strings.Join(lines, "\n")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// The rows are paged by the connection's cursor until a page names no
// next one, at most gitlabCIJobsMaxPages pages.
func TestGitLabPipelineRowsPageToTheCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		lastPage  int
		wantPages int
	}{
		{"a short pipeline ends at its last page", 2, 2},
		{"a long pipeline stops at the cap", 100, gitlabCIJobsMaxPages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, calls := gitlabPipelineJobsCore(t, func(call forgeAPICall) forgeAPIAnswer {
				page := 1
				if after, ok := call.Vars["after"].(string); ok {
					if _, err := fmt.Sscanf(after, "page%d", &page); err != nil {
						t.Errorf("after = %q", after)
					}
				}
				nodes := make([]string, gitlabPageSize)
				for i := range nodes {
					id := int64(page*1000 + i)
					nodes[i] = gitlabJobNode("Ci::Build", id, fmt.Sprintf("job%d", id), "test", "SUCCESS", "")
				}
				next := ""
				if page < tc.lastPage {
					next = fmt.Sprintf("page%d", page+1)
				}
				return gitlabJobsPage([]string{"test"}, next, nodes...)
			})
			pipeline, err := core.ReadPipeline(t.Context(), gitlabTestRepo(), "7002", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(pipeline.Stages[0].Jobs); n != tc.wantPages*gitlabPageSize {
				t.Fatalf("rows = %d, want %d", n, tc.wantPages*gitlabPageSize)
			}
			if n := len(calls.all()) - 1; n != tc.wantPages {
				t.Fatalf("PipelineJobs pages = %d, want %d", n, tc.wantPages)
			}
		})
	}
}

// An answer the rows cannot come from fails the read: a malformed or
// unknown job id, a pipeline GitLab does not answer, GraphQL errors.
func TestGitLabPipelineRowsRefuseAnUnreadableAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer forgeAPIAnswer
		want   string
	}{
		{"a build id that is not a number", gitlabJobsPage([]string{"test"}, "", `{"id":"gid://gitlab/Ci::Build/abc","name":"x","status":"SUCCESS","stage":{"name":"test"}}`), "gid://gitlab/Ci::Build/abc"},
		{"an id that is not a gid", gitlabJobsPage([]string{"test"}, "", `{"id":"12","name":"x","status":"SUCCESS","stage":{"name":"test"}}`), `"12"`},
		{"an unknown kind", gitlabJobsPage([]string{"test"}, "", gitlabJobNode("Ci::Processable", 5, "x", "test", "SUCCESS", "")), "Ci::Processable"},
		{"no pipeline", githubData(`{"project":{"pipeline":null}}`), "no pipeline 7002"},
		{"no project", githubData(`{"project":null}`), "no pipeline 7002"},
		{"a body that is not the shape", githubData(`{"project":{"pipeline":{"jobs":{"nodes":{}}}}}`), "decode"},
		{"GraphQL errors", forgeAPIAnswer{Body: `{"data":null,"errors":[{"message":"Internal server error"}]}`}, "Internal server error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			core, _ := gitlabPipelineJobsCore(t, func(forgeAPICall) forgeAPIAnswer { return tc.answer })
			_, err := core.ReadPipeline(t.Context(), gitlabTestRepo(), "7002", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadPipeline = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}
