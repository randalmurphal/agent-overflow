package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/forgefake"
)

// fakeBin is ao-mockforge, built once per run and executed by a real
// git.Core in place of gh and glab. These tests hold the fake's answers
// to the app's own parsers: a shape the fake gets wrong fails here, not
// as a blank review pane in a browser spec.
var fakeBin string

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
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// rig is one harness-side fake forge and an isolated Core that reaches it.
type rig struct {
	engine *forgefake.Engine
	core   *gitops.Core
}

func newRig(t *testing.T, fixture forgefake.Fixture) *rig {
	t.Helper()
	engine := forgefake.New(forgefake.Options{})
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
	return &rig{engine: engine, core: gitops.NewCore(gitops.WithIsolatedForgeCLIs(fakeBin, env))}
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
			{Author: "bob", Body: "range"}, {Author: "alice", Body: "reply"},
		}},
		{Path: "app.go", Side: "file", Comments: []forgefake.Comment{{Author: "bob", Body: "file level"}}},
		{Path: "app.go", Line: intPtr(1), Side: "left", Outdated: true, Comments: []forgefake.Comment{{Author: "bob", Body: "old"}}},
	}, manyThreads(60)...)
	return forgefake.Fixture{Viewer: "alice", Repos: []forgefake.Repo{{
		Forge: "github", Project: "acme/widgets",
		Attachments: []forgefake.Attachment{{URL: "https://github.com/user-attachments/assets/1a2b", ContentType: "image/png", Base64: base64.StdEncoding.EncodeToString(pngBytes)}},
		Pulls: []forgefake.Pull{
			{
				Number: 7, Title: "Add widgets", Body: "![shot](https://github.com/user-attachments/assets/1a2b)",
				Author: "alice", HeadRef: "feat/widgets", HeadSHA: strings.Repeat("a", 40), Diff: diff, Draft: true,
				Mergeable: "conflicts",
				Comments:  manyComments(55, "conversation"),
				Threads:   threads,
				Reviews: []forgefake.Review{
					{Author: "bob", State: "COMMENTED"}, {Author: "bob", State: "CHANGES_REQUESTED", Body: "fix"},
				},
				CI: &forgefake.Pipeline{ID: 555, Name: "Build", Jobs: []forgefake.Job{
					{ID: 901, Name: "unit", Status: "success", Log: "all green", Steps: []forgefake.Step{{Name: "checkout", Status: "success"}}},
					{ID: 902, Name: "lint", Status: "failed", Log: "lint failed"},
					{ID: 903, Name: "deploy", Status: "pending"},
				}},
			},
			{Number: 5, Title: "Old", State: "merged", HeadRef: "old-branch", HeadSHA: strings.Repeat("b", 40)},
		},
	}}}
}

func gitlabFixture() forgefake.Fixture {
	return forgefake.Fixture{Repos: []forgefake.Repo{{
		Forge: "gitlab", Project: "grp/sub/tool", ID: 4242,
		Attachments: []forgefake.Attachment{{Secret: "0123456789abcdef0123456789abcdef", Filename: "diagram one.svg", ContentType: "image/svg+xml", Text: "<svg xmlns=\"http://www.w3.org/2000/svg\"/>"}},
		Pulls: []forgefake.Pull{{
			Number: 3, Title: "Fix tool", Body: "![d](/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg)",
			Author: "dave", HeadRef: "fix", HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("d", 40), Diff: diff,
			Comments: manyComments(40, "note"),
			Threads: append([]forgefake.Thread{
				{Path: "app.go", Line: intPtr(3), StartLine: intPtr(2), Resolved: true, Comments: []forgefake.Comment{{Body: "range"}, {Body: "reply"}}},
				{Path: "app.go", Line: intPtr(1), Side: "left", Outdated: true, Comments: []forgefake.Comment{{Body: "old side"}}},
			}, manyThreads(20)...),
			Reviews: []forgefake.Review{{Author: "erin", State: "APPROVED"}},
			CI: &forgefake.Pipeline{ID: 777, Jobs: []forgefake.Job{
				{ID: 11, Name: "build", Stage: "build", Status: "success", Log: "built"},
				{ID: 12, Name: "test", Stage: "test", Status: "running", StartedAt: "2026-01-01T00:00:00Z"},
			}},
		}, {Number: 2, Title: "Merged", State: "merged", HeadRef: "done", HeadSHA: strings.Repeat("e", 40)}},
	}}}
}

func TestGitHubReadsParseThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, githubFixture())
	ref := gitops.PRReference{Forge: "github", Namespace: "acme", Repo: "widgets", Number: 7}

	meta, err := r.core.ForgeByID("github").ViewPR("", "acme/widgets", 7)
	if err != nil || meta.Title != "Add widgets" || meta.HeadRefName != "feat/widgets" || meta.URL != "https://github.com/acme/widgets/pull/7" {
		t.Fatalf("ViewPR = %+v, %v", meta, err)
	}

	detail, err := r.core.GetPRDetail("", ref)
	if err != nil {
		t.Fatalf("GetPRDetail: %v", err)
	}
	if !detail.ViewerIsAuthor || !detail.Draft || detail.State != "open" || detail.HeadSHA != strings.Repeat("a", 40) ||
		detail.Additions != 2 || detail.Deletions != 1 || detail.ChangedFiles != 1 || detail.Mergeability != gitops.MergeabilityConflicts ||
		detail.ReviewDecision != "CHANGES_REQUESTED" || len(detail.LatestReviews) != 1 || detail.LatestReviews[0].Body != "fix" ||
		detail.Checks.Total != 3 || detail.Checks.Success != 1 || detail.Checks.Failure != 1 || detail.Checks.Pending != 1 {
		t.Fatalf("GetPRDetail = %+v", detail)
	}

	threads, err := r.core.ListReviewThreads("", ref)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	// 63 review threads over two GraphQL pages, then 55 conversation
	// comments over two more.
	if len(threads) != 63+55 {
		t.Fatalf("threads = %d, want %d", len(threads), 63+55)
	}
	ranged, file, outdated := threads[0], threads[1], threads[2]
	if !ranged.IsResolved || *ranged.Line != 3 || *ranged.StartLine != 2 || ranged.Side != "right" ||
		len(ranged.Comments) != 2 || ranged.Comments[1].ReplyTo == nil || ranged.Comments[1].ReplyTo.DatabaseID != ranged.Comments[0].DatabaseID {
		t.Fatalf("range thread = %+v", ranged)
	}
	if file.Side != "file" || file.Line != nil {
		t.Fatalf("file thread = %+v", file)
	}
	if !outdated.IsOutdated || outdated.Side != "left" {
		t.Fatalf("outdated thread = %+v", outdated)
	}
	if last := threads[len(threads)-1]; last.Path != "" || last.Comments[0].Body != "conversation 54" {
		t.Fatalf("last conversation comment = %+v", last)
	}

	if got, err := r.core.GetPRDiff("", ref); err != nil || got != diff {
		t.Fatalf("GetPRDiff = %q, %v", got, err)
	}

	pipeline, err := r.core.ListPRCIJobs("", ref)
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	if len(pipeline.Stages) != 1 || pipeline.Stages[0].Name != "Build" || len(pipeline.Stages[0].Jobs) != 3 ||
		pipeline.Stages[0].Jobs[0].ID != "901" || len(pipeline.Stages[0].Jobs[0].Steps) != 1 || !pipeline.Stages[0].Jobs[0].LogsAvailable ||
		pipeline.Stages[0].Jobs[2].LogsAvailable {
		t.Fatalf("ListPRCIJobs = %+v", pipeline)
	}
	if log, err := r.core.GetCIJobLog("", ref, "902"); err != nil || log != "lint failed" {
		t.Fatalf("GetCIJobLog = %q, %v", log, err)
	}
	if _, err := r.core.GetCIJobLog("", ref, "903"); err == nil {
		t.Fatal("a queued job served a log")
	}

	data, name, err := r.core.FetchAttachment("", ref, "https://github.com/user-attachments/assets/1a2b", 1<<20)
	if err != nil || !bytes.Equal(data, pngBytes) || name != "1a2b" {
		t.Fatalf("FetchAttachment = %d bytes %q, %v", len(data), name, err)
	}
	if _, _, err := r.core.FetchAttachment("", ref, "https://github.com/user-attachments/assets/ffff", 1<<20); err == nil {
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

func TestGitLabReadsParseThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, gitlabFixture())
	ref := gitops.PRReference{Forge: "gitlab", Namespace: "grp/sub", Repo: "tool", Number: 3}

	detail, err := r.core.GetPRDetail("", ref)
	if err != nil {
		t.Fatalf("GetPRDetail: %v", err)
	}
	if detail.Title != "Fix tool" || detail.AuthorLogin != "dave" || detail.State != "open" || detail.ChangedFiles != 1 ||
		detail.URL != "https://gitlab.com/grp/sub/tool/-/merge_requests/3" || detail.ReviewDecision != "APPROVED" ||
		len(detail.LatestReviews) != 1 || detail.LatestReviews[0].AuthorLogin != "erin" || detail.DiffRefs == nil ||
		detail.DiffRefs.BaseSHA != strings.Repeat("d", 40) || detail.Checks.Total != 1 || detail.Checks.Pending != 1 {
		t.Fatalf("GetPRDetail = %+v", detail)
	}

	threads, err := r.core.ListReviewThreads("", ref)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	// 22 diff threads and 40 notes over two pages of 50.
	if len(threads) != 62 {
		t.Fatalf("threads = %d, want 62", len(threads))
	}
	ranged, outdated := threads[0], threads[1]
	if !ranged.IsResolved || !ranged.IsResolvable || *ranged.Line != 3 || *ranged.StartLine != 2 || ranged.Side != "right" || len(ranged.Comments) != 2 {
		t.Fatalf("range thread = %+v", ranged)
	}
	if !outdated.IsOutdated || outdated.Side != "left" || *outdated.Line != 1 {
		t.Fatalf("outdated thread = %+v", outdated)
	}
	if last := threads[61]; last.Path != "" || last.IsResolvable || last.Comments[0].Body != "note 39" {
		t.Fatalf("last note = %+v", last)
	}

	if got, err := r.core.GetPRDiff("", ref); err != nil || got != diff {
		t.Fatalf("GetPRDiff = %q, %v", got, err)
	}
	pipeline, err := r.core.ListPRCIJobs("", ref)
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	if pipeline.URL != "https://gitlab.com/grp/sub/tool/-/pipelines/777" || len(pipeline.Stages) != 2 ||
		pipeline.Stages[0].Name != "build" || pipeline.Stages[0].Jobs[0].DurationSeconds != 60 || pipeline.Stages[1].Jobs[0].ID != "12" {
		t.Fatalf("ListPRCIJobs = %+v", pipeline)
	}
	if log, err := r.core.GetCIJobLog("", ref, "11"); err != nil || log != "built" {
		t.Fatalf("GetCIJobLog = %q, %v", log, err)
	}

	svg := "<svg xmlns=\"http://www.w3.org/2000/svg\"/>"
	for _, href := range []string{
		"/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg",
		"https://gitlab.com/grp/sub/tool/-/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg",
		"/-/project/4242/uploads/0123456789abcdef0123456789abcdef/diagram%20one.svg",
	} {
		data, name, err := r.core.FetchAttachment("", ref, href, 1<<20)
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

	open, err := r.core.ListOpenPRs(gh, "feat/widgets")
	if err != nil || len(open) != 1 || open[0].Number != 7 || open[0].State != "open" {
		t.Fatalf("gh ListOpenPRs = %+v, %v", open, err)
	}
	merged, err := r.core.ListMergedPRHeads(gh, 10)
	if err != nil || len(merged) != 1 || merged[0].HeadRefName != "old-branch" {
		t.Fatalf("gh ListMergedPRHeads = %+v, %v", merged, err)
	}
	openMR, err := r.core.ListOpenPRs(gl, "fix")
	if err != nil || len(openMR) != 1 || openMR[0].Number != 3 {
		t.Fatalf("glab ListOpenPRs = %+v, %v", openMR, err)
	}
	mergedMR, err := r.core.ListMergedPRHeads(gl, 10)
	if err != nil || len(mergedMR) != 1 || mergedMR[0].HeadOid != strings.Repeat("e", 40) {
		t.Fatalf("glab ListMergedPRHeads = %+v, %v", mergedMR, err)
	}
}

func TestUnimplementedInvocationSurfacesItsArgvToTheApp(t *testing.T) {
	r := newRig(t, githubFixture())
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/acme/widgets.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v\n%s", err, out)
	}
	_, err := r.core.CreatePR(dir, "Title", "Body", "main", false)
	if err == nil || !strings.Contains(err.Error(), `unhandled gh invocation`) || !strings.Contains(err.Error(), `gh "pr" "create" "--title" "Title"`) {
		t.Fatalf("CreatePR error = %v, want the fake's unhandled report with the argv", err)
	}
	log := r.engine.Invocations(0).Invocations
	if len(log) != 1 || !log[0].Unhandled || log[0].Args[0] != "pr" || log[0].Cwd == "" {
		t.Fatalf("recorded %+v", log)
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
