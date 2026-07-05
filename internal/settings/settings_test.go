package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	if got.StreamingEnabled != true {
		t.Error("StreamingEnabled = false, want true")
	}
	if got.ClaudeEnabled != true {
		t.Error("ClaudeEnabled = false, want true")
	}
	if got.CodexEnabled != true {
		t.Error("CodexEnabled = false, want true")
	}
	if got.DefaultThreadEnvMode != "local" {
		t.Errorf("DefaultThreadEnvMode = %q, want %q", got.DefaultThreadEnvMode, "local")
	}
	if got.WorktreeBranchPrefix != "ao-" {
		t.Errorf("WorktreeBranchPrefix = %q, want %q", got.WorktreeBranchPrefix, "ao-")
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
	if got.ProjectSortMode != "lastActivity" {
		t.Errorf("ProjectSortMode = %q, want %q", got.ProjectSortMode, "lastActivity")
	}
	if got.CollapsedProjects != nil {
		t.Errorf("CollapsedProjects = %v, want nil", got.CollapsedProjects)
	}
	if got.PaneLayout.Version != CurrentPaneLayoutVersion {
		t.Errorf("PaneLayout.Version = %d, want %d", got.PaneLayout.Version, CurrentPaneLayoutVersion)
	}
	if got.PaneLayout.Panes != nil {
		t.Errorf("PaneLayout.Panes = %v, want nil", got.PaneLayout.Panes)
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

func TestMalformedJSONPreservesOriginalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	rawCorrupt := []byte("{this is broken")
	if err := os.WriteFile(path, rawCorrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	_ = svc.Get()

	// The original settings.json must be moved aside, not silently
	// dropped. Find a sibling matching settings.json.corrupt-* and
	// confirm it carries the bytes we wrote.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var preservedPath string
	for _, ent := range entries {
		name := ent.Name()
		if name == "settings.json" || ent.IsDir() {
			continue
		}
		if len(name) > len("settings.json.corrupt-") && name[:len("settings.json.corrupt-")] == "settings.json.corrupt-" {
			preservedPath = filepath.Join(dir, name)
			break
		}
	}
	if preservedPath == "" {
		t.Fatalf("expected a settings.json.corrupt-* file in %s; got %d entries", dir, len(entries))
	}
	preserved, err := os.ReadFile(preservedPath)
	if err != nil {
		t.Fatalf("read preserved corrupt file: %v", err)
	}
	if string(preserved) != string(rawCorrupt) {
		t.Errorf("preserved corrupt file = %q, want %q", preserved, rawCorrupt)
	}
	// settings.json itself should now be absent (we moved it aside)
	// until the next writeSparse re-creates it.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("settings.json still present after corruption-aside; expected ENOENT, got %v", err)
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

	// "theme" is the user-set value; "$schemaVersion" is stamped on
	// every write so a future loader can branch on a missing/older
	// version. Two entries total — anything else means a sparse-write
	// regression that's writing default-valued fields.
	if len(fileMap) != 2 {
		t.Errorf("file contains %d keys, want 2 (theme + $schemaVersion); contents: %s", len(fileMap), string(data))
	}
	if fileMap["theme"] != "dark" {
		t.Errorf("file theme = %v, want %q", fileMap["theme"], "dark")
	}
	// SchemaVersion stamped at write time. The literal value matches
	// CurrentSchemaVersion; if the version bumps we update both.
	if got, _ := fileMap["$schemaVersion"].(float64); int(got) != CurrentSchemaVersion {
		t.Errorf("file $schemaVersion = %v, want %d", fileMap["$schemaVersion"], CurrentSchemaVersion)
	}
}

// TestNetworkSettingsBindAllRoundTripAndSparseDefault confirms the
// Phase E LAN-bind toggle persists through the same path as every
// other setting: Update writes the patch, the file omits the key when
// it equals the default, and a fresh service reload yields the same
// in-memory value.
func TestNetworkSettingsBindAllRoundTripAndSparseDefault(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{"network": map[string]any{"bindAll": true}})
	if err != nil {
		t.Fatalf("Update(bindAll=true) error = %v", err)
	}
	if !updated.Network.BindAll {
		t.Fatal("Network.BindAll = false, want true")
	}

	reloaded := NewService(dir).Get()
	if !reloaded.Network.BindAll {
		t.Fatal("reloaded Network.BindAll = false, want true")
	}

	// Toggle back to default; file must omit the network key entirely
	// since the zero-valued struct equals DefaultSettings.Network.
	updated, err = svc.Update(map[string]any{"network": map[string]any{"bindAll": false}})
	if err != nil {
		t.Fatalf("Update(bindAll=false) error = %v", err)
	}
	if updated.Network.BindAll {
		t.Fatal("Network.BindAll = true, want false")
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal settings file: %v", err)
	}
	if _, ok := fileMap["network"]; ok {
		t.Fatalf("settings file = %s, want network omitted when default", string(data))
	}
}

// TestWindowGeometryRoundTripAndSparseDefault confirms the desktop window
// placement persists like every other nested setting: Update writes the
// patch, a fresh service reload yields the same placement, and the file omits
// the key once it equals the (never-saved) default again.
func TestWindowGeometryRoundTripAndSparseDefault(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{"window": map[string]any{
		"x": 120, "y": 80, "width": 1100, "height": 720, "maximized": true, "valid": true,
	}})
	if err != nil {
		t.Fatalf("Update(window) error = %v", err)
	}
	if updated.Window.X != 120 || updated.Window.Width != 1100 || !updated.Window.Maximized || !updated.Window.Valid {
		t.Fatalf("Window = %+v, want x=120 w=1100 maximized valid", updated.Window)
	}

	reloaded := NewService(dir).Get()
	if reloaded.Window.X != 120 || reloaded.Window.Height != 720 || !reloaded.Window.Valid {
		t.Fatalf("reloaded Window = %+v, want persisted placement", reloaded.Window)
	}

	// Reset to the zero (never-saved) placement; the file must omit the
	// window key entirely since it now equals DefaultSettings.Window.
	if _, err := svc.Update(map[string]any{"window": map[string]any{}}); err != nil {
		t.Fatalf("Update(window reset) error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal settings file: %v", err)
	}
	if _, ok := fileMap["window"]; ok {
		t.Fatalf("settings file = %s, want window omitted when default", string(data))
	}
}

// TestCollapseDiffPreviewsRoundTripAndSparseDefault confirms the
// chat-timeline diff-collapse toggle persists like every other
// setting: Update writes the patch, a fresh service reload yields the
// same value, and the file omits the key once it equals the default
// again.
func TestCollapseDiffPreviewsRoundTripAndSparseDefault(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{"collapseDiffPreviews": true})
	if err != nil {
		t.Fatalf("Update(collapseDiffPreviews=true) error = %v", err)
	}
	if !updated.CollapseDiffPreviews {
		t.Fatal("CollapseDiffPreviews = false, want true")
	}

	reloaded := NewService(dir).Get()
	if !reloaded.CollapseDiffPreviews {
		t.Fatal("reloaded CollapseDiffPreviews = false, want true")
	}

	updated, err = svc.Update(map[string]any{"collapseDiffPreviews": false})
	if err != nil {
		t.Fatalf("Update(collapseDiffPreviews=false) error = %v", err)
	}
	if updated.CollapseDiffPreviews {
		t.Fatal("CollapseDiffPreviews = true, want false")
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal settings file: %v", err)
	}
	if _, ok := fileMap["collapseDiffPreviews"]; ok {
		t.Fatalf("settings file = %s, want collapseDiffPreviews omitted when default", string(data))
	}
}

// TestLowPowerModeRoundTripAndSparseDefault confirms the low-power
// rendering toggle persists like every other setting: Update writes
// the patch, a fresh service reload yields the same value, and the
// file omits the key once it equals the default again.
func TestLowPowerModeRoundTripAndSparseDefault(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{"lowPowerMode": true})
	if err != nil {
		t.Fatalf("Update(lowPowerMode=true) error = %v", err)
	}
	if !updated.LowPowerMode {
		t.Fatal("LowPowerMode = false, want true")
	}

	reloaded := NewService(dir).Get()
	if !reloaded.LowPowerMode {
		t.Fatal("reloaded LowPowerMode = false, want true")
	}

	updated, err = svc.Update(map[string]any{"lowPowerMode": false})
	if err != nil {
		t.Fatalf("Update(lowPowerMode=false) error = %v", err)
	}
	if updated.LowPowerMode {
		t.Fatal("LowPowerMode = true, want false")
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal settings file: %v", err)
	}
	if _, ok := fileMap["lowPowerMode"]; ok {
		t.Fatalf("settings file = %s, want lowPowerMode omitted when default", string(data))
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
	// SchemaVersion is the only key that should land — every user-set
	// value validated to the default and was sparse-omitted, but the
	// version stamp goes on every write.
	if len(fileMap) != 1 {
		t.Fatalf("settings file = %s, want only $schemaVersion", string(data))
	}
	if got, _ := fileMap["$schemaVersion"].(float64); int(got) != CurrentSchemaVersion {
		t.Fatalf("settings file = %s, want $schemaVersion = %d", string(data), CurrentSchemaVersion)
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

func TestRetiredChatDefaultSettingsAreDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	raw := []byte(`{
  "defaultProvider": "codex",
  "defaultModelClaude": "claude-sonnet-4-6",
  "defaultModelCodex": "gpt-5.4",
  "defaultRuntimeMode": "approval-required",
  "modelContextWindows": {"claude-sonnet-4-6": 200000},
  "unknownFutureSetting": {"keep": true}
}`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{"theme": "dark"})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Theme != "dark" {
		t.Fatalf("Theme = %q, want dark", updated.Theme)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal settings file: %v", err)
	}
	for _, key := range []string{
		"defaultProvider",
		"defaultModelClaude",
		"defaultModelCodex",
		"defaultRuntimeMode",
		"modelContextWindows",
	} {
		if _, ok := fileMap[key]; ok {
			t.Fatalf("retired key %q survived in settings file: %s", key, string(data))
		}
	}
	if _, ok := fileMap["unknownFutureSetting"]; !ok {
		t.Fatalf("unknown future setting was dropped: %s", string(data))
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

// TestUpdatePreservesUnknownFields ensures that fields the Settings struct
// doesn't know about (forward-compat or downgrade scenarios) survive an
// Update call. Without unknown-field preservation, the round-trip through
// json.Marshal(Settings) drops anything not mapped to a struct field.
func TestUpdatePreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Seed the file with known + unknown fields of varied types.
	initial := map[string]any{
		"theme":             "system",
		"futureFlag":        true,
		"futureString":      "hello",
		"futureNumber":      42,
		"futureObject":      map[string]any{"nested": "value", "count": 7},
		"futureArray":       []any{"a", "b", "c"},
		"futureNullLiteral": nil,
	}
	raw, err := json.MarshalIndent(initial, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	// Updating an unrelated known field must not drop the unknowns.
	if _, err := svc.Update(map[string]any{"theme": "dark"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var fileMap map[string]any
	if err := json.Unmarshal(after, &fileMap); err != nil {
		t.Fatalf("unmarshal after update: %v", err)
	}

	// Known field should reflect the update.
	if fileMap["theme"] != "dark" {
		t.Errorf("theme = %v, want %q", fileMap["theme"], "dark")
	}

	// All unknowns must round-trip.
	if v, ok := fileMap["futureFlag"].(bool); !ok || v != true {
		t.Errorf("futureFlag = %v (%T), want true", fileMap["futureFlag"], fileMap["futureFlag"])
	}
	if v, ok := fileMap["futureString"].(string); !ok || v != "hello" {
		t.Errorf("futureString = %v, want %q", fileMap["futureString"], "hello")
	}
	// JSON decoded numbers land as float64 in a map[string]any.
	if v, ok := fileMap["futureNumber"].(float64); !ok || v != 42 {
		t.Errorf("futureNumber = %v, want 42", fileMap["futureNumber"])
	}
	obj, ok := fileMap["futureObject"].(map[string]any)
	if !ok {
		t.Fatalf("futureObject = %v (%T), want map", fileMap["futureObject"], fileMap["futureObject"])
	}
	if obj["nested"] != "value" {
		t.Errorf("futureObject.nested = %v, want %q", obj["nested"], "value")
	}
	if obj["count"] != float64(7) {
		t.Errorf("futureObject.count = %v, want 7", obj["count"])
	}
	arr, ok := fileMap["futureArray"].([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("futureArray = %v (%T)", fileMap["futureArray"], fileMap["futureArray"])
	}
	if arr[0] != "a" || arr[1] != "b" || arr[2] != "c" {
		t.Errorf("futureArray = %v, want [a b c]", arr)
	}
	// Null must be present as a key; it survives as nil in map[string]any.
	if _, present := fileMap["futureNullLiteral"]; !present {
		t.Errorf("futureNullLiteral key missing")
	}
}

// TestAddRecentWorkspacePreservesUnknownFields guards the other writer on
// the service. Adding a workspace path must not drop unknowns either, since
// it goes through writeSparse.
func TestAddRecentWorkspacePreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	initial := map[string]any{
		"futureFlag": true,
	}
	raw, err := json.MarshalIndent(initial, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(dir)
	svc.AddRecentWorkspace("/tmp/workspace")

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(after, &fileMap); err != nil {
		t.Fatalf("unmarshal after add: %v", err)
	}
	if v, ok := fileMap["futureFlag"].(bool); !ok || v != true {
		t.Errorf("futureFlag = %v (%T), want true", fileMap["futureFlag"], fileMap["futureFlag"])
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

// TestUpdateSettings_RejectsRemoteEndpointsKey pins the patch-boundary
// guard added to Service.Update: a caller that includes
// "remoteEndpoints" in a generic patch must be refused outright. The
// underlying applyPatch does wholesale top-level merge, so an accepted
// patch carrying a redacted (token-empty) slice would clobber every
// saved token. The dedicated CRUD helpers (AddRemoteEndpoint /
// UpdateRemoteEndpoint / DeleteRemoteEndpoint / TouchRemoteEndpoint)
// are the only sanctioned write path for that field.
func TestUpdateSettings_RejectsRemoteEndpointsKey(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	patches := []map[string]any{
		{"remoteEndpoints": []any{}},
		{"remoteEndpoints": nil},
		{"theme": "dark", "remoteEndpoints": []any{map[string]any{"id": "x", "url": "ws://h/", "token": ""}}},
	}
	for i, patch := range patches {
		if _, err := svc.Update(patch); err == nil {
			t.Errorf("patch %d: Update accepted patch carrying remoteEndpoints, want error", i)
		}
	}

	// The companion patch with theme set should not have persisted —
	// reject-at-boundary means atomicity: no half-applied write.
	got := svc.Get()
	if got.Theme == "dark" {
		t.Errorf("theme leaked through despite rejected patch: %+v", got)
	}
}

// TestGetSettings_UpdateSettings_RoundTripPreservesTokens guards the
// full round-trip: GetSettings (token-redacted) -> mutate one field ->
// Update(full-struct-as-patch). Even though today's frontend uses
// sparse patches, a future caller / refactor must not be able to clobber
// the persisted tokens. With the patch-boundary guard, Update returns
// an error and the on-disk tokens remain intact.
func TestGetSettings_UpdateSettings_RoundTripPreservesTokens(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if _, err := svc.AddRemoteEndpoint("Tailnet", "ws://10.0.0.5:54321/", "real-secret-token"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Simulate a hostile / regressing caller doing
	// GetSettings -> mutate -> Update(full struct as map).
	full := svc.Get()
	// Redact tokens like GetSettings on App does.
	for i := range full.RemoteEndpoints {
		full.RemoteEndpoints[i].Token = ""
	}
	patch := map[string]any{
		"theme":           "dark",
		"remoteEndpoints": full.RemoteEndpoints,
	}

	if _, err := svc.Update(patch); err == nil {
		t.Fatal("Update accepted full-struct patch with remoteEndpoints, want error")
	}

	// The persisted token must remain intact — neither the rejection
	// path nor any partial-write fallout is allowed to drop the value.
	reloaded := NewService(dir).Get()
	if len(reloaded.RemoteEndpoints) != 1 {
		t.Fatalf("endpoint count after rejected patch = %d, want 1; %+v", len(reloaded.RemoteEndpoints), reloaded.RemoteEndpoints)
	}
	if reloaded.RemoteEndpoints[0].Token != "real-secret-token" {
		t.Fatalf("token clobbered by rejected patch: %q", reloaded.RemoteEndpoints[0].Token)
	}
	// Theme should also be untouched (atomicity of the rejection).
	if reloaded.Theme == "dark" {
		t.Fatalf("theme leaked through despite rejected patch: %+v", reloaded)
	}
}

// TestLoad_AcceptsFileWithoutSchemaVersion documents the backward-compat
// guarantee for files written by older builds that pre-date schema
// versioning. The loader must not refuse / reset such files.
func TestLoad_AcceptsFileWithoutSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := NewService(dir).Get()
	if got.Theme != "dark" {
		t.Fatalf("Theme = %q, want dark (loader rejected unversioned file?)", got.Theme)
	}
}

// TestLoad_AcceptsForwardCompatSchemaVersion documents the
// forward-compat guarantee: a file from a future build (higher schema
// version) loads with whatever fields the current struct knows about,
// rather than refusing the whole file. captureUnknownFields preserves
// any future keys for a re-write.
func TestLoad_AcceptsForwardCompatSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"$schemaVersion":99,"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := NewService(dir).Get()
	if got.Theme != "dark" {
		t.Fatalf("Theme = %q, want dark (loader rejected forward-version file?)", got.Theme)
	}
}

func TestProjectSortModeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{
		"projectSortMode":   "manual",
		"collapsedProjects": []any{"proj-1", "proj-2"},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.ProjectSortMode != "manual" {
		t.Errorf("ProjectSortMode = %q, want %q", updated.ProjectSortMode, "manual")
	}
	if len(updated.CollapsedProjects) != 2 || updated.CollapsedProjects[0] != "proj-1" {
		t.Errorf("CollapsedProjects = %v, want [proj-1 proj-2]", updated.CollapsedProjects)
	}

	reloaded := NewService(dir).Get()
	if reloaded.ProjectSortMode != "manual" {
		t.Errorf("reloaded ProjectSortMode = %q, want %q", reloaded.ProjectSortMode, "manual")
	}
	if len(reloaded.CollapsedProjects) != 2 {
		t.Errorf("reloaded CollapsedProjects = %v, want 2 entries", reloaded.CollapsedProjects)
	}

	// Resetting to default should sparse-omit both keys.
	updated, err = svc.Update(map[string]any{
		"projectSortMode":   "lastActivity",
		"collapsedProjects": []any{},
	})
	if err != nil {
		t.Fatalf("Update(reset) error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fileMap["projectSortMode"]; ok {
		t.Errorf("file still contains projectSortMode when default; contents: %s", string(data))
	}
	if _, ok := fileMap["collapsedProjects"]; ok {
		t.Errorf("file still contains collapsedProjects when empty; contents: %s", string(data))
	}
}

func TestProjectSortModeValidation(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	_, err := svc.Update(map[string]any{"projectSortMode": "bogus"})
	if err == nil {
		t.Fatal("expected error for invalid projectSortMode, got nil")
	}

	for _, mode := range []string{"lastActivity", "createdAt", "manual"} {
		_, err := svc.Update(map[string]any{"projectSortMode": mode})
		if err != nil {
			t.Errorf("Update(projectSortMode=%q) error = %v", mode, err)
		}
	}
}

// TestUsagePeriodRoundTripAndValidation — the usage period persists in
// Go settings (not webview localStorage: the transport binds an
// ephemeral port, so the webview origin — and its localStorage —
// changes every launch). Round-trip, reject-invalid, and sanitize-on-
// load must all hold or the selection silently resets to the default.
func TestUsagePeriodRoundTripAndValidation(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	if got := svc.Get().UsagePeriod; got != "month" {
		t.Fatalf("default UsagePeriod = %q, want month", got)
	}

	for _, period := range []string{"day", "week", "all", "month"} {
		if _, err := svc.Update(map[string]any{"usagePeriod": period}); err != nil {
			t.Errorf("Update(usagePeriod=%q) error = %v", period, err)
		}
	}
	if _, err := svc.Update(map[string]any{"usagePeriod": "week"}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if reloaded := NewService(dir).Get().UsagePeriod; reloaded != "week" {
		t.Errorf("reloaded UsagePeriod = %q, want week", reloaded)
	}

	if _, err := svc.Update(map[string]any{"usagePeriod": "30d"}); err == nil {
		t.Fatal("expected error for invalid usagePeriod (legacy rolling value), got nil")
	}

	// Load path: dirty JSON sanitizes back to the default.
	raw := `{"usagePeriod":"bogus"}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if loaded := NewService(dir).Get().UsagePeriod; loaded != "month" {
		t.Errorf("sanitized UsagePeriod = %q, want month", loaded)
	}
}

func TestCollapsedProjectsSanitization(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	// Write path: dedup, trim, drop empty.
	updated, err := svc.Update(map[string]any{
		"collapsedProjects": []any{"", "  ", " p1 ", "p1", "p2"},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if len(updated.CollapsedProjects) != 2 ||
		updated.CollapsedProjects[0] != "p1" ||
		updated.CollapsedProjects[1] != "p2" {
		t.Errorf("write-path sanitize: got %v, want [p1 p2]", updated.CollapsedProjects)
	}

	// Load path: write dirty JSON directly and verify Get() sanitizes.
	raw := `{"projectSortMode":"bogus","collapsedProjects":["","  x  ","x","y"]}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	loaded := NewService(dir).Get()
	if loaded.ProjectSortMode != "lastActivity" {
		t.Errorf("load-path sanitize: ProjectSortMode = %q, want %q", loaded.ProjectSortMode, "lastActivity")
	}
	if len(loaded.CollapsedProjects) != 2 ||
		loaded.CollapsedProjects[0] != "x" ||
		loaded.CollapsedProjects[1] != "y" {
		t.Errorf("load-path sanitize: CollapsedProjects = %v, want [x y]", loaded.CollapsedProjects)
	}
}

func TestHiddenModelsRoundTripAndSanitization(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	// Write path: dedup, trim, drop empty; both provider lists persist.
	updated, err := svc.Update(map[string]any{
		"claudeHiddenModels": []any{"", " claude-opus-4-5 ", "claude-opus-4-5", "claude-haiku-4-5"},
		"codexHiddenModels":  []any{"gpt-5.2"},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if len(updated.ClaudeHiddenModels) != 2 ||
		updated.ClaudeHiddenModels[0] != "claude-opus-4-5" ||
		updated.ClaudeHiddenModels[1] != "claude-haiku-4-5" {
		t.Errorf("write-path sanitize: ClaudeHiddenModels = %v, want [claude-opus-4-5 claude-haiku-4-5]", updated.ClaudeHiddenModels)
	}
	if len(updated.CodexHiddenModels) != 1 || updated.CodexHiddenModels[0] != "gpt-5.2" {
		t.Errorf("CodexHiddenModels = %v, want [gpt-5.2]", updated.CodexHiddenModels)
	}

	reloaded := NewService(dir).Get()
	if len(reloaded.ClaudeHiddenModels) != 2 || len(reloaded.CodexHiddenModels) != 1 {
		t.Errorf("reloaded hidden models = %v / %v, want 2 / 1 entries",
			reloaded.ClaudeHiddenModels, reloaded.CodexHiddenModels)
	}

	// Load path: dirty JSON sanitizes on Get().
	raw := `{"claudeHiddenModels":["","  claude-opus-4-5  ","claude-opus-4-5"]}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	loaded := NewService(dir).Get()
	if len(loaded.ClaudeHiddenModels) != 1 || loaded.ClaudeHiddenModels[0] != "claude-opus-4-5" {
		t.Errorf("load-path sanitize: ClaudeHiddenModels = %v, want [claude-opus-4-5]", loaded.ClaudeHiddenModels)
	}

	// Emptying the lists sparse-omits the keys.
	if _, err := svc.Update(map[string]any{
		"claudeHiddenModels": []any{},
		"codexHiddenModels":  []any{},
	}); err != nil {
		t.Fatalf("Update(reset) error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fileMap["claudeHiddenModels"]; ok {
		t.Errorf("file still contains claudeHiddenModels when empty; contents: %s", string(data))
	}
	if _, ok := fileMap["codexHiddenModels"]; ok {
		t.Errorf("file still contains codexHiddenModels when empty; contents: %s", string(data))
	}
}

func TestHiddenModelsRejectsOversizedList(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	oversized := make([]any, MaxHiddenModels+1)
	for i := range oversized {
		oversized[i] = "model-" + strconv.Itoa(i)
	}
	// The write path must reject rather than silently truncate — only
	// the load path caps quietly (a mangled file shouldn't brick Get).
	_, err := svc.Update(map[string]any{"claudeHiddenModels": oversized})
	if err == nil {
		t.Fatal("expected error for oversized claudeHiddenModels, got nil")
	}
	if !strings.Contains(err.Error(), "claudeHiddenModels") {
		t.Fatalf("error = %v, want mention of claudeHiddenModels", err)
	}
	if got := NewService(dir).Get().ClaudeHiddenModels; got != nil {
		t.Fatalf("ClaudeHiddenModels = %v, want nil after rejected update", got)
	}
}

func TestHiddenModelsForProvider(t *testing.T) {
	s := Settings{
		ClaudeHiddenModels: []string{"claude-opus-4-5"},
		CodexHiddenModels:  []string{"gpt-5.2"},
	}
	// claude-tui shares the claude list — one binary, one catalog.
	for _, providerName := range []string{"claude", "claude-tui"} {
		got := s.HiddenModelsForProvider(providerName)
		if len(got) != 1 || got[0] != "claude-opus-4-5" {
			t.Errorf("HiddenModelsForProvider(%q) = %v, want [claude-opus-4-5]", providerName, got)
		}
	}
	if got := s.HiddenModelsForProvider("codex"); len(got) != 1 || got[0] != "gpt-5.2" {
		t.Errorf("HiddenModelsForProvider(codex) = %v, want [gpt-5.2]", got)
	}
	if got := s.HiddenModelsForProvider("unknown"); got != nil {
		t.Errorf("HiddenModelsForProvider(unknown) = %v, want nil", got)
	}
}

func TestPaneLayoutRoundTripAndSparseDefault(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{
		"paneLayout": map[string]any{
			"version":       1,
			"focusedPaneId": "right",
			"panes": []any{
				map[string]any{"paneId": "left", "threadId": "thread-left", "ratio": 0.75},
				map[string]any{"paneId": "right", "threadId": "thread-right", "ratio": 1.25},
			},
		},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.PaneLayout.Version != CurrentPaneLayoutVersion {
		t.Fatalf("PaneLayout.Version = %d, want %d", updated.PaneLayout.Version, CurrentPaneLayoutVersion)
	}
	if len(updated.PaneLayout.Panes) != 2 {
		t.Fatalf("PaneLayout.Panes = %v, want 2 panes", updated.PaneLayout.Panes)
	}
	if updated.PaneLayout.FocusedPaneID != "right" {
		t.Fatalf("FocusedPaneID = %q, want right", updated.PaneLayout.FocusedPaneID)
	}

	reloaded := NewService(dir).Get()
	if reloaded.PaneLayout.FocusedPaneID != "right" {
		t.Fatalf("reloaded FocusedPaneID = %q, want right", reloaded.PaneLayout.FocusedPaneID)
	}
	if len(reloaded.PaneLayout.Panes) != 2 || reloaded.PaneLayout.Panes[1].ThreadID != "thread-right" {
		t.Fatalf("reloaded PaneLayout.Panes = %+v", reloaded.PaneLayout.Panes)
	}

	updated, err = svc.Update(map[string]any{
		"paneLayout": map[string]any{
			"version":       1,
			"focusedPaneId": "",
			"panes":         []any{},
		},
	})
	if err != nil {
		t.Fatalf("Update(reset) error = %v", err)
	}
	if len(updated.PaneLayout.Panes) != 0 {
		t.Fatalf("reset PaneLayout.Panes = %+v, want empty", updated.PaneLayout.Panes)
	}

	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fileMap map[string]any
	if err := json.Unmarshal(data, &fileMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := fileMap["paneLayout"]; ok {
		t.Fatalf("settings file = %s, want paneLayout omitted when default", string(data))
	}
}

func TestPaneLayoutSanitization(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)

	updated, err := svc.Update(map[string]any{
		"paneLayout": map[string]any{
			"version":       1,
			"focusedPaneId": "missing",
			"panes": []any{
				map[string]any{"paneId": " left ", "threadId": " thread-left ", "ratio": 0.75},
				map[string]any{"paneId": "left", "threadId": "thread-left-duplicate", "ratio": 1},
				map[string]any{"paneId": "right", "threadId": "thread-right", "ratio": -1},
				map[string]any{"paneId": "wide", "threadId": "thread-wide", "ratio": 1000},
				map[string]any{"paneId": strings.Repeat("x", MaxPaneLayoutIDLength+1), "threadId": "thread-long-pane", "ratio": 1},
				map[string]any{"paneId": "long-thread", "threadId": strings.Repeat("x", MaxPaneLayoutThreadIDLen+1), "ratio": 1},
				map[string]any{"paneId": "bad\"] [data-pane-id=\"x", "threadId": "thread-bad", "ratio": 1},
			},
		},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	want := []PaneLayoutPane{
		{PaneID: "left", ThreadID: "thread-left", Ratio: 0.75},
		{PaneID: "right", ThreadID: "thread-right", Ratio: 1},
		{PaneID: "wide", ThreadID: "thread-wide", Ratio: MaxPaneLayoutRatio},
	}
	if len(updated.PaneLayout.Panes) != len(want) {
		t.Fatalf("PaneLayout.Panes = %+v, want %+v", updated.PaneLayout.Panes, want)
	}
	for i := range want {
		if updated.PaneLayout.Panes[i] != want[i] {
			t.Fatalf("PaneLayout.Panes[%d] = %+v, want %+v", i, updated.PaneLayout.Panes[i], want[i])
		}
	}
	if updated.PaneLayout.FocusedPaneID != "" {
		t.Fatalf("FocusedPaneID = %q, want empty when focused pane is not restored", updated.PaneLayout.FocusedPaneID)
	}

	raw := `{"paneLayout":{"version":2,"focusedPaneId":"left","panes":[{"paneId":"left","threadId":"thread-left","ratio":1}]}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	loaded := NewService(dir).Get()
	if loaded.PaneLayout.Version != CurrentPaneLayoutVersion {
		t.Fatalf("load-path version = %d, want %d", loaded.PaneLayout.Version, CurrentPaneLayoutVersion)
	}
	if len(loaded.PaneLayout.Panes) != 0 {
		t.Fatalf("load-path panes = %+v, want empty for unsupported version", loaded.PaneLayout.Panes)
	}
}
