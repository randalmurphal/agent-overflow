package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGetReturnsDefaultsOnMissingFile(t *testing.T) {
	svc := NewService(t.TempDir())
	got := svc.Get()

	if got.Theme != "system" {
		t.Errorf("Theme = %q, want %q", got.Theme, "system")
	}
	if got.DefaultProvider != "claude" {
		t.Errorf("DefaultProvider = %q, want %q", got.DefaultProvider, "claude")
	}
	if got.StreamingEnabled != true {
		t.Error("StreamingEnabled = false, want true")
	}
	if got.ClaudeEnabled != true {
		t.Error("ClaudeEnabled = false, want true")
	}
	if got.CodexEnabled != true {
		t.Error("CodexEnabled = false, want true")
	}
	if got.DefaultModelClaude != "claude-sonnet-4-6" {
		t.Errorf("DefaultModelClaude = %q, want %q", got.DefaultModelClaude, "claude-sonnet-4-6")
	}
	if got.DefaultModelCodex != "gpt-5.4" {
		t.Errorf("DefaultModelCodex = %q, want %q", got.DefaultModelCodex, "gpt-5.4")
	}
	if got.RecentWorkspaces != nil {
		t.Errorf("RecentWorkspaces = %v, want nil", got.RecentWorkspaces)
	}
	if got.ObservabilityTracingEnabled {
		t.Error("ObservabilityTracingEnabled = true, want false by default")
	}
	if got.ObservabilityEventLogEnabled {
		t.Error("ObservabilityEventLogEnabled = true, want false by default")
	}
	if got.ObservabilityOtlpEndpoint != "" {
		t.Errorf("ObservabilityOtlpEndpoint = %q, want empty by default", got.ObservabilityOtlpEndpoint)
	}
}

func TestGetReturnsDefaultsOnMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{not valid json!!!"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	got := svc.Get()

	if got.Theme != "system" {
		t.Errorf("Theme = %q, want %q", got.Theme, "system")
	}
	if got.StreamingEnabled != true {
		t.Error("StreamingEnabled = false, want true")
	}
	if got.ConfirmDelete != true {
		t.Error("ConfirmDelete = false, want true")
	}
}

func TestUpdatePersistsAndSparseSerializes(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{"theme": "dark"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Theme != "dark" {
		t.Errorf("Theme = %q, want %q", updated.Theme, "dark")
	}

	// Read the file directly to verify sparse serialization.
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}

	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal settings file: %v", err)
	}

	// Only "theme" should be in the file (everything else matches defaults).
	if len(fileMap) != 1 {
		t.Errorf("file contains %d keys, want 1; contents: %s", len(fileMap), string(data))
	}
	if fileMap["theme"] != "dark" {
		t.Errorf("file theme = %v, want %q", fileMap["theme"], "dark")
	}
}

func TestUpdateMergesOverDefaults(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{"theme": "dark"})
	if err != nil {
		t.Fatal(err)
	}

	// Changed field.
	if updated.Theme != "dark" {
		t.Errorf("Theme = %q, want %q", updated.Theme, "dark")
	}
	// All other fields should still be defaults.
	if updated.DefaultProvider != "claude" {
		t.Errorf("DefaultProvider = %q, want %q", updated.DefaultProvider, "claude")
	}
	if updated.StreamingEnabled != true {
		t.Error("StreamingEnabled = false, want true")
	}
	if updated.ConfirmArchive != true {
		t.Error("ConfirmArchive = false, want true")
	}
	if updated.ClaudeBinaryPath != "claude" {
		t.Errorf("ClaudeBinaryPath = %q, want %q", updated.ClaudeBinaryPath, "claude")
	}

	// Read it back via Get to ensure cache and file are consistent.
	svc2 := NewService(dir)
	got := svc2.Get()
	if got.Theme != "dark" {
		t.Errorf("re-read Theme = %q, want %q", got.Theme, "dark")
	}
	if got.StreamingEnabled != true {
		t.Error("re-read StreamingEnabled = false, want true")
	}
}

func TestGetReloadsWhenFileChangesOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	svc := NewService(dir)

	if got := svc.Get(); got.Theme != "system" {
		t.Fatalf("initial Theme = %q, want %q", got.Theme, "system")
	}

	data := []byte("{\n  \"theme\": \"dark\"\n}\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	got := svc.Get()
	if got.Theme != "dark" {
		t.Fatalf("Theme after external edit = %q, want %q", got.Theme, "dark")
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	var wg sync.WaitGroup
	const readers = 20
	const writers = 10

	// Spin up readers.
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				got := svc.Get()
				// Just verify no panic and we get a valid theme.
				if got.Theme == "" {
					t.Error("Get returned empty theme")
				}
			}
		}()
	}

	// Spin up writers.
	themes := []string{"dark", "light", "system"}
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				theme := themes[(i+j)%len(themes)]
				_, err := svc.Update(map[string]any{"theme": theme})
				if err != nil {
					t.Errorf("Update failed: %v", err)
				}
			}
		}()
	}

	wg.Wait()

	// Final read should return a valid settings object.
	final := svc.Get()
	if final.Theme == "" {
		t.Error("final Get returned empty theme")
	}
}

func TestAddRecentWorkspaceDeduplicatesAndCaps(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	// Add 12 workspaces.
	for i := range 12 {
		svc.AddRecentWorkspace(filepath.Join("/projects", string(rune('a'+i))))
	}

	got := svc.Get()
	if len(got.RecentWorkspaces) != 10 {
		t.Fatalf("RecentWorkspaces length = %d, want 10", len(got.RecentWorkspaces))
	}

	// Most recent should be the last added (index 11 -> 'l').
	if got.RecentWorkspaces[0] != filepath.Join("/projects", "l") {
		t.Errorf("first workspace = %q, want %q", got.RecentWorkspaces[0], filepath.Join("/projects", "l"))
	}

	// Oldest surviving should be index 2 -> 'c' (a and b were evicted).
	if got.RecentWorkspaces[9] != filepath.Join("/projects", "c") {
		t.Errorf("last workspace = %q, want %q", got.RecentWorkspaces[9], filepath.Join("/projects", "c"))
	}

	// Add a duplicate that's already in the list (e.g., 'f' at index 6 from original).
	svc.AddRecentWorkspace(filepath.Join("/projects", "f"))
	got = svc.Get()

	if len(got.RecentWorkspaces) != 10 {
		t.Fatalf("after dedup, RecentWorkspaces length = %d, want 10", len(got.RecentWorkspaces))
	}
	if got.RecentWorkspaces[0] != filepath.Join("/projects", "f") {
		t.Errorf("after dedup, first = %q, want %q", got.RecentWorkspaces[0], filepath.Join("/projects", "f"))
	}

	// Verify no duplicates.
	seen := make(map[string]bool)
	for _, ws := range got.RecentWorkspaces {
		if seen[ws] {
			t.Errorf("duplicate workspace: %q", ws)
		}
		seen[ws] = true
	}
}

func TestPathAccessor(t *testing.T) {
	dir := "/some/config/dir"
	svc := NewService(dir)
	want := filepath.Join(dir, "settings.json")
	if svc.Path() != want {
		t.Errorf("Path() = %q, want %q", svc.Path(), want)
	}
}

func TestBlankBinaryPathsSerializeAsDefaults(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{
		"claudeBinaryPath": "   ",
		"codexBinaryPath":  "",
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.ClaudeBinaryPath != DefaultSettings.ClaudeBinaryPath {
		t.Fatalf("ClaudeBinaryPath = %q, want %q", updated.ClaudeBinaryPath, DefaultSettings.ClaudeBinaryPath)
	}
	if updated.CodexBinaryPath != DefaultSettings.CodexBinaryPath {
		t.Fatalf("CodexBinaryPath = %q, want %q", updated.CodexBinaryPath, DefaultSettings.CodexBinaryPath)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal settings file: %v", err)
	}
	if len(fileMap) != 0 {
		t.Fatalf("settings file = %s, want empty sparse object", string(data))
	}
}

func TestObservabilityDefaults(t *testing.T) {
	svc := NewService(t.TempDir())
	got := svc.Get()
	if got.ObservabilityTracingEnabled {
		t.Error("ObservabilityTracingEnabled = true, want false by default")
	}
	if got.ObservabilityEventLogEnabled {
		t.Error("ObservabilityEventLogEnabled = true, want false by default")
	}
	if got.ObservabilityOtlpEndpoint != "" {
		t.Errorf("ObservabilityOtlpEndpoint = %q, want empty by default", got.ObservabilityOtlpEndpoint)
	}
}

func TestObservabilitySettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{
		"observabilityTracingEnabled":  true,
		"observabilityOtlpEndpoint":    "  localhost:4317  ",
		"observabilityEventLogEnabled": true,
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !updated.ObservabilityTracingEnabled {
		t.Error("ObservabilityTracingEnabled = false, want true")
	}
	if !updated.ObservabilityEventLogEnabled {
		t.Error("ObservabilityEventLogEnabled = false, want true")
	}
	if updated.ObservabilityOtlpEndpoint != "localhost:4317" {
		t.Errorf("ObservabilityOtlpEndpoint = %q, want %q (trimmed)", updated.ObservabilityOtlpEndpoint, "localhost:4317")
	}

	// Reload from disk to ensure persistence survives service restart.
	svc2 := NewService(dir)
	reloaded := svc2.Get()
	if !reloaded.ObservabilityTracingEnabled {
		t.Error("After reload: tracing enabled should persist")
	}
	if reloaded.ObservabilityOtlpEndpoint != "localhost:4317" {
		t.Errorf("After reload: endpoint = %q, want %q", reloaded.ObservabilityOtlpEndpoint, "localhost:4317")
	}
	if !reloaded.ObservabilityEventLogEnabled {
		t.Error("After reload: event log enabled should persist")
	}
}

func TestObservabilityOtlpEndpointBlankSerializesAsDefault(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{
		"observabilityOtlpEndpoint": "   ",
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	// Whitespace trims to empty, which matches the default, so the field
	// should not appear in the sparse file.
	if updated.ObservabilityOtlpEndpoint != "" {
		t.Errorf("trimmed endpoint = %q, want empty", updated.ObservabilityOtlpEndpoint)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := fileMap["observabilityOtlpEndpoint"]; present {
		t.Errorf("file contains observabilityOtlpEndpoint when it should be sparse-omitted: %s", string(data))
	}
}
