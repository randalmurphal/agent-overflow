package store

import (
	"errors"
	"testing"
)

func TestUIState_GetEmptyScopeReturnsEmptyMap(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetUIState("client:fresh")
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil map for a fresh scope")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

func TestUIState_SetGetRoundTrip(t *testing.T) {
	s := newTestStore(t)

	entries := map[string]string{
		"sidebar:width":            "312",
		"sidebar:collapsedGroups":  `["proj-a","proj-b"]`,
		"terminalsGroup:collapsed": "1",
	}
	if err := s.SetUIState("client:abc", entries); err != nil {
		t.Fatalf("SetUIState: %v", err)
	}

	got, err := s.GetUIState("client:abc")
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("expected %d entries, got %d: %v", len(entries), len(got), got)
	}
	for key, want := range entries {
		if got[key] != want {
			t.Errorf("key %q: got %q, want %q", key, got[key], want)
		}
	}
}

func TestUIState_UpsertOverwrites(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetUIState("client:abc", map[string]string{"sidebar:width": "280"}); err != nil {
		t.Fatalf("SetUIState initial: %v", err)
	}
	if err := s.SetUIState("client:abc", map[string]string{"sidebar:width": "344"}); err != nil {
		t.Fatalf("SetUIState overwrite: %v", err)
	}

	got, err := s.GetUIState("client:abc")
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if got["sidebar:width"] != "344" {
		t.Fatalf("expected overwritten value 344, got %q", got["sidebar:width"])
	}
	if len(got) != 1 {
		t.Fatalf("upsert must not duplicate rows: %v", got)
	}
}

func TestUIState_ScopesAreIsolated(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetUIState("client:desktop", map[string]string{"sidebar:width": "400"}); err != nil {
		t.Fatalf("SetUIState desktop: %v", err)
	}
	if err := s.SetUIState("client:laptop", map[string]string{"sidebar:width": "240"}); err != nil {
		t.Fatalf("SetUIState laptop: %v", err)
	}

	desktop, err := s.GetUIState("client:desktop")
	if err != nil {
		t.Fatalf("GetUIState desktop: %v", err)
	}
	laptop, err := s.GetUIState("client:laptop")
	if err != nil {
		t.Fatalf("GetUIState laptop: %v", err)
	}
	if desktop["sidebar:width"] != "400" || laptop["sidebar:width"] != "240" {
		t.Fatalf("scopes bled into each other: desktop=%v laptop=%v", desktop, laptop)
	}
}

func TestUIState_DeleteRemovesOnlyNamedKeys(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetUIState("client:abc", map[string]string{"a": "1", "b": "2", "c": "3"}); err != nil {
		t.Fatalf("SetUIState: %v", err)
	}
	if err := s.DeleteUIState("client:abc", []string{"a", "c", "never-existed"}); err != nil {
		t.Fatalf("DeleteUIState: %v", err)
	}

	got, err := s.GetUIState("client:abc")
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if len(got) != 1 || got["b"] != "2" {
		t.Fatalf("expected only b=2 to remain, got %v", got)
	}
}

func TestUIState_DeleteEmptyKeysIsNoOp(t *testing.T) {
	s := newTestStore(t)

	if err := s.SetUIState("client:abc", map[string]string{"a": "1"}); err != nil {
		t.Fatalf("SetUIState: %v", err)
	}
	if err := s.DeleteUIState("client:abc", nil); err != nil {
		t.Fatalf("DeleteUIState with nil keys: %v", err)
	}
	got, err := s.GetUIState("client:abc")
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("no-op delete must not remove rows: %v", got)
	}
}

func TestUIState_EmptyScopeRejected(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.GetUIState("  "); !errors.Is(err, ErrEmptyUIStateScope) {
		t.Fatalf("GetUIState empty scope: got %v, want ErrEmptyUIStateScope", err)
	}
	if err := s.SetUIState("", map[string]string{"a": "1"}); !errors.Is(err, ErrEmptyUIStateScope) {
		t.Fatalf("SetUIState empty scope: got %v, want ErrEmptyUIStateScope", err)
	}
	if err := s.DeleteUIState("", []string{"a"}); !errors.Is(err, ErrEmptyUIStateScope) {
		t.Fatalf("DeleteUIState empty scope: got %v, want ErrEmptyUIStateScope", err)
	}
}

func TestUIState_EmptyKeyRejected(t *testing.T) {
	s := newTestStore(t)

	err := s.SetUIState("client:abc", map[string]string{" ": "1"})
	if err == nil {
		t.Fatal("expected error for empty key")
	}
	// The whole batch must be rejected — no partial writes.
	got, getErr := s.GetUIState("client:abc")
	if getErr != nil {
		t.Fatalf("GetUIState: %v", getErr)
	}
	if len(got) != 0 {
		t.Fatalf("rejected batch must not persist anything: %v", got)
	}
}
