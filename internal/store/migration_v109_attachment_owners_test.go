package store

import (
	"path/filepath"
	"testing"
)

func TestMigrationV109RetainsLegacyForkAttachmentOwners(t *testing.T) {
	db := migrateThrough(t, 108)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	for _, id := range []string{"source", "fork", "foreign"} {
		mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES(?,'p','t','claude','/p',1,1)`, id)
	}
	mustExec(t, db, `INSERT INTO attachments(id,thread_id,filename,mime_type,size,relative_path,created_at,kind,thumbnail_data,thumbnail_mime) VALUES('a','source','a.png','image/png',1,'source/a.png',1,'image',X'6162','image/png')`)
	mustExec(t, db, `INSERT INTO items(thread_id,id,turn_index,item_index,kind,role,status,summary,meta,created_at,updated_at) VALUES('fork','u',0,0,'user_text','user','completed','image','{"attachments":[{"id":"a","threadId":"source"}]}',1,1)`)
	migrateFrom(t, db, 108)
	var owners int
	if err := db.QueryRow(`SELECT count(*) FROM attachment_owners WHERE attachment_id='a'`).Scan(&owners); err != nil || owners != 2 {
		t.Fatalf("owners=%d err=%v", owners, err)
	}
	assertPlanUses(t, db, "idx_attachment_owners_attachment", `EXPLAIN QUERY PLAN SELECT thread_id FROM attachment_owners WHERE attachment_id=?`, "a")
	mustExec(t, db, `DELETE FROM threads WHERE id='source'`)
	var origin, thumb string
	if err := db.QueryRow(`SELECT thread_id,thumbnail_data FROM attachments WHERE id='a'`).Scan(&origin, &thumb); err != nil || origin != "source" || thumb != "ab" {
		t.Fatalf("retained asset %q/%q: %v", origin, thumb, err)
	}
	mustExec(t, db, `DELETE FROM threads WHERE id='fork'`)
	if err := db.QueryRow(`SELECT count(*) FROM attachments`).Scan(&owners); err != nil || owners != 0 {
		t.Fatalf("last owner GC=%d err=%v", owners, err)
	}
	if _, err := db.Exec(`INSERT INTO attachments(id,thread_id,filename,mime_type,size,relative_path,created_at) VALUES('bad','missing','a','image/png',1,'a',1)`); err == nil {
		t.Fatal("accepted attachment without a live owner")
	}
}

func TestAttachmentOwnersRestoreAfterOriginalDeleted(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"source", "fork"} {
		if err := s.CreateThread(makeThread(id, "claude")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertAttachment(Attachment{ID: "a", ThreadID: "source", Filename: "a.png", MimeType: "image/png", RelativePath: "source/a.png", Size: 1, CreatedAt: 1, Kind: AttachmentKindImage}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.db, `INSERT INTO attachment_owners VALUES('fork','a')`)
	if err := s.DeleteThread("source"); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "owners.sqlite")
	if err := s.SnapshotTo(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThread("fork"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RestoreFrom(snapshot); err != nil {
		t.Fatal(err)
	}
	owned, err := s.OwnsAttachment("fork", "a")
	if err != nil || !owned {
		t.Fatalf("restored owner=%v err=%v", owned, err)
	}
	owned, err = s.OwnsAttachment("source", "a")
	if err != nil || owned {
		t.Fatalf("restored deleted owner=%v err=%v", owned, err)
	}
	if err := s.DeleteThread("fork"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetAttachment("a"); err != nil || found {
		t.Fatalf("restored GC found=%v err=%v", found, err)
	}
}
