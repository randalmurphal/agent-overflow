package store

import "testing"

// v147 adds a nullable shell column: rows written before it read as ordinary
// attachments, and a shell result round-trips through the one writer.
func TestAttachmentShellMigrationKeepsExistingRowsOrdinary(t *testing.T) {
	db := migrateThrough(t, attachmentShellMigrationVersion-1)
	mustExec(t, db, `INSERT INTO projects(id,path,name,slug,created_at,updated_at) VALUES('p','/p','p','p',1,1)`)
	mustExec(t, db, `INSERT INTO threads(id,project_id,title,provider,workspace_path,created_at,updated_at) VALUES('t1','p','t','claude','/p',1,1)`)
	mustExec(t, db, `INSERT INTO attachments(id,thread_id,filename,mime_type,size,relative_path,created_at,kind)
		VALUES ('a1','t1','notes.txt','text/plain',5,'t1/a1/notes.txt',1,'file')`)
	if err := applyMigration(db, migrationByVersion(t, attachmentShellMigrationVersion)); err != nil {
		t.Fatal(err)
	}
	got, err := scanAttachment(db.QueryRow(`SELECT ` + attachmentColumns + ` FROM attachments WHERE id = 'a1'`))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Shell.IsZero() || got.Filename != "notes.txt" || got.Kind != AttachmentKindFile {
		t.Fatalf("pre-migration row after v147: %+v", got)
	}
}

func TestAttachmentShellRoundTripsAndIsFileOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	thread := makeThread("t1", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	shell := AttachmentShell{Command: "git status", ExitCode: 1, DurationMs: 42, Truncated: true, TimedOut: true}
	want := Attachment{
		ID: "a1", ThreadID: "t1", Filename: "output.txt", MimeType: "text/plain", Size: 3,
		RelativePath: "t1/a1/output.txt", CreatedAt: 7, Kind: AttachmentKindFile, Shell: shell,
	}
	if err := s.InsertAttachment(want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetAttachment("a1")
	if err != nil || !ok || got != want {
		t.Fatalf("round trip: %+v ok=%v err=%v", got, ok, err)
	}
	withThumb, ok, err := s.GetAttachmentWithThumbnail("a1")
	if err != nil || !ok || withThumb.Attachment != want {
		t.Fatalf("thumbnail read: %+v ok=%v err=%v", withThumb.Attachment, ok, err)
	}
	image := want
	image.ID, image.Kind = "a2", AttachmentKindImage
	if err := s.InsertAttachment(image); err == nil {
		t.Fatal("shell output was stored as an image")
	}
}
