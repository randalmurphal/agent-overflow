package projectapp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/worktreesetup"
)

type workspaceResolverFunc func(string, string) (string, bool, error)

func (resolve workspaceResolverFunc) FindWorktree(projectPath, candidate string) (string, bool, error) {
	return resolve(projectPath, candidate)
}

func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return New(Deps{
		Store: database,
		Now:   func() time.Time { return time.UnixMilli(1234) },
	}), database
}

func TestServiceProjectLifecycle(t *testing.T) {
	service, database := newTestService(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "workspace")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	project, err := service.Create("  " + path + "  ")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantPath, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	if project.Path != wantPath || project.Name != "workspace" {
		t.Fatalf("Create() = %+v, want path %q and name workspace", project, wantPath)
	}
	if project.CreatedAt != 1234 || project.UpdatedAt != 1234 {
		t.Fatalf("timestamps = (%d, %d), want (1234, 1234)", project.CreatedAt, project.UpdatedAt)
	}

	projects, err := service.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(projects) != 1 || projects[0].Project.ID != project.ID || projects[0].ThreadCount != 0 {
		t.Fatalf("List() = %+v, want created project with zero threads", projects)
	}

	renamed, err := service.Rename(project.ID, "  Shiny  ")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Project.Name != "Shiny" || renamed.Project.Path != wantPath {
		t.Fatalf("Rename() = %+v, want trimmed name and immutable path", renamed)
	}
	if !renamed.Changed {
		t.Fatal("Rename() reported no change for a name that moved")
	}

	if _, err := service.Archive(project.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	projects, err = service.List()
	if err != nil {
		t.Fatalf("List after Archive: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("List after Archive = %+v, want no active projects", projects)
	}
	unarchived, err := service.Unarchive(project.ID)
	if err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if unarchived.Project.Archived {
		t.Fatal("Unarchive returned Archived=true")
	}

	secondPath := filepath.Join(parent, "second")
	if err := os.Mkdir(secondPath, 0o755); err != nil {
		t.Fatalf("Mkdir second: %v", err)
	}
	second, err := service.Create(secondPath)
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if _, err := service.UpdateSortPositions([]string{second.ID, project.ID}); err != nil {
		t.Fatalf("UpdateSortPositions: %v", err)
	}
	for id, wantPosition := range map[string]int{second.ID: 0, project.ID: 1} {
		got, err := database.GetProject(id)
		if err != nil {
			t.Fatalf("GetProject(%s): %v", id, err)
		}
		if got.SortPosition != wantPosition {
			t.Errorf("GetProject(%s).SortPosition = %d, want %d", id, got.SortPosition, wantPosition)
		}
	}
}

func TestServiceCreateValidationAndDuplicateContract(t *testing.T) {
	service, _ := newTestService(t)

	if _, err := service.Create(" \t "); err == nil || err.Error() != "create project: path is required" {
		t.Fatalf("Create(empty) error = %v, want required-path error", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := service.Create(missing); err == nil || !strings.HasPrefix(err.Error(), "create project: stat ") {
		t.Fatalf("Create(missing) error = %v, want stat error", err)
	}
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := service.Create(file); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("Create(file) error = %v, want not-a-directory error", err)
	}

	directory := t.TempDir()
	if _, err := service.Create(directory); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := service.Create(directory); !errors.Is(err, store.ErrProjectPathInUse) {
		t.Fatalf("Create(duplicate) error = %v, want ErrProjectPathInUse", err)
	}
	if _, err := service.Rename("unused", " \t "); err == nil || err.Error() != "rename project: name is required" {
		t.Fatalf("Rename(empty) error = %v, want required-name error", err)
	}
}

func TestServiceUnavailableStoreErrorsPreserveBindingContract(t *testing.T) {
	service := New(Deps{})
	tests := []struct {
		name string
		call func() error
		want string
	}{
		{name: "list", call: func() error { _, err := service.List(); return err }, want: "list projects: store unavailable"},
		{name: "create", call: func() error { _, err := service.Create("path"); return err }, want: "create project: store unavailable"},
		{name: "rename", call: func() error { _, err := service.Rename("id", "name"); return err }, want: "rename project: store unavailable"},
		{name: "archive", call: func() error { _, err := service.Archive("id"); return err }, want: "archive project: store unavailable"},
		{name: "unarchive", call: func() error { _, err := service.Unarchive("id"); return err }, want: "unarchive project: store unavailable"},
		{name: "sort", call: func() error { _, err := service.UpdateSortPositions(nil); return err }, want: "update project sort positions: store unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestServiceWorkspaceAndSetupValidationOrderPreservesBindingContract(t *testing.T) {
	service := New(Deps{})
	if _, err := service.ProjectForWorkspaceOperation(" "); err == nil || err.Error() != "store unavailable" {
		t.Fatalf("ProjectForWorkspaceOperation error = %v, want store unavailable", err)
	}
	if _, err := service.GetWorktreeSetup(" "); err == nil || err.Error() != "project id is required" {
		t.Fatalf("GetWorktreeSetup error = %v, want required id", err)
	}
	if _, _, err := service.SetWorktreeSetup(" ", worktreesetup.Config{Run: [][]string{{}}}); err == nil || err.Error() != "project id is required" {
		t.Fatalf("SetWorktreeSetup error = %v, want required id before recipe validation", err)
	}
}

func TestServiceResolvesProjectWorkspaceMembership(t *testing.T) {
	service, database := newTestService(t)
	project := store.Project{ID: "project", Path: filepath.Join(t.TempDir(), "repo"), Name: "repo"}
	if _, err := database.CreateProject(project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	service.deps.Workspace = workspaceResolverFunc(func(projectPath, candidate string) (string, bool, error) {
		if projectPath != project.Path || candidate != "/candidate" {
			t.Fatalf("FindWorktree(%q, %q)", projectPath, candidate)
		}
		return "/canonical/worktree", true, nil
	})

	row, err := service.ProjectForWorkspaceOperation("  project  ")
	if err != nil || row.ID != project.ID {
		t.Fatalf("ProjectForWorkspaceOperation = (%+v, %v)", row, err)
	}
	for _, source := range []string{"", "  " + project.Path + "  "} {
		got, err := service.ResolveSourceWorkspace(row, source)
		if err != nil || got != project.Path {
			t.Fatalf("ResolveSourceWorkspace(%q) = (%q, %v), want project root", source, got, err)
		}
	}
	got, err := service.ResolveSourceWorkspace(row, " /candidate ")
	if err != nil || got != "/canonical/worktree" {
		t.Fatalf("ResolveSourceWorkspace(candidate) = (%q, %v)", got, err)
	}
}

func TestServiceRejectsUnregisteredSourceWorkspace(t *testing.T) {
	service, _ := newTestService(t)
	service.deps.Workspace = workspaceResolverFunc(func(string, string) (string, bool, error) {
		return "", false, nil
	})
	_, err := service.ResolveSourceWorkspace(store.Project{ID: "p", Path: "/repo"}, "/other")
	if err == nil || err.Error() != "/other is not a workspace of project /repo" {
		t.Fatalf("ResolveSourceWorkspace error = %v", err)
	}
}

func TestServiceWorktreeSetupRoundTripValidationAndClear(t *testing.T) {
	service, database := newTestService(t)
	project := store.Project{ID: "project", Path: t.TempDir(), Name: "repo"}
	if _, err := database.CreateProject(project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	want := worktreesetup.Config{
		Copy: []string{".env"}, Run: [][]string{{"make", "install"}}, Timeout: "15m",
	}
	got, _, err := service.SetWorktreeSetup(" project ", worktreesetup.Config{
		Copy: want.Copy, Run: want.Run, Timeout: " 15m ",
	})
	if err != nil || !slices.Equal(got.Copy, want.Copy) || got.Timeout != want.Timeout || len(got.Run) != 1 {
		t.Fatalf("SetWorktreeSetup = (%+v, %v), want %+v", got, err, want)
	}
	if _, _, err := service.SetWorktreeSetup(project.ID, worktreesetup.Config{Run: [][]string{{}}}); err == nil {
		t.Fatal("SetWorktreeSetup accepted an empty argv")
	}
	got, err = service.GetWorktreeSetup(project.ID)
	if err != nil || got.Timeout != want.Timeout {
		t.Fatalf("GetWorktreeSetup after rejected save = (%+v, %v)", got, err)
	}
	got, _, err = service.SetWorktreeSetup(project.ID, worktreesetup.Config{})
	if err != nil || !got.IsZero() {
		t.Fatalf("clear SetWorktreeSetup = (%+v, %v)", got, err)
	}
	if _, found, err := database.ProjectWorktreeSetup(project.ID); err != nil || found {
		t.Fatalf("cleared persisted recipe = (found %v, err %v)", found, err)
	}
}

func TestServiceWorkflowFootprintFindsEveryDeletionRoot(t *testing.T) {
	service, database := newTestService(t)
	project := store.Project{ID: "project", Path: t.TempDir(), Name: "repo"}
	if _, err := database.CreateProject(project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	items := []store.WorkItem{
		{ID: "root", ProjectID: project.ID, Goal: "root", WorkflowID: "build", WorkflowScope: "project", State: "running", Source: "manual", CreatedAt: 1},
		{ID: "child", ProjectID: project.ID, Goal: "child", WorkflowID: "build", WorkflowScope: "project", State: "running", Source: "call", ParentItemID: "root", ParentPhaseID: "phase", ParentAttempt: 1, CallDepth: 1, CreatedAt: 2},
		{ID: "orphan", ProjectID: project.ID, Goal: "orphan", WorkflowID: "build", WorkflowScope: "project", State: "running", Source: "call", ParentItemID: "missing", ParentPhaseID: "phase", ParentAttempt: 1, CallDepth: 1, CreatedAt: 3},
	}
	for _, item := range items {
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatalf("CreateWorkItem(%s): %v", item.ID, err)
		}
	}
	if err := database.CreateAutomation(store.Automation{
		ID: "automation", ProjectID: project.ID, WorkflowID: "build", WorkflowScope: "project",
		Name: "nightly", Trigger: json.RawMessage(`{"kind":"cron"}`), CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("CreateAutomation: %v", err)
	}

	footprint, err := service.WorkflowFootprint(project.ID)
	if err != nil {
		t.Fatalf("WorkflowFootprint: %v", err)
	}
	if !footprint.HasWork() || footprint.RunCount() != 3 || footprint.AutomationCount() != 1 {
		t.Fatalf("WorkflowFootprint counts = runs %d, automations %d, hasWork %v", footprint.RunCount(), footprint.AutomationCount(), footprint.HasWork())
	}
	rootIDs := make([]string, 0, len(footprint.Roots()))
	for _, root := range footprint.Roots() {
		rootIDs = append(rootIDs, root.ID)
	}
	if !slices.Equal(rootIDs, []string{"root", "orphan"}) {
		t.Fatalf("WorkflowFootprint roots = %v, want [root orphan]", rootIDs)
	}
	same, err := service.WorkflowFootprint(project.ID)
	if err != nil || !footprint.SameAs(same) {
		t.Fatalf("stable footprint SameAs = %v, err %v", footprint.SameAs(same), err)
	}
	if err := database.CreateWorkItem(store.WorkItem{
		ID: "new", ProjectID: project.ID, Goal: "new", WorkflowID: "build", WorkflowScope: "project",
		State: "running", Source: "manual", CreatedAt: 4,
	}); err != nil {
		t.Fatalf("CreateWorkItem(new): %v", err)
	}
	changed, err := service.WorkflowFootprint(project.ID)
	if err != nil || footprint.SameAs(changed) {
		t.Fatalf("changed footprint SameAs = %v, err %v", footprint.SameAs(changed), err)
	}
}
