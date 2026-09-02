package gitapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
)

// newResolveTestService builds a service over a real repository plus one
// registered worktree. Everything lives under t.TempDir(); nothing here can
// reach a provider binary or a real provider home.
func newResolveTestService(t *testing.T) (service *Service, project string, worktree string) {
	t.Helper()
	database, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	project = testutil.InitGitRepo(t)
	worktree = filepath.Join(t.TempDir(), "wt")
	testutil.RunGit(t, project, "worktree", "add", "-b", "feature/wt", worktree)

	now := time.Now().UnixMilli()
	if err := database.CreateProject(store.Project{
		ID: "project", Path: project, Name: "project", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return New(Deps{Store: database, Core: gitops.NewCore()}), project, worktree
}

func TestResolveWorkspaceAcceptsProjectRootAndRegisteredWorktrees(t *testing.T) {
	service, project, worktree := newResolveTestService(t)

	for name, ref := range map[string]WorkspaceRef{
		"empty path":   {ProjectID: "project"},
		"project root": {ProjectID: "project", WorkspacePath: project},
	} {
		gotProject, gotWorkspace, err := service.ResolveWorkspace(ref)
		if err != nil {
			t.Fatalf("%s: ResolveWorkspace() error = %v", name, err)
		}
		if !gitops.SameFilesystemPath(gotProject, project) || !gitops.SameFilesystemPath(gotWorkspace, project) {
			t.Fatalf("%s: ResolveWorkspace() = (%q, %q), want the project root %q",
				name, gotProject, gotWorkspace, project)
		}
	}

	gotProject, gotWorkspace, err := service.ResolveWorkspace(
		WorkspaceRef{ProjectID: "project", WorkspacePath: worktree})
	if err != nil {
		t.Fatalf("registered worktree: ResolveWorkspace() error = %v", err)
	}
	if !gitops.SameFilesystemPath(gotProject, project) {
		t.Fatalf("registered worktree: project = %q, want %q", gotProject, project)
	}
	if !gitops.SameFilesystemPath(gotWorkspace, worktree) {
		t.Fatalf("registered worktree: workspace = %q, want %q", gotWorkspace, worktree)
	}
}

func TestResolveWorkspaceRefusesPathsOutsideTheProject(t *testing.T) {
	service, project, _ := newResolveTestService(t)

	outside := t.TempDir()
	sibling := testutil.InitGitRepo(t)

	// A SUBDIRECTORY of the project is the case a "which repository does
	// this path belong to" test alone would wrongly accept: it belongs to
	// this repository but is not a checkout, and accepting it would scope
	// every workspace read to an arbitrary folder.
	subdir := filepath.Join(project, "src")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	for name, path := range map[string]string{
		"plain directory":          outside,
		"unrelated repo root":      sibling,
		"nonexistent path":         filepath.Join(outside, "nested"),
		"subdirectory of the root": subdir,
	} {
		if _, _, err := service.ResolveWorkspace(
			WorkspaceRef{ProjectID: "project", WorkspacePath: path},
		); err == nil {
			t.Fatalf("%s: ResolveWorkspace(%q) accepted a path outside the project", name, path)
		} else if !strings.Contains(err.Error(), "is not a workspace of project") {
			t.Fatalf("%s: ResolveWorkspace(%q) error = %v, want a membership refusal", name, path, err)
		}
	}
}

// ResolveWorkspace runs on the hot path — every @-mention keystroke, every
// hunk-gap click, every status subscribe — so it must answer membership from
// git's on-disk layout rather than a `git worktree list` per call. With PATH
// emptied no `git` binary resolves at all, so a resolve that still succeeds
// spawned nothing. (internal/git has no injectable runner to count spawns
// with; this is the tripwire available without inventing one.)
func TestResolveWorkspaceSpawnsNoGit(t *testing.T) {
	service, project, worktree := newResolveTestService(t)

	t.Setenv("PATH", "")

	gotProject, gotWorkspace, err := service.ResolveWorkspace(
		WorkspaceRef{ProjectID: "project", WorkspacePath: worktree})
	if err != nil {
		t.Fatalf("ResolveWorkspace() with no git on PATH error = %v", err)
	}
	if !gitops.SameFilesystemPath(gotProject, project) ||
		!gitops.SameFilesystemPath(gotWorkspace, worktree) {
		t.Fatalf("ResolveWorkspace() = (%q, %q), want (%q, %q)",
			gotProject, gotWorkspace, project, worktree)
	}

	// The refusal path must not need git either.
	if _, _, err := service.ResolveWorkspace(
		WorkspaceRef{ProjectID: "project", WorkspacePath: t.TempDir()},
	); err == nil || !strings.Contains(err.Error(), "is not a workspace of project") {
		t.Fatalf("ResolveWorkspace() error = %v, want a membership refusal", err)
	}
}

func TestResolveWorkspaceRefusesUnknownProject(t *testing.T) {
	service, project, _ := newResolveTestService(t)

	// Even the RIGHT directory is refused when the project id does not name
	// a row: the id is the authority, the path is only checked against it.
	if _, _, err := service.ResolveWorkspace(
		WorkspaceRef{ProjectID: "ghost", WorkspacePath: project},
	); err == nil {
		t.Fatal("ResolveWorkspace() accepted an unknown project id")
	}
	if _, _, err := service.ResolveWorkspace(WorkspaceRef{}); err == nil {
		t.Fatal("ResolveWorkspace() accepted an empty project id")
	}
}
