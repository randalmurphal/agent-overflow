package threadapp

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/chatmodel"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

type testModels struct {
	remembered []store.Thread
}

func (m *testModels) Seed(providerName, model string) store.ChatModelProfile {
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
func (m *testModels) Remember(thread store.Thread) { m.remembered = append(m.remembered, thread) }

type testWorkspace struct {
	currentBranch string
	findPath      string
	findBranch    string
	createPath    string
	createBranch  string
	// origin is what ObserveOrigin reports for any path. Zero by default, so
	// a test that says nothing about git provenance asserts the "workspace is
	// not a repository" shape for free.
	origin store.ThreadOrigin
}

func (w testWorkspace) CurrentBranch(string) string { return w.currentBranch }

func (w testWorkspace) ObserveOrigin(string) store.ThreadOrigin { return w.origin }
func (w testWorkspace) FindWorktree(string, string) (string, string, bool, error) {
	return w.findPath, w.findBranch, w.findPath != "", nil
}
func (w testWorkspace) CreateWorktree(context.Context, string, string) (string, string, error) {
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

type pullRequestPort struct {
	workspace string
	project   store.Project
}

func (p pullRequestPort) ResolveWorkspace(gitops.PRReference) string { return p.workspace }
func (p pullRequestPort) Load(string, gitops.PRReference) (gitops.PRMetadata, string, error) {
	return gitops.PRMetadata{Title: "Fix the thing", Body: "Details"}, "diff --git a/a b/a", nil
}
func (p pullRequestPort) EnsureProject(string) (store.Project, error) { return p.project, nil }

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
		Workspace: testWorkspace{currentBranch: "main"},
		Now:       func() time.Time { return time.UnixMilli(1234) },
		NewID:     func() string { return "thread" },
	})
	return service, database, models
}

func TestCreatePreservesProjectAndWorkspaceDistinction(t *testing.T) {
	service, database, models := newServiceFixture(t)
	setup := &setupRecorder{store: database}
	recent := &recentRecorder{}
	service.deps.Workspace = testWorkspace{
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
	if len(models.remembered) != 1 || models.remembered[0].ID != thread.ID {
		t.Fatalf("remembered = %+v", models.remembered)
	}
}

func TestCreateStartsSetupOnlyForWorktreeItCuts(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	setup := &setupRecorder{store: database}
	service.deps.Workspace = testWorkspace{
		createPath: "/repo-worktrees/new", createBranch: "feature/new",
	}
	service.deps.WorktreeSetup = setup
	thread, err := service.Create(CreateOptions{ProjectID: "project", WorktreeBranch: "feature/new"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if thread.WorkspacePath != "/repo-worktrees/new" || thread.Branch != "feature/new" {
		t.Fatalf("created worktree thread = %+v", thread)
	}
	if len(setup.started) != 1 {
		t.Fatalf("setup calls = started %v", setup.started)
	}
}

func TestCreateFromPROwnsRowAndFirstItemSaga(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	ids := []string{"pr-thread", "pr-item"}
	service.deps.NewID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	project, err := database.GetProject("project")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	thread, err := service.CreateFromPR(
		PullRequestOptions{
			Project:  "owner/repo",
			Number:   42,
			Provider: "claude",
			Model:    "claude-sonnet-4-6",
			Forge:    "github",
		},
		pullRequestPort{workspace: "/repo", project: project},
	)
	if err != nil {
		t.Fatalf("CreateFromPR: %v", err)
	}
	if thread.Title != "PR #42: Fix the thing" || thread.PRRef == "" || thread.WorkspacePath != "/repo" {
		t.Fatalf("thread = %+v", thread)
	}
	item, found, err := database.GetThreadItem(thread.ID, "pr-item")
	if err != nil || !found || item.Kind != "user_text" || item.Role != "user" {
		t.Fatalf("first item = %+v, %v, %v", item, found, err)
	}
	bitbucket := PullRequestOptions{Project: "owner/repo", Number: 1, Provider: "claude", Forge: "bitbucket"}
	if _, err := service.CreateFromPR(bitbucket, pullRequestPort{}); err == nil {
		t.Fatal("CreateFromPR(bitbucket) error = nil")
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
	thread.Mode = "discussion"
	if err := database.UpdateThread(thread); err != nil {
		t.Fatalf("UpdateThread discussion: %v", err)
	}
	if _, err := service.UpdateMode(thread.ID, "chat"); err == nil {
		t.Fatal("UpdateMode(discussion -> chat) error = nil")
	}
}

func TestModePolicyValidatesSetAndCreateModes(t *testing.T) {
	service, _, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project", Mode: "plan"})
	if err != nil || thread.Mode != "plan" {
		t.Fatalf("Create(plan) = %+v, %v", thread, err)
	}
	for _, mode := range []string{"", "nonsense", "PLAN", "design", "discussion", "workflow"} {
		if _, err := service.UpdateMode(thread.ID, mode); err == nil {
			t.Errorf("UpdateMode(%q) error = nil", mode)
		}
	}
	for _, mode := range []string{"design", "discussion", "DISCUSSION", "workflow", "bogus"} {
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

func TestForkStorePolicyAndClaudeRemap(t *testing.T) {
	service, database, _ := newServiceFixture(t)
	thread, err := service.Create(CreateOptions{ProjectID: "project"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	meta, err := usermessage.MergeProviderIDs("", "old-user", "old-parent")
	if err != nil {
		t.Fatalf("MergeProviderIDs: %v", err)
	}
	item := store.Item{ID: "user", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Summary: "hello", Meta: meta, CreatedAt: 1}
	if err := database.InsertItem(item); err != nil {
		t.Fatalf("InsertItem: %v", err)
	}
	if err := database.UpsertMessageAnchor(store.MessageAnchor{
		ThreadID: thread.ID, UserItemID: item.ID, TurnIndex: 1,
		ProviderUserMessageID: "old-user", ProviderParentUUID: "old-parent", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertMessageAnchor: %v", err)
	}
	if err := service.EnsureCanFork(thread, nil); err != nil {
		t.Fatalf("EnsureCanFork: %v", err)
	}
	if err := service.ApplyClaudeProviderIDRemap(thread.ID, map[string]string{
		"old-user": "new-user", "old-parent": "new-parent",
	}); err != nil {
		t.Fatalf("ApplyClaudeProviderIDRemap: %v", err)
	}
	got, found, err := database.GetThreadItem(thread.ID, item.ID)
	if err != nil || !found {
		t.Fatalf("GetThreadItem = %+v, %v, %v", got, found, err)
	}
	if usermessage.ReadProviderItemID(got.Meta) != "new-user" || usermessage.ReadProviderParentUUID(got.Meta) != "new-parent" {
		t.Fatalf("remapped meta = %s", got.Meta)
	}
	anchor, found, err := database.GetMessageAnchor(thread.ID, item.ID)
	if err != nil || !found || anchor.ProviderUserMessageID != "new-user" || anchor.ProviderParentUUID != "new-parent" {
		t.Fatalf("remapped anchor = %+v, %v, %v", anchor, found, err)
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
