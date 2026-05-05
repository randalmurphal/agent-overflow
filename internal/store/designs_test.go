package store

import (
	"strings"
	"testing"
	"time"
)

func TestDesignSnapshotRoundTrip(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-design", "codex")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	snap := DesignSnapshot{
		ID:        "snap-1",
		ThreadID:  thread.ID,
		Label:     "first cut",
		DirPath:   "/tmp/design/snap-1",
		Auto:      false,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.InsertDesignSnapshot(snap); err != nil {
		t.Fatalf("InsertDesignSnapshot: %v", err)
	}

	got, err := s.GetDesignSnapshot(thread.ID, snap.ID)
	if err != nil {
		t.Fatalf("GetDesignSnapshot: %v", err)
	}
	if got != snap {
		t.Fatalf("snapshot mismatch: got %+v want %+v", got, snap)
	}
}

func TestListDesignSnapshotsNewestFirst(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-list", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	older := DesignSnapshot{
		ID:        "snap-older",
		ThreadID:  thread.ID,
		Label:     "v1",
		DirPath:   "/tmp/design/older",
		CreatedAt: time.Now().UnixMilli(),
	}
	newer := DesignSnapshot{
		ID:        "snap-newer",
		ThreadID:  thread.ID,
		Label:     "v2",
		DirPath:   "/tmp/design/newer",
		Auto:      true,
		CreatedAt: time.Now().UnixMilli() + 10,
	}
	for _, snap := range []DesignSnapshot{older, newer} {
		if err := s.InsertDesignSnapshot(snap); err != nil {
			t.Fatalf("InsertDesignSnapshot(%s): %v", snap.ID, err)
		}
	}

	all, err := s.ListDesignSnapshots(thread.ID)
	if err != nil {
		t.Fatalf("ListDesignSnapshots: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("count = %d, want 2", len(all))
	}
	if all[0].ID != newer.ID {
		t.Fatalf("expected newest first, got %q", all[0].ID)
	}
	if !all[0].Auto {
		t.Fatalf("expected newer.Auto = true, got false")
	}
}

func TestDesignSnapshotChildrenAndDelete(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-children", "codex")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	parent := DesignSnapshot{
		ID:        "snap-parent",
		ThreadID:  thread.ID,
		DirPath:   "/tmp/design/parent",
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := s.InsertDesignSnapshot(parent); err != nil {
		t.Fatalf("InsertDesignSnapshot(parent): %v", err)
	}
	child := DesignSnapshot{
		ID:               "snap-child",
		ThreadID:         thread.ID,
		ParentSnapshotID: parent.ID,
		DirPath:          "/tmp/design/child",
		CreatedAt:        time.Now().UnixMilli() + 5,
	}
	if err := s.InsertDesignSnapshot(child); err != nil {
		t.Fatalf("InsertDesignSnapshot(child): %v", err)
	}

	hasChildren, err := s.HasDesignSnapshotChildren(parent.ID)
	if err != nil {
		t.Fatalf("HasDesignSnapshotChildren: %v", err)
	}
	if !hasChildren {
		t.Fatalf("expected parent to have children")
	}

	if err := s.DeleteDesignSnapshot(thread.ID, child.ID); err != nil {
		t.Fatalf("DeleteDesignSnapshot(child): %v", err)
	}
	hasChildren, err = s.HasDesignSnapshotChildren(parent.ID)
	if err != nil {
		t.Fatalf("HasDesignSnapshotChildren after delete: %v", err)
	}
	if hasChildren {
		t.Fatalf("expected no children after delete")
	}
}

func TestInsertDesignSnapshotRequiresExistingThread(t *testing.T) {
	s := newTestStore(t)

	err := s.InsertDesignSnapshot(DesignSnapshot{
		ID:        "snap-orphan",
		ThreadID:  "missing-thread",
		DirPath:   "/tmp/orphan",
		CreatedAt: time.Now().UnixMilli(),
	})
	if err == nil {
		t.Fatal("expected foreign key error, got nil")
	}
	if !strings.Contains(err.Error(), "insert design snapshot") {
		t.Fatalf("unexpected error: %v", err)
	}
}
