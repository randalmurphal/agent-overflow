package store

import (
	"errors"
	"testing"
	"time"

	"database/sql"
)

func TestAttachmentInsertAndGetRoundTrip(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-attach", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	a := Attachment{
		ID:           "att-1",
		ThreadID:     thread.ID,
		Filename:     "screenshot.png",
		MimeType:     "image/png",
		Size:         1024,
		RelativePath: "thread-attach/att-1.png",
		CreatedAt:    time.Now().UnixMilli(),
	}
	if err := s.InsertAttachment(a); err != nil {
		t.Fatalf("InsertAttachment: %v", err)
	}

	got, ok, err := s.GetAttachment(a.ID)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if !ok {
		t.Fatal("GetAttachment: expected to find attachment")
	}
	if got != a {
		t.Fatalf("attachment mismatch: got %+v want %+v", got, a)
	}
}

func TestAttachmentGetMissingReturnsFalse(t *testing.T) {
	s := newTestStore(t)

	_, ok, err := s.GetAttachment("never-created")
	if err != nil {
		t.Fatalf("GetAttachment error: %v", err)
	}
	if ok {
		t.Fatal("expected not-found, got ok=true")
	}
}

func TestAttachmentListFiltersByThread(t *testing.T) {
	s := newTestStore(t)

	threadA := makeThread("thread-a", "claude")
	threadB := makeThread("thread-b", "codex")
	if err := s.CreateThread(threadA); err != nil {
		t.Fatalf("CreateThread A: %v", err)
	}
	if err := s.CreateThread(threadB); err != nil {
		t.Fatalf("CreateThread B: %v", err)
	}

	base := time.Now().UnixMilli()
	records := []Attachment{
		{ID: "a1", ThreadID: threadA.ID, Filename: "one.png", MimeType: "image/png", Size: 10, RelativePath: "thread-a/a1.png", CreatedAt: base},
		{ID: "a2", ThreadID: threadA.ID, Filename: "two.png", MimeType: "image/png", Size: 20, RelativePath: "thread-a/a2.png", CreatedAt: base + 1},
		{ID: "b1", ThreadID: threadB.ID, Filename: "three.png", MimeType: "image/png", Size: 30, RelativePath: "thread-b/b1.png", CreatedAt: base + 2},
	}
	for _, rec := range records {
		if err := s.InsertAttachment(rec); err != nil {
			t.Fatalf("insert %s: %v", rec.ID, err)
		}
	}

	got, err := s.ListAttachments(threadA.ID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 attachments for thread-a, got %d", len(got))
	}
	if got[0].ID != "a1" || got[1].ID != "a2" {
		t.Fatalf("expected chronological order; got %+v", got)
	}
}

func TestAttachmentDeleteRemovesRow(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-del", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	a := Attachment{
		ID:           "to-delete",
		ThreadID:     thread.ID,
		Filename:     "tmp.png",
		MimeType:     "image/png",
		Size:         1,
		RelativePath: "thread-del/to-delete.png",
		CreatedAt:    time.Now().UnixMilli(),
	}
	if err := s.InsertAttachment(a); err != nil {
		t.Fatalf("InsertAttachment: %v", err)
	}
	if err := s.DeleteAttachment(a.ID); err != nil {
		t.Fatalf("DeleteAttachment: %v", err)
	}
	_, ok, err := s.GetAttachment(a.ID)
	if err != nil {
		t.Fatalf("GetAttachment after delete: %v", err)
	}
	if ok {
		t.Fatal("expected attachment to be gone")
	}

	// Deleting again must surface a no-rows error so callers can tell they
	// tried to drop something that was never there.
	err = s.DeleteAttachment(a.ID)
	if err == nil {
		t.Fatal("expected error deleting missing attachment")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows, got %v", err)
	}
}

func TestAttachmentCascadesOnThreadDelete(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-cascade", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	a := Attachment{
		ID:           "cascade-att",
		ThreadID:     thread.ID,
		Filename:     "x.png",
		MimeType:     "image/png",
		Size:         1,
		RelativePath: "thread-cascade/cascade-att.png",
		CreatedAt:    time.Now().UnixMilli(),
	}
	if err := s.InsertAttachment(a); err != nil {
		t.Fatalf("InsertAttachment: %v", err)
	}
	if err := s.DeleteThread(thread.ID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	got, err := s.ListAttachments(thread.ID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected attachments to cascade; got %+v", got)
	}
}

func TestAttachmentThumbnailRoundTrip(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-thumb", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	a := Attachment{
		ID:           "att-thumb",
		ThreadID:     thread.ID,
		Filename:     "shot.png",
		MimeType:     "image/png",
		Size:         123,
		RelativePath: "thread-thumb/att-thumb.png",
		CreatedAt:    time.Now().UnixMilli(),
	}
	if err := s.InsertAttachment(a); err != nil {
		t.Fatalf("InsertAttachment: %v", err)
	}

	// Fresh row → no thumbnail cached yet.
	if data, mime, hit, err := s.GetAttachmentThumbnail(a.ID); err != nil {
		t.Fatalf("GetAttachmentThumbnail (fresh): %v", err)
	} else if hit || data != nil || mime != "" {
		t.Fatalf("expected uncached miss, got data=%d mime=%q hit=%v", len(data), mime, hit)
	}

	// SetAttachmentThumbnail persists both columns together.
	thumb := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'} // bogus JPEG header is fine for round-trip
	if err := s.SetAttachmentThumbnail(a.ID, thumb, "image/jpeg"); err != nil {
		t.Fatalf("SetAttachmentThumbnail: %v", err)
	}
	gotData, gotMime, hit, err := s.GetAttachmentThumbnail(a.ID)
	if err != nil {
		t.Fatalf("GetAttachmentThumbnail: %v", err)
	}
	if !hit {
		t.Fatal("expected thumbnail to be present after Set")
	}
	if string(gotData) != string(thumb) {
		t.Fatalf("thumbnail bytes round-trip mismatch")
	}
	if gotMime != "image/jpeg" {
		t.Fatalf("thumbnail mime = %q, want image/jpeg", gotMime)
	}
}

func TestAttachmentThumbnailRequiresAttachment(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetAttachmentThumbnail("missing-id", []byte{0xff, 0xd8}, "image/jpeg"); err == nil {
		t.Fatal("expected error setting thumbnail on missing attachment")
	}
}

func TestAttachmentThumbnailRejectsEmpty(t *testing.T) {
	s := newTestStore(t)
	thread := makeThread("thread-thumb-empty", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	a := Attachment{
		ID:           "att-empty",
		ThreadID:     thread.ID,
		Filename:     "shot.png",
		MimeType:     "image/png",
		Size:         1,
		RelativePath: "thread-thumb-empty/att-empty.png",
		CreatedAt:    time.Now().UnixMilli(),
	}
	if err := s.InsertAttachment(a); err != nil {
		t.Fatalf("InsertAttachment: %v", err)
	}
	if err := s.SetAttachmentThumbnail(a.ID, nil, "image/jpeg"); err == nil {
		t.Fatal("expected error on empty data")
	}
	if err := s.SetAttachmentThumbnail(a.ID, []byte{1}, ""); err == nil {
		t.Fatal("expected error on empty mime")
	}
}

func TestAttachmentInsertRequiresThread(t *testing.T) {
	s := newTestStore(t)

	err := s.InsertAttachment(Attachment{
		ID:           "orphan",
		ThreadID:     "no-such-thread",
		Filename:     "x.png",
		MimeType:     "image/png",
		Size:         1,
		RelativePath: "orphan.png",
		CreatedAt:    time.Now().UnixMilli(),
	})
	if err == nil {
		t.Fatal("expected foreign-key error inserting orphan attachment")
	}
}
