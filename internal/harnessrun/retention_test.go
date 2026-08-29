package harnessrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func retainedRun(t *testing.T, registry *ArtifactRegistry, base, id string, size int) ArtifactEntry {
	t.Helper()
	root := filepath.Join(base, id)
	plan := testPlan(root, OwnershipFresh)
	plan.RunID = id
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if id == "new" {
		started = started.Add(time.Hour)
	}
	s, err := NewWithOptions(plan, started, SupervisorOptions{Retention: registry})
	if err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if err := os.WriteFile(filepath.Join(root, "evidence.bin"), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(context.Background(), errors.New("failed"), FailureAction, nil); err == nil {
		t.Fatal("failed run returned nil")
	}
	entries, err := registry.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.RunID == id {
			return entry
		}
	}
	t.Fatalf("run %q was not retained", id)
	return ArtifactEntry{}
}

func TestRetentionDryRunDoesNotMutateRootsOrAccounting(t *testing.T) {
	registry, err := NewArtifactRegistry(filepath.Join(t.TempDir(), "registry"), RetentionPolicy{MaxBytes: 2000, MaxRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	old := retainedRun(t, registry, base, "old", 20)
	retainedRun(t, registry, base, "new", 20)
	result, err := registry.Clean(CleanOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pruned) != 1 || result.Pruned[0].RunID != old.RunID {
		t.Fatalf("dry-run pruned = %+v", result.Pruned)
	}
	if _, err := os.Stat(old.Root); err != nil {
		t.Fatalf("dry-run removed root: %v", err)
	}
	entries, err := registry.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("dry-run changed registry: %+v", entries)
	}
	result, err = registry.Clean(CleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pruned) != 1 || result.Pruned[0].RunID != old.RunID {
		t.Fatalf("clean pruned = %+v", result.Pruned)
	}
	if _, err := os.Stat(old.Root); !os.IsNotExist(err) {
		t.Fatalf("old root still exists after clean: %v", err)
	}
}

func TestRetentionPinUnpinProtectsOldestSelection(t *testing.T) {
	registry, err := NewArtifactRegistry(filepath.Join(t.TempDir(), "registry"), RetentionPolicy{MaxBytes: 500, MaxRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	old := retainedRun(t, registry, base, "old", 10)
	newer := retainedRun(t, registry, base, "new", 10)
	if err := registry.Pin(old.RunID); err != nil {
		t.Fatal(err)
	}
	result, err := registry.Clean(CleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pruned) != 1 || result.Pruned[0].RunID != newer.RunID {
		t.Fatalf("pinned clean pruned = %+v", result.Pruned)
	}
	if err := registry.Unpin(old.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Clean(CleanOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old.Root); !os.IsNotExist(err) {
		t.Fatalf("unpinned root remains: %v", err)
	}
}

func TestRetentionRejectsManifestIdentityChange(t *testing.T) {
	registry, err := NewArtifactRegistry(filepath.Join(t.TempDir(), "registry"), RetentionPolicy{MaxBytes: 500, MaxRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	entry := retainedRun(t, registry, t.TempDir(), "run", 5)
	manifestPath := filepath.Join(entry.Root, ManifestFileName)
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestData = append(manifestData, '\n')
	if err := os.WriteFile(manifestPath, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Clean(CleanOptions{}); err == nil {
		t.Fatal("tampered retained root was deleted")
	}
	if _, err := os.Stat(entry.Root); err != nil {
		t.Fatalf("tampered root disappeared: %v", err)
	}
}

func TestRetentionNeverPrunesLeasedOriginalRoot(t *testing.T) {
	registry, err := NewArtifactRegistry(filepath.Join(t.TempDir(), "registry"), RetentionPolicy{MaxBytes: 1, MaxRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	entry := retainedRun(t, registry, t.TempDir(), "leased", 5)
	lease, err := AcquireLease(entry.DataRoot, "active", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	result, err := registry.Clean(CleanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pruned) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("leased clean result = %+v", result)
	}
	if _, err := os.Stat(entry.Root); err != nil {
		t.Fatalf("leased retained root disappeared: %v", err)
	}
}
