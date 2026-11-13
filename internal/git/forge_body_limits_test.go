package git

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// pad is a JSON string of n filler bytes, the bulk of a measured record
// (an embedded repository, a description, a note body).
func pad(n int) string {
	return `"` + strings.Repeat("x", n) + `"`
}

// pageOf joins n records record builds into a JSON array.
func pageOf(n int, record func(i int) string) string {
	records := make([]string, n)
	for i := range records {
		records[i] = record(i)
	}
	return "[" + strings.Join(records, ",") + "]"
}

// overDefault fails unless body is past forgeapi's 1 MiB default, so
// each case below proves its read needs its own bound.
func overDefault(t *testing.T, body string) string {
	t.Helper()
	if len(body) <= 1<<20 {
		t.Fatalf("page is %d bytes, not past the 1 MiB default", len(body))
	}
	return body
}

// A list read whose measured page passes the 1 MiB default reads a full
// page of records at the measured size: 100 GitHub runs of 14.5 KB,
// 100 jobs of 80 steps, 100 GitLab merge requests of 20 KB in each list
// that reads the full view, 100 discussions of 12 KB and 100 commented-on
// events of 12 KB.
func TestForgeListReadsTakeAPageOverTheDefaultBodyBound(t *testing.T) {
	t.Parallel()
	gitlabMR := func(i int) string {
		return fmt.Sprintf(`{"iid":%d,"project_id":1,"source_project_id":1,"state":"merged","merged_at":"2026-10-01T00:00:00Z","source_branch":"b%d","sha":"%040d","web_url":"u%d","title":"t","description":%s}`,
			i+1, i, i, i, pad(20_000))
	}

	t.Run("GitHub runs list", func(t *testing.T) {
		t.Parallel()
		page := overDefault(t, `{"total_count":100,"workflow_runs":`+pageOf(100, func(i int) string {
			return fmt.Sprintf(`{"id":%d,"name":"CI","head_branch":"main","event":"push","status":"completed","conclusion":"success","repository":{"description":%s}}`, 9000+i, pad(14_500))
		})+`}`)
		core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			if strings.HasPrefix(call.Path, "repos/acme/widgets/actions/runs?") {
				return forgeAPIAnswer{Body: page}
			}
			return forgeUnexpected(t, call)
		})
		rows, err := core.ListPipelines(t.Context(), githubTestRepo(), PipelineListFilter{Limit: 100})
		if err != nil || len(rows) != 100 {
			t.Fatalf("ListPipelines = %d rows, %v", len(rows), err)
		}
	})

	t.Run("GitHub run jobs list", func(t *testing.T) {
		t.Parallel()
		step := func(i int) string {
			return fmt.Sprintf(`{"number":%d,"name":"step %d of the job, named as a workflow names it","status":"completed","conclusion":"success","started_at":"2026-10-20T10:00:00Z","completed_at":"2026-10-20T10:00:30Z"}`, i+1, i)
		}
		page := overDefault(t, `{"total_count":100,"jobs":`+pageOf(100, func(i int) string {
			return fmt.Sprintf(`{"id":%d,"name":"shard %d","status":"completed","conclusion":"success","started_at":"2026-10-20T10:00:00Z","completed_at":"2026-10-20T10:01:00Z","steps":%s}`, 100+i, i, pageOf(80, step))
		})+`}`)
		core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			switch {
			case call.Path == "repos/acme/widgets/actions/runs/9003":
				return forgeAPIAnswer{Body: `{"id":9003,"name":"CI","status":"completed","conclusion":"success"}`}
			case strings.HasPrefix(call.Path, "repos/acme/widgets/actions/runs/9003/jobs?"):
				return forgeAPIAnswer{Body: page}
			}
			return forgeUnexpected(t, call)
		})
		pipeline, err := core.ReadPipeline(t.Context(), githubTestRepo(), "9003", nil, []string{"100"})
		if err != nil || len(pipeline.Stages) != 1 || len(pipeline.Stages[0].Jobs) != 100 || len(pipeline.Stages[0].Jobs[0].Steps) != 80 {
			t.Fatalf("ReadPipeline = %+v, %v", pipeline.Stages, err)
		}
	})

	t.Run("GitLab merged and open merge request lists", func(t *testing.T) {
		t.Parallel()
		page := overDefault(t, pageOf(100, gitlabMR))
		core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			if strings.HasPrefix(call.Path, gitlabTestProject+"/merge_requests?") {
				return forgeAPIAnswer{Body: page}
			}
			return forgeUnexpected(t, call)
		})
		cwd := t.TempDir()
		if forge := core.recordOrigin(cwd, originIdentity{url: "git@gitlab.com:grp/sub/app.git", known: true}, core.nowFn()); forge != "gitlab" {
			t.Fatalf("origin classified as %q", forge)
		}
		heads, err := core.ListMergedPRHeads(t.Context(), cwd, 100)
		if err != nil || len(heads) != 100 {
			t.Fatalf("ListMergedPRHeads = %d heads, %v", len(heads), err)
		}
		pulls, err := core.ListOpenPRs(t.Context(), cwd, "b1")
		if err != nil || len(pulls) != 100 {
			t.Fatalf("ListOpenPRs = %d pulls, %v", len(pulls), err)
		}
		client, err := core.gitlabAPI(testForgeHost)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := readGitLabMRList(t.Context(), client, gitlabTestRef(1).Repository())
		if err != nil || len(listed) != 100 {
			t.Fatalf("readGitLabMRList = %d, %v", len(listed), err)
		}
		tracked := make([]TrackedPR, 100)
		for i := range tracked {
			tracked[i] = TrackedPR{Ref: gitlabTestRef(i + 1)}
		}
		states, err := core.ReadPRStates(t.Context(), tracked)
		if err != nil || len(states) != 100 {
			t.Fatalf("ReadPRStates = %d, %v", len(states), err)
		}
	})

	t.Run("GitLab discussions", func(t *testing.T) {
		t.Parallel()
		mr := gitlabTestProject + "/merge_requests/7"
		page := overDefault(t, pageOf(100, func(i int) string {
			return fmt.Sprintf(`{"id":"d%d","notes":[{"id":%d,"body":%s,"system":false,"created_at":"t","author":{"username":"rev"}}]}`, i, i+1, pad(12_000))
		}))
		core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			switch {
			case call.Path == mr:
				return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
			case strings.HasPrefix(call.Path, mr+"/discussions?"):
				return forgeAPIAnswer{Body: page}
			case call.Path == gitlabTestProject:
				return forgeAPIAnswer{Body: gitlabTestProjectSettings}
			case call.Path == "user":
				return forgeAPIAnswer{Body: `{"id":1,"username":"viewer"}`}
			}
			return forgeUnexpected(t, call)
		})
		threads, err := core.ListReviewThreads(t.Context(), gitlabTestRef(7))
		if err != nil || len(threads) != 100 {
			t.Fatalf("ListReviewThreads = %d, %v", len(threads), err)
		}
	})

	t.Run("GitLab viewer events", func(t *testing.T) {
		t.Parallel()
		page := overDefault(t, pageOf(100, func(i int) string {
			return fmt.Sprintf(`{"project_id":1,"action_name":"commented on","created_at":"2026-11-0%dT00:00:00Z","note":{"noteable_type":"MergeRequest","noteable_iid":%d,"body":%s}}`, 1+i%9, i+1, pad(12_000))
		}))
		core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			if strings.HasPrefix(call.Path, "users/1/events?") {
				return forgeAPIAnswer{Body: page}
			}
			return forgeUnexpected(t, call)
		})
		client, err := core.gitlabAPI(testForgeHost)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
		at, _, err := readGitLabEvents(t.Context(), client, gitlabViewer{Username: "viewer", ID: 1}, now, now.AddDate(0, 0, -30), "commented")
		if err != nil || len(at) != 100 {
			t.Fatalf("readGitLabEvents = %d, %v", len(at), err)
		}
	})
}
