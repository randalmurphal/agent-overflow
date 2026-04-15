package store

import (
	"strings"
	"testing"
	"time"
)

func TestDesignArtifactRoundTrip(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-design", "codex")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	artifact := DesignArtifact{
		ID:          "artifact-1",
		ThreadID:    thread.ID,
		Title:       "Landing page",
		Description: "Homepage concept",
		Kind:        "render",
		HTMLPath:    "/tmp/design.html",
		CreatedAt:   time.Now().UnixMilli(),
	}
	if err := s.InsertDesignArtifact(artifact); err != nil {
		t.Fatalf("InsertDesignArtifact: %v", err)
	}

	got, err := s.GetDesignArtifact(thread.ID, artifact.ID)
	if err != nil {
		t.Fatalf("GetDesignArtifact: %v", err)
	}

	if got != artifact {
		t.Fatalf("artifact mismatch: got %+v want %+v", got, artifact)
	}
}

func TestListDesignArtifactsFiltersByKind(t *testing.T) {
	s := newTestStore(t)

	thread := makeThread("thread-filter", "claude")
	if err := s.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	render := DesignArtifact{
		ID:          "artifact-render",
		ThreadID:    thread.ID,
		Title:       "Render",
		Description: "",
		Kind:        "render",
		HTMLPath:    "/tmp/render.html",
		CreatedAt:   time.Now().UnixMilli(),
	}
	option := DesignArtifact{
		ID:          "artifact-option",
		ThreadID:    thread.ID,
		Title:       "Option",
		Description: "Alternate",
		Kind:        "option",
		HTMLPath:    "/tmp/option.html",
		CreatedAt:   time.Now().UnixMilli() + 1,
	}
	for _, artifact := range []DesignArtifact{render, option} {
		if err := s.InsertDesignArtifact(artifact); err != nil {
			t.Fatalf("InsertDesignArtifact(%s): %v", artifact.ID, err)
		}
	}

	all, err := s.ListDesignArtifacts(thread.ID, "")
	if err != nil {
		t.Fatalf("ListDesignArtifacts(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all count = %d, want 2", len(all))
	}
	if all[0].ID != option.ID {
		t.Fatalf("expected newest artifact first, got %q", all[0].ID)
	}

	filtered, err := s.ListDesignArtifacts(thread.ID, "option")
	if err != nil {
		t.Fatalf("ListDesignArtifacts(option): %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("filtered count = %d, want 1", len(filtered))
	}
	if filtered[0].ID != option.ID {
		t.Fatalf("filtered artifact = %q, want %q", filtered[0].ID, option.ID)
	}
}

func TestInsertDesignArtifactRequiresExistingThread(t *testing.T) {
	s := newTestStore(t)

	err := s.InsertDesignArtifact(DesignArtifact{
		ID:        "artifact-orphan",
		ThreadID:  "missing-thread",
		Title:     "Orphan",
		Kind:      "render",
		HTMLPath:  "/tmp/orphan.html",
		CreatedAt: time.Now().UnixMilli(),
	})
	if err == nil {
		t.Fatal("expected foreign key error, got nil")
	}
	if !strings.Contains(err.Error(), "insert design artifact") {
		t.Fatalf("unexpected error: %v", err)
	}
}
