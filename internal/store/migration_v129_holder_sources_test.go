package store

import (
	"slices"
	"testing"
)

// TestMigrationV129HolderSources: every holder, made by a split or retired
// from a deleted fork, names no source after the migration, so no split
// reuses one; a fork keeps its source.
func TestMigrationV129HolderSources(t *testing.T) {
	db := migrateThrough(t, 128)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,mode,created_at,updated_at,fork_source_thread_id,fork_cut_turn_index,fork_cut_item_index,fork_source_title) VALUES
		('S','p','Source','claude','/p','chat',1,1,'',0,0,''),
		('F','p','Fork','claude','/p','chat',1,1,'S',1,0,'Source'),
		('R',NULL,'Retired','claude','','holder',1,1,'S',2,0,'Source'),
		('H',NULL,'Source','claude','','holder',1,1,'S',0,0,'Source')`)
	migrateFromThrough(t, db, 128, 129)
	sources, err := queryIDs(db, `SELECT id || ':' || fork_source_thread_id FROM threads ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"F:S", "H:", "R:", "S:"}; !slices.Equal(sources, want) {
		t.Fatalf("sources = %v, want %v", sources, want)
	}
}
