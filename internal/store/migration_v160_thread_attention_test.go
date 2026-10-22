package store

import (
	"database/sql"
	"testing"
)

// v160 over a populated database: an existing thread has no flag, and the
// CHECK refuses a kind the tool cannot raise.
func TestMigrationV160ThreadAttention(t *testing.T) {
	path := fileDBThrough(t, threadAttentionMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived)
		VALUES ('t', 'p', 't', 'claude', '/p', '', 1, 1, 0)`)

	if err := applyMigration(db, migrationByVersion(t, threadAttentionMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", threadAttentionMigrationVersion, err)
	}

	got, err := queryIDs(db, `SELECT attention_kind || '|' || attention_reason || '|' || COALESCE(attention_at, 'null') FROM threads`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "||null" {
		t.Fatalf("existing thread = %v, want no flag", got)
	}
	for _, kind := range AttentionKinds() {
		if _, err := db.Exec(`UPDATE threads SET attention_kind = ?, attention_at = 5 WHERE id = 't'`, kind); err != nil {
			t.Fatalf("kind %q refused: %v", kind, err)
		}
	}
	if _, err := db.Exec(`UPDATE threads SET attention_kind = 'urgent' WHERE id = 't'`); err == nil {
		t.Fatal("the CHECK accepted an unknown kind")
	}
}
