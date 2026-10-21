package threadapp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/chatmodel"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

type testModels struct{}

func (m *testModels) Seed(_ context.Context, providerName, model string) store.ChatModelProfile {
	return chatmodel.FallbackProfile(providerName, model, string(provider.Claude))
}
func (m *testModels) Sanitize(profile store.ChatModelProfile) store.ChatModelProfile {
	return chatmodel.SanitizeProfile(profile)
}
func (m *testModels) SupportsReasoningEffort(providerName, model, effort string) bool {
	return provider.ReasoningEffortSupportedForModel(providerName, model, effort)
}
func (m *testModels) CoerceReasoningEffort(providerName, model, effort string) string {
	return string(provider.CoerceReasoningEffortForModel(providerName, model, provider.NormalizeReasoningEffort(effort)))
}
func (m *testModels) SupportsFastMode(providerName, model string) bool {
	return chatmodel.SupportsStoredFastMode(providerName, model)
}
func (m *testModels) ContextWindowOptions(providerName, model string) []provider.ContextWindowOption {
	return chatmodel.ContextWindowOptions(providerName, model)
}
func (m *testModels) DraftDefaults(providerName, model, effort string, fastMode bool) (string, bool) {
	return m.CoerceReasoningEffort(providerName, model, effort), fastMode && m.SupportsFastMode(providerName, model)
}

func (m *testModels) DefaultModel(providerName string) string {
	return chatmodel.FallbackModelForProvider(providerName)
}

func (m *testModels) ValidateModel(providerName, model string) error {
	return provider.ValidateModelID(providerName, model, provider.ModelsForProvider(providerName))
}

type testWorkspace struct {
	currentBranch string
	findPath      string
	findBranch    string
	createPath    string
	createBranch  string
	// createErr fails the cut, and cuts records what each CreateWorktree call
	// was asked for, which is the only place a test can see that the caller's
	// base choice reached git.
	createErr error
	cuts      []WorktreeCut
	// origin is what ObserveOrigin reports for any path. Zero by default, so
	// a test that says nothing about git provenance asserts the "workspace is
	// not a repository" shape for free.
	origin store.ThreadOrigin
}

func (w *testWorkspace) CurrentBranch(string) string { return w.currentBranch }

func (w *testWorkspace) ObserveOrigin(string) store.ThreadOrigin { return w.origin }
func (w *testWorkspace) FindWorktree(string, string) (string, string, bool, error) {
	return w.findPath, w.findBranch, w.findPath != "", nil
}
func (w *testWorkspace) CreateWorktree(_ context.Context, _ string, cut WorktreeCut) (string, string, error) {
	w.cuts = append(w.cuts, cut)
	if w.createErr != nil {
		return "", "", w.createErr
	}
	return w.createPath, w.createBranch, nil
}

type setupRecorder struct {
	started []string
	store   *store.Store
}

func (r *setupRecorder) Start(thread store.Thread) {
	r.started = append(r.started, thread.ID)
	_ = r.store.SetThreadWorktreeSetupState(thread.ID, store.WorktreeSetupStateRunning)
}

type recentRecorder struct {
	paths   []string
	buckets []string
	classes []string
}

func (r *recentRecorder) AddRecentWorkspace(bucket, class, path string) {
	r.buckets = append(r.buckets, bucket)
	r.classes = append(r.classes, class)
	r.paths = append(r.paths, path)
}

func newServiceFixture(t *testing.T) (*Service, *store.Store, *testModels) {
	t.Helper()
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.CreateProject(store.Project{
		ID: "project", Path: "/repo", Name: "repo", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	models := &testModels{}
	service := New(Deps{
		Store:     database,
		Models:    models,
		Workspace: &testWorkspace{currentBranch: "main"},
		Now:       func() time.Time { return time.UnixMilli(1234) },
		NewID:     func() string { return "thread" },
	})
	return service, database, models
}

func TestCreatePreservesProjectAndWorkspaceDistinction(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	setup := &setupRecorder{store: database}
	recent := &recentRecorder{}
	service.deps.Workspace = &testWorkspace{
		findPath: "/repo-worktrees/feature", findBranch: "feature/one",
	}
	service.deps.WorktreeSetup = setup
	service.deps.RecentWorkspaces = recent

	thread, err := service.Create(CreateOptions{
		ProjectID: "project", WorktreePath: "/repo-worktrees/feature",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if thread.ProjectPath != "/repo" || thread.WorkspacePath != "/repo-worktrees/feature" || thread.WorktreePath != "/repo-worktrees/feature" {
		t.Fatalf("thread paths = project %q workspace %q worktree %q", thread.ProjectPath, thread.WorkspacePath, thread.WorktreePath)
	}
	if thread.Branch != "feature/one" {
		t.Fatalf("Branch = %q, want feature/one", thread.Branch)
	}
	if thread.WorktreeSetupState != store.WorktreeSetupStateNone {
		t.Fatalf("WorktreeSetupState = %q, want empty", thread.WorktreeSetupState)
	}
	if len(setup.started) != 0 {
		t.Fatalf("setup calls = started %v", setup.started)
	}
	if len(recent.paths) != 1 || recent.paths[0] != thread.WorkspacePath {
		t.Fatalf("recent paths = %v", recent.paths)
	}
}

func TestCreateStartsSetupOnlyForWorktreeItCuts(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	setup := &setupRecorder{store: database}
	workspace := &testWorkspace{
		createPath: "/repo-worktrees/new", createBranch: "feature/new",
	}
	service.deps.Workspace = workspace
	service.deps.WorktreeSetup = setup
	thread, err := service.Create(CreateOptions{
		ProjectID:         "project",
		WorktreeBranch:    "feature/new",
		WorktreeBase:      "  release/2  ",
		WorktreeBaseLocal: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if thread.WorkspacePath != "/repo-worktrees/new" || thread.Branch != "feature/new" {
		t.Fatalf("created worktree thread = %+v", thread)
	}
	if len(setup.started) != 1 {
		t.Fatalf("setup calls = started %v", setup.started)
	}
	want := WorktreeCut{Branch: "feature/new", Base: "release/2", BaseLocal: true}
	if len(workspace.cuts) != 1 || workspace.cuts[0] != want {
		t.Fatalf("cuts = %+v, want one %+v", workspace.cuts, want)
	}
}

// A worktree that cannot be cut has no thread to belong to, so the failure
// is the whole creation's failure: no row, no setup run, and the git reason
// travels out to the caller.
func TestCreateFailsWhenTheWorktreeCannotBeCut(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	setup := &setupRecorder{store: database}
	service.deps.Workspace = &testWorkspace{createErr: errors.New("branch already checked out")}
	service.deps.WorktreeSetup = setup

	_, err := service.Create(CreateOptions{ProjectID: "project", WorktreeBranch: "feature/new"})
	if err == nil {
		t.Fatal("Create reported a thread for a worktree git refused to cut")
	}
	if !strings.Contains(err.Error(), "branch already checked out") {
		t.Fatalf("Create error = %v, want git's reason", err)
	}
	threads, listErr := database.ListThreads()
	if listErr != nil {
		t.Fatalf("ListThreads: %v", listErr)
	}
	if len(threads) != 0 {
		t.Fatalf("threads = %+v, want none", threads)
	}
	if len(setup.started) != 0 {
		t.Fatalf("setup ran for a worktree that was never cut: %v", setup.started)
	}
}

func TestLifecycleAndWorkspaceWideBranchUpdate(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	first, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	service.deps.NewID = func() string { return "second" }
	second, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if err := database.InsertItem(store.Item{ID: "item", ThreadID: first.ID, TurnIndex: 0, Kind: "user_text", Role: "user", Summary: "hello", CreatedAt: 1}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	listed, err := service.List()
	if err != nil || len(listed) != 1 || listed[0].ID != first.ID {
		t.Fatalf("List = %+v, %v; want only materialized thread", listed, err)
	}
	rows, err := service.UpdateBranch("/repo", "feature/shared")
	if err != nil || len(rows) != 2 {
		t.Fatalf("UpdateBranch = %+v, %v", rows, err)
	}
	if _, _, err := service.Archive(first.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	archived, err := service.ListArchived()
	if err != nil || len(archived) != 1 || archived[0].ID != first.ID {
		t.Fatalf("ListArchived = %+v, %v", archived, err)
	}
	if _, _, err := service.Unarchive(first.ID); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if _, _, err := service.Pin(second.ID); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if _, _, err := service.Unpin(second.ID); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
}

func TestModelSelectionClearsProviderStateAndHonorsProviderLock(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project", Provider: "claude", Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	thread.SessionRef = "claude-session"
	thread.PendingForkRef = "parent"
	thread.PendingForkResumeAt = "leaf"
	if err := database.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	update, err := service.UpdateModelSelection(thread.ID, "codex", "gpt-5.4")
	if err != nil {
		t.Fatalf("UpdateModelSelection: %v", err)
	}
	if !update.ProviderChanged() || update.Thread.SessionRef != "" || update.Thread.PendingForkRef != "" || update.Thread.PendingForkResumeAt != "" {
		t.Fatalf("provider switch result = %+v", update)
	}

	if err := database.InsertItem(store.Item{ID: "user", ThreadID: thread.ID, TurnIndex: 0, Kind: "user_text", Role: "user", Summary: "hello", CreatedAt: 1}); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	if _, err := service.UpdateProvider(thread.ID, "claude"); err == nil || err.Error() != "update provider: thread is locked to codex (start a new thread to use claude)" {
		t.Fatalf("UpdateProvider locked error = %v", err)
	}
}

func TestModePolicyKeepsThreadTypeImmutable(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := service.UpdateMode(thread.ID, "plan")
	if err != nil || updated.Thread.Mode != "plan" {
		t.Fatalf("UpdateMode = %+v, %v", updated, err)
	}
	thread.Mode = "workflow"
	if err := database.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread workflow: %v", err)
	}
	if _, err := service.UpdateMode(thread.ID, "chat"); err == nil {
		t.Fatal("UpdateMode(workflow -> chat) error = nil")
	}
}

func TestModePolicyValidatesSetAndCreateModes(t *testing.T) {
	service, _, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project", Mode: "plan"})
	if err != nil || thread.Mode != "plan" {
		t.Fatalf("Create(plan) = %+v, %v", thread, err)
	}
	for _, mode := range []string{"", "nonsense", "PLAN", "design", "workflow"} {
		if _, err := service.UpdateMode(thread.ID, mode); err == nil {
			t.Errorf("UpdateMode(%q) error = nil", mode)
		}
	}
	for _, mode := range []string{"design", "workflow", "bogus"} {
		service.deps.NewID = func() string { return "bad-" + mode }
		if _, err := service.Create(CreateOptions{ProjectID: "project", Mode: mode}); err == nil {
			t.Errorf("Create(%q) error = nil", mode)
		}
	}
}

func TestThreadLocksSerializeSameKeyAndAllowDifferentKeys(t *testing.T) {
	service := New(Deps{})
	unlock := service.Lock("one")

	acquired := make(chan struct{})
	release := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		secondUnlock := service.Lock("one")
		close(acquired)
		<-release
		secondUnlock()
	}()
	deadline := time.Now().Add(time.Second)
	for service.Refs("one") != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-acquired:
		t.Fatal("same-key waiter acquired while held")
	default:
	}
	otherUnlock := service.Lock("two")
	otherUnlock()
	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("same-key waiter did not acquire after release")
	}
	close(release)
	wait.Wait()
}

func TestEnsureCanForkStorePolicy(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := service.EnsureCanFork(thread, nil); err == nil {
		t.Fatal("EnsureCanFork on an empty thread = nil, want refusal")
	}
	item := store.Item{ID: "user", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Summary: "hello", CreatedAt: 1}
	if err := database.InsertItem(item); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	if err := service.EnsureCanFork(thread, nil); err != nil {
		t.Fatalf("EnsureCanFork(nil): %v", err)
	}
	for _, turn := range []int{0, 1} {
		if err := service.EnsureCanFork(thread, &turn); err != nil {
			t.Fatalf("EnsureCanFork(%d): %v", turn, err)
		}
	}
	for _, turn := range []int{-1, 2} {
		if err := service.EnsureCanFork(thread, &turn); err == nil {
			t.Fatalf("EnsureCanFork(%d) = nil, want refusal", turn)
		}
	}
}

func TestUpdateBranchMatchesCanonicalWorkspaceSpelling(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := symlinkForTest(realRoot, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	thread, err := service.Create(CreateOptions{ProjectID: "project", WorkspaceOverride: gitops.CanonicalPath(realRoot)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rows, err := service.UpdateBranch(link, "feature/canonical")
	if err != nil || len(rows) != 1 || rows[0].ID != thread.ID {
		t.Fatalf("UpdateBranch = %+v, %v", rows, err)
	}
	stored, err := database.GetThread(thread.ID)
	if err != nil || stored.Branch != "feature/canonical" {
		t.Fatalf("stored thread = %+v, %v", stored, err)
	}
}

// TestUpdateBranchReachesRowsStoredUnderAnotherSpelling is the other
// direction of the canonical match, and the one macOS exercises on every
// checkout: the ROW holds a spelling the caller never sees (`/var/...` from
// a temp dir, a symlinked checkout path) while the caller observes the
// resolved one. Matching the caller's spelling and its canonical form found
// nothing; the stored spellings have to be resolved too.
func TestUpdateBranchReachesRowsStoredUnderAnotherSpelling(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := symlinkForTest(realRoot, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	thread, err := service.Create(CreateOptions{ProjectID: "project", WorkspaceOverride: link})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if stored, _ := database.GetThread(thread.ID); stored.WorkspacePath != link {
		t.Fatalf("fixture: WorkspacePath = %q, want the link spelling %q", stored.WorkspacePath, link)
	}
	rows, err := service.UpdateBranch(gitops.CanonicalPath(realRoot), "feature/resolved")
	if err != nil || len(rows) != 1 || rows[0].ID != thread.ID {
		t.Fatalf("UpdateBranch = %+v, %v", rows, err)
	}
	stored, err := database.GetThread(thread.ID)
	if err != nil || stored.Branch != "feature/resolved" {
		t.Fatalf("stored thread = %+v, %v", stored, err)
	}
	// A row in an unrelated directory is untouched: resolving spellings must
	// widen the match to the same directory and nowhere else.
	service.deps.NewID = func() string { return "elsewhere" }
	other, err := service.Create(CreateOptions{ProjectID: "project", WorkspaceOverride: t.TempDir()})
	if err != nil {
		t.Fatalf("Create(other): %v", err)
	}
	if rows, err := service.UpdateBranch(link, "feature/again"); err != nil || len(rows) != 1 || rows[0].ID != thread.ID {
		t.Fatalf("UpdateBranch(link) = %+v, %v; want only the linked thread", rows, err)
	}
	if got, _ := database.GetThread(other.ID); got.Branch == "feature/again" {
		t.Fatal("a thread in another directory took the branch")
	}
}

// TestDefaultsCarriesItsCallersDeadlineIntoTheStore pins the half of the
// bound that lives here: the caller's context must reach the store reads.
// An expired context is the deterministic stand-in for the pool that is
// wedged long enough for the deadline to expire; internal/store covers a
// genuinely blocked read pool.
func TestDefaultsCarriesItsCallersDeadlineIntoTheStore(t *testing.T) {
	service, _, _ := newServiceFixture(t)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if _, err := service.Defaults(expired, CreateOptions{ProjectID: "project"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Defaults(expired ctx) = %v, want context.DeadlineExceeded", err)
	}
	// The same call under a live context still answers, so the failure above
	// is the deadline and not a broken fixture.
	defaults, err := service.Defaults(context.Background(), CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Defaults: %v", err)
	}
	if defaults.WorkspacePath != "/repo" {
		t.Fatalf("WorkspacePath = %q, want /repo", defaults.WorkspacePath)
	}
}

// A thread in a home has no project and the home as its workspace; it
// reads no branch or origin, joins no recent workspaces, and records the
// assistant it helps. Project, group, worktree, branch and workspace
// choices are refused beside a home, and a relative home is refused.
func TestCreateInAHome(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	recent := &recentRecorder{}
	service.deps.RecentWorkspaces = recent
	service.deps.Workspace = &testWorkspace{currentBranch: "main", origin: store.ThreadOrigin{Branch: "main", HeadCommit: "0123456789abcdef0123456789abcdef01234567"}}
	ids := []string{"assistant", "helper"}
	made := 0
	service.deps.NewID = func() string {
		made++
		if made <= len(ids) {
			return ids[made-1]
		}
		return fmt.Sprintf("thread-%d", made)
	}

	thread, err := service.Create(CreateOptions{Home: " /config/agents/a1 ", Title: "Ops"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	reloaded, err := database.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ProjectID != "" || reloaded.WorkspacePath != "/config/agents/a1" || reloaded.Branch != "" || !reloaded.Origin.IsZero() || reloaded.HelperOfAgentID != "" {
		t.Fatalf("home thread = project %q workspace %q branch %q origin %+v helperOf %q",
			reloaded.ProjectID, reloaded.WorkspacePath, reloaded.Branch, reloaded.Origin, reloaded.HelperOfAgentID)
	}
	if len(recent.paths) != 0 {
		t.Fatalf("a home joined the recent workspaces: %v", recent.paths)
	}
	if err := database.CreateStandingAgent(store.StandingAgent{ID: "a1", ThreadID: thread.ID, Home: "/config/agents/a1"}); err != nil {
		t.Fatal(err)
	}
	helper, err := service.Create(CreateOptions{Home: "/config/agents/a1", HelperOfAgentID: "a1"})
	if err != nil {
		t.Fatalf("Create helper: %v", err)
	}
	if reloaded, err := database.GetThread(helper.ID); err != nil || reloaded.HelperOfAgentID != "a1" {
		t.Fatalf("helper = %+v, %v", reloaded, err)
	}

	for name, opts := range map[string]CreateOptions{
		"project":   {Home: "/h", ProjectID: "project"},
		"group":     {Home: "/h", GroupID: "g"},
		"worktree":  {Home: "/h", WorktreeBranch: "feature"},
		"inherited": {Home: "/h", WorktreePath: "/repo-worktrees/feature"},
		"workspace": {Home: "/h", WorkspaceOverride: "/elsewhere"},
		"branch":    {Home: "/h", Branch: "feature"},
		"relative":  {Home: "agents/a1"},
	} {
		if _, err := service.Create(opts); err == nil {
			t.Errorf("%s: a thread was created", name)
		}
	}
	// A helper lives in its assistant's home, never in a project.
	for name, opts := range map[string]CreateOptions{
		"helper in a project": {ProjectID: "project", HelperOfAgentID: "a1"},
		"helper with no home": {HelperOfAgentID: "a1"},
	} {
		if _, err := service.Create(opts); err == nil || !strings.Contains(err.Error(), "a helper lives in its assistant's home") {
			t.Errorf("%s = %v, want the helper refused", name, err)
		}
	}
}

// An assistant's delete deletes what lives under it before the mark. A
// helper made after they went (a spawn that read the assistant before the
// pass) refuses the mark and sends the delete back for it, so the record
// goes with nothing left without it; one that keeps coming gives up with
// the assistant whole.
func TestDeletingAnAssistantDeletesWhatLivesUnderItFirst(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	next := 0
	service.deps.NewID = func() string { next++; return fmt.Sprintf("thread-%d", next) }
	assistant := func(t *testing.T, id string) store.Thread {
		t.Helper()
		home := "/config/agents/" + id
		thread, err := service.Create(CreateOptions{Home: home, Title: id})
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CreateStandingAgent(store.StandingAgent{ID: id, ThreadID: thread.ID, Home: home}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Create(CreateOptions{Home: home, HelperOfAgentID: id}); err != nil {
			t.Fatal(err)
		}
		return thread
	}
	// dependents deletes the threads under the assistant, as the app's
	// port does, and then lets late make one more.
	dependents := func(passes *int, late func(pass int) bool) DeletePorts {
		return DeletePorts{AssistantDependents: func(threadID string) error {
			*passes++
			agent, _, err := database.GetStandingAgent(threadID)
			if err != nil {
				return err
			}
			ids, err := database.AssistantDependentThreadIDs(agent.ID)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := service.DeleteThread(id, DeletePorts{}); err != nil {
					return err
				}
			}
			if late(*passes) {
				_, err = service.Create(CreateOptions{Home: agent.Home, HelperOfAgentID: agent.ID})
			}
			return err
		}}
	}

	ops := assistant(t, "ops")
	passes := 0
	if err := service.DeleteThread(ops.ID, dependents(&passes, func(pass int) bool { return pass == 1 })); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	if passes != 2 {
		t.Fatalf("passes = %d, want a second for the late helper", passes)
	}
	if outside, err := database.ThreadIDsOutsideProjects(); err != nil || len(outside) != 0 {
		t.Fatalf("threads left outside every project = %q %v", outside, err)
	}
	if _, found, err := database.GetAgent("ops"); err != nil || found {
		t.Fatalf("the record after the delete = %v %v", found, err)
	}

	busy := assistant(t, "busy")
	passes = 0
	err := service.DeleteThread(busy.ID, dependents(&passes, func(int) bool { return true }))
	var left *store.AssistantDependentsError
	if !errors.As(err, &left) || left.Helpers != 1 || passes != maxDependentPasses {
		t.Fatalf("a delete helpers keep coming to = %v after %d passes", err, passes)
	}
	if _, found, err := database.GetStandingAgent(busy.ID); err != nil || !found {
		t.Fatalf("the assistant after the delete gave up = %v %v", found, err)
	}
	if pending, err := database.ListPendingThreadDeletes(); err != nil || len(pending) != 0 {
		t.Fatalf("pending deletes = %q %v, want the assistant unmarked", pending, err)
	}
}
