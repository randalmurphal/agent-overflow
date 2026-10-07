package store

import "testing"

// v140 adds an empty identity_error to every existing project and keeps the
// identity already stored beside it; UpdateProjectIdentity then records and
// clears it.
func TestProjectIdentityErrorMigration(t *testing.T) {
	db := migrateThrough(t, projectIdentityErrorMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects (id, path, name, slug, created_at, updated_at, remote_url, root_commit)
		VALUES ('p', '/p', 'p', 'p', 1, 1, 'git@example.com:o/r.git', 'abc')`)

	if err := applyMigration(db, migrationByVersion(t, projectIdentityErrorMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", projectIdentityErrorMigrationVersion, err)
	}

	var remoteURL, rootCommit, identityError string
	if err := db.QueryRow(`SELECT remote_url, root_commit, identity_error FROM projects WHERE id = 'p'`).
		Scan(&remoteURL, &rootCommit, &identityError); err != nil {
		t.Fatalf("read project after v140: %v", err)
	}
	if remoteURL != "git@example.com:o/r.git" || rootCommit != "abc" || identityError != "" {
		t.Fatalf("row after v140 = (%q, %q, %q), want the identity kept and no error", remoteURL, rootCommit, identityError)
	}
}

func TestUpdateProjectIdentityRecordsAndClearsTheError(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	created, err := s.CreateProject(Project{ID: "p", Path: "/p", Name: "p", RepositoryID: "r", IdentitySource: "c"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	failed, changed, err := s.UpdateProjectIdentity(created.ID, ProjectIdentity{RepositoryID: "r", IdentitySource: "c", Error: "git refused"})
	if err != nil || !changed {
		t.Fatalf("record error: changed=%v err=%v", changed, err)
	}
	if failed.IdentityError != "git refused" || failed.RepositoryID != "r" || failed.IdentitySource != "c" {
		t.Fatalf("row with error = %+v", failed)
	}
	if _, changed, err := s.UpdateProjectIdentity(created.ID, ProjectIdentity{RepositoryID: "r", IdentitySource: "c", Error: "git refused"}); err != nil || changed {
		t.Fatalf("repeat write: changed=%v err=%v, want a no-op", changed, err)
	}
	cleared, changed, err := s.UpdateProjectIdentity(created.ID, ProjectIdentity{RepositoryID: "r", IdentitySource: "c"})
	if err != nil || !changed || cleared.IdentityError != "" {
		t.Fatalf("clear error: row=%+v changed=%v err=%v", cleared, changed, err)
	}
	if _, _, err := s.UpdateProjectIdentity(created.ID, ProjectIdentity{RepositoryID: "r", IdentitySource: "c", Error: "again"}); err != nil {
		t.Fatalf("record error again: %v", err)
	}
	listed, err := s.ListProjectsWithThreadCounts()
	if err != nil {
		t.Fatalf("ListProjectsWithThreadCounts: %v", err)
	}
	for _, row := range listed {
		if row.Project.ID == created.ID && row.Project.IdentityError != "again" {
			t.Fatalf("listed row = %+v, want the recorded error on the sidebar read", row.Project)
		}
	}
}
