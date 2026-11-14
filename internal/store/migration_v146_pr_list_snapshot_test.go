package store

import (
	"testing"
)

// v146 drops pr_views with its rows and creates pr_list_projects, whose
// key must be set.
func TestPRListSnapshotMigrationDropsPRViewsAndAddsTheSnapshotTable(t *testing.T) {
	db := migrateThrough(t, prListSnapshotMigrationVersion-1)
	mustExec(t, db, `INSERT INTO pr_views(pr_key,opened_at,head_sha) VALUES ('github:o/r:1',1,'h')`)
	if err := applyMigration(db, migrationByVersion(t, prListSnapshotMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'pr_views'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pr_views after v146: %d objects, %v", n, err)
	}
	mustExec(t, db, `INSERT INTO pr_list_projects(repo_key,project_json,read_at) VALUES ('github:o/r','{}',1)`)
	if _, err := db.Exec(`INSERT INTO pr_list_projects(repo_key,project_json,read_at) VALUES ('','{}',1)`); err == nil {
		t.Fatal("an empty repository key was stored")
	}
	if _, err := db.Exec(`INSERT INTO pr_list_projects(repo_key,project_json) VALUES ('gitlab:g/r','{}')`); err == nil {
		t.Fatal("a read without its time was stored")
	}
}

// A repository's read replaces the one before; pruning keeps only the named repositories, and pruning
// with none named empties the table.
func TestPRListProjectsStoreReadsAndPrune(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, project := range []PRListProject{
		{RepoKey: "github:o/r", ProjectJSON: []byte(`{"rows":[1]}`), ReadAt: 10},
		{RepoKey: "gitlab:g/r", ProjectJSON: []byte(`{"rows":[]}`), ReadAt: 30},
		{RepoKey: "github:o/r", ProjectJSON: []byte(`{"rows":[2]}`), ReadAt: 20},
		{RepoKey: "github:o/gone", ProjectJSON: []byte(`{}`), ReadAt: 5},
	} {
		if err := s.PutPRListProject(project); err != nil {
			t.Fatal(err)
		}
	}
	for name, bad := range map[string]PRListProject{
		"no key":  {ProjectJSON: []byte(`{}`), ReadAt: 1},
		"no time": {RepoKey: "k", ProjectJSON: []byte(`{}`)},
		"no JSON": {RepoKey: "k", ProjectJSON: []byte(`{`), ReadAt: 1},
	} {
		if err := s.PutPRListProject(bad); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	all, err := s.ListPRListProjects()
	if err != nil || len(all) != 3 || string(all["github:o/r"].ProjectJSON) != `{"rows":[2]}` || all["github:o/r"].ReadAt != 20 {
		t.Fatalf("reads = %+v, %v", all, err)
	}
	if n, err := s.DeletePRListProjectsExcept([]string{"github:o/r", "gitlab:g/r", "github:o/r"}); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if all, _ = s.ListPRListProjects(); len(all) != 2 {
		t.Fatalf("after prune = %+v", all)
	}
	if n, err := s.DeletePRListProjectsExcept(nil); err != nil || n != 2 {
		t.Fatalf("prune to none = %d, %v", n, err)
	}
}
