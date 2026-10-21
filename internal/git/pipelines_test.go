package git

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func listQuery(t *testing.T, path string) url.Values {
	t.Helper()
	_, raw, _ := strings.Cut(path, "?")
	query, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("query of %s: %v", path, err)
	}
	return query
}

// The recorded runs list parses into one row per run: a pull request's
// run with its number, a release's as a tag, a push's as a branch.
func TestGitHubListPipelinesParsesTheRuns(t *testing.T) {
	t.Parallel()
	answer := readTestdata(t, "github-actions-runs.json")
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if strings.HasPrefix(call.Path, "repos/acme/widgets/actions/runs?") {
			return forgeAPIAnswer{Body: answer}
		}
		return forgeUnexpected(t, call)
	})
	rows, err := core.ListPipelines(t.Context(), githubTestRepo(), PipelineListFilter{Ref: "feature", Status: CIStatusFailed, Limit: 30})
	if err != nil {
		t.Fatalf("ListPipelines: %v", err)
	}
	if q := listQuery(t, calls.all()[0].Path); q.Encode() != "branch=feature&per_page=30&status=failure" {
		t.Fatalf("runs query = %s", q.Encode())
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	want := PipelineSummary{
		ID: "9003", Status: CIStatusRunning, Name: "CI", Ref: "feature", RefKind: PipelineRefMergeRequest, MRNumber: 12,
		SHA: "aaaa000000000000000000000000000000000003", Title: "Fix the parser", Source: "pull_request",
		CreatedAt: "2026-10-20T10:00:00Z", UpdatedAt: "2026-10-20T10:05:00Z", URL: "https://github.com/acme/widgets/actions/runs/9003",
	}
	if rows[0] != want {
		t.Errorf("pull request run = %+v\nwant              %+v", rows[0], want)
	}
	if release := rows[1]; release.RefKind != PipelineRefTag || release.Ref != "v1.4.0" || release.Status != CIStatusSuccess || release.MRNumber != 0 {
		t.Errorf("release run = %+v", release)
	}
	if push := rows[2]; push.RefKind != PipelineRefBranch || push.Status != CIStatusFailed || push.Source != "push" || push.Title != "Merge pull request #10" {
		t.Errorf("push run = %+v", push)
	}
}

// The recorded pipelines list parses into rows: a merge request's
// pipeline with its iid, a tag's (which GitLab's list does not mark), a
// manual one. GitLab's list carries no workflow name or commit title.
func TestGitLabListPipelinesParsesThePipelines(t *testing.T) {
	t.Parallel()
	answer := readTestdata(t, "gitlab-pipelines.json")
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/pipelines?") {
			return forgeAPIAnswer{Body: answer}
		}
		return forgeUnexpected(t, call)
	})
	rows, err := core.ListPipelines(t.Context(), gitlabTestRepo(), PipelineListFilter{Ref: "refs/merge-requests/21/head", Status: CIStatusManual})
	if err != nil {
		t.Fatalf("ListPipelines: %v", err)
	}
	if q := listQuery(t, calls.all()[0].Path); q.Encode() != "per_page=100&ref=refs%2Fmerge-requests%2F21%2Fhead&status=manual" {
		t.Fatalf("pipelines query = %s", q.Encode())
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	want := PipelineSummary{
		ID: "7003", Status: CIStatusRunning, Ref: "refs/merge-requests/21/head", RefKind: PipelineRefMergeRequest, MRNumber: 21,
		SHA: "bbbb000000000000000000000000000000000003", Source: "merge_request_event",
		CreatedAt: "2026-10-20T10:00:00.120Z", UpdatedAt: "2026-10-20T10:01:00.000Z", URL: "https://gitlab.example/grp/sub/app/-/pipelines/7003",
	}
	if rows[0] != want {
		t.Errorf("merge request pipeline = %+v\nwant                     %+v", rows[0], want)
	}
	if tag := rows[1]; tag.RefKind != PipelineRefBranch || tag.Ref != "v2.0.0" || tag.Status != CIStatusSuccess {
		t.Errorf("tag pipeline = %+v", tag)
	}
	if manual := rows[2]; manual.Status != CIStatusManual || manual.Source != "schedule" || manual.Name != "" || manual.Title != "" {
		t.Errorf("manual pipeline = %+v", manual)
	}
}

// The filter is validated inside: a status the forge cannot filter by,
// a malformed ref and a negative limit send nothing; a limit past 100
// reads 100.
func TestListPipelinesValidatesTheFilter(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if strings.Contains(call.Path, "/actions/runs?") || strings.Contains(call.Path, "/pipelines?") {
			return forgeAPIAnswer{Body: `{"workflow_runs":[]}`}
		}
		return forgeUnexpected(t, call)
	})
	for name, tc := range map[string]struct {
		repo   RepoReference
		filter PipelineListFilter
	}{
		"github manual":   {githubTestRepo(), PipelineListFilter{Status: CIStatusManual}},
		"unknown status":  {gitlabTestRepo(), PipelineListFilter{Status: "queued"}},
		"negative limit":  {githubTestRepo(), PipelineListFilter{Limit: -1}},
		"padded ref":      {gitlabTestRepo(), PipelineListFilter{Ref: " main"}},
		"flag ref":        {gitlabTestRepo(), PipelineListFilter{Ref: "-main"}},
		"control ref":     {githubTestRepo(), PipelineListFilter{Ref: "ma\nin"}},
		"long ref":        {githubTestRepo(), PipelineListFilter{Ref: strings.Repeat("r", 256)}},
		"no host":         {RepoReference{Forge: "github", Namespace: "acme", Repo: "widgets"}, PipelineListFilter{}},
		"unknown forge":   {RepoReference{Forge: "gitea", Host: "x.example", Namespace: "a", Repo: "b"}, PipelineListFilter{}},
		"github subgroup": {RepoReference{Forge: "github", Host: testForgeHost, Namespace: "a/b", Repo: "c"}, PipelineListFilter{}},
	} {
		if _, err := core.ListPipelines(t.Context(), tc.repo, tc.filter); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := len(calls.all()); n != 0 {
		t.Fatalf("refused filters sent %v", calls.ops())
	}
	if _, err := core.ListPipelines(t.Context(), githubTestRepo(), PipelineListFilter{Limit: 500, Status: CIStatusPending}); err != nil {
		t.Fatalf("ListPipelines: %v", err)
	}
	if q := listQuery(t, calls.all()[0].Path); q.Get("per_page") != "100" || q.Get("status") != "queued" {
		t.Fatalf("capped query = %s", q.Encode())
	}
	if _, err := (nullForge{}).ListPipelines(t.Context(), githubTestRepo(), PipelineListFilter{}); !errors.Is(err, ErrUnsupportedForge) {
		t.Fatalf("nullForge.ListPipelines = %v", err)
	}
}

// ReadPipeline on GitHub reads the run and its jobs into one stage named
// after the workflow, with steps on the stepsFor jobs only, paging the
// jobs list.
func TestGitHubReadPipelineReadsTheRunAndItsJobs(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "repos/acme/widgets/actions/runs/9003":
			return forgeAPIAnswer{Body: `{"id":9003,"name":"CI","head_branch":"feature","head_sha":"abc123","status":"in_progress","conclusion":null,"html_url":"https://github.com/acme/widgets/actions/runs/9003"}`}
		case strings.HasPrefix(call.Path, "repos/acme/widgets/actions/runs/9003/jobs?"):
			q := listQuery(t, call.Path)
			if q.Get("page") == "" {
				jobs := make([]string, 100)
				for i := range jobs {
					jobs[i] = fmt.Sprintf(`{"id":%d,"name":"shard %d","status":"completed","conclusion":"success","started_at":"2026-10-20T10:00:00Z","completed_at":"2026-10-20T10:00:30Z","html_url":"https://github.com/acme/widgets/actions/runs/9003/job/%d","steps":[{"number":1,"name":"run","status":"completed","conclusion":"success"}]}`, 100+i, i, 100+i)
				}
				next := "<" + call.RESTBase + "repos/acme/widgets/actions/runs/9003/jobs?per_page=100&page=2>; rel=\"next\""
				return forgeAPIAnswer{Header: http.Header{"Link": {next}}, Body: `{"total_count":101,"jobs":[` + strings.Join(jobs, ",") + `]}`}
			}
			return forgeAPIAnswer{Body: `{"total_count":101,"jobs":[{"id":7,"name":"deploy","status":"queued","conclusion":null,"started_at":null,"completed_at":null,"html_url":"https://github.com/acme/widgets/actions/runs/9003/job/7","steps":[]}]}`}
		}
		return forgeUnexpected(t, call)
	})
	pipeline, err := core.ReadPipeline(t.Context(), githubTestRepo(), "9003", nil, []string{"100"})
	if err != nil {
		t.Fatalf("ReadPipeline: %v", err)
	}
	if pipeline.ID != "9003" || pipeline.Status != CIStatusRunning || pipeline.Ref != "feature" || pipeline.SHA != "abc123" ||
		pipeline.URL != "https://github.com/acme/widgets/actions/runs/9003" || len(pipeline.Stages) != 1 {
		t.Fatalf("pipeline = %+v", pipeline)
	}
	stage := pipeline.Stages[0]
	if stage.Name != "CI" || stage.Status != CIStatusPending || len(stage.Jobs) != 101 {
		t.Fatalf("stage = %s %s with %d jobs", stage.Name, stage.Status, len(stage.Jobs))
	}
	first, last := stage.Jobs[0], stage.Jobs[100]
	if first.ID != "100" || first.Status != CIStatusSuccess || first.DurationSeconds != 30 || !first.LogsAvailable || len(first.Steps) != 1 || first.URL == "" {
		t.Errorf("first job = %+v", first)
	}
	if stage.Jobs[1].Steps != nil {
		t.Errorf("a job outside stepsFor carries steps: %+v", stage.Jobs[1])
	}
	if last.ID != "7" || last.Status != CIStatusPending || last.LogsAvailable {
		t.Errorf("queued job = %+v", last)
	}
	if ops := calls.ops(); len(ops) != 3 {
		t.Fatalf("requests = %v, want the run and two jobs pages", ops)
	}
}

// ReadPipeline on GitLab reads the pipeline, then its jobs as ReadPR
// reads a head pipeline's.
func TestGitLabReadPipelineReadsThePipelineAndItsJobs(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "projects/grp%2Fsub%2Fapp/pipelines/7002":
			return forgeAPIAnswer{Body: `{"id":7002,"status":"success","ref":"v2.0.0","tag":true,"sha":"def456","source":"push","web_url":"https://gitlab.example/grp/sub/app/-/pipelines/7002"}`}
		case strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/pipelines/7002/jobs?"):
			return forgeAPIAnswer{Body: `[{"id":12,"name":"deploy","stage":"release","status":"success","started_at":"2026-10-19T10:01:00Z","duration":4.5},
				{"id":11,"name":"build","stage":"build","status":"success","started_at":"2026-10-19T10:00:00Z","duration":60}]`}
		}
		return forgeUnexpected(t, call)
	})
	pipeline, err := core.ReadPipeline(t.Context(), gitlabTestRepo(), "7002", nil, nil)
	if err != nil {
		t.Fatalf("ReadPipeline: %v", err)
	}
	if pipeline.ID != "7002" || pipeline.Status != CIStatusSuccess || pipeline.Ref != "v2.0.0" || pipeline.SHA != "def456" ||
		pipeline.URL != "https://gitlab.example/grp/sub/app/-/pipelines/7002" || len(pipeline.Stages) != 2 ||
		pipeline.Stages[0].Name != "build" || pipeline.Stages[1].Jobs[0].Name != "deploy" {
		t.Fatalf("pipeline = %+v", pipeline)
	}
	if ops := calls.ops(); len(ops) != 2 {
		t.Fatalf("requests = %v", ops)
	}
}

// A pipeline id is a decimal number before it reaches a path, and a
// GitLab answer for another pipeline is refused.
func TestReadPipelineValidatesTheID(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path == "projects/grp%2Fsub%2Fapp/pipelines/5" {
			return forgeAPIAnswer{Body: `{"id":6,"status":"success"}`}
		}
		return forgeUnexpected(t, call)
	})
	for _, id := range []string{"", "abc", "1/jobs", "-1", "1.5"} {
		if _, err := core.ReadPipeline(t.Context(), githubTestRepo(), id, nil, nil); err == nil {
			t.Errorf("ReadPipeline(%q) accepted it", id)
		}
	}
	if n := len(calls.all()); n != 0 {
		t.Fatalf("refused ids sent %v", calls.ops())
	}
	if _, err := core.ReadPipeline(t.Context(), gitlabTestRepo(), "5", nil, nil); err == nil || !strings.Contains(err.Error(), "answered pipeline 6") {
		t.Fatalf("mismatched answer = %v", err)
	}
	if _, err := (nullForge{}).ReadPipeline(t.Context(), githubTestRepo(), "1", nil, nil); !errors.Is(err, ErrUnsupportedForge) {
		t.Fatalf("nullForge.ReadPipeline = %v", err)
	}
}

// A PR's CI read names the ref and commit its pipeline ran on: GitLab's
// head pipeline with its id, GitHub's head checks with the head commit
// and no single id.
func TestPRCIReadNamesTheRefAndCommit(t *testing.T) {
	t.Parallel()
	gl, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "projects/grp%2Fsub%2Fapp/merge_requests/3":
			return forgeAPIAnswer{Body: `{"iid":3,"sha":"head2","head_pipeline":{"id":77,"status":"running","ref":"refs/merge-requests/3/head","sha":"head2","web_url":"https://gitlab.example/p/77"}}`}
		case strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/pipelines/77/jobs?"):
			return forgeAPIAnswer{Body: `[{"id":1,"name":"unit","stage":"test","status":"running","started_at":"2026-10-20T10:00:00Z"}]`}
		}
		return forgeUnexpected(t, call)
	})
	ci, err := gl.ListPRCIJobs(t.Context(), gitlabTestRef(3), nil, nil)
	if err != nil || ci.ID != "77" || ci.Ref != "refs/merge-requests/3/head" || ci.SHA != "head2" {
		t.Fatalf("GitLab PR CI = %+v, %v", ci, err)
	}
	gh, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op != "PRTick" {
			return forgeUnexpected(t, call)
		}
		return githubData(`{"repository":{"pullRequest":{"number":1,"headRefName":"feature","commits":{"nodes":[{"commit":{"id":"C_1","oid":"abc123","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[
			{"__typename":"CheckRun","databaseId":5,"name":"unit","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-10-20T10:00:00Z","completedAt":"2026-10-20T10:01:00Z","detailsUrl":"https://github.com/o/r/actions/runs/9/job/5","checkSuite":{"workflowRun":{"databaseId":9,"workflow":{"name":"CI"}}}}]}}}}]}}}}`)
	})
	ci, err = gh.ListPRCIJobs(t.Context(), githubTestRef(1), nil, nil)
	if err != nil || ci.ID != "" || ci.Ref != "feature" || ci.SHA != "abc123" || len(ci.Stages) != 1 {
		t.Fatalf("GitHub PR CI = %+v, %v", ci, err)
	}
	if query := calls.all()[0].Query; !strings.Contains(query, "number headRefName") || !strings.Contains(query, "commit { id oid statusCheckRollup") {
		t.Fatalf("PRTick does not select the head ref and commit: %s", query)
	}
}
