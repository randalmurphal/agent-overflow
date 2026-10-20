package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/testutil/providerpath"
)

// fakeBin is ao-mockforge, built once per run and executed by a real
// git.Core in place of gh and glab. The same Core sends its forge API
// requests (every GitHub and GitLab read) to the engine's HTTP mounts. These tests
// hold the fake's answers to the app's own parsers: a shape the fake gets
// wrong fails here, not as a blank review pane in a browser spec.
var fakeBin string

// rigAPIToken is the fixed token the rig's forge API transport presents.
const rigAPIToken = "rig-token"

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "ao-mockforge-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mktemp: %v\n", err)
		os.Exit(1)
	}
	fakeBin = filepath.Join(tmp, "ao-mockforge")
	if out, err := exec.Command("go", "build", "-o", fakeBin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build ao-mockforge: %v\n%s", err, out)
		os.Exit(1)
	}
	code := providerpath.Run(m)
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// rig is one harness-side fake forge and an isolated Core that reaches
// it: its CLIs through ao-mockforge and the control channel, its forge
// API transport through the engine's HTTP listener.
type rig struct {
	engine *forgefake.Engine
	core   *gitops.Core
}

func newRig(t *testing.T, fixture forgefake.Fixture) *rig {
	t.Helper()
	engine := forgefake.New(forgefake.Options{APIToken: rigAPIToken})
	if _, err := engine.Seed(fixture); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv, err := control.NewServer(control.ServerConfig{
		Resolve: func(control.Registration) (control.Assignment, error) { return control.Assignment{}, nil },
		Forge:   engine.Handle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	env := []string{control.EnvAddr + "=" + srv.Addr(), control.EnvToken + "=" + srv.Token()}
	api := httptest.NewServer(engine)
	t.Cleanup(api.Close)
	svc, err := forgeapi.New(forgeapi.Options{Version: "test", Isolated: &forgeapi.Isolated{BaseURL: api.URL, Token: rigAPIToken}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return &rig{engine: engine, core: gitops.NewCore(gitops.WithIsolatedForgeCLIs(fakeBin, env), gitops.WithForgeAPI(svc))}
}

func intPtr(n int) *int { return &n }

// pngBytes is a 1x1 PNG.
var pngBytes, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

const diff = "diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1,3 +1,4 @@\n package app\n-var x = 1\n+var x = 2\n+var y = 3\n"

func manyComments(n int, prefix string) []forgefake.Comment {
	out := make([]forgefake.Comment, n)
	for i := range out {
		out[i] = forgefake.Comment{Author: "reviewer", Body: fmt.Sprintf("%s %d", prefix, i)}
	}
	return out
}

func manyThreads(n int) []forgefake.Thread {
	out := make([]forgefake.Thread, n)
	for i := range out {
		out[i] = forgefake.Thread{Path: "app.go", Line: intPtr(2), Comments: []forgefake.Comment{{Body: fmt.Sprintf("thread %d", i)}}}
	}
	return out
}

func githubFixture() forgefake.Fixture {
	threads := append([]forgefake.Thread{
		{Path: "app.go", Line: intPtr(3), StartLine: intPtr(2), Resolved: true, Comments: []forgefake.Comment{
			{Author: "bob", AuthorName: "Bob Smith", Body: "range"}, {Author: "alice", Body: "reply"},
		}},
		{Path: "app.go", Side: "file", Comments: []forgefake.Comment{{Author: "bob", Body: "file level"}}},
		{Path: "app.go", Line: intPtr(1), Side: "left", Outdated: true, Comments: []forgefake.Comment{{Author: "bob", Body: "old"}}},
	}, manyThreads(110)...)
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "github", Project: "acme/widgets",
		Attachments: []forgefake.Attachment{{URL: "https://github.com/user-attachments/assets/1a2b", ContentType: "image/png", Base64: base64.StdEncoding.EncodeToString(pngBytes)}},
		Pulls: []forgefake.Pull{
			{
				Number: 7, Title: "Add widgets", Body: "![shot](https://github.com/user-attachments/assets/1a2b)",
				Author: "alice", AuthorName: "Alice Ng", HeadRef: "feat/widgets", HeadSHA: strings.Repeat("a", 40), Diff: diff, Draft: true,
				Mergeable: "conflicts",
				Comments:  manyComments(130, "conversation"),
				Threads:   threads,
				Reviews: []forgefake.Review{
					{Author: "bob", State: "COMMENTED"}, {Author: "bob", State: "CHANGES_REQUESTED", Body: "fix"},
				},
				CI: &forgefake.Pipeline{ID: 555, Name: "Build", Jobs: []forgefake.Job{
					{ID: 901, Name: "unit", Status: "success", Log: "all green", Steps: []forgefake.Step{{Name: "checkout", Status: "success"}}},
					{ID: 902, Name: "lint", Status: "failed", Log: "lint failed"},
					{ID: 903, Name: "deploy", Status: "pending"},
					{ID: 904, Name: "package", Status: "success", Log: "packaged", LogWithheld: true},
				}},
			},
			{Number: 5, Title: "Old", State: "merged", HeadRef: "old-branch", HeadSHA: strings.Repeat("b", 40)},
			{
				Number: 6, Title: "Busy", HeadRef: "busy",
				Threads: []forgefake.Thread{{Path: "app.go", Line: intPtr(2), Comments: manyComments(130, "reply")}},
				CI:      &forgefake.Pipeline{ID: 556, Name: "Matrix", Jobs: manyJobs(120)},
			},
		},
	}}}
}

func manyJobs(n int) []forgefake.Job {
	out := make([]forgefake.Job, n)
	for i := range out {
		out[i] = forgefake.Job{Name: fmt.Sprintf("job %d", i), Status: "success"}
	}
	return out
}

func gitlabFixture() forgefake.Fixture {
	return forgefake.Fixture{Repos: []forgefake.Repo{{
		Forge: "gitlab", Project: "grp/sub/tool", ID: 4242,
		Attachments: []forgefake.Attachment{{Secret: "0123456789abcdef0123456789abcdef", Filename: "diagram one.svg", ContentType: "image/svg+xml", Text: "<svg xmlns=\"http://www.w3.org/2000/svg\"/>"}},
		Pulls: []forgefake.Pull{{
			Number: 3, Title: "Fix tool", Body: "![d](/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg)",
			Author: "dave", AuthorName: "Dave Jones", HeadRef: "fix", HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("d", 40), Diff: diff,
			Comments: manyComments(40, "note"),
			Threads: append([]forgefake.Thread{
				{Path: "app.go", Line: intPtr(3), StartLine: intPtr(2), Resolved: true, Comments: []forgefake.Comment{{Body: "range"}, {Body: "reply"}}},
				{Path: "app.go", Line: intPtr(1), Side: "left", Outdated: true, Comments: []forgefake.Comment{{Body: "old side"}}},
			}, manyThreads(20)...),
			Reviews: []forgefake.Review{{Author: "erin", AuthorName: "Erin Lee", State: "APPROVED"}},
			CI: &forgefake.Pipeline{ID: 777, Jobs: []forgefake.Job{
				{ID: 11, Name: "build", Stage: "build", Status: "success", Log: "built"},
				{ID: 12, Name: "test", Stage: "test", Status: "running", StartedAt: "2026-01-01T00:00:00Z", Log: gitlabSectionTrace},
				{ID: 13, Name: "package", Stage: "build", Status: "success", Log: "packaged", LogWithheld: true},
			}},
		}, {Number: 2, Title: "Merged", State: "merged", HeadRef: "done", HeadSHA: strings.Repeat("e", 40)}},
	}}}
}

func TestGitHubReadsParseThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, githubFixture())
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 7}

	detail, err := r.core.GetPRDetail(t.Context(), ref)
	if err != nil {
		t.Fatalf("GetPRDetail: %v", err)
	}
	if detail.Title != "Add widgets" || detail.URL != "https://github.com/acme/widgets/pull/7" || detail.HeadRefName != "feat/widgets" ||
		!detail.ViewerIsAuthor || !detail.Draft || detail.State != "open" || detail.HeadSHA != strings.Repeat("a", 40) ||
		detail.Additions != 2 || detail.Deletions != 1 || detail.ChangedFiles != 1 || detail.Mergeability != gitops.MergeabilityConflicts ||
		detail.ReviewDecision != "CHANGES_REQUESTED" || len(detail.LatestReviews) != 1 || detail.LatestReviews[0].Body != "fix" ||
		detail.Checks.Total != 4 || detail.Checks.Success != 2 || detail.Checks.Failure != 1 || detail.Checks.Pending != 1 {
		t.Fatalf("GetPRDetail = %+v", detail)
	}
	// A reviewer without a display name answers as a bot.
	if detail.AuthorLogin != "alice" || detail.AuthorName != "Alice Ng" || detail.LatestReviews[0].AuthorName != "" {
		t.Fatalf("GetPRDetail authors = %q/%q, review %+v", detail.AuthorLogin, detail.AuthorName, detail.LatestReviews[0])
	}

	threads, err := r.core.ListReviewThreads(t.Context(), ref)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	// 113 review threads over two GraphQL pages, then 130 conversation
	// comments over two more.
	if len(threads) != 113+130 {
		t.Fatalf("threads = %d, want %d", len(threads), 113+130)
	}
	ranged, file, outdated := threads[0], threads[1], threads[2]
	if !ranged.IsResolved || *ranged.Line != 3 || *ranged.StartLine != 2 || ranged.Side != "right" ||
		len(ranged.Comments) != 2 || ranged.Comments[1].ReplyTo == nil || ranged.Comments[1].ReplyTo.DatabaseID != ranged.Comments[0].DatabaseID {
		t.Fatalf("range thread = %+v", ranged)
	}
	if root, reply := ranged.Comments[0], ranged.Comments[1]; root.AuthorLogin != "bob" || root.AuthorName != "Bob Smith" ||
		reply.AuthorLogin != "alice" || reply.AuthorName != "" {
		t.Fatalf("range thread authors = %+v", ranged.Comments)
	}
	if file.Side != "file" || file.Line != nil {
		t.Fatalf("file thread = %+v", file)
	}
	if !outdated.IsOutdated || outdated.Side != "left" || outdated.Line != nil || outdated.OriginalLine == nil || *outdated.OriginalLine != 1 {
		t.Fatalf("outdated thread = %+v", outdated)
	}
	if ranged.OriginalLine == nil || *ranged.OriginalLine != 3 || ranged.OriginalStartLine == nil || *ranged.OriginalStartLine != 2 {
		t.Fatalf("range thread original lines = %v %v", ranged.OriginalLine, ranged.OriginalStartLine)
	}
	if last := threads[len(threads)-1]; last.Path != "" || last.Comments[0].Body != "conversation 129" {
		t.Fatalf("last conversation comment = %+v", last)
	}

	// Steps are read for the followed job only, through the REST jobs list.
	pipeline, err := r.core.ListPRCIJobs(t.Context(), ref, nil, []string{"901"})
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	if len(pipeline.Stages) != 1 || pipeline.Stages[0].Name != "Build" || len(pipeline.Stages[0].Jobs) != 4 ||
		pipeline.Stages[0].Jobs[0].ID != "901" || len(pipeline.Stages[0].Jobs[0].Steps) != 1 || !pipeline.Stages[0].Jobs[0].LogsAvailable ||
		pipeline.Stages[0].Jobs[0].Steps[0].Name != "checkout" || pipeline.Stages[0].Jobs[0].Steps[0].Status != gitops.CIStatusSuccess ||
		pipeline.Stages[0].Jobs[1].Steps != nil || pipeline.Stages[0].Jobs[2].LogsAvailable {
		t.Fatalf("ListPRCIJobs = %+v", pipeline)
	}
	log, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "902"})
	if err != nil || log.text != "\xef\xbb\xbflint failed" || log.etag == "" || log.status != http.StatusOK {
		t.Fatalf("StreamCIJobLog = %+v, %v", log, err)
	}
	if again, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "902", ETag: log.etag}); err != nil || !again.notModified {
		t.Fatalf("revalidated StreamCIJobLog = %+v, %v", again, err)
	}
	// A resume reads the rest of the blob from its offset.
	if rest, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "902", From: 8}); err != nil || rest.status != http.StatusPartialContent || rest.offset != 8 || rest.text != "failed" || rest.etag != log.etag {
		t.Fatalf("resumed StreamCIJobLog = %+v, %v", rest, err)
	}
	if _, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "903"}); err == nil {
		t.Fatal("a queued job served a log")
	}
	if _, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "904"}); !errors.Is(err, gitops.ErrCIJobLogNotFound) {
		t.Fatalf("withheld log error = %v, want ErrCIJobLogNotFound", err)
	}

	// A thread and a rollup past one page are read through their node
	// page operations.
	busy := ref
	busy.Number = 6
	read, err := r.core.ReadPR(t.Context(), busy, gitops.PRReadParts{Detail: true, Threads: true, CI: true}, nil, nil)
	if err != nil {
		t.Fatalf("ReadPR(#6): %v", err)
	}
	if len(read.Threads) != 1 || len(read.Threads[0].Comments) != 130 || read.Threads[0].Comments[129].Body != "reply 129" ||
		read.Detail.Checks.Total != 120 || len(read.CI.Stages) != 1 || len(read.CI.Stages[0].Jobs) != 120 {
		t.Fatalf("ReadPR(#6) = %d threads, checks %+v, stages %d", len(read.Threads), read.Detail.Checks, len(read.CI.Stages))
	}
	ops := map[string]int{}
	for _, inv := range r.engine.Invocations(0).Invocations {
		ops[inv.Operation]++
	}
	for _, op := range []string{"PRTick", "PRThreadsPage", "PRCommentsPage", "ThreadCommentsPage", "RollupContextsPage"} {
		if ops[op] == 0 {
			t.Errorf("no %s request in %v", op, ops)
		}
	}

	data, name, err := r.core.FetchAttachment(t.Context(), ref, "https://github.com/user-attachments/assets/1a2b", 1<<20)
	if err != nil || !bytes.Equal(data, pngBytes) || name != "1a2b" {
		t.Fatalf("FetchAttachment = %d bytes %q, %v", len(data), name, err)
	}
	if _, _, err := r.core.FetchAttachment(t.Context(), ref, "https://github.com/user-attachments/assets/ffff", 1<<20); err == nil {
		t.Fatal("an unseeded attachment downloaded")
	}

	var unhandled []forgefake.Invocation
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			unhandled = append(unhandled, inv)
		}
	}
	if len(unhandled) != 0 {
		t.Fatalf("the app made invocations the fake does not implement: %+v", unhandled)
	}
}

// gitlabSectionTrace is a GitLab 17 timestamped trace with the section
// protocol as the runner writes it: an end and the next start share a
// line, each start carries its header after the erasing \r.
const gitlabSectionTrace = "2026-01-01T00:00:00.100000Z 00O \x1b[0Ksection_start:1767225600:prepare_executor\r\x1b[0K\x1b[0K\x1b[36;1mPreparing the \"docker\" executor\x1b[0;m\n" +
	"2026-01-01T00:00:01.200000Z 00O Using docker image alpine\n" +
	"2026-01-01T00:00:02.300000Z 00O \x1b[0Ksection_end:1767225602:prepare_executor\r\x1b[0K\x1b[0Ksection_start:1767225602:step_script[collapsed=true]\r\x1b[0K\x1b[0K\x1b[36;1mExecuting \"step_script\"\x1b[0;m\n" +
	"2026-01-01T00:00:03.400000Z 00O $ make test\n"

// streamedLog is one job log answer as the CI log cache receives it.
type streamedLog struct {
	text, etag  string
	status      int
	offset      int64
	notModified bool
}

// streamLog reads a job log through the core the way the CI log cache
// does.
func streamLog(t *testing.T, core *gitops.Core, ref gitops.PRReference, req gitops.CIJobLogRequest) (streamedLog, error) {
	t.Helper()
	var out streamedLog
	var body strings.Builder
	notModified, err := core.StreamCIJobLog(t.Context(), ref, req, func(st gitops.CIJobLogStart) (io.Writer, error) {
		out.status, out.etag, out.offset = st.Status, st.ETag, st.Offset
		return &body, nil
	})
	out.text, out.notModified = body.String(), notModified
	return out, err
}

// TestGitHubRunningJobLogThroughTheAppsForgeCode: the app's GitHub CI code
// against the fake's Actions endpoints while the jobs API reports a job
// running. The log answers 404 until its blob exists, then the log,
// revalidated by its ETag and re-read when it moves; steps carry GitHub's
// numbers and times.
func TestGitHubRunningJobLogThroughTheAppsForgeCode(t *testing.T) {
	steps := []forgefake.Step{
		{Name: "Set up job", Status: "success", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:01Z"},
		{Number: 3, Name: "Run make test", Status: "running", StartedAt: "2026-01-01T00:00:01Z"},
		{Name: "Post Run make test", Status: "pending"},
	}
	fixture := forgefake.Fixture{Repos: []forgefake.Repo{{
		Forge: "github", Project: "acme/live",
		Pulls: []forgefake.Pull{{Number: 1, Title: "Live", HeadRef: "live", CI: &forgefake.Pipeline{ID: 600, Name: "CI", Jobs: []forgefake.Job{
			{ID: 601, Name: "test", Status: "running", Log: "2026-01-01T00:00:00.1000000Z Current runner version: '2.337.0'\n", LogWithheld: true, Steps: steps},
		}}}},
	}}}
	r := newRig(t, fixture)
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "live", Number: 1}
	reseed := func(mutate func(job *forgefake.Job)) {
		t.Helper()
		mutate(&fixture.Repos[0].Pulls[0].CI.Jobs[0])
		if _, err := r.engine.Seed(fixture); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
	}

	if r.core.CILogStreams(ref) {
		t.Fatal("GitHub reports that it streams running logs")
	}
	if _, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "601"}); !errors.Is(err, gitops.ErrCIJobLogNotFound) {
		t.Fatalf("running log before its blob exists: %v, want ErrCIJobLogNotFound", err)
	}
	reseed(func(job *forgefake.Job) { job.LogWithheld = false })
	// The blob starts with the byte order mark GitHub writes; the CI log
	// cache drops it.
	first, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "601"})
	if err != nil || first.text != "\xef\xbb\xbf2026-01-01T00:00:00.1000000Z Current runner version: '2.337.0'\n" || first.etag == "" {
		t.Fatalf("running log = %+v, %v", first, err)
	}
	if same, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "601", ETag: first.etag}); err != nil || !same.notModified {
		t.Fatalf("unchanged running log = %+v, %v; want NotModified", same, err)
	}
	reseed(func(job *forgefake.Job) { job.Log += "2026-01-01T00:00:01.2000000Z ##[group]Run make test\n" })
	grown, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "601", ETag: first.etag})
	if err != nil || grown.notModified || grown.etag == first.etag || !strings.HasSuffix(grown.text, "##[group]Run make test\n") {
		t.Fatalf("grown running log = %+v, %v", grown, err)
	}

	pipeline, err := r.core.ListPRCIJobs(t.Context(), ref, nil, []string{"601"})
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	want := []gitops.CIStep{
		{Number: 1, Name: "Set up job", Status: gitops.CIStatusSuccess, StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:01Z"},
		{Number: 3, Name: "Run make test", Status: gitops.CIStatusRunning, StartedAt: "2026-01-01T00:00:01Z"},
		{Number: 4, Name: "Post Run make test", Status: gitops.CIStatusPending},
	}
	if job := gitops.FindCIJob(pipeline, "601"); job == nil || !job.LogsAvailable || !slices.Equal(job.Steps, want) {
		t.Fatalf("job = %+v, want steps %+v", job, want)
	}
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Fatalf("the app made an invocation the fake does not implement: %+v", inv)
		}
	}
}

func TestGitLabReadsParseThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, gitlabFixture())
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp/sub", Repo: "tool", Number: 3}

	detail, err := r.core.GetPRDetail(t.Context(), ref)
	if err != nil {
		t.Fatalf("GetPRDetail: %v", err)
	}
	if detail.Title != "Fix tool" || detail.AuthorLogin != "dave" || detail.State != "open" || detail.ChangedFiles != 1 ||
		detail.URL != "https://gitlab.com/grp/sub/tool/-/merge_requests/3" || detail.ReviewDecision != "APPROVED" ||
		len(detail.LatestReviews) != 1 || detail.LatestReviews[0].AuthorLogin != "erin" || detail.DiffRefs == nil ||
		detail.DiffRefs.BaseSHA != strings.Repeat("d", 40) || detail.Checks.Total != 1 || detail.Checks.Pending != 1 ||
		detail.AuthorName != "Dave Jones" || detail.LatestReviews[0].AuthorName != "Erin Lee" {
		t.Fatalf("GetPRDetail = %+v", detail)
	}

	threads, err := r.core.ListReviewThreads(t.Context(), ref)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	// 22 diff threads and 40 notes in one page of 100.
	if len(threads) != 62 {
		t.Fatalf("threads = %d, want 62", len(threads))
	}
	ranged, outdated := threads[0], threads[1]
	if !ranged.IsResolved || !ranged.IsResolvable || *ranged.Line != 3 || *ranged.StartLine != 2 || ranged.Side != "right" || len(ranged.Comments) != 2 ||
		ranged.Comments[0].AuthorLogin != "dave" || ranged.Comments[0].AuthorName != "Dave Jones" {
		t.Fatalf("range thread = %+v", ranged)
	}
	if !outdated.IsOutdated || outdated.Side != "left" || *outdated.Line != 1 || outdated.OriginalLine != nil {
		t.Fatalf("outdated thread = %+v", outdated)
	}
	if last := threads[61]; last.Path != "" || last.IsResolvable || last.Comments[0].Body != "note 39" {
		t.Fatalf("last note = %+v", last)
	}

	pipeline, err := r.core.ListPRCIJobs(t.Context(), ref, nil, nil)
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	if pipeline.URL != "https://gitlab.com/grp/sub/tool/-/pipelines/777" || len(pipeline.Stages) != 2 ||
		pipeline.Stages[0].Name != "build" || pipeline.Stages[0].Jobs[0].DurationSeconds != 60 || pipeline.Stages[1].Jobs[0].ID != "12" {
		t.Fatalf("ListPRCIJobs = %+v", pipeline)
	}
	log, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "11"})
	if err != nil || log.text != "built" || log.etag == "" {
		t.Fatalf("StreamCIJobLog = %+v, %v", log, err)
	}
	if again, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "11", ETag: log.etag}); err != nil || !again.notModified {
		t.Fatalf("revalidated StreamCIJobLog = %+v, %v", again, err)
	}
	// A completed trace answers a resume from its offset.
	if rest, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "11", From: 2}); err != nil || rest.status != http.StatusPartialContent || rest.offset != 2 || rest.text != "ilt" {
		t.Fatalf("resumed trace = %+v, %v", rest, err)
	}
	// The merge request read revalidates its ETag: a second read is a 304
	// the transport answers from its store.
	if again, err := r.core.GetPRDetail(t.Context(), ref); err != nil || again.Title != detail.Title {
		t.Fatalf("second GetPRDetail = %+v, %v", again, err)
	}
	if !slices.ContainsFunc(r.engine.Invocations(0).Invocations, func(inv forgefake.Invocation) bool {
		return inv.Route == "glab api merge request" && inv.Status == http.StatusNotModified
	}) {
		t.Fatal("the second merge request read was not a 304")
	}
	if _, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "13"}); !errors.Is(err, gitops.ErrCIJobLogNotFound) {
		t.Fatalf("withheld trace error = %v, want ErrCIJobLogNotFound", err)
	}
	// A running trace arrives as the runner wrote it, section markers
	// and all, for the CI log cache to clean and index; it ignores a
	// resume's Range and answers whole.
	if live, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "12"}); err != nil || live.text != gitlabSectionTrace {
		t.Fatalf("running trace = %q, %v; want %q", live.text, err, gitlabSectionTrace)
	}
	if live, err := streamLog(t, r.core, ref, gitops.CIJobLogRequest{JobID: "12", From: 10}); err != nil || live.status != http.StatusOK || live.text != gitlabSectionTrace {
		t.Fatalf("resumed running trace = %d %q, %v; want the whole trace", live.status, live.text, err)
	}

	// One pump tick reads the merge request once and derives every part
	// from it: approvals for the detail, discussions for the threads and
	// the head pipeline's jobs for CI.
	before := len(r.engine.Invocations(0).Invocations)
	read, err := r.core.ReadPR(t.Context(), ref, gitops.PRReadParts{Detail: true, Threads: true, CI: true}, nil, nil)
	if err != nil {
		t.Fatalf("ReadPR: %v", err)
	}
	if read.Detail.Title != "Fix tool" || len(read.Threads) != 62 || !read.HasCI || len(read.CI.Stages) != 2 {
		t.Fatalf("ReadPR = %+v", read)
	}
	routes := map[string]int{}
	for _, inv := range r.engine.Invocations(0).Invocations[before:] {
		if inv.Via != forgefake.ViaHTTP || inv.Forge != "gitlab" {
			t.Fatalf("ReadPR made a non-HTTP call: %+v", inv)
		}
		routes[inv.Route]++
	}
	if want := map[string]int{"glab api merge request": 1, "glab api approvals": 1, "glab api discussions": 1, "glab api pipeline jobs": 1}; !maps.Equal(routes, want) {
		t.Fatalf("ReadPR requests = %v, want %v", routes, want)
	}

	svg := "<svg xmlns=\"http://www.w3.org/2000/svg\"/>"
	for _, href := range []string{
		"/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg",
		"https://gitlab.com/grp/sub/tool/-/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg",
		"/-/project/4242/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg",
	} {
		data, name, err := r.core.FetchAttachment(t.Context(), ref, href, 1<<20)
		if err != nil || string(data) != svg || name != "diagram one.svg" {
			t.Fatalf("FetchAttachment(%s) = %q %q, %v", href, data, name, err)
		}
	}

	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Fatalf("unimplemented invocation: %+v", inv)
		}
	}
}

// TestPRReadsAddressTheReferencesHost: a PR on GitHub Enterprise or
// self-hosted GitLab is read from its own host, whatever the default. The
// reference keeps the URL's port, and a forge API request names it as
// given.
// The same reference on the public host is a not-found the app surfaces,
// never an answer from the wrong forge.
// The fake's rate limits read as the forge's through the app's transport:
// an exhausted pool is a *forgeapi.RateLimitedError until its reset, and
// a pool under the reserve refuses background reads but serves the
// user's own.
func TestRateLimitsReadThroughTheAppsTransport(t *testing.T) {
	for _, tc := range []struct {
		forge, pool string
		fixture     forgefake.Fixture
		ref         gitops.PRReference
	}{
		{"github", "graphql", githubFixture(), gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 7}},
		{"gitlab", "throttle_authenticated_api", gitlabFixture(), gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp/sub", Repo: "tool", Number: 3}},
	} {
		t.Run(tc.forge, func(t *testing.T) {
			r := newRig(t, tc.fixture)
			reset := time.Now().Add(time.Hour).Truncate(time.Second)
			if err := r.engine.SetRateLimit(forgefake.RateLimit{Forge: tc.forge, Pool: tc.pool, Remaining: 100, Reset: reset.Unix()}); err != nil {
				t.Fatal(err)
			}
			// The first answer tells the transport the pool is low (GitLab's
			// detail is two requests, so its second is already refused);
			// the next background read is refused under the reserve, the
			// user's own goes through.
			var limited *forgeapi.RateLimitedError
			if _, err := r.core.GetPRDetail(t.Context(), tc.ref); err != nil && (!errors.As(err, &limited) || !limited.Reserve) {
				t.Fatalf("first read: %v", err)
			}
			_, err := r.core.GetPRDetail(t.Context(), tc.ref)
			if !errors.As(err, &limited) || !limited.Reserve || limited.Until.Unix() < reset.Unix()-1 {
				t.Fatalf("background read under the reserve = %v", err)
			}
			if _, err := r.core.GetPRDetail(forgeapi.WithInteractive(t.Context()), tc.ref); err != nil {
				t.Fatalf("interactive read under the reserve: %v", err)
			}

			if err := r.engine.SetRateLimit(forgefake.RateLimit{Forge: tc.forge, Pool: tc.pool, Remaining: 0, Reset: reset.Unix()}); err != nil {
				t.Fatal(err)
			}
			_, err = r.core.GetPRDetail(forgeapi.WithInteractive(t.Context()), tc.ref)
			if !errors.As(err, &limited) || limited.Reserve || limited.Until.Unix() < reset.Unix()-1 || limited.Until.Unix() > reset.Unix()+1 {
				t.Fatalf("read of an exhausted pool = %v", err)
			}
		})
	}
}

func TestPRReadsAddressTheReferencesHost(t *testing.T) {
	github, gitlab := githubFixture(), gitlabFixture()
	github.Repos[0].Host = "ghe.example:8443"
	gitlab.Repos[0].Host = "gitlab.example.com:8443"
	r := newRig(t, forgefake.Fixture{Repos: append(github.Repos, gitlab.Repos...)})
	refs := []gitops.PRReference{
		{Forge: "github", Host: "ghe.example:8443", Namespace: "acme", Repo: "widgets", Number: 7},
		{Forge: "gitlab", Host: "gitlab.example.com:8443", Namespace: "grp/sub", Repo: "tool", Number: 3},
	}
	for _, ref := range refs {
		if detail, err := r.core.GetPRDetail(t.Context(), ref); err != nil || detail.Number != ref.Number {
			t.Fatalf("%s GetPRDetail = %+v, %v", ref.Host, detail, err)
		}
		if threads, err := r.core.ListReviewThreads(t.Context(), ref); err != nil || len(threads) == 0 {
			t.Fatalf("%s ListReviewThreads = %d threads, %v", ref.Host, len(threads), err)
		}
		if pipeline, err := r.core.ListPRCIJobs(t.Context(), ref, nil, nil); err != nil || len(pipeline.Stages) == 0 {
			t.Fatalf("%s ListPRCIJobs = %+v, %v", ref.Host, pipeline, err)
		}
	}
	if _, _, err := r.core.FetchAttachment(t.Context(), refs[1], "/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg", 1<<20); err != nil {
		t.Fatalf("self-hosted GitLab FetchAttachment: %v", err)
	}
	reads := r.engine.Invocations(0).Invocations
	if len(reads) == 0 {
		t.Fatal("no invocations recorded")
	}
	hosts := map[string]string{"github": "ghe.example:8443", "gitlab": "gitlab.example.com:8443"}
	for _, inv := range reads {
		if inv.Via != forgefake.ViaHTTP || inv.Host != hosts[inv.Forge] {
			t.Fatalf("read %+v does not name its reference's host", inv)
		}
	}

	for _, ref := range refs {
		public := ref
		public.Host = map[string]string{"github": "github.com", "gitlab": "gitlab.com"}[ref.Forge]
		_, err := r.core.GetPRDetail(t.Context(), public)
		if err == nil || strings.Contains(err.Error(), "unhandled") {
			t.Fatalf("%s PR read on %s: %v, want the forge's not-found", ref.Forge, public.Host, err)
		}
	}
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Fatalf("unimplemented invocation: %+v", inv)
		}
	}
}

// The two lists that name no repository resolve it from the checkout's
// origin, as the real CLIs do.
func TestPullListsResolveTheCheckoutsOrigin(t *testing.T) {
	fixture := githubFixture()
	fixture.Repos = append(fixture.Repos, gitlabFixture().Repos...)
	r := newRig(t, fixture)
	checkout := func(origin string) string {
		dir := t.TempDir()
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		return dir
	}
	gh := checkout("git@github.com:acme/widgets.git")
	gl := checkout("https://gitlab.com/grp/sub/tool.git")

	open, err := r.core.ListOpenPRs(t.Context(), gh, "feat/widgets")
	if err != nil || len(open) != 1 || open[0].Number != 7 || open[0].State != "open" {
		t.Fatalf("gh ListOpenPRs = %+v, %v", open, err)
	}
	merged, err := r.core.ListMergedPRHeads(t.Context(), gh, 10)
	if err != nil || len(merged) != 1 || merged[0].HeadRefName != "old-branch" {
		t.Fatalf("gh ListMergedPRHeads = %+v, %v", merged, err)
	}
	openMR, err := r.core.ListOpenPRs(t.Context(), gl, "fix")
	if err != nil || len(openMR) != 1 || openMR[0].Number != 3 {
		t.Fatalf("glab ListOpenPRs = %+v, %v", openMR, err)
	}
	mergedMR, err := r.core.ListMergedPRHeads(t.Context(), gl, 10)
	if err != nil || len(mergedMR) != 1 || mergedMR[0].HeadOid != strings.Repeat("e", 40) {
		t.Fatalf("glab ListMergedPRHeads = %+v, %v", mergedMR, err)
	}
}

// The merged-PR detection reads the state and merge time of tracked pulls in
// one request per repository: GitHub by the node id the open-PR lookup
// reported, GitLab by iid. A pull the forge no longer resolves is absent.
func TestPRStatesReadThroughTheAppsForgeCode(t *testing.T) {
	fixture := githubFixture()
	fixture.Repos = append(fixture.Repos, gitlabFixture().Repos...)
	fixture.Repos[0].Pulls[1].MergedAt = "2026-10-13T12:00:00Z"
	fixture.Repos[0].Pulls[1].NodeID = "PR_old"
	r := newRig(t, fixture)
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:acme/widgets.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	open, err := r.core.ListOpenPRs(t.Context(), dir, "feat/widgets")
	if err != nil || len(open) != 1 || open[0].ID == "" {
		t.Fatalf("gh ListOpenPRs = %+v, %v; want the pull with its node id", open, err)
	}
	gh := func(number int, id string) gitops.TrackedPR {
		return gitops.TrackedPR{Ref: gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: number}, NodeID: id}
	}
	states, err := r.core.ReadPRStates(t.Context(), []gitops.TrackedPR{gh(7, open[0].ID), gh(5, "PR_old"), gh(99, "PR_gone")})
	if err != nil {
		t.Fatalf("gh ReadPRStates: %v", err)
	}
	merged := states[5]
	if len(states) != 2 || states[7].State != "open" || merged.State != "merged" || !merged.MergedAt.Equal(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("gh ReadPRStates = %+v", states)
	}
	gl := func(number int) gitops.TrackedPR {
		return gitops.TrackedPR{Ref: gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp/sub", Repo: "tool", Number: number}}
	}
	mrStates, err := r.core.ReadPRStates(t.Context(), []gitops.TrackedPR{gl(3), gl(2), gl(40)})
	if err != nil {
		t.Fatalf("glab ReadPRStates: %v", err)
	}
	if len(mrStates) != 2 || mrStates[3].State != "open" || mrStates[2].State != "merged" || mrStates[2].MergedAt.IsZero() {
		t.Fatalf("glab ReadPRStates = %+v", mrStates)
	}
	if _, err := r.core.ReadPRStates(t.Context(), []gitops.TrackedPR{gh(7, open[0].ID), gl(3)}); err == nil {
		t.Fatal("a read naming two repositories was accepted")
	}
	reads := 0
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Fatalf("unimplemented invocation: %+v", inv)
		}
		if inv.Route == "gh graphql PRStates" || (inv.Route == "glab api merge request list" && strings.Contains(inv.Path, "iids")) {
			reads++
		}
	}
	if reads != 2 {
		t.Fatalf("state reads = %d, want one per repository", reads)
	}
}

// A fork's pull or merge request from a branch of the same name is not
// the checkout's: the app's open-PR lookup skips it on both forges.
func TestOpenPRLookupSkipsForksThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "acme/widgets", Pulls: []forgefake.Pull{
			{Number: 1, Title: "Fork only", HeadRef: "lonely", HeadRepo: "stranger/widgets"},
			{Number: 2, Title: "Fork", HeadRef: "topic", HeadRepo: "stranger/widgets"},
			{Number: 3, Title: "Own", HeadRef: "topic"},
		}},
		{Forge: "gitlab", Project: "grp/tool", Pulls: []forgefake.Pull{
			{Number: 1, Title: "Fork only", HeadRef: "lonely", HeadRepo: "stranger/tool"},
			{Number: 2, Title: "Own", HeadRef: "topic"},
			{Number: 3, Title: "Fork", HeadRef: "topic", HeadRepo: "stranger/tool"},
		}},
	}})
	for _, tc := range []struct {
		origin string
		own    int
	}{
		{"git@github.com:acme/widgets.git", 3},
		{"git@gitlab.com:grp/tool.git", 2},
	} {
		dir := t.TempDir()
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", tc.origin}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		if open, err := r.core.ListOpenPRs(t.Context(), dir, "lonely"); err != nil || len(open) != 0 {
			t.Fatalf("%s: a fork's pull alone = %+v, %v; want none", tc.origin, open, err)
		}
		if open, err := r.core.ListOpenPRs(t.Context(), dir, "topic"); err != nil || len(open) != 1 || open[0].Number != tc.own {
			t.Fatalf("%s: own and fork = %+v, %v; want only #%d", tc.origin, open, err, tc.own)
		}
	}
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Fatalf("unimplemented invocation: %+v", inv)
		}
	}
}

// The app's own create call opens a pull or merge request for the
// checkout's branch, and the open-PR lookup git status makes finds it.
func TestCreatePROpensOneTheAppThenFinds(t *testing.T) {
	fixture := githubFixture()
	fixture.Repos = append(fixture.Repos, gitlabFixture().Repos...)
	r := newRig(t, fixture)
	checkout := func(origin, branch string) string {
		dir := t.TempDir()
		for _, args := range [][]string{
			{"init", "-q", "-b", "main"},
			{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
			{"checkout", "-q", "-b", branch},
			{"remote", "add", "origin", origin},
		} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		return dir
	}
	gh := checkout("git@github.com:acme/widgets.git", "topic")
	url, err := r.core.CreatePR(t.Context(), gh, "Topic", "Why", "", true)
	if err != nil || url != "https://github.com/acme/widgets/pull/8" {
		t.Fatalf("gh CreatePR = %q, %v", url, err)
	}
	open, err := r.core.ListOpenPRs(t.Context(), gh, "topic")
	if err != nil || len(open) != 1 || open[0].Number != 8 || open[0].URL != url {
		t.Fatalf("gh ListOpenPRs after create = %+v, %v", open, err)
	}
	if _, err := r.core.CreatePR(t.Context(), gh, "Again", "", "", false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second gh CreatePR error = %v", err)
	}

	gl := checkout("https://gitlab.com/grp/sub/tool.git", "topic")
	mrURL, err := r.core.CreatePR(t.Context(), gl, "Topic", "Why", "", false)
	if err != nil || mrURL != "https://gitlab.com/grp/sub/tool/-/merge_requests/4" {
		t.Fatalf("glab CreatePR = %q, %v", mrURL, err)
	}
	openMR, err := r.core.ListOpenPRs(t.Context(), gl, "topic")
	if err != nil || len(openMR) != 1 || openMR[0].Number != 4 {
		t.Fatalf("glab ListOpenPRs after create = %+v, %v", openMR, err)
	}

	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Fatalf("unimplemented invocation: %+v", inv)
		}
	}
}

func TestUnimplementedInvocationSurfacesItsArgvToTheApp(t *testing.T) {
	fixture := githubFixture()
	fixture.Repos = append(fixture.Repos, gitlabFixture().Repos...)
	r := newRig(t, fixture)
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 7}
	err := r.core.ReplyToThread(t.Context(), ref, "", 99, "Body")
	if err == nil || !strings.Contains(err.Error(), "unhandled http request") || !strings.Contains(err.Error(), "POST repos/acme/widgets/pulls/7/comments/99/replies") {
		t.Fatalf("GitHub ReplyToThread error = %v, want the fake's unhandled report with the request", err)
	}
	mr := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp/sub", Repo: "tool", Number: 3}
	err = r.core.ReplyToThread(t.Context(), mr, "abc", 0, "Body")
	if err == nil || !strings.Contains(err.Error(), "unhandled http request") || !strings.Contains(err.Error(), "POST projects/grp%2Fsub%2Ftool/merge_requests/3/discussions/abc/notes") {
		t.Fatalf("GitLab ReplyToThread error = %v, want the fake's unhandled report with the request", err)
	}
	log := r.engine.Invocations(0).Invocations
	if len(log) != 2 {
		t.Fatalf("recorded %+v", log)
	}
	for i, forge := range []string{"github", "gitlab"} {
		if inv := log[i]; !inv.Unhandled || inv.Via != forgefake.ViaHTTP || inv.Method != "POST" || inv.Forge != forge {
			t.Fatalf("recorded %+v", log)
		}
	}
}

func TestStandaloneFakeRefusesToAnswer(t *testing.T) {
	cmd := exec.Command(fakeBin, "api", "user")
	cmd.Env = append(filteredEnv(), gitops.ForgeCLINameEnv+"=gh")
	out, err := cmd.CombinedOutput()
	if exitCode(err) != 1 || !strings.Contains(string(out), "no harness control channel") || !strings.Contains(string(out), `gh "api" "user"`) {
		t.Fatalf("standalone run: exit %d, %q", exitCode(err), out)
	}

	cmd = exec.Command(fakeBin, "api", "user")
	cmd.Env = filteredEnv()
	out, err = cmd.CombinedOutput()
	if exitCode(err) != 2 || !strings.Contains(string(out), gitops.ForgeCLINameEnv) {
		t.Fatalf("run without a CLI name: exit %d, %q", exitCode(err), out)
	}
}

func TestUnreachableHarnessIsACLIFailure(t *testing.T) {
	cmd := exec.Command(fakeBin, "api", "user")
	cmd.Env = append(filteredEnv(), gitops.ForgeCLINameEnv+"=glab", control.EnvAddr+"=127.0.0.1:1", control.EnvToken+"=x")
	out, err := cmd.CombinedOutput()
	if exitCode(err) != 1 || !strings.Contains(string(out), "the harness did not answer") {
		t.Fatalf("unreachable harness: exit %d, %q", exitCode(err), out)
	}
}

func filteredEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, control.EnvAddr+"=") || strings.HasPrefix(kv, control.EnvToken+"=") || strings.HasPrefix(kv, gitops.ForgeCLINameEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return -1
}

func TestRepositoryIdentityThroughTheFake(t *testing.T) {
	t.Parallel()
	for _, forge := range []string{"github", "gitlab"} {
		t.Run(forge, func(t *testing.T) {
			r := newRig(t, forgefake.Fixture{Repos: []forgefake.Repo{{Forge: forge, Project: "owner/repo", ID: 123}}})
			result := r.core.ResolveRepository(context.Background(), t.TempDir(), gitops.RepoIdentity{RemoteURL: "https://" + forge + ".com/owner/repo"})
			if result.RepositoryID != forge+":"+forge+".com:123" || result.LookupError != "" {
				t.Fatalf("identity: %+v", result)
			}
		})
	}
}

func TestRepositorySSHIdentityThroughTheFake(t *testing.T) {
	t.Parallel()
	r := newRig(t, forgefake.Fixture{SSHHosts: map[string]string{"work-github": "github.com"}, Repos: []forgefake.Repo{{Forge: "github", Project: "owner/repo", ID: 456}}})
	result := r.core.ResolveRepository(context.Background(), t.TempDir(), gitops.RepoIdentity{RemoteURL: "git@work-github:owner/repo.git"})
	if result.RepositoryID != "github:github.com:456" || result.LookupError != "" {
		t.Fatalf("alias: %+v", result)
	}
	calls := r.engine.Invocations(0).Invocations
	if len(calls) != 2 || calls[0].CLI != "ssh" || calls[1].Via != forgefake.ViaHTTP || calls[1].Route != "gh repository identity" {
		t.Fatalf("calls: %+v", calls)
	}
	for _, call := range calls {
		if call.Unhandled {
			t.Fatalf("unhandled: %+v", call)
		}
	}
}

// mergeRead reads ref's detail, events and threads through the app's
// ReadPR, failing the test on an error.
func mergeRead(t *testing.T, r *rig, ref gitops.PRReference) gitops.PRRead {
	t.Helper()
	read, err := r.core.ReadPR(t.Context(), ref, gitops.PRReadParts{Detail: true, Threads: true, Events: true, CI: true}, nil, nil)
	if err != nil {
		t.Fatalf("ReadPR %s: %v", ref.Project(), err)
	}
	return read
}

func blockerKinds(readiness gitops.MergeReadiness) []string {
	out := []string{}
	for _, blocker := range readiness.Blockers {
		out = append(out, blocker.Kind)
	}
	return out
}

func eventKinds(events []gitops.ReviewEvent) []string {
	out := []string{}
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}

// explained is err as the app explains it to the user.
func explained(t *testing.T, ref gitops.PRReference, action gitops.ForgeAction, err error) *gitops.ForgeActionError {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded, want a forge refusal", action)
	}
	out, ok := errors.AsType[*gitops.ForgeActionError](gitops.ExplainForgeError(ref, action, err))
	if !ok {
		t.Fatalf("ExplainForgeError(%v) is not a ForgeActionError", err)
	}
	return out
}

func githubMergeFixture(ci string, reviews ...forgefake.Review) forgefake.Fixture {
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "github", Project: "acme/merge", ID: 5150,
		Merge: &forgefake.RepoMerge{AutoMergeAllowed: true, DefaultMethod: "squash"},
		Pulls: []forgefake.Pull{
			{
				Number: 9, Title: "Ship it", Author: "octo", HeadRef: "ship", HeadSHA: strings.Repeat("9", 40),
				Readiness: &forgefake.Readiness{RequiredApprovals: 1, RequiredChecks: []string{"unit"}},
				Reviews:   reviews,
				CI: &forgefake.Pipeline{ID: 990, Jobs: []forgefake.Job{
					{ID: 991, Name: "unit", Status: ci}, {ID: 992, Name: "lint", Status: "failed", Log: "lint"},
				}},
			},
			{Number: 10, Title: "Behind", Author: "octo", HeadRef: "behind", HeadSHA: strings.Repeat("a", 40), Readiness: &forgefake.Readiness{Behind: true}},
			{Number: 11, Title: "Drafty", Author: "octo", HeadRef: "drafty", Draft: true},
			{Number: 12, Title: "Mine", Author: "alice", HeadRef: "mine"},
		},
	}}}
}

// The Merge section's GitHub reads and every action, through the app's
// own adapters: each action changes what the next read answers, and a
// stale head changes nothing.
func TestGitHubMergeActionsThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, githubMergeFixture("running"))
	ref := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 9}
	ctx := forgeapi.WithInteractive(t.Context())
	head := strings.Repeat("9", 40)

	read := mergeRead(t, r, ref)
	merge := read.Detail.Merge
	if got := read.Detail.Readiness; got.State != gitops.MergeStateBlocked || !slices.Equal(blockerKinds(got), []string{"approvals", "checks"}) ||
		got.Blockers[0].Text != "Needs 1 more approval" || got.Blockers[1].Text != "1 required check pending" {
		t.Fatalf("blocked readiness = %+v", got)
	}
	if !slices.Equal(merge.Methods, []string{"squash", "merge", "rebase"}) || !merge.AutoMergeAvailable || !merge.ViewerCanMerge ||
		!merge.ViewerCanApprove || merge.ViewerApproved || !merge.DeleteBranchOffered || merge.AutoMerge != nil {
		t.Fatalf("merge options = %+v", merge)
	}

	// Approve: the viewer's approval counts, the approvals blocker goes,
	// and the review is a timeline event.
	if err := r.core.ApprovePR(ctx, ref, head); err != nil {
		t.Fatalf("ApprovePR: %v", err)
	}
	read = mergeRead(t, r, ref)
	if got := read.Detail.Readiness; !slices.Equal(blockerKinds(got), []string{"checks"}) || !read.Detail.Merge.ViewerApproved || read.Detail.Merge.ViewerApprovalID == "" {
		t.Fatalf("after approve: readiness %+v, merge %+v", got, read.Detail.Merge)
	}
	if kinds := eventKinds(read.Events); !slices.Equal(kinds, []string{"approved"}) || read.Events[0].ActorLogin != "alice" || read.Events[0].Ref != head {
		t.Fatalf("after approve: events %+v", read.Events)
	}

	// Unapprove finds the approval itself (ViewerApproval) and dismisses it.
	if err := r.core.UnapprovePR(ctx, ref, ""); err != nil {
		t.Fatalf("UnapprovePR: %v", err)
	}
	read = mergeRead(t, r, ref)
	if got := read.Detail.Readiness; !slices.Equal(blockerKinds(got), []string{"approvals", "checks"}) || read.Detail.Merge.ViewerApproved {
		t.Fatalf("after unapprove: readiness %+v, merge %+v", got, read.Detail.Merge)
	}
	if kinds := eventKinds(read.Events); !slices.Equal(kinds, []string{"review_dismissed"}) || read.Events[0].Ref != "alice" || read.Events[0].Text != "Approval withdrawn" {
		t.Fatalf("after unapprove: events %+v", read.Events)
	}
	if err := r.core.UnapprovePR(ctx, ref, ""); explained(t, ref, gitops.ActionUnapprove, err).Message != "You have no approval to withdraw." {
		t.Fatalf("a second unapprove = %v", err)
	}

	// A merge now while blocked is GitHub's refusal; auto-merge arms.
	if _, err := r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "squash", HeadSHA: head}); explained(t, ref, gitops.ActionMerge, err).Kind != gitops.ForgeErrorRefused {
		t.Fatalf("merge while blocked = %v", err)
	}
	outcome, err := r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "squash", HeadSHA: head, AutoMerge: true})
	if err != nil || !outcome.AutoMerge {
		t.Fatalf("auto-merge = %+v, %v", outcome, err)
	}
	read = mergeRead(t, r, ref)
	if armed := read.Detail.Merge.AutoMerge; armed == nil || armed.Method != "squash" || armed.ByLogin != "alice" || !armed.CanCancel || armed.At == "" ||
		armed.WaitsFor != "required reviews and checks" {
		t.Fatalf("armed auto-merge = %+v", armed)
	}
	if kinds := eventKinds(read.Events); kinds[len(kinds)-1] != "auto_merge_set" || read.Events[len(kinds)-1].Text != "squash" {
		t.Fatalf("after auto-merge: events %+v", read.Events)
	}
	if err := r.core.CancelPRAutoMerge(ctx, ref); err != nil {
		t.Fatalf("CancelPRAutoMerge: %v", err)
	}
	read = mergeRead(t, r, ref)
	if read.Detail.Merge.AutoMerge != nil || eventKinds(read.Events)[len(read.Events)-1] != "auto_merge_canceled" {
		t.Fatalf("after cancel: merge %+v, events %+v", read.Detail.Merge, read.Events)
	}

	// The checks pass and a reviewer approved: ready. A merge naming a
	// head the pull no longer has is refused and changes nothing.
	if _, err := r.engine.Seed(githubMergeFixture("success", forgefake.Review{Author: "bob", State: "APPROVED"})); err != nil {
		t.Fatal(err)
	}
	if got := mergeRead(t, r, ref).Detail.Readiness; got.State != gitops.MergeStateReady {
		t.Fatalf("ready readiness = %+v", got)
	}
	stale := explained(t, ref, gitops.ActionMerge, func() error {
		_, err := r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "merge", HeadSHA: strings.Repeat("8", 40)})
		return err
	}())
	if stale.Kind != gitops.ForgeErrorStaleHead || stale.Message != "The branch moved after you loaded this pull request. Reload and try again." {
		t.Fatalf("a stale head = %+v", stale)
	}
	if got := mergeRead(t, r, ref).Detail; got.State != "open" || got.Readiness.State != gitops.MergeStateReady {
		t.Fatalf("after a stale merge: %+v", got)
	}
	outcome, err = r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "merge", HeadSHA: head, DeleteBranch: true})
	if err != nil || !outcome.Merged || len(outcome.SHA) != 40 {
		t.Fatalf("merge = %+v, %v", outcome, err)
	}
	read = mergeRead(t, r, ref)
	if got := read.Detail; got.State != "merged" || got.Readiness.State != gitops.MergeStateMerged || got.MergedBy != "alice" || got.MergedAt == "" || got.ClosedAt == "" {
		t.Fatalf("merged detail = %+v", got)
	}
	if kinds := eventKinds(read.Events); kinds[len(kinds)-1] != "merged" || read.Events[len(kinds)-1].Ref != outcome.SHA {
		t.Fatalf("merged events = %+v", read.Events)
	}
	deleted := false
	for _, inv := range r.engine.Invocations(0).Invocations {
		deleted = deleted || (inv.Route == "gh api delete branch" && inv.Status == 204 && inv.Path == "repos/acme/merge/git/refs/heads/ship")
	}
	if !deleted {
		t.Fatal("the merge did not delete the head branch")
	}

	// Update branch moves a behind head; mark ready takes a draft out.
	behind := ref
	behind.Number = 10
	if got := mergeRead(t, r, behind).Detail.Readiness; !slices.Equal(blockerKinds(got), []string{"rebase"}) {
		t.Fatalf("behind readiness = %+v", got)
	}
	if err := r.core.UpdatePRBranch(ctx, behind, strings.Repeat("a", 40)); err != nil {
		t.Fatalf("UpdatePRBranch: %v", err)
	}
	if got := mergeRead(t, r, behind).Detail; got.HeadSHA == strings.Repeat("a", 40) || got.Readiness.State != gitops.MergeStateReady {
		t.Fatalf("after update: %+v", got)
	}
	draft := ref
	draft.Number = 11
	if got := mergeRead(t, r, draft).Detail.Readiness; got.State != gitops.MergeStateDraft {
		t.Fatalf("draft readiness = %+v", got)
	}
	if err := r.core.MarkPRReady(ctx, draft); err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
	if read := mergeRead(t, r, draft); read.Detail.Draft || read.Detail.Readiness.State != gitops.MergeStateReady || !slices.Equal(eventKinds(read.Events), []string{"marked_ready"}) {
		t.Fatalf("after mark ready: %+v, events %+v", read.Detail, read.Events)
	}

	// The author cannot approve: no Approve, the reason, and GitHub's 422.
	mine := ref
	mine.Number = 12
	if got := mergeRead(t, r, mine).Detail.Merge; got.ViewerCanApprove || got.CannotApproveReason != "You cannot approve your own pull request." {
		t.Fatalf("the author's merge options = %+v", got)
	}
	if err := r.core.ApprovePR(ctx, mine, strings.Repeat("0", 40)); explained(t, mine, gitops.ActionApprove, err).Kind != gitops.ForgeErrorRefused {
		t.Fatalf("the author's approval = %v", err)
	}
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Errorf("unhandled %s %s: %s", inv.Method, inv.Path, inv.Detail)
		}
	}
}

func gitlabMergeFixture(ci string, reviews ...forgefake.Review) forgefake.Fixture {
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "gitlab", Project: "grp/merge", ID: 6160,
		Merge: &forgefake.RepoMerge{PipelineMustSucceed: true, DeleteBranchOnMerge: true},
		Pulls: []forgefake.Pull{
			{
				Number: 4, Title: "Ship it", Author: "octo", HeadRef: "ship", HeadSHA: strings.Repeat("4", 40),
				Readiness: &forgefake.Readiness{RequiredApprovals: 1},
				Reviews:   reviews,
				CI:        &forgefake.Pipeline{ID: 440, Jobs: []forgefake.Job{{ID: 441, Name: "unit", Status: ci}}},
			},
			{Number: 5, Title: "Draft: Drafty", Author: "octo", HeadRef: "drafty"},
		},
	}, {
		Forge: "gitlab", Project: "grp/linear", ID: 6161,
		Merge: &forgefake.RepoMerge{MergeMethod: "ff"},
		Pulls: []forgefake.Pull{{Number: 1, Title: "Behind", Author: "octo", HeadRef: "behind", HeadSHA: strings.Repeat("b", 40), Readiness: &forgefake.Readiness{Behind: true}}},
	}}}
}

// The Merge section's GitLab reads and every action, through the app's
// own adapters.
func TestGitLabMergeActionsThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, gitlabMergeFixture("running"))
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}
	ctx := forgeapi.WithInteractive(t.Context())
	head := strings.Repeat("4", 40)

	read := mergeRead(t, r, ref)
	merge := read.Detail.Merge
	if got := read.Detail.Readiness; got.State != gitops.MergeStateBlocked || !slices.Equal(blockerKinds(got), []string{"checks", "approvals"}) ||
		got.Blockers[0].Text != "The pipeline is still running" || got.Blockers[1].Text != "Needs 1 more approval" {
		t.Fatalf("blocked readiness = %+v", got)
	}
	if !slices.Equal(merge.Methods, []string{"merge", "squash"}) || !merge.AutoMergeAvailable || !merge.ViewerCanMerge || !merge.ViewerCanApprove ||
		merge.ViewerApproved || !merge.DeleteBranchOffered || !merge.DeleteBranchDefault {
		t.Fatalf("merge options = %+v", merge)
	}

	// An approval of a head that moved is GitLab's 409 and changes nothing.
	stale := explained(t, ref, gitops.ActionApprove, r.core.ApprovePR(ctx, ref, strings.Repeat("0", 40)))
	if stale.Kind != gitops.ForgeErrorStaleHead || stale.Message != "The branch moved after you loaded this. Reload and try again." {
		t.Fatalf("a stale approval = %+v", stale)
	}
	if mergeRead(t, r, ref).Detail.Merge.ViewerApproved {
		t.Fatal("a refused approval approved")
	}
	if err := r.core.ApprovePR(ctx, ref, head); err != nil {
		t.Fatalf("ApprovePR: %v", err)
	}
	read = mergeRead(t, r, ref)
	if got := read.Detail; !slices.Equal(blockerKinds(got.Readiness), []string{"checks"}) || !got.Merge.ViewerApproved || got.Merge.ViewerCanApprove || len(got.LatestReviews) != 1 {
		t.Fatalf("after approve: %+v", got)
	}
	if kinds := eventKinds(read.Events); !slices.Equal(kinds, []string{"approved"}) || read.Events[0].ActorLogin != "alice" {
		t.Fatalf("after approve: events %+v", read.Events)
	}
	if err := r.core.UnapprovePR(ctx, ref, ""); err != nil {
		t.Fatalf("UnapprovePR: %v", err)
	}
	read = mergeRead(t, r, ref)
	if got := read.Detail; !slices.Equal(blockerKinds(got.Readiness), []string{"checks", "approvals"}) || got.Merge.ViewerApproved || !got.Merge.ViewerCanApprove {
		t.Fatalf("after unapprove: %+v", got)
	}
	if kinds := eventKinds(read.Events); !slices.Equal(kinds, []string{"approved", "unapproved"}) {
		t.Fatalf("after unapprove: events %+v", read.Events)
	}

	// Merge now is refused while blocked (405); auto-merge arms.
	if _, err := r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "merge", HeadSHA: head}); explained(t, ref, gitops.ActionMerge, err).Kind != gitops.ForgeErrorRefused {
		t.Fatalf("merge while blocked = %v", err)
	}
	outcome, err := r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "merge", HeadSHA: head, AutoMerge: true, DeleteBranch: true})
	if err != nil || !outcome.AutoMerge {
		t.Fatalf("auto-merge = %+v, %v", outcome, err)
	}
	read = mergeRead(t, r, ref)
	if armed := read.Detail.Merge.AutoMerge; armed == nil || armed.Method != "merge" || armed.ByLogin != "alice" || !armed.CanCancel || armed.At == "" ||
		armed.WaitsFor != "all merge checks for "+head+" pass" {
		t.Fatalf("armed auto-merge = %+v", armed)
	}
	if err := r.core.CancelPRAutoMerge(ctx, ref); err != nil {
		t.Fatalf("CancelPRAutoMerge: %v", err)
	}
	read = mergeRead(t, r, ref)
	if read.Detail.Merge.AutoMerge != nil || !slices.Equal(eventKinds(read.Events), []string{"approved", "unapproved", "auto_merge_set", "auto_merge_canceled"}) {
		t.Fatalf("after cancel: merge %+v, events %+v", read.Detail.Merge, read.Events)
	}
	if err := r.core.CancelPRAutoMerge(ctx, ref); explained(t, ref, gitops.ActionAutoMerge, err).Kind != gitops.ForgeErrorStaleHead {
		// GitLab answers a cancel with nothing armed 409.
		t.Fatalf("a second cancel = %v", err)
	}

	// The pipeline passed and a reviewer approved: ready, then merged.
	if _, err := r.engine.Seed(gitlabMergeFixture("success", forgefake.Review{Author: "bob", State: "APPROVED"})); err != nil {
		t.Fatal(err)
	}
	if got := mergeRead(t, r, ref).Detail.Readiness; got.State != gitops.MergeStateReady {
		t.Fatalf("ready readiness = %+v", got)
	}
	outcome, err = r.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "squash", HeadSHA: head, DeleteBranch: true})
	if err != nil || !outcome.Merged || len(outcome.SHA) != 40 {
		t.Fatalf("merge = %+v, %v", outcome, err)
	}
	read = mergeRead(t, r, ref)
	if got := read.Detail; got.State != "merged" || got.Readiness.State != gitops.MergeStateMerged || got.MergedBy != "alice" || got.MergedAt == "" {
		t.Fatalf("merged detail = %+v", got)
	}
	if kinds := eventKinds(read.Events); kinds[len(kinds)-1] != "merged" || read.Events[len(kinds)-1].Ref != outcome.SHA {
		t.Fatalf("merged events = %+v", read.Events)
	}

	// Mark ready drops the title's draft prefix; a rebase moves the head.
	draft := ref
	draft.Number = 5
	if got := mergeRead(t, r, draft).Detail; got.Readiness.State != gitops.MergeStateDraft || !got.Draft {
		t.Fatalf("draft detail = %+v", got)
	}
	if err := r.core.MarkPRReady(ctx, draft); err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
	if read := mergeRead(t, r, draft); read.Detail.Draft || read.Detail.Title != "Drafty" || !slices.Equal(eventKinds(read.Events), []string{"marked_ready"}) {
		t.Fatalf("after mark ready: %+v, events %+v", read.Detail, read.Events)
	}
	linear := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "linear", Number: 1}
	if got := mergeRead(t, r, linear).Detail; !slices.Equal(blockerKinds(got.Readiness), []string{"rebase"}) || !slices.Equal(got.Merge.Methods, []string{"rebase", "squash"}) {
		t.Fatalf("behind detail = %+v", got)
	}
	if err := r.core.UpdatePRBranch(ctx, linear, strings.Repeat("b", 40)); err != nil {
		t.Fatalf("UpdatePRBranch: %v", err)
	}
	if got := mergeRead(t, r, linear).Detail; got.HeadSHA == strings.Repeat("b", 40) || got.Readiness.State != gitops.MergeStateReady {
		t.Fatalf("after rebase: %+v", got)
	}
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Errorf("unhandled %s %s: %s", inv.Method, inv.Path, inv.Detail)
		}
	}
}

// A refusal the knob arms reaches the app as the forge's own: a token
// without the scope is named in the sentence on both forges, and a 401 on
// a mutation the login survives is GitLab refusing the action.
func TestRefusalsReachTheAppsExplanations(t *testing.T) {
	ctx := forgeapi.WithInteractive(t.Context())
	gl := newRig(t, gitlabMergeFixture("running"))
	ref := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "merge", Number: 4}
	if err := gl.engine.SetRefusal(forgefake.Refusal{Route: "glab api approve", Status: 403, Body: []byte(`{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token.","scope":"api"}`)}); err != nil {
		t.Fatal(err)
	}
	refused := explained(t, ref, gitops.ActionApprove, gl.core.ApprovePR(ctx, ref, strings.Repeat("4", 40)))
	if refused.Kind != gitops.ForgeErrorForbidden || !strings.Contains(refused.Message, "lacks the `api` scope") {
		t.Fatalf("a scope refusal = %+v", refused)
	}
	if mergeRead(t, gl, ref).Detail.Merge.ViewerApproved {
		t.Fatal("a refused approval approved")
	}
	if err := gl.engine.SetRefusal(forgefake.Refusal{Route: "glab api merge", Status: 401, Body: []byte(`{"message":"401 Unauthorized"}`)}); err != nil {
		t.Fatal(err)
	}
	_, err := gl.core.MergePR(ctx, ref, gitops.MergeRequest{Method: "merge", HeadSHA: strings.Repeat("4", 40), AutoMerge: true})
	if got := explained(t, ref, gitops.ActionMerge, err); got.Kind != gitops.ForgeErrorRefused || got.Message != "GitLab did not let you merge grp/merge!4." {
		t.Fatalf("a refused merge = %+v (%v)", got, err)
	}
	if mergeRead(t, gl, ref).Detail.Merge.AutoMerge != nil {
		t.Fatal("a refused auto-merge armed")
	}

	gh := newRig(t, githubMergeFixture("running"))
	ghRef := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "merge", Number: 9}
	if err := gh.engine.SetRefusal(forgefake.Refusal{Route: "gh api pull reviews", Status: 403, Header: map[string]string{"X-Accepted-OAuth-Scopes": "repo", "X-OAuth-Scopes": "read:org"}, Body: []byte(`{"message":"Resource not accessible by integration"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := explained(t, ghRef, gitops.ActionApprove, gh.core.ApprovePR(ctx, ghRef, strings.Repeat("9", 40))); got.Kind != gitops.ForgeErrorForbidden || !strings.Contains(got.Message, "lacks the `repo` scope") {
		t.Fatalf("a GitHub scope refusal = %+v", got)
	}
	if err := gh.engine.SetRefusal(forgefake.Refusal{Route: "gh graphql MergePR", Status: 200, Times: 1, Body: []byte(`{"data":{"mergePullRequest":null},"errors":[{"type":"UNPROCESSABLE","path":["mergePullRequest"],"message":"Head branch was modified. Review and try the merge again."}]}`)}); err != nil {
		t.Fatal(err)
	}
	_, err = gh.core.MergePR(ctx, ghRef, gitops.MergeRequest{Method: "merge", HeadSHA: strings.Repeat("9", 40)})
	if got := explained(t, ghRef, gitops.ActionMerge, err); got.Kind != gitops.ForgeErrorStaleHead {
		t.Fatalf("a GraphQL refusal = %+v", got)
	}
	for _, rig := range []*rig{gl, gh} {
		for _, inv := range rig.engine.Invocations(0).Invocations {
			if inv.Unhandled {
				t.Errorf("unhandled %s %s: %s", inv.Method, inv.Path, inv.Detail)
			}
		}
	}
}
