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

// TestUpdateProjectSortPositionsAssignsDensePositions covers the
// happy path: dense 0..N-1 positions assigned in slice order, regardless
// of prior values.
func TestUpdateProjectSortPositionsAssignsDensePositions(t *testing.T) {
	s := newTestStore(t)

	a := newProject("p-a", "/tmp/a", "A")
	a.SortPosition = 17
	b := newProject("p-b", "/tmp/b", "B")
	b.SortPosition = 3
	c := newProject("p-c", "/tmp/c", "C")
	c.SortPosition = 99
	for _, p := range []Project{a, b, c} {
		if err := s.CreateProject(p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.ID, err)
		}
	}

	if err := s.UpdateProjectSortPositions([]string{"p-c", "p-a", "p-b"}); err != nil {
		t.Fatalf("UpdateProjectSortPositions: %v", err)
	}

	for _, want := range []struct {
		id  string
		pos int
	}{
		{"p-c", 0},
		{"p-a", 1},
		{"p-b", 2},
	} {
		got, err := s.GetProject(want.id)
		if err != nil {
			t.Fatalf("GetProject(%s): %v", want.id, err)
		}
		if got.SortPosition != want.pos {
			t.Errorf("%s sort_position = %d, want %d", want.id, got.SortPosition, want.pos)
		}
	}
}

// TestUpdateProjectSortPositionsEmptySliceIsNoop pins the documented
// "empty input is a no-op" semantics — important because callers might
// emit an empty list when the user cancels a drag mid-flight.
func TestUpdateProjectSortPositionsEmptySliceIsNoop(t *testing.T) {
	s := newTestStore(t)

	p := newProject("p-only", "/tmp/only", "Only")
	p.SortPosition = 42
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if err := s.UpdateProjectSortPositions(nil); err != nil {
		t.Fatalf("UpdateProjectSortPositions(nil): %v", err)
	}
	if err := s.UpdateProjectSortPositions([]string{}); err != nil {
		t.Fatalf("UpdateProjectSortPositions([]): %v", err)
	}

	got, err := s.GetProject(p.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.SortPosition != 42 {
		t.Errorf("sort_position = %d, want 42 (untouched)", got.SortPosition)
	}
}

// TestUpdateProjectSortPositionsOmittedIdsKeepPosition pins the
// documented "ids not in the supplied slice keep their existing
// positions" semantics — partial reorders should not zero out other
// projects.
func TestUpdateProjectSortPositionsOmittedIdsKeepPosition(t *testing.T) {
	s := newTestStore(t)

	a := newProject("p-a", "/tmp/a", "A")
	a.SortPosition = 17
	b := newProject("p-b", "/tmp/b", "B")
	b.SortPosition = 99
	for _, p := range []Project{a, b} {
		if err := s.CreateProject(p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.ID, err)
		}
	}

	if err := s.UpdateProjectSortPositions([]string{"p-a"}); err != nil {
		t.Fatalf("UpdateProjectSortPositions: %v", err)
	}

	gotA, _ := s.GetProject("p-a")
	if gotA.SortPosition != 0 {
		t.Errorf("p-a sort_position = %d, want 0", gotA.SortPosition)
	}
	gotB, _ := s.GetProject("p-b")
	if gotB.SortPosition != 99 {
		t.Errorf("p-b sort_position = %d, want 99 (omitted, untouched)", gotB.SortPosition)
	}
}

// TestUpdateProjectSortPositionsBumpsUpdatedAt confirms reorder counts
// as project activity (the project's row should re-surface on the next
// "by latest activity" sort if a thread under it was active).
func TestUpdateProjectSortPositionsBumpsUpdatedAt(t *testing.T) {
	s := newTestStore(t)

	p := newProject("p-bump", "/tmp/bump", "Bump")
	p.UpdatedAt = 1
	if err := s.CreateProject(p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if err := s.UpdateProjectSortPositions([]string{"p-bump"}); err != nil {
		t.Fatalf("UpdateProjectSortPositions: %v", err)
	}

	got, err := s.GetProject(p.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.UpdatedAt <= 1 {
		t.Errorf("updated_at = %d, want > 1 (bumped to nowMillis)", got.UpdatedAt)
	}
}
