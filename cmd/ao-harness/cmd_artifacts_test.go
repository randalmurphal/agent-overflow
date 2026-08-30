package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harnessrun"
)

func seedCLIArtifact(t *testing.T, registryDir string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "failed")
	plan := harnessrun.RunPlan{
		Version: harnessrun.PlanVersion, RunID: "cli-run", Workload: "test",
		DataRoot: root, Ownership: harnessrun.OwnershipFresh,
	}
	registry, err := harnessrun.NewArtifactRegistry(registryDir, harnessrun.RetentionPolicy{MaxBytes: 1, MaxRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := harnessrun.NewWithOptions(plan, time.Now().UTC(), harnessrun.SupervisorOptions{Retention: registry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evidence"), []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Finish(context.Background(), errors.New("failed"), harnessrun.FailureAction, nil); err == nil {
		t.Fatal("failed supervisor returned nil")
	}
	return plan.RunID
}

func runArtifactsCLI(t *testing.T, registryDir string, args ...string) (int, string, string) {
	t.Helper()
	args = append([]string{"artifacts"}, args...)
	args = append(args, "--artifact-registry-dir", registryDir)
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestArtifactsListAndPinJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "registry")
	runID := seedCLIArtifact(t, dir)
	code, stdout, stderr := runArtifactsCLI(t, dir, "list", "-o", "json")
	if code != exitOK || stderr != "" {
		t.Fatalf("list code=%d stderr=%q", code, stderr)
	}
	var listing struct {
		Policy  harnessrun.RetentionPolicy `json:"policy"`
		Entries []harnessrun.ArtifactEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(stdout), &listing); err != nil {
		t.Fatalf("list JSON: %v\n%s", err, stdout)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].RunID != runID {
		t.Fatalf("list entries = %+v", listing.Entries)
	}
	code, stdout, stderr = runArtifactsCLI(t, dir, "pin", runID, "-o", "json")
	if code != exitOK || stderr != "" || !strings.Contains(stdout, `"pinned": true`) {
		t.Fatalf("pin code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestArtifactsCleanDryRunKeepsRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "registry")
	seedCLIArtifact(t, dir)
	entries, err := func() ([]harnessrun.ArtifactEntry, error) {
		registry, err := harnessrun.OpenArtifactRegistry(harnessrun.RegistryOptions{Directory: dir})
		if err != nil {
			return nil, err
		}
		return registry.List()
	}()
	if err != nil {
		t.Fatal(err)
	}
	root := entries[0].Root
	code, stdout, stderr := runArtifactsCLI(t, dir, "clean", "--dry-run", "-o", "json")
	if code != exitOK || stderr != "" {
		t.Fatalf("clean code=%d stderr=%q", code, stderr)
	}
	var result harnessrun.CleanResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("clean JSON: %v\n%s", err, stdout)
	}
	if !result.DryRun || len(result.Pruned) != 1 {
		t.Fatalf("clean result = %+v", result)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("dry-run removed root: %v", err)
	}
}
