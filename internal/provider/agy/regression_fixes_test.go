package agy

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider/acp"
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

func TestExtractReadableErrorTextSkipsProtobufMetadataAndMatchesNotFound(t *testing.T) {
	t.Parallel()
	raw := []byte("\x0a\x1etype.googleapis.com/google.rpc.ErrorInfo\x00google.rpc.ErrorInfo\x00HTTP 404 NOT_FOUND: model endpoint missing")
	got := extractReadableErrorText(raw)
	if got != "HTTP 404 NOT_FOUND: model endpoint missing" {
		t.Fatalf("extractReadableErrorText = %q, want human error sentence", got)
	}
	override := classifyErrorStepPayload(raw)
	if override == nil || override.Failure == nil || override.Failure.Code != "not_found" {
		t.Fatalf("classifyErrorStepPayload = %+v, want failure code not_found", override)
	}
}

func TestNormalizeToolCompleteCompletedDoesNotMarkBackgroundZombie(t *testing.T) {
	t.Parallel()
	fl := NewFlavor("")
	started := acp.NormalizedToolStart{ToolName: "Bash", ItemType: "command_execution"}
	norm := fl.NormalizeToolComplete(&acp.ToolCallUpdate{
		ToolCallID: "cmd-1",
		Name:       "run_command",
		Status:     "completed",
		RawOutput:  json.RawMessage(`"Tool is running as a background task with task id: s/task-1\nTask logs are available at: file:///tmp/t.log"`),
	}, started)
	if norm.IsBackground {
		t.Fatal("completed run_command must not set IsBackground=true (would create a permanent tray zombie)")
	}
}

func TestExtractPendingQuestionAcceptsNormalizedAskUserQuestion(t *testing.T) {
	t.Parallel()
	fl := NewFlavor("")
	questions, ok := fl.ExtractPendingQuestion(&acp.ToolCallUpdate{
		ToolCallID: "q-1",
		Name:       "AskUserQuestion",
		RawInput: json.RawMessage(`{
			"questions": [{
				"id": "q1",
				"header": "Choice",
				"question": "Pick one",
				"options": [{"label": "Alpha"}, {"label": "Beta"}],
				"multiSelect": true
			}]
		}`),
	})
	if !ok || len(questions) != 1 || len(questions[0].Options) != 2 || !questions[0].MultiSelect {
		t.Fatalf("ExtractPendingQuestion normalized = ok:%v questions:%+v", ok, questions)
	}
}

func TestClassifyPermissionAutoAcceptEditsPromptsOnSecurityWarning(t *testing.T) {
	t.Parallel()
	fl := NewFlavor("")
	cls := fl.ClassifyPermission("t1", "turn-1", "req-1", acp.RequestPermissionParams{
		ToolCall: acp.ToolCallUpdate{
			ToolCallID: "edit-outside",
			Name:       "write_file",
			Kind:       "edit",
		},
		Options: []acp.PermissionOption{
			{
				OptionID: "allow_once",
				Name:     "Allow once",
				Kind:     "allow_once",
				Meta:     json.RawMessage(`{"agy.security.warning":{"title":"Outside workspace","message":"Path escapes workspace"}}`),
			},
			{OptionID: "reject_once", Name: "Deny", Kind: "reject_once"},
		},
	}, acp.PolicyAutoAcceptEdits)
	if cls.Action != acp.PermissionAskUser {
		t.Fatalf("Action = %v, want PermissionAskUser when agy.security.warning is present", cls.Action)
	}
}
