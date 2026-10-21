package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
)

// prThreadCase is a project whose origin reads as github.com/acme/widgets
// (the forge detection cache holds that identity) while its fetches go to
// a local bare repository, with the fake forge behind the API.
type prThreadCase struct {
	app     *App
	dbPath  string
	repo    string
	bare    string
	project store.Project
	engine  *forgefake.Engine
}

var openPRThreadPR = gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "widgets", Number: 5}

func newPRThreadCase(t *testing.T) *prThreadCase {
	t.Helper()
	app, dbPath := newTestAppWithStorePath(t)
	repo, bare := initGitRepoWithOrigin(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatalf("ensureProjectForWorkspace: %v", err)
	}
	c := &prThreadCase{app: app, dbPath: dbPath, repo: repo, bare: bare, project: project}
	c.engine = forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	c.seed(t, "open")
	useForgefake(t, app, c.engine)
	testutil.RunGit(t, repo, "remote", "set-url", "origin", "https://github.com/acme/widgets.git")
	if forge := app.gitCore().DetectForge(repo); forge != "github" {
		t.Fatalf("DetectForge = %q", forge)
	}
	testutil.RunGit(t, repo, "remote", "set-url", "origin", bare)
	return c
}

func (c *prThreadCase) seed(t *testing.T, state string) {
	t.Helper()
	if _, err := c.engine.Seed(forgefake.Fixture{Repos: []forgefake.Repo{{Forge: "github", Project: "acme/widgets", Pulls: []forgefake.Pull{
		{Number: 5, Title: "Fork work", HeadRef: "fork-work", HeadRepo: "carol/widgets", State: state, NodeID: "PR_five"},
	}}}}); err != nil {
		t.Fatal(err)
	}
}

// push commits a file on a scratch branch of the clone and pushes it to
// origin as ref, leaving no local branch behind.
func (c *prThreadCase) push(t *testing.T, ref string) {
	t.Helper()
	testutil.RunGit(t, c.repo, "checkout", "-q", "-b", "scratch-push")
	if err := os.WriteFile(filepath.Join(c.repo, strings.ReplaceAll(ref, "/", "-")+".txt"), []byte(ref+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, c.repo, "add", ".")
	testutil.RunGit(t, c.repo, "-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "-m", ref)
	testutil.RunGit(t, c.repo, "push", "-q", "origin", "scratch-push:"+ref)
	testutil.RunGit(t, c.repo, "checkout", "-q", "main")
	testutil.RunGit(t, c.repo, "branch", "-q", "-D", "scratch-push")
}

func (c *prThreadCase) request(head string, cross bool) OpenPRThreadRequest {
	return OpenPRThreadRequest{
		ProjectID: c.project.ID, PR: openPRThreadPR, HeadRefName: head, CrossRepository: cross,
		URL: "https://github.com/acme/widgets/pull/5", NodeID: "PR_five", Title: "Fork work",
	}
}

func (c *prThreadCase) threads(t *testing.T) []store.Thread {
	t.Helper()
	threads, err := c.app.store.ListThreadsByProject(c.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	return threads
}

func branchExists(repo, branch string) bool {
	return testutil.RunGitAllowError(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) == nil
}

// A head in the repository is checked out as its branch, fetched from
// origin, and the PR is recorded for the thread; "Threads on this MR"
// finds it beside a thread already on that branch.
func TestOpenPRThreadOnABranchOfTheRepository(t *testing.T) {
	t.Parallel()
	c := newPRThreadCase(t)
	c.push(t, "refs/heads/feature-x")

	result, err := c.app.OpenPRThread(context.Background(), c.request("feature-x", false))
	if err != nil {
		t.Fatalf("OpenPRThread: %v", err)
	}
	if result.Branch != "feature-x" || result.WorktreePath == "" || result.ThreadID == "" {
		t.Fatalf("result = %+v", result)
	}
	if out := c.app.gitCore().CurrentBranch(result.WorktreePath); out != "feature-x" {
		t.Fatalf("worktree branch = %q", out)
	}
	row, ok, err := c.app.store.GetThreadPullRequest(result.ThreadID)
	if err != nil || !ok || row.Number != 5 || row.Project != "acme/widgets" || row.Branch != "feature-x" || row.NodeID != "PR_five" || !samePath(row.WorktreePath, result.WorktreePath) {
		t.Fatalf("tracked PR = %+v ok=%v err=%v", row, ok, err)
	}

	// A thread already on the branch in the project root.
	other := testThread("on-the-branch")
	other.ProjectID, other.WorkspacePath, other.Branch = c.project.ID, c.repo, "feature-x"
	other.UpdatedAt = time.Now().Add(time.Hour).UnixMilli()
	if err := c.app.store.CreateThread(other); err != nil {
		t.Fatal(err)
	}
	listed, err := c.app.ListThreadsOnPR(context.Background(), PRThreadsRequest{ProjectID: c.project.ID, PR: openPRThreadPR, HeadRefName: "feature-x"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, thread := range listed {
		ids = append(ids, thread.ThreadID)
		if thread.Status == "" || thread.WorktreeBranch != "feature-x" {
			t.Fatalf("thread = %+v", thread)
		}
	}
	if !slices.Equal(ids, []string{other.ID, result.ThreadID}) {
		t.Fatalf("threads on the PR = %v, want %v then the opened thread, each once", ids, other.ID)
	}
}

// A head branch origin does not have is refused before anything is made:
// the create path would cut a new branch of that name from main.
func TestOpenPRThreadRefusesAHeadBranchOriginLacks(t *testing.T) {
	t.Parallel()
	c := newPRThreadCase(t)
	if _, err := c.app.OpenPRThread(context.Background(), c.request("not-pushed", false)); err == nil || !strings.Contains(err.Error(), "not-pushed") {
		t.Fatalf("OpenPRThread = %v", err)
	}
	if threads := c.threads(t); len(threads) != 0 || branchExists(c.repo, "not-pushed") {
		t.Fatalf("threads = %d, branch made = %v", len(threads), branchExists(c.repo, "not-pushed"))
	}
}

// A fork's head is fetched from the forge's head ref into pr/N, and the
// recorded PR is the only link to it: the merged-PR cleanup reads its
// state by node id and removes the worktree once it merged.
func TestOpenPRThreadFetchesAForkHeadAndMergeCleanupFindsIt(t *testing.T) {
	t.Parallel()
	c := newPRThreadCase(t)
	c.push(t, "refs/pull/5/head")
	head := strings.TrimSpace(gitOutput(t, c.bare, "rev-parse", "refs/pull/5/head"))

	result, err := c.app.OpenPRThread(context.Background(), c.request("fork-work", true))
	if err != nil {
		t.Fatalf("OpenPRThread: %v", err)
	}
	if result.Branch != "pr/5" {
		t.Fatalf("branch = %q, want pr/5", result.Branch)
	}
	if got := strings.TrimSpace(gitOutput(t, result.WorktreePath, "rev-parse", "HEAD")); got != head {
		t.Fatalf("worktree head = %s, want the PR head %s", got, head)
	}
	listed, err := c.app.ListThreadsOnPR(context.Background(), PRThreadsRequest{ProjectID: c.project.ID, PR: openPRThreadPR, HeadRefName: "fork-work", CrossRepository: true})
	if err != nil || len(listed) != 1 || listed[0].ThreadID != result.ThreadID || listed[0].WorktreeBranch != "pr/5" {
		t.Fatalf("threads on the PR = %+v, %v", listed, err)
	}

	c.seed(t, "merged")
	c.app.mergedPRPoll(context.Background(), time.Now())
	if row, _, _ := c.app.store.GetThreadPullRequest(result.ThreadID); row.MergedAt == 0 {
		t.Fatalf("the poll did not see the merge: %+v", row)
	}
	c.app.mergedPRActPass(context.Background())
	row, _, err := c.app.store.GetThreadPullRequest(result.ThreadID)
	if err != nil || row.Outcome != store.PROutcomeRemoved {
		t.Fatalf("outcome = %q (%v), want removed", row.Outcome, err)
	}
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
}

// A pr/N at another commit may hold work: refused, nothing made.
func TestOpenPRThreadRefusesADivergedPRBranch(t *testing.T) {
	t.Parallel()
	c := newPRThreadCase(t)
	c.push(t, "refs/pull/5/head")
	testutil.RunGit(t, c.repo, "branch", "pr/5", "main")
	if _, err := c.app.OpenPRThread(context.Background(), c.request("fork-work", true)); !errors.Is(err, gitops.ErrPRBranchDiverged) {
		t.Fatalf("OpenPRThread = %v, want ErrPRBranchDiverged", err)
	}
	if threads := c.threads(t); len(threads) != 0 {
		t.Fatalf("threads = %d", len(threads))
	}
}

// A failure once the thread exists takes back the thread, its worktree
// and the pr/N the call made, and returns the cause.
func TestOpenPRThreadRollsBackWhenTheRecordFails(t *testing.T) {
	t.Parallel()
	c := newPRThreadCase(t)
	c.push(t, "refs/pull/5/head")
	execOnStore(t, c.dbPath, `CREATE TRIGGER refuse_pr_record BEFORE INSERT ON thread_pull_requests BEGIN SELECT RAISE(ABORT, 'injected record failure'); END`)

	_, err := c.app.OpenPRThread(context.Background(), c.request("fork-work", true))
	if err == nil || !strings.Contains(err.Error(), "injected record failure") {
		t.Fatalf("OpenPRThread = %v, want the record's failure", err)
	}
	if threads := c.threads(t); len(threads) != 0 {
		t.Fatalf("threads left = %+v", threads)
	}
	if branchExists(c.repo, "pr/5") {
		t.Fatal("pr/5 was left behind")
	}
	worktrees, err := c.app.gitCore().ListWorktrees(c.repo)
	if err != nil || len(worktrees) != 1 {
		t.Fatalf("worktrees = %+v (%v), want the main checkout alone", worktrees, err)
	}
}

func TestOpenPRThreadValidatesBeforeAnySideEffect(t *testing.T) {
	t.Parallel()
	c := newPRThreadCase(t)
	c.push(t, "refs/heads/feature-x")
	cases := map[string]func(*OpenPRThreadRequest){
		"no url":            func(r *OpenPRThreadRequest) { r.URL = "" },
		"github no node id": func(r *OpenPRThreadRequest) { r.NodeID = "" },
		"no project":        func(r *OpenPRThreadRequest) { r.ProjectID = "" },
		"bad head branch":   func(r *OpenPRThreadRequest) { r.HeadRefName = "-x" },
		"another repository": func(r *OpenPRThreadRequest) {
			r.PR = gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "other", Number: 5}
		},
	}
	for name, mutate := range cases {
		req := c.request("feature-x", false)
		mutate(&req)
		if _, err := c.app.OpenPRThread(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if threads := c.threads(t); len(threads) != 0 || branchExists(c.repo, "feature-x") {
		t.Fatalf("a refused request made a thread (%d) or the branch", len(threads))
	}
}

// Opening a thread needs threads:operate and listing them threads:read,
// beside the git:operate floor the methods carry.
func TestOpenPRThreadAndListThreadsOnPRNeedTheThreadScopes(t *testing.T) {
	t.Parallel()
	app := identityApp(t)
	gitOnly := pairSessionWithScopes(t, app, "thumb-git", []identity.Scope{identity.ScopeGitOperate})
	_, err := app.OpenPRThread(callFrom(gitOnly.ID, false), OpenPRThreadRequest{})
	wantScopeRefusal(t, err, transport.ScopeThreadsOperate)
	_, err = app.ListThreadsOnPR(callFrom(gitOnly.ID, false), PRThreadsRequest{})
	wantScopeRefusal(t, err, transport.ScopeThreadsRead)
}
