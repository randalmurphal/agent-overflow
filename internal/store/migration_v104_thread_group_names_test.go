package store

import (
	"database/sql"
	"strings"
	"testing"
)

// TestMigrationV104MergesDuplicateGroupNames drives v104 over a database
// that already holds two groups of one name in one project, which is what
// the unconstrained resolve-then-insert could write. The survivor is the
// oldest row and it inherits the other's threads; a group of the same name
// in ANOTHER project is untouched, because a group belongs to one project.
func TestMigrationV104MergesDuplicateGroupNames(t *testing.T) {
	db := migrateThrough(t, 103)

	mustExec(t, db, `INSERT INTO projects (id, path, name, slug, created_at, updated_at)
		VALUES ('p-v104', '/v104', 'v104', 'v104', 1, 1),
		       ('p-v104b', '/v104b', 'v104b', 'v104b', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_groups (id, project_id, name, created_at, updated_at)
		VALUES ('g-keep', 'p-v104', 'Auth work', 10, 10),
		       ('g-dup', 'p-v104', 'auth WORK', 20, 20),
		       ('g-other-project', 'p-v104b', 'Auth work', 30, 30)`)
	for _, row := range []struct{ id, group string }{
		{"t-keep", "g-keep"},
		{"t-dup", "g-dup"},
		{"t-other", "g-other-project"},
	} {
		mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode, group_id)
			VALUES (?, ?, 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat', ?)`, row.id, "p-v104", row.group)
	}

	migrateFrom(t, db, 103)

	var names []string
	rows, err := db.Query(`SELECT id FROM thread_groups WHERE project_id = 'p-v104'`)
	if err != nil {
		t.Fatalf("list migrated groups: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan migrated group: %v", err)
		}
		names = append(names, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list migrated groups: %v", err)
	}
	if len(names) != 1 || names[0] != "g-keep" {
		t.Fatalf("groups after merge = %v, want [g-keep]", names)
	}

	for _, threadID := range []string{"t-keep", "t-dup"} {
		var groupID sql.NullString
		if err := db.QueryRow(`SELECT group_id FROM threads WHERE id = ?`, threadID).Scan(&groupID); err != nil {
			t.Fatalf("read %s group: %v", threadID, err)
		}
		if groupID.String != "g-keep" {
			t.Fatalf("%s group = %q, want g-keep", threadID, groupID.String)
		}
	}
	var otherProjectGroup string
	if err := db.QueryRow(
		`SELECT id FROM thread_groups WHERE project_id = 'p-v104b'`,
	).Scan(&otherProjectGroup); err != nil {
		t.Fatalf("read the other project's group: %v", err)
	}
	if otherProjectGroup != "g-other-project" {
		t.Fatalf("other project group = %q, want g-other-project", otherProjectGroup)
	}
}

// TestMigrationV104RefusesASecondGroupOfTheSameName is the constraint
// itself: the index is what makes the resolve-or-insert a decision rather
// than a race, so a raw insert past the accessor has to fail.
func TestMigrationV104RefusesASecondGroupOfTheSameName(t *testing.T) {
	db := migrateThrough(t, 104)

	mustExec(t, db, `INSERT INTO projects (id, path, name, slug, created_at, updated_at)
		VALUES ('p-unique', '/unique', 'unique', 'unique', 1, 1),
		       ('p-unique2', '/unique2', 'unique2', 'unique2', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_groups (id, project_id, name, created_at, updated_at)
		VALUES ('g-one', 'p-unique', 'Release', 1, 1)`)

	_, err := db.Exec(`INSERT INTO thread_groups (id, project_id, name, created_at, updated_at)
		VALUES ('g-two', 'p-unique', 'release', 2, 2)`)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second group of the same name: err = %v, want a UNIQUE failure", err)
	}
	mustExec(t, db, `INSERT INTO thread_groups (id, project_id, name, created_at, updated_at)
		VALUES ('g-three', 'p-unique2', 'Release', 3, 3)`)
}

// TestCreateThreadGroupReturnsTheExistingGroup is the accessor half: every
// creator resolves first, so naming a group that is already there joins it
// instead of failing on the index.
func TestCreateThreadGroupReturnsTheExistingGroup(t *testing.T) {
	s := newTestStore(t)

	first, err := s.CreateThreadGroup(defaultTestProjectID, "Auth work")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	second, err := s.CreateThreadGroup(defaultTestProjectID, "  auth WORK  ")
	if err != nil {
		t.Fatalf("CreateThreadGroup again: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second create = %s, want the existing %s", second.ID, first.ID)
	}
	if second.Name != first.Name {
		t.Fatalf("second create renamed the group to %q, want %q", second.Name, first.Name)
	}
	groups, err := s.ListThreadGroups()
	if err != nil {
		t.Fatalf("ListThreadGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
}
