package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestValidClientID(t *testing.T) {
	valid := []string{
		"11111111-2222-3333-4444-555555555555", // uuid shape (Go + crypto.randomUUID)
		"abcd1234",                             // minimum length
		strings.Repeat("a", 64),                // maximum length
	}
	for _, id := range valid {
		if !validClientID(id) {
			t.Errorf("validClientID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		"short",                  // under 8 chars
		strings.Repeat("a", 65),  // over 64 chars
		"has space in the id",    // space
		"under_score-in-here-ok", // underscore not in charset
		"client:injected-scope",  // colon must never reach scope building
		"../../etc/passwd-xxxx",  // path-ish chars
	}
	for _, id := range invalid {
		if validClientID(id) {
			t.Errorf("validClientID(%q) = true, want false", id)
		}
	}
}

func TestUIStateBindings_RoundTripAndScopeIsolation(t *testing.T) {
	app := newTestAppWithStore(t)
	const clientA = "11111111-2222-3333-4444-555555555555"
	const clientB = "99999999-8888-7777-6666-555555555555"

	if err := app.SetUIState(clientA, map[string]string{"sidebar:width": "312"}); err != nil {
		t.Fatalf("SetUIState: %v", err)
	}
	if err := app.SetUIState(clientB, map[string]string{"sidebar:width": "250"}); err != nil {
		t.Fatalf("SetUIState B: %v", err)
	}

	got, err := app.GetUIState(clientA)
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if got["sidebar:width"] != "312" {
		t.Fatalf("client A bucket = %v, want sidebar:width=312", got)
	}

	if err := app.DeleteUIState(clientA, []string{"sidebar:width"}); err != nil {
		t.Fatalf("DeleteUIState: %v", err)
	}
	got, err = app.GetUIState(clientA)
	if err != nil {
		t.Fatalf("GetUIState after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("client A bucket after delete = %v, want empty", got)
	}
	// B's identical key is untouched — buckets are per-client.
	gotB, err := app.GetUIState(clientB)
	if err != nil {
		t.Fatalf("GetUIState B: %v", err)
	}
	if gotB["sidebar:width"] != "250" {
		t.Fatalf("client B bucket = %v, want sidebar:width=250", gotB)
	}
}

func TestUIStateBindings_RejectsInvalidClientAndOversizeInput(t *testing.T) {
	app := newTestAppWithStore(t)

	if _, err := app.GetUIState("client:escape"); err == nil {
		t.Fatal("GetUIState with invalid client id: want error, got nil")
	}
	if err := app.SetUIState("bad id", map[string]string{"a": "1"}); err == nil {
		t.Fatal("SetUIState with invalid client id: want error, got nil")
	}

	const client = "11111111-2222-3333-4444-555555555555"
	if err := app.SetUIState(client, map[string]string{
		"too-big": strings.Repeat("v", maxUIStateValueLen+1),
	}); err == nil {
		t.Fatal("SetUIState with oversize value: want error, got nil")
	}
	if err := app.SetUIState(client, map[string]string{
		strings.Repeat("k", maxUIStateKeyLen+1): "v",
	}); err == nil {
		t.Fatal("SetUIState with oversize key: want error, got nil")
	}

	oversizeBatch := make(map[string]string, maxUIStateBatch+1)
	for i := 0; i <= maxUIStateBatch; i++ {
		oversizeBatch["key-"+strconv.Itoa(i)] = "v"
	}
	if err := app.SetUIState(client, oversizeBatch); err == nil {
		t.Fatal("SetUIState with oversize batch: want error, got nil")
	}
}

func TestMigrateUIStateFromSettings_MovesLegacyKeysOnce(t *testing.T) {
	app := newTestAppWithStore(t)
	configDir := t.TempDir()

	legacy := `{
		"theme": "dark",
		"paneLayout": {"version":1,"panes":[{"paneId":"p1","threadId":"t1","ratio":1}]},
		"collapsedProjects": ["proj-a","proj-b"]
	}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	migrateUIStateFromSettings(configDir, app.store)

	clientID := ensureClientIDIn(configDir)
	if clientID == "" {
		t.Fatal("ensureClientIDIn returned empty id")
	}
	bucket, err := app.store.GetUIState("client:" + clientID)
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if !strings.Contains(bucket["paneLayout"], `"paneId":"p1"`) {
		t.Fatalf("paneLayout not migrated: %q", bucket["paneLayout"])
	}
	if !strings.Contains(bucket["sidebar:collapsedProjects"], "proj-a") {
		t.Fatalf("collapsedProjects not migrated: %q", bucket["sidebar:collapsedProjects"])
	}
	// theme is a real settings field, not view state — must not move.
	if _, ok := bucket["theme"]; ok {
		t.Fatal("theme leaked into the ui_state bucket")
	}

	// Re-running (settings.json still holds the stale keys until its
	// next sparse save) must not overwrite newer bucket values.
	if err := app.store.SetUIState("client:"+clientID, map[string]string{
		"sidebar:collapsedProjects": `["only-c"]`,
	}); err != nil {
		t.Fatalf("SetUIState: %v", err)
	}
	migrateUIStateFromSettings(configDir, app.store)
	bucket, err = app.store.GetUIState("client:" + clientID)
	if err != nil {
		t.Fatalf("GetUIState after rerun: %v", err)
	}
	if bucket["sidebar:collapsedProjects"] != `["only-c"]` {
		t.Fatalf("rerun clobbered newer bucket value: %q", bucket["sidebar:collapsedProjects"])
	}
}

func TestMigrateUIStateFromSettings_NoFileIsNoOp(t *testing.T) {
	app := newTestAppWithStore(t)
	configDir := t.TempDir()

	migrateUIStateFromSettings(configDir, app.store)

	clientID := ensureClientIDIn(configDir)
	bucket, err := app.store.GetUIState("client:" + clientID)
	if err != nil {
		t.Fatalf("GetUIState: %v", err)
	}
	if len(bucket) != 0 {
		t.Fatalf("bucket = %v, want empty when settings.json is absent", bucket)
	}
}

func TestUIStateBindings_NilStore(t *testing.T) {
	app := &App{}
	const client = "11111111-2222-3333-4444-555555555555"
	if _, err := app.GetUIState(client); err == nil {
		t.Fatal("GetUIState with nil store: want error, got nil")
	}
	if err := app.SetUIState(client, map[string]string{"a": "1"}); err == nil {
		t.Fatal("SetUIState with nil store: want error, got nil")
	}
	if err := app.DeleteUIState(client, []string{"a"}); err == nil {
		t.Fatal("DeleteUIState with nil store: want error, got nil")
	}
}
