package agy

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func createTestConvDB(t *testing.T, appDataDir, sessionID string) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(appDataDir, "conversations", sessionID+".db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, error_details BLOB, step_payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTurnOneErrorStepDetectedWhenDBMissingBeforePrompt(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	sessionID := "brand-new-session-1"
	flavor := NewFlavor(appDataDir)

	// Before Turn 1, conversations/<id>.db does not exist yet.
	before := flavor.BeforePrompt(context.Background(), sessionID)
	if before == nil {
		t.Fatal("BeforePrompt returned nil for brand-new session; Turn 1 step_type=17 errors would be swallowed")
	}

	// Simulate agy creating the DB and recording a step_type=17 quota error on Turn 1.
	db := createTestConvDB(t, appDataDir, sessionID)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, error_details, step_payload) VALUES (0, 14, X'', X''), (1, 17, ?, X'')`,
		[]byte("metadata-header-1 metadata-header-2 QUOTA_EXHAUSTED for gemini-4-neon")); err != nil {
		t.Fatal(err)
	}

	override := flavor.AfterPrompt(context.Background(), sessionID, "end_turn", before)
	if override == nil || override.Failure == nil || override.Failure.Code != "rate_limit" {
		t.Fatalf("AfterPrompt = %+v, want rate_limit override on Turn 1", override)
	}
}

func TestTurnErrorStepIgnoredAfterSuccessfulRetryPlannerStep(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	sessionID := "retried-session-1"
	flavor := NewFlavor(appDataDir)

	before := flavor.BeforePrompt(context.Background(), sessionID)
	db := createTestConvDB(t, appDataDir, sessionID)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, error_details, step_payload) VALUES (0, 14, X'', X''), (1, 17, ?, X''), (2, 15, X'', X'')`,
		[]byte("503 UNAVAILABLE transient retry")); err != nil {
		t.Fatal(err)
	}

	if override := flavor.AfterPrompt(context.Background(), sessionID, "end_turn", before); override != nil {
		t.Fatalf("retried error followed by PLANNER_RESPONSE should not fail turn; got %+v", override)
	}
}

func TestDetectCompactionCheckpointStepType23(t *testing.T) {
	t.Parallel()
	appDataDir := t.TempDir()
	sessionID := "compact-session-1"
	flavor := NewFlavor(appDataDir)

	db := createTestConvDB(t, appDataDir, sessionID)
	defer func() { _ = db.Close() }()
	// Historical checkpoint present before resume.
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, error_details, step_payload) VALUES (10, 23, X'', X'')`); err != nil {
		t.Fatal(err)
	}

	cursor := flavor.InitCompactionCursor(context.Background(), sessionID)
	if cursor != 10 {
		t.Fatalf("InitCompactionCursor = %d, want 10", cursor)
	}
	if _, ev := flavor.DetectCompaction(context.Background(), sessionID, cursor); ev != nil {
		t.Fatalf("historical checkpoint must not re-emit, got %+v", ev)
	}

	// New compaction occurs at idx 42.
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, error_details, step_payload) VALUES (42, 23, X'', X'')`); err != nil {
		t.Fatal(err)
	}
	nextIdx, ev := flavor.DetectCompaction(context.Background(), sessionID, cursor)
	if nextIdx != 42 || ev == nil || ev.ItemID != "agy-compact-42" {
		t.Fatalf("DetectCompaction = (%d, %+v), want (42, agy-compact-42)", nextIdx, ev)
	}
}

func TestQuotaHTTP403FallsBackWithoutUnauthorized(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:retrieveUserQuotaSummary" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v1internal:fetchAvailableModels" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":{"gemini-4-neon":{"modelProvider":"MODEL_PROVIDER_GOOGLE","quotaInfo":{"remainingFraction":0.75}}}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cred := []byte(`{"access_token":"tok-123"}`)
	snap, err := probeRateLimitsAtEndpoints(context.Background(), srv.Client(), []string{srv.URL}, cred, time.Now())
	if err != nil {
		t.Fatalf("probeRateLimitsAtEndpoints returned error on 403 fallback: %v", err)
	}
	if len(snap.Limits) == 0 {
		t.Fatal("expected fallback limits from fetchAvailableModels after 403")
	}
}

func TestCollectBrainTransferFilesSkipsSystemGenerated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sid := "sess-transfer-1"
	planPath := filepath.Join(root, "brain", sid, "plan.md")
	sysPath := filepath.Join(root, "brain", sid, ".system_generated", "logs", "transcript.jsonl")
	for _, p := range []string{planPath, sysPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := collectBrainTransferFiles(context.Background(), root, sid)
	if err != nil {
		t.Fatalf("collectBrainTransferFiles: %v", err)
	}
	if len(files) != 1 || filepath.Base(files[0].Path) != "plan.md" {
		t.Fatalf("expected only plan.md in transfer files, got %+v", files)
	}
}
