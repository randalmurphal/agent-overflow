package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
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
// pipeline with its iid, a push's, a manual one. GitLab's list carries no
// workflow name or commit title. A merge request ref names no tag, so a
// list of one reads no tag scope.
func TestGitLabListPipelinesParsesThePipelines(t *testing.T) {
	t.Parallel()
	answer := readTestdata(t, "gitlab-pipelines.json")
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/pipelines?") && !strings.Contains(call.Path, "scope=") {
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
	if push := rows[1]; push.RefKind != PipelineRefBranch || push.Ref != "v2.0.0" || push.Status != CIStatusSuccess {
		t.Errorf("push pipeline = %+v", push)
	}
	if manual := rows[2]; manual.Status != CIStatusManual || manual.Source != "schedule" || manual.Name != "" || manual.Title != "" {
		t.Errorf("manual pipeline = %+v", manual)
	}
	if len(calls.all()) != 1 {
		t.Fatalf("calls = %d, want the list alone for a merge request ref", len(calls.all()))
	}
}

// gitlabListPipeline is a pipelines list entry.
func gitlabListPipeline(id int64, ref, status string) map[string]any {
	return map[string]any{
		"id": id, "sha": fmt.Sprintf("%040d", id), "ref": ref, "status": status, "source": "push",
		"created_at": "2026-10-17T10:00:00.000Z", "updated_at": "2026-10-17T10:30:00.000Z",
		"web_url": fmt.Sprintf("https://gitlab.example/grp/sub/app/-/pipelines/%d", id),
	}
}

func gitlabListBody(t *testing.T, pipelines ...map[string]any) string {
	t.Helper()
	body, err := json.Marshal(pipelines)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// gitlabPipelinesFake answers the pipelines list, its tag scope and the
// single pipeline reads from fields a test changes between listings.
type gitlabPipelinesFake struct {
	mu    sync.Mutex
	list  []map[string]any
	scope []map[string]any
	// single is each pipeline's tag flag; a missing id answers 404 and
	// one in refused answers 403.
	single  map[int64]bool
	refused map[int64]bool
}

func (f *gitlabPipelinesFake) answer(t *testing.T) func(call forgeAPICall) forgeAPIAnswer {
	return func(call forgeAPICall) forgeAPIAnswer {
		f.mu.Lock()
		defer f.mu.Unlock()
		const prefix = "projects/grp%2Fsub%2Fapp/pipelines"
		switch {
		case strings.HasPrefix(call.Path, prefix+"?") && strings.Contains(call.Path, "scope=tags"):
			return forgeAPIAnswer{Body: gitlabListBody(t, f.scope...)}
		case strings.HasPrefix(call.Path, prefix+"?"):
			return forgeAPIAnswer{Body: gitlabListBody(t, f.list...)}
		case strings.HasPrefix(call.Path, prefix+"/"):
			id, err := strconv.ParseInt(strings.TrimPrefix(call.Path, prefix+"/"), 10, 64)
			if err != nil {
				break
			}
			if f.refused[id] {
				return forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"403 Forbidden"}`}
			}
			tag, ok := f.single[id]
			if !ok {
				return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Not found"}`}
			}
			return forgeAPIAnswer{Body: fmt.Sprintf(`{"id":%d,"ref":"x","status":"success","tag":%v}`, id, tag)}
		}
		return forgeUnexpected(t, call)
	}
}

func (f *gitlabPipelinesFake) set(fn func(*gitlabPipelinesFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// singleReads is the single pipeline reads among calls[from:].
func singleReads(calls []forgeAPICall, from int) []string {
	var out []string
	for _, call := range calls[from:] {
		if id, ok := strings.CutPrefix(call.Path, "projects/grp%2Fsub%2Fapp/pipelines/"); ok {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func pipelineKinds(rows []PipelineSummary) string {
	var got []string
	for _, row := range rows {
		got = append(got, row.ID+":"+row.Ref+":"+row.RefKind)
	}
	return strings.Join(got, " ")
}

// A pipeline in GitLab's tag scope (each tag's latest tag pipeline) is a
// tag's, one on a ref the scope does not name a branch's, and one on a
// ref the scope names but not in it (an older run of the tag, or a
// branch of the same name) is classified by its single pipeline's tag
// flag, read once per pipeline while it stays listed. The scope is read
// without the status filter.
func TestGitLabListPipelinesClassifiesTagsByTheScopeAndTheSinglePipeline(t *testing.T) {
	t.Parallel()
	fake := &gitlabPipelinesFake{
		list: []map[string]any{
			gitlabListPipeline(7002, "release", "success"),
			gitlabListPipeline(6995, "release", "success"), // the branch named release
			gitlabListPipeline(6990, "v2", "success"),      // an older run of tag v2
			gitlabListPipeline(6985, "v2", "success"),      // deleted since the list
			gitlabListPipeline(6980, "main", "success"),
		},
		scope:  []map[string]any{gitlabListPipeline(7005, "v2", "failed"), gitlabListPipeline(7002, "release", "success")},
		single: map[int64]bool{6995: false, 6990: true},
	}
	core, calls := newForgeAPICore(t, fake.answer(t))
	list := func() []PipelineSummary {
		t.Helper()
		rows, err := core.ListPipelines(t.Context(), gitlabTestRepo(), PipelineListFilter{Status: CIStatusSuccess})
		if err != nil {
			t.Fatalf("ListPipelines: %v", err)
		}
		return rows
	}

	rows := list()
	if got := calls.all(); listQuery(t, got[1].Path).Encode() != "per_page=100&scope=tags" {
		t.Fatalf("calls = %+v, want the list then the tag scope without the status filter", got)
	}
	if got := singleReads(calls.all(), 0); strings.Join(got, " ") != "6985 6990 6995" {
		t.Fatalf("single reads = %v, want the ambiguous pipelines only", got)
	}
	if got, want := pipelineKinds(rows), "7002:release:tag 6995:release:branch 6990:v2:tag 6980:main:branch"; got != want {
		t.Fatalf("rows = %s\nwant   %s", got, want)
	}

	// Listed again, the remembered pipelines cost no read.
	fake.set(func(f *gitlabPipelinesFake) { f.list = slices.Delete(f.list, 3, 4) })
	before := len(calls.all())
	rows = list()
	if got := singleReads(calls.all(), before); len(got) != 0 || len(calls.all()) != before+2 {
		t.Fatalf("second listing read %v, want only the list and the scope", got)
	}
	if got, want := pipelineKinds(rows), "7002:release:tag 6995:release:branch 6990:v2:tag 6980:main:branch"; got != want {
		t.Fatalf("second rows = %s\nwant          %s", got, want)
	}

	// A newer run of tag release leaves 7002 out of the scope; it was
	// remembered from the scope and costs no read.
	fake.set(func(f *gitlabPipelinesFake) {
		f.list = append([]map[string]any{gitlabListPipeline(7010, "release", "success")}, f.list...)
		f.scope = []map[string]any{gitlabListPipeline(7010, "release", "success"), gitlabListPipeline(7005, "v2", "failed")}
	})
	before = len(calls.all())
	rows = list()
	if got := singleReads(calls.all(), before); len(got) != 0 {
		t.Fatalf("third listing read %v, want none", got)
	}
	if got, want := pipelineKinds(rows), "7010:release:tag 7002:release:tag 6995:release:branch 6990:v2:tag 6980:main:branch"; got != want {
		t.Fatalf("third rows = %s\nwant         %s", got, want)
	}

	// A refused single read fails the listing.
	fake.set(func(f *gitlabPipelinesFake) {
		f.list = append(f.list, gitlabListPipeline(6970, "v2", "success"))
		f.refused = map[int64]bool{6970: true}
	})
	if _, err := core.ListPipelines(t.Context(), gitlabTestRepo(), PipelineListFilter{Status: CIStatusSuccess}); err == nil {
		t.Fatal("a refused single pipeline read did not fail the listing")
	}
}

// The listed and appended rows are one list of at most Limit, newest
// first: a tag's latest pipeline newer than a listed one (created between
// the reads) takes the oldest listed row's place, an older one is past
// the limit, and the status filter applies to the appended rows.
func TestGitLabListPipelinesAnswersAtMostTheLimit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status string
		list   []map[string]any
		want   string
	}{
		"status filter": {
			status: CIStatusSuccess,
			list:   []map[string]any{gitlabListPipeline(500, "a", "success"), gitlabListPipeline(490, "b", "success"), gitlabListPipeline(480, "c", "success")},
			want:   "510:t1:tag 500:a:branch 490:b:branch",
		},
		"no filter": {
			list: []map[string]any{gitlabListPipeline(500, "a", "success"), gitlabListPipeline(490, "b", "failed"), gitlabListPipeline(480, "c", "success")},
			want: "510:t1:tag 505:t2:tag 500:a:branch",
		},
	} {
		fake := &gitlabPipelinesFake{
			list:  tc.list,
			scope: []map[string]any{gitlabListPipeline(510, "t1", "success"), gitlabListPipeline(505, "t2", "failed"), gitlabListPipeline(400, "t3", "success")},
		}
		core, calls := newForgeAPICore(t, fake.answer(t))
		rows, err := core.ListPipelines(t.Context(), gitlabTestRepo(), PipelineListFilter{Status: tc.status, Limit: 3})
		if err != nil {
			t.Fatalf("%s: ListPipelines: %v", name, err)
		}
		if got := pipelineKinds(rows); got != tc.want {
			t.Errorf("%s: rows = %s\nwant   %s", name, got, tc.want)
		}
		if n := len(calls.all()); n != 2 {
			t.Errorf("%s: %d calls, want the list and the scope", name, n)
		}
	}
}

// A list of one ref reads that ref's tag scope once, without the status
// filter. A ref the scope does not name is a branch with no further read;
// one it names has every listed pipeline outside the scope read once.
func TestGitLabListPipelinesClassifiesARefWithOneTagRead(t *testing.T) {
	t.Parallel()
	for ref, wantKind := range map[string]string{"v2.0.0": PipelineRefTag, "main": PipelineRefBranch} {
		fake := &gitlabPipelinesFake{
			list:   []map[string]any{gitlabListPipeline(6995, ref, "failed"), gitlabListPipeline(6990, ref, "failed")},
			single: map[int64]bool{6995: true, 6990: true},
		}
		if ref == "v2.0.0" {
			fake.scope = []map[string]any{gitlabListPipeline(7002, ref, "success")}
		}
		core, calls := newForgeAPICore(t, fake.answer(t))
		rows, err := core.ListPipelines(t.Context(), gitlabTestRepo(), PipelineListFilter{Ref: ref, Status: CIStatusFailed})
		if err != nil {
			t.Fatalf("%s: ListPipelines: %v", ref, err)
		}
		got := calls.all()
		if listQuery(t, got[1].Path).Encode() != "per_page=100&ref="+ref+"&scope=tags" {
			t.Fatalf("%s: calls = %+v, want the list then the ref's tag scope", ref, got)
		}
		wantReads := map[string]string{"v2.0.0": "6990 6995", "main": ""}[ref]
		if reads := singleReads(got, 0); strings.Join(reads, " ") != wantReads {
			t.Fatalf("%s: single reads = %v, want [%s]", ref, reads, wantReads)
		}
		if len(rows) != 2 || rows[0].RefKind != wantKind || rows[1].RefKind != wantKind {
			t.Fatalf("%s: rows = %+v, want two %s rows and the success not appended", ref, rows, wantKind)
		}
	}
}

// The tag scope is read page by page until a page reaches past the oldest
// listed pipeline: the latest pipeline of a listed pipeline's tag is no
// older than it.
func TestGitLabListPipelinesPagesTheTagScopeToTheOldestListedPipeline(t *testing.T) {
	t.Parallel()
	page := func(first int64, extra map[string]any) string {
		var pipelines []map[string]any
		for id := first; len(pipelines) < gitlabPageSize; id-- {
			if extra != nil && id == extra["id"].(int64) {
				pipelines = append(pipelines, extra)
				continue
			}
			pipelines = append(pipelines, gitlabListPipeline(id, fmt.Sprintf("t%d", id), "failed"))
		}
		return gitlabListBody(t, pipelines...)
	}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if !strings.HasPrefix(call.Path, "projects/grp%2Fsub%2Fapp/pipelines?") {
			return forgeUnexpected(t, call)
		}
		query := listQuery(t, call.Path)
		if query.Get("scope") != "tags" {
			return forgeAPIAnswer{Body: gitlabListBody(t, gitlabListPipeline(5000, "main", "success"), gitlabListPipeline(100, "old", "success"))}
		}
		next := http.Header{"X-Next-Page": {"0"}}
		switch query.Get("page") {
		case "":
			next.Set("X-Next-Page", "2")
			return forgeAPIAnswer{Header: next, Body: page(9000, nil)}
		case "2":
			next.Set("X-Next-Page", "3")
			return forgeAPIAnswer{Header: next, Body: page(400, nil)}
		case "3":
			// Full, but past the oldest listed id: the last page read.
			next.Set("X-Next-Page", "4")
			return forgeAPIAnswer{Header: next, Body: page(150, gitlabListPipeline(100, "old", "success"))}
		}
		return forgeUnexpected(t, call)
	})
	rows, err := core.ListPipelines(t.Context(), gitlabTestRepo(), PipelineListFilter{Status: CIStatusSuccess, Limit: 2})
	if err != nil {
		t.Fatalf("ListPipelines: %v", err)
	}
	if got := len(calls.all()); got != 4 {
		t.Fatalf("calls = %d, want the list and three tag scope pages", got)
	}
	if len(rows) != 2 || rows[0].RefKind != PipelineRefBranch || rows[1].ID != "100" || rows[1].RefKind != PipelineRefTag {
		t.Fatalf("rows = %+v", rows)
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
