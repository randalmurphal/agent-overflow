package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// tagSpawnRepo is a clone whose origin/main, tag v1.0 and local main are
// three different commits, with a branch "both" at origin/main and a tag
// "both" at v1.0.
func tagSpawnRepo(t *testing.T) (repo, originMain, tagged, localMain string) {
	t.Helper()
	repo, _ = initGitRepoWithOrigin(t)
	originMain = gitRevParse(t, repo, "origin/main^{commit}")
	runGit(t, repo, "commit", "--allow-empty", "-m", "tagged")
	runGit(t, repo, "tag", "v1.0")
	runGit(t, repo, "tag", "both")
	runGit(t, repo, "branch", "both", originMain)
	runGit(t, repo, "commit", "--allow-empty", "-m", "after the tag")
	return repo, originMain, gitRevParse(t, repo, "v1.0^{commit}"), gitRevParse(t, repo, "main")
}

// detached reports whether the checkout at dir is on no branch.
func detached(dir string) bool {
	return testutil.RunGitAllowError(dir, "symbolic-ref", "--quiet", "HEAD") != nil
}

// TestThreadSpawnChecksATagOutDetached: a worktree name that is no branch
// but is a tag, bare or spelled refs/tags/<name>, gets a fresh worktree
// detached at the tag and a thread on no branch; a name that is both a
// branch and a tag is the branch; base and base_local are refused for a
// tag, as is a tag spelling no tag has.
func TestThreadSpawnChecksATagOutDetached(t *testing.T) {
	t.Parallel()
	repo, originMain, tagged, _ := tagSpawnRepo(t)
	f := newRequestFixtureIn(t, repo)
	f.mockClaude(t, "on it")
	spawn := func(call threadtools.SpawnCall) store.Thread {
		t.Helper()
		call.Prompt = "look"
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), call)
		if err != nil {
			t.Fatalf("Spawn(%+v): %v", call, err)
		}
		spawned, err := f.app.store.GetThread(ack.ThreadID)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		return spawned
	}

	bare := spawn(threadtools.SpawnCall{WorktreeBranch: "v1.0"})
	qualified := spawn(threadtools.SpawnCall{WorktreeBranch: "refs/tags/v1.0"})
	for name, thread := range map[string]store.Thread{"bare": bare, "refs/tags/": qualified} {
		if thread.WorktreePath == "" || !gitops.SameFilesystemPath(thread.WorkspacePath, thread.WorktreePath) {
			t.Fatalf("%s tag: workspace %q worktree %q, want a fresh worktree", name, thread.WorkspacePath, thread.WorktreePath)
		}
		if thread.Branch != "" {
			t.Errorf("%s tag: thread branch = %q, want none", name, thread.Branch)
		}
		if head := gitRevParse(t, thread.WorktreePath, "HEAD"); head != tagged || !detached(thread.WorktreePath) {
			t.Errorf("%s tag: HEAD %s detached=%v, want detached at %s", name, head, detached(thread.WorktreePath), tagged)
		}
		if base := filepath.Base(thread.WorktreePath); !strings.HasPrefix(base, "v1.0") {
			t.Errorf("%s tag: worktree directory %q, want one named after the tag", name, base)
		}
	}
	if gitops.SameFilesystemPath(bare.WorktreePath, qualified.WorktreePath) {
		t.Errorf("two spawns at one tag share %s, want a worktree each", bare.WorktreePath)
	}
	if branchExists(repo, "v1.0") {
		t.Error("a tag spawn made a branch named after the tag")
	}

	both := spawn(threadtools.SpawnCall{WorktreeBranch: "both"})
	if both.Branch != "both" || both.WorktreePath == "" {
		t.Fatalf("branch and tag of one name: worktree %q branch %q, want a checkout of the branch", both.WorktreePath, both.Branch)
	}
	if head := gitRevParse(t, both.WorktreePath, "HEAD"); head != originMain || detached(both.WorktreePath) {
		t.Errorf("branch and tag of one name: HEAD %s detached=%v, want branch both at %s", head, detached(both.WorktreePath), originMain)
	}

	rows := func() int {
		requests, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 100, 0)
		if err != nil {
			t.Fatalf("ListThreadRequestsByCaller: %v", err)
		}
		return len(requests)
	}
	runGit(t, repo, "branch", "rel", originMain)
	runGit(t, repo, "tag", "rel", tagged)
	before := rows()
	for name, tc := range map[string]struct {
		call threadtools.SpawnCall
		want string
	}{
		"branch of a tag's name, not checked out, with base": {threadtools.SpawnCall{Prompt: "x", WorktreeBranch: "rel", WorktreeBase: "main"}, `Branch "rel" already exists`},
		"tag with base":               {threadtools.SpawnCall{Prompt: "x", WorktreeBranch: "v1.0", WorktreeBase: "main"}, `"v1.0" is a tag, so worktree checks it out as it is`},
		"tag with base_local":         {threadtools.SpawnCall{Prompt: "x", WorktreeBranch: "v1.0", WorktreeBaseLocal: true}, `"v1.0" is a tag, so worktree checks it out as it is`},
		"refs/tags/ tag with base":    {threadtools.SpawnCall{Prompt: "x", WorktreeBranch: "refs/tags/v1.0", WorktreeBase: "main"}, "is a tag"},
		"branch of a tag's name+base": {threadtools.SpawnCall{Prompt: "x", WorktreeBranch: "both", WorktreeBase: "main"}, `Branch "both" is checked out at`},
		"missing tag":                 {threadtools.SpawnCall{Prompt: "x", WorktreeBranch: "refs/tags/nope"}, `Neither this computer nor origin has a tag "nope"`},
		"unknown base":                {threadtools.SpawnCall{Prompt: "x", WorktreeBranch: "b", WorktreeBase: "nowhere"}, "Name an existing branch or tag as base"},
		"unknown local base":          {threadtools.SpawnCall{Prompt: "x", WorktreeBranch: "b", WorktreeBase: "nowhere", WorktreeBaseLocal: true}, "no local branch or tag"},
	} {
		_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), tc.call)
		if code := publicCode(t, err); code != threadtools.CodeInvalidRequest || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: code %q, error %v, want an invalid request saying %q", name, code, err, tc.want)
		}
	}
	if after := rows(); after != before {
		t.Errorf("refused spawns left %d request rows behind", after-before)
	}
}

// TestThreadSpawnCutsANewBranchFromATag: base resolves a branch first and
// then a tag, bare or spelled refs/tags/<name>; a tag base is local, so
// base_local takes it too, and the new branch has no upstream.
func TestThreadSpawnCutsANewBranchFromATag(t *testing.T) {
	t.Parallel()
	repo, originMain, tagged, _ := tagSpawnRepo(t)
	f := newRequestFixtureIn(t, repo)
	f.mockClaude(t, "on it")

	for name, tc := range map[string]struct {
		call threadtools.SpawnCall
		head string
	}{
		"bare tag":                 {threadtools.SpawnCall{WorktreeBranch: "from-tag", WorktreeBase: "v1.0"}, tagged},
		"refs/tags/ tag":           {threadtools.SpawnCall{WorktreeBranch: "from-qualified", WorktreeBase: "refs/tags/v1.0"}, tagged},
		"tag with base_local":      {threadtools.SpawnCall{WorktreeBranch: "from-tag-local", WorktreeBase: "v1.0", WorktreeBaseLocal: true}, tagged},
		"branch and tag, local":    {threadtools.SpawnCall{WorktreeBranch: "from-both-local", WorktreeBase: "both", WorktreeBaseLocal: true}, originMain},
		"branch and tag, fetched":  {threadtools.SpawnCall{WorktreeBranch: "from-both", WorktreeBase: "both"}, originMain},
		"qualified tag over local": {threadtools.SpawnCall{WorktreeBranch: "from-both-tag", WorktreeBase: "refs/tags/both", WorktreeBaseLocal: true}, tagged},
	} {
		tc.call.Prompt = "fix"
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), tc.call)
		if err != nil {
			t.Fatalf("%s: Spawn: %v", name, err)
		}
		spawned, err := f.app.store.GetThread(ack.ThreadID)
		if err != nil {
			t.Fatalf("%s: GetThread: %v", name, err)
		}
		if spawned.Branch != tc.call.WorktreeBranch || spawned.WorktreePath == "" {
			t.Fatalf("%s: worktree %q branch %q, want a fresh %s", name, spawned.WorktreePath, spawned.Branch, tc.call.WorktreeBranch)
		}
		if head := gitRevParse(t, spawned.WorktreePath, "HEAD"); head != tc.head {
			t.Errorf("%s: starts at %s, want %s", name, head, tc.head)
		}
		hasUpstream := testutil.RunGitAllowError(spawned.WorktreePath, "rev-parse", "--verify", "--quiet", "@{upstream}") == nil
		if tc.head == tagged && hasUpstream {
			t.Errorf("%s: a branch cut from a tag has an upstream", name)
		}
	}
}

// TestThreadSpawnChecksOutATagOnlyOriginHad: a tag the checkout has not
// fetched yet is resolved after the fetch that brings it, so the thread
// runs detached at the tag rather than on a new branch of its name cut
// from the current branch.
func TestThreadSpawnChecksOutATagOnlyOriginHad(t *testing.T) {
	t.Parallel()
	repo, bare := initGitRepoWithOrigin(t)
	sibling := t.TempDir()
	testutil.RunGit(t, sibling, "clone", "-q", bare, ".")
	commit := func(message string) {
		testutil.RunGit(t, sibling, "-c", "user.email=s@example.com", "-c", "user.name=S", "commit", "-q", "--allow-empty", "-m", message)
	}
	commit("tagged upstream")
	testutil.RunGit(t, sibling, "tag", "T")
	tagged := gitRevParse(t, sibling, "HEAD")
	commit("after the tag")
	testutil.RunGit(t, sibling, "push", "-q", "origin", "main", "refs/tags/T")
	if testutil.RunGitAllowError(repo, "rev-parse", "--verify", "--quiet", "refs/tags/T") == nil {
		t.Fatal("the checkout already has tag T")
	}

	f := newRequestFixtureIn(t, repo)
	f.mockClaude(t, "on it")
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "look", WorktreeBranch: "T"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	spawned, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if spawned.WorktreePath == "" || spawned.Branch != "" {
		t.Fatalf("worktree %q branch %q, want a fresh worktree on no branch", spawned.WorktreePath, spawned.Branch)
	}
	if head := gitRevParse(t, spawned.WorktreePath, "HEAD"); head != tagged || !detached(spawned.WorktreePath) {
		t.Errorf("HEAD %s detached=%v, want detached at the tag's %s", head, detached(spawned.WorktreePath), tagged)
	}
	if branchExists(repo, "T") {
		t.Error("the spawn created a branch named after the tag")
	}
}

// TestThreadSpawnAddressesABranchNamedLikeATag: refs/heads/<name> names a
// branch explicitly, so a branch literally named refs/tags/v1.0 is
// reachable beside the tag v1.0, which refs/tags/v1.0 still names.
func TestThreadSpawnAddressesABranchNamedLikeATag(t *testing.T) {
	t.Parallel()
	repo, _, tagged, localMain := tagSpawnRepo(t)
	runGit(t, repo, "branch", "refs/tags/v1.0", localMain)
	f := newRequestFixtureIn(t, repo)
	f.mockClaude(t, "on it")
	spawn := func(worktree string) store.Thread {
		t.Helper()
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "look", WorktreeBranch: worktree})
		if err != nil {
			t.Fatalf("Spawn(%s): %v", worktree, err)
		}
		spawned, err := f.app.store.GetThread(ack.ThreadID)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		return spawned
	}
	branch := spawn("refs/heads/refs/tags/v1.0")
	if branch.Branch != "refs/tags/v1.0" || branch.WorktreePath == "" || detached(branch.WorktreePath) {
		t.Errorf("explicit branch: worktree %q branch %q, want a checkout of branch refs/tags/v1.0", branch.WorktreePath, branch.Branch)
	} else if head := gitRevParse(t, branch.WorktreePath, "HEAD"); head != localMain {
		t.Errorf("explicit branch HEAD = %s, want %s", head, localMain)
	}
	tag := spawn("refs/tags/v1.0")
	if tag.Branch != "" || tag.WorktreePath == "" || !detached(tag.WorktreePath) {
		t.Errorf("tag: worktree %q branch %q, want detached", tag.WorktreePath, tag.Branch)
	} else if head := gitRevParse(t, tag.WorktreePath, "HEAD"); head != tagged {
		t.Errorf("tag HEAD = %s, want %s", head, tagged)
	}
}

// TestThreadSpawnKeepsResolvedBranchNamesVerbatim: a name that resolution
// or git produced is created and recorded exactly, never tidied again:
// refs/heads/refs/heads/topic is the branch refs/heads/topic, and a branch
// whose segments start with "_" keeps them, on a fresh checkout and when a
// later spawn reuses it.
func TestThreadSpawnKeepsResolvedBranchNamesVerbatim(t *testing.T) {
	t.Parallel()
	repo, _, tagged, _ := tagSpawnRepo(t)
	runGit(t, repo, "branch", "_wip/x")
	f := newRequestFixtureIn(t, repo)
	f.mockClaude(t, "on it")
	spawn := func(call threadtools.SpawnCall) store.Thread {
		t.Helper()
		call.Prompt = "look"
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), call)
		if err != nil {
			t.Fatalf("Spawn(%+v): %v", call, err)
		}
		spawned, err := f.app.store.GetThread(ack.ThreadID)
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		return spawned
	}
	onBranch := func(label string, thread store.Thread, want string) {
		t.Helper()
		if thread.Branch != want || thread.WorktreePath == "" {
			t.Errorf("%s: worktree %q branch %q, want branch %q", label, thread.WorktreePath, thread.Branch, want)
			return
		}
		if head := gitOutput(t, thread.WorktreePath, "symbolic-ref", "HEAD"); strings.TrimSpace(head) != "refs/heads/"+want {
			t.Errorf("%s: git is on %q, want refs/heads/%s", label, strings.TrimSpace(head), want)
		}
	}

	runGit(t, repo, "branch", "refs/heads/held")
	onBranch("existing qualified branch", spawn(threadtools.SpawnCall{WorktreeBranch: "refs/heads/refs/heads/held"}), "refs/heads/held")
	onBranch("reused existing qualified branch", spawn(threadtools.SpawnCall{WorktreeBranch: "refs/heads/refs/heads/held"}), "refs/heads/held")

	onBranch("existing _wip/x", spawn(threadtools.SpawnCall{WorktreeBranch: "_wip/x"}), "_wip/x")
	onBranch("reused _wip/x", spawn(threadtools.SpawnCall{WorktreeBranch: "_wip/x"}), "_wip/x")

	created := spawn(threadtools.SpawnCall{WorktreeBranch: "refs/heads/refs/heads/topic", WorktreeBase: "refs/tags/v1.0"})
	onBranch("new qualified branch", created, "refs/heads/topic")
	if head, _, err := gitops.NewCore().Execute(repo, "rev-parse", "--verify", "refs/heads/refs/heads/topic"); err != nil || strings.TrimSpace(head) != tagged {
		t.Errorf("refs/heads/topic starts at %q (%v), want the tag's %s", strings.TrimSpace(head), err, tagged)
	}
	// show-ref, not rev-parse: rev-parse would also read refs/heads/topic
	// as the branch named refs/heads/topic.
	if testutil.RunGitAllowError(repo, "show-ref", "--verify", "--quiet", "refs/heads/topic") == nil {
		t.Error("the spawn created branch topic")
	}
	onBranch("reused new qualified branch", spawn(threadtools.SpawnCall{WorktreeBranch: "refs/heads/refs/heads/topic"}), "refs/heads/topic")

	// A typed plain name for a new branch is still tidied, once.
	onBranch("typed new name", spawn(threadtools.SpawnCall{WorktreeBranch: "_new thing"}), "new-thing")
	sidebar, err := f.app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: f.caller.ProjectID, Provider: string(provider.Claude), Model: "claude-opus-4-7",
		Mode: threadmode.ModeChat, WorktreeBranch: "_side thing",
	})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	onBranch("sidebar typed new name", sidebar, "side-thing")
}

// TestCreateThreadRefusesAnInvalidTagRef: a worktree spelled as a tag ref
// that git would refuse fails the create and leaves no thread, branch or
// worktree, rather than being tidied into an unrelated branch.
func TestCreateThreadRefusesAnInvalidTagRef(t *testing.T) {
	t.Parallel()
	dbPath := storetest.ClonePath(t)
	app, _ := setupE2EAppOn(t, dbPath, t.TempDir())
	repo, _, _, _ := tagSpawnRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	branchesBefore := gitOutput(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads")
	for _, worktree := range []string{"refs/tags/v1..0", "refs/heads/a..b"} {
		thread, err := app.CreateThread(t.Context(), CreateThreadOptions{
			ProjectID: project.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7",
			Mode: threadmode.ModeChat, WorktreeBranch: worktree,
		})
		if err == nil {
			t.Errorf("CreateThread(%s) = thread %s on %q, want a refusal", worktree, thread.ID, thread.Branch)
		}
	}
	if threads, err := app.store.ListThreadsByActivity(store.ThreadSearchFilter{ProjectID: project.ID}); err != nil || len(threads) != 0 {
		t.Errorf("threads = %d (%v), want none", len(threads), err)
	}
	if after := gitOutput(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads"); after != branchesBefore {
		t.Errorf("branches after the refusals:\n%s\nwant:\n%s", after, branchesBefore)
	}
	if worktrees, err := app.gitCore().ListWorktrees(repo); err != nil || len(worktrees) != 1 {
		t.Errorf("worktrees = %+v (%v), want the main checkout alone", worktrees, err)
	}
}

// TestTagWorktreeCutIsUndoneWithoutDeletingABranch: a thread row that cannot
// be written after a tag worktree was cut takes the worktree back and
// deletes no branch.
func TestTagWorktreeCutIsUndoneWithoutDeletingABranch(t *testing.T) {
	t.Parallel()
	dbPath := storetest.ClonePath(t)
	app, _ := setupE2EAppOn(t, dbPath, t.TempDir())
	repo, _, _, _ := tagSpawnRepo(t)
	project, err := app.ensureProjectForWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	branchesBefore := gitOutput(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads")
	execOnStore(t, dbPath, `CREATE TRIGGER refuse_thread BEFORE INSERT ON threads BEGIN SELECT RAISE(ABORT, 'injected create failure'); END`)

	_, err = app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: project.ID, Provider: string(provider.Claude), Model: "claude-opus-4-7",
		Mode: threadmode.ModeChat, WorktreeBranch: gitops.TagRef("both").Wire(),
	})
	if err == nil || !strings.Contains(err.Error(), "injected create failure") {
		t.Fatalf("CreateThread = %v, want the row's failure", err)
	}
	if worktrees, err := app.gitCore().ListWorktrees(repo); err != nil || len(worktrees) != 1 {
		t.Fatalf("worktrees = %+v (%v), want the main checkout alone", worktrees, err)
	}
	if entries, err := os.ReadDir(app.worktreesBaseDir(repo)); err == nil && len(entries) != 0 {
		t.Errorf("worktree directory left behind: %v", entries)
	}
	if after := gitOutput(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads"); after != branchesBefore {
		t.Errorf("branches after the undo:\n%s\nwant:\n%s", after, branchesBefore)
	}
}
