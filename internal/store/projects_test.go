package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// newProject returns a Project struct seeded with now() timestamps. Tests
// overwrite fields as needed.
func newProject(id, path, name string) Project {
	now := time.Now().UnixMilli()
	return Project{
		ID:        id,
		Path:      path,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestCreateProjectHappyPath(t *testing.T) {
	s := newTestStore(t)
	p := newProject("p1", "/tmp/p1", "P1")
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject(): %v", err)
	}
	got, err := s.GetProject("p1")
	if err != nil {
		t.Fatalf("GetProject(): %v", err)
	}
	if got.Path != p.Path || got.Name != p.Name {
		t.Fatalf("GetProject returned %+v, want %+v", got, p)
	}
}

func TestCreateProjectDuplicatePathRejected(t *testing.T) {
	s := newTestStore(t)
	p := newProject("p1", "/tmp/shared", "first")
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("first CreateProject: %v", err)
	}
	dup := newProject("p2", "/tmp/shared", "second")
	err := s.CreateProject(dup)
	if !errors.Is(err, ErrProjectPathInUse) {
		t.Fatalf("CreateProject(duplicate path) = %v, want ErrProjectPathInUse", err)
	}
}

func TestGetProjectByPath(t *testing.T) {
	s := newTestStore(t)
	p := newProject("p1", "/tmp/lookup", "lookup")
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	got, err := s.GetProjectByPath("/tmp/lookup")
	if err != nil {
		t.Fatalf("GetProjectByPath: %v", err)
	}
	if got.ID != "p1" {
		t.Fatalf("GetProjectByPath returned id=%q, want p1", got.ID)
	}

	_, err = s.GetProjectByPath("/nonexistent")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetProjectByPath(missing) = %v, want sql.ErrNoRows", err)
	}
}

func TestListProjectsOrdersByName(t *testing.T) {
	s := newTestStore(t)
	// newTestStore seeds a default project — account for it in the list.
	_ = s
	for _, p := range []Project{
		newProject("b", "/tmp/b", "banana"),
		newProject("a", "/tmp/a", "apple"),
		newProject("c", "/tmp/c", "cherry"),
	} {
		if err := s.CreateProject(p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.ID, err)
		}
	}

	got, err := s.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects(): %v", err)
	}
	// Order by name asc: Default Test Project, apple, banana, cherry.
	wantNames := []string{"Default Test Project", "apple", "banana", "cherry"}
	if len(got) != len(wantNames) {
		t.Fatalf("len(got) = %d, want %d (%+v)", len(got), len(wantNames), got)
	}
	for i, w := range wantNames {
		if got[i].Name != w {
			t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, w)
		}
	}
}

func TestListProjectsWithThreadCounts(t *testing.T) {
	s := newTestStore(t)

	empty := newProject("proj-empty", "/tmp/empty", "empty")
	populated := newProject("proj-populated", "/tmp/populated", "populated")
	if err := s.CreateProject(empty); err != nil {
		t.Fatalf("CreateProject(empty): %v", err)
	}
	if err := s.CreateProject(populated); err != nil {
		t.Fatalf("CreateProject(populated): %v", err)
	}

	// Two threads in "populated", updated_at 100/200.
	for i, ts := range []int64{100, 200} {
		th := makeThread("t"+string(rune('0'+i)), "claude")
		th.ProjectID = populated.ID
		th.UpdatedAt = ts
		if err := s.CreateThread(th); err != nil {
			t.Fatalf("CreateThread: %v", err)
		}
	}

	got, err := s.ListProjectsWithThreadCounts()
	if err != nil {
		t.Fatalf("ListProjectsWithThreadCounts: %v", err)
	}

	byID := make(map[string]ProjectWithCounts, len(got))
	for _, pwc := range got {
		byID[pwc.Project.ID] = pwc
	}

	if pwc, ok := byID["proj-empty"]; !ok || pwc.ThreadCount != 0 || pwc.LastActive != 0 {
		t.Errorf("empty project: got %+v, want count=0 lastActive=0", pwc)
	}
	if pwc, ok := byID["proj-populated"]; !ok || pwc.ThreadCount != 2 || pwc.LastActive != 200 {
		t.Errorf("populated project: got %+v, want count=2 lastActive=200", pwc)
	}
}

func TestUpdateProjectName(t *testing.T) {
	s := newTestStore(t)
	p := newProject("p1", "/tmp/upd", "old-name")
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.UpdateProjectName("p1", "new-name"); err != nil {
		t.Fatalf("UpdateProjectName: %v", err)
	}
	got, _ := s.GetProject("p1")
	if got.Name != "new-name" {
		t.Fatalf("Name = %q, want new-name", got.Name)
	}
}

func TestArchiveAndUnarchiveProject(t *testing.T) {
	s := newTestStore(t)
	p := newProject("p1", "/tmp/arc", "archive-me")
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if err := s.ArchiveProject("p1"); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}
	list, _ := s.ListProjects()
	for _, pr := range list {
		if pr.ID == "p1" {
			t.Fatal("archived project still in ListProjects")
		}
	}

	if err := s.UnarchiveProject("p1"); err != nil {
		t.Fatalf("UnarchiveProject: %v", err)
	}
	got, _ := s.GetProject("p1")
	if got.Archived {
		t.Fatal("project still archived after UnarchiveProject")
	}
}

func TestDeleteProjectCascadesThreads(t *testing.T) {
	s := newTestStore(t)
	p := newProject("proj-del", "/tmp/del", "delete-me")
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	th := makeThread("t-del", "claude")
	th.ProjectID = p.ID
	if err := s.CreateThread(th); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	ids, err := s.ListThreadIDsForProject(p.ID)
	if err != nil {
		t.Fatalf("ListThreadIDsForProject: %v", err)
	}
	if len(ids) != 1 || ids[0] != "t-del" {
		t.Fatalf("ListThreadIDsForProject = %v, want [t-del]", ids)
	}

	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	_, err = s.GetThread("t-del")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetThread after DeleteProject = %v, want sql.ErrNoRows", err)
	}
}
