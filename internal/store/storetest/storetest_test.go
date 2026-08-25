package storetest_test

import (
	"os"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }

func TestCloneYieldsAWorkingMigratedStore(t *testing.T) {
	s := storetest.Clone(t)
	if err := s.CreateProject(store.Project{
		ID: "p1", Path: "/tmp/storetest", Name: "Storetest", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("create project on clone: %v", err)
	}
	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "p1" {
		t.Fatalf("clone must hold exactly the project this test wrote; got %+v", projects)
	}
}

// The template is bare-migrated on purpose: a package that needs a project
// seeds its own, so nothing inherits a row it did not ask for.
func TestCloneStartsWithNoProjects(t *testing.T) {
	projects, err := storetest.Clone(t).ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("template must be bare-migrated; got %+v", projects)
	}
}

func TestClonesAreIndependentFiles(t *testing.T) {
	first, second := storetest.Clone(t), storetest.Clone(t)
	if err := first.CreateProject(store.Project{
		ID: "only-in-first", Path: "/tmp/storetest", Name: "First", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("create project on first clone: %v", err)
	}
	projects, err := second.ListProjects()
	if err != nil {
		t.Fatalf("list projects on second clone: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("a write to one clone must not reach another; got %+v", projects)
	}
}

func TestClonePathReopensTheSameFile(t *testing.T) {
	path := storetest.ClonePath(t)
	first, err := store.New(path)
	if err != nil {
		t.Fatalf("open clone path: %v", err)
	}
	if err := first.CreateProject(store.Project{
		ID: "p1", Path: "/tmp/storetest", Name: "Storetest", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := store.New(path)
	if err != nil {
		t.Fatalf("reopen clone path: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close reopened: %v", err)
		}
	})
	projects, err := second.ListProjects()
	if err != nil {
		t.Fatalf("list projects after reopen: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "p1" {
		t.Fatalf("reopening the path must see the first handle's write; got %+v", projects)
	}
}
