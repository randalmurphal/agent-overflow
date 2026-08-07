package selfupdate

import (
	"os"
	"testing"
	"time"
)

func TestMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Marker{
		ExpectedVersion: "0.0.11",
		PriorVersion:    "0.0.10",
		StagedAt:        time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
	}
	if err := SaveMarker(dir, want); err != nil {
		t.Fatalf("SaveMarker: %v", err)
	}

	got, err := LoadMarker(dir)
	if err != nil {
		t.Fatalf("LoadMarker: %v", err)
	}
	if got == nil {
		t.Fatal("LoadMarker returned nil after a save")
	}
	if got.ExpectedVersion != want.ExpectedVersion || got.PriorVersion != want.PriorVersion {
		t.Fatalf("marker = %+v, want %+v", *got, want)
	}
	if !got.StagedAt.Equal(want.StagedAt) {
		t.Fatalf("StagedAt = %v, want %v", got.StagedAt, want.StagedAt)
	}
}

func TestLoadMarkerAbsent(t *testing.T) {
	got, err := LoadMarker(t.TempDir())
	if err != nil {
		t.Fatalf("LoadMarker on an empty dir: %v", err)
	}
	if got != nil {
		t.Fatalf("LoadMarker on an empty dir = %+v, want nil", *got)
	}
}

func TestClearMarkerIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := SaveMarker(dir, Marker{ExpectedVersion: "0.0.11", PriorVersion: "0.0.10", StagedAt: time.Now()}); err != nil {
		t.Fatalf("SaveMarker: %v", err)
	}
	for i := range 2 {
		if err := ClearMarker(dir); err != nil {
			t.Fatalf("ClearMarker call %d: %v", i+1, err)
		}
	}
	got, err := LoadMarker(dir)
	if err != nil {
		t.Fatalf("LoadMarker after clear: %v", err)
	}
	if got != nil {
		t.Fatalf("marker survived ClearMarker: %+v", *got)
	}
}

func TestLoadMarkerCorruptFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(MarkerPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt marker: %v", err)
	}
	got, err := LoadMarker(dir)
	if err == nil {
		t.Fatalf("LoadMarker on a corrupt marker = (%v, nil), want an error", got)
	}
	if got != nil {
		t.Fatalf("LoadMarker on a corrupt marker returned %+v, want nil", *got)
	}
}

func TestSaveMarkerRejectsUnusableExpectedVersion(t *testing.T) {
	if err := SaveMarker(t.TempDir(), Marker{PriorVersion: "0.0.10"}); err == nil {
		t.Fatal("SaveMarker with an empty ExpectedVersion = nil, want an error")
	}
}
