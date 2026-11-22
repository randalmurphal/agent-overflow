package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"

	_ "modernc.org/sqlite"
)

func writeSharedPoolAgyBinary(t *testing.T, logPath string) string {
	t.Helper()
	script := `#!/bin/bash
LOG_FILE="` + logPath + `"
n=0
while IFS= read -r line; do
  if [ -n "$LOG_FILE" ]; then
    printf '%s\n' "$line" >> "$LOG_FILE"
  fi
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentInfo":{"name":"antigravity-mock","version":"1.5.13"},"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"list":{},"resume":{},"close":{}}}}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"session/new"'; then
    n=$((n+1))
    sid="agy-pool-$$-$n"
    printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"%s","configOptions":[{"id":"model","name":"Model","category":"model","type":"select","currentValue":"gemini-3.1-pro-high","options":[{"value":"gemini-3.1-pro-high","name":"Gemini 3.1 Pro (High)"},{"value":"gemini-3-flash","name":"Gemini 3 Flash"}]},{"id":"thought_level","name":"Thought Level","category":"thought_level","type":"select","currentValue":"high","options":[{"value":"low","name":"Low"},{"value":"medium","name":"Medium"},{"value":"high","name":"High"},{"value":"xhigh","name":"Extra High"}]}],"modes":{"currentModeId":"auto","availableModes":[{"id":"ask","name":"Ask"},{"id":"auto","name":"Auto"},{"id":"plan","name":"Plan"}]}}}\n' "$id" "$sid"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"session/set_config_option"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"configOptions":[{"id":"model","name":"Model","category":"model","type":"select","currentValue":"gemini-3-flash","options":[{"value":"gemini-3.1-pro-high","name":"Gemini 3.1 Pro (High)"},{"value":"gemini-3-flash","name":"Gemini 3 Flash"}]},{"id":"thought_level","name":"Thought Level","category":"thought_level","type":"select","currentValue":"low","options":[{"value":"low","name":"Low"},{"value":"medium","name":"Medium"},{"value":"high","name":"High"},{"value":"xhigh","name":"Extra High"}]}]}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"session/set_mode"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"session/prompt"'; then
    sid=$(/bin/echo "$line" | /usr/bin/grep -o '"sessionId":"[^"]*"' | /usr/bin/head -1 | /usr/bin/cut -d'"' -f4)
    printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"%s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Reply from %s"}}}}\n' "$sid" "$sid"
    printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"%s","update":{"sessionUpdate":"usage_update","used":42,"size":1048576}}}\n' "$sid"
    printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn","usage":{"inputTokens":42,"outputTokens":10}}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"session/close"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  else
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  fi
done
`
	path := filepath.Join(t.TempDir(), "agy-shared-pool.sh")
	mockexec.Write(t, path, script)
	return path
}

func TestAgyPoolSharesOneACPServerAcrossThreadsAndAppliesLiveConfig(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	logPath := filepath.Join(t.TempDir(), "wire.log")
	binary := writeSharedPoolAgyBinary(t, logPath)
	if _, err := app.settings.Update(map[string]any{"agyBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}

	threadA, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}
	threadB, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}

	if err := app.StartSession(threadA.ID); err != nil {
		t.Fatalf("StartSession(A): %v", err)
	}
	if err := app.StartSession(threadB.ID); err != nil {
		t.Fatalf("StartSession(B): %v", err)
	}

	sessA, okA := app.sessionManager().get(threadA.ID)
	sessB, okB := app.sessionManager().get(threadB.ID)
	if !okA || !okB || sessA.ACP == nil || sessB.ACP == nil {
		t.Fatalf("expected live ACP sessions, got okA=%v okB=%v", okA, okB)
	}
	pidA := sessA.ACP.PID()
	pidB := sessB.ACP.PID()
	if pidA <= 0 || pidA != pidB {
		t.Fatalf("expected shared agy acp server PID, got threadA=%d threadB=%d", pidA, pidB)
	}
	tokenBefore := sessA.Token

	// Live config update (model + effort + mode) must apply without restarting
	// the session or changing the pooled process PID.
	if err := app.store.UpdateThreadModelSettings(threadA.ID, string(provider.Agy), store.ThreadModelSettings{
		Provider:        string(provider.Agy),
		Model:           "gemini-3-flash",
		ReasoningEffort: "low",
		ContextWindow:   provider.AgyStandardContextWindow,
		RuntimeMode:     string(provider.RuntimeApprovalRequired),
	}); err != nil {
		t.Fatalf("UpdateThreadModelSettings: %v", err)
	}
	if err := app.store.UpdateMode(threadA.ID, "plan"); err != nil {
		t.Fatalf("UpdateMode: %v", err)
	}
	if !app.liveApplySessionConfig(threadA.ID, false) {
		t.Fatal("liveApplySessionConfig returned false for ACP model/effort/mode change; want zero-restart live apply")
	}

	sessAAfter, ok := app.sessionManager().get(threadA.ID)
	if !ok {
		t.Fatal("session A missing after live config update")
	}
	if sessAAfter.Token != tokenBefore || sessAAfter.ACP.PID() != pidA {
		t.Fatalf("ACP live update unexpectedly restarted session: token %q->%q pid %d->%d",
			tokenBefore, sessAAfter.Token, pidA, sessAAfter.ACP.PID())
	}

	// UpdateThreadMode must also apply live on an active ACP session (e.g.
	// toggling Plan -> Chat from the composer or Implement Plan).
	if _, err := app.UpdateThreadMode(threadA.ID, "chat"); err != nil {
		t.Fatalf("UpdateThreadMode(chat): %v", err)
	}
	sessAfterMode, ok := app.sessionManager().get(threadA.ID)
	if !ok || sessAfterMode.Token != tokenBefore || sessAfterMode.ACP.PID() != pidA {
		t.Fatalf("UpdateThreadMode unexpectedly restarted ACP session")
	}

	wireBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read wire log: %v", err)
	}
	wire := string(wireBytes)
	if !strings.Contains(wire, `"method":"session/set_config_option"`) || !strings.Contains(wire, `"gemini-3-flash"`) {
		t.Errorf("wire log missing live model config option update:\n%s", wire)
	}
	if !strings.Contains(wire, `"method":"session/set_mode"`) || !strings.Contains(wire, `"plan"`) {
		t.Errorf("wire log missing live mode update:\n%s", wire)
	}
	if !strings.Contains(wire, `"modeId":"ask"`) {
		t.Errorf("wire log missing UpdateThreadMode live switch back to ask mode:\n%s", wire)
	}

	// Send a turn on thread A and verify the response lands on the timeline.
	if err := app.SendMessage(context.Background(), threadA.ID, "hello agy", nil); err != nil {
		t.Fatalf("SendMessage(A): %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var foundAssistant bool
	for time.Now().Before(deadline) {
		items, listErr := app.store.ListItems(threadA.ID)
		if listErr == nil {
			for _, item := range items {
				if item.Kind == "assistant_text" && strings.Contains(item.Summary, "Reply from ") {
					foundAssistant = true
					break
				}
			}
		}
		if foundAssistant {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !foundAssistant {
		t.Fatal("timed out waiting for Antigravity assistant response on timeline")
	}

	// Drain and stop cleanly without deadlock.
	app.drainAgyConnections()
	if err := app.StopSession(threadA.ID); err != nil {
		t.Fatalf("StopSession(A): %v", err)
	}
	if err := app.StopSession(threadB.ID); err != nil {
		t.Fatalf("StopSession(B): %v", err)
	}
}

func TestAgyNativeSQLiteForkClonesConversation(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}
	srcRef := "11111111-2222-3333-4444-555555555555"
	if _, err := app.store.UpdateSessionRef(thread.ID, srcRef); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(app.agyAppDataDir(), "conversations", srcRef+".db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE trajectory_meta (trajectory_id TEXT, cascade_id TEXT);
		INSERT INTO trajectory_meta VALUES ('old-traj', '` + srcRef + `');
		CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, error_details BLOB, step_payload BLOB);
		INSERT INTO steps (idx, step_type, step_payload) VALUES (0, 14, X'01'), (1, 15, X'02'), (2, 14, X'03'), (3, 15, X'04');
	`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}

	source, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	forkState, err := app.resolveForkResumeState(context.Background(), source, nil, nil, "")
	if err != nil {
		t.Fatalf("resolveForkResumeState(agy): %v", err)
	}
	if forkState.SessionRef == "" || forkState.SessionRef == srcRef {
		t.Fatalf("expected distinct forked session ref, got %q", forkState.SessionRef)
	}
	clonedPath := filepath.Join(app.agyAppDataDir(), "conversations", forkState.SessionRef+".db")
	if _, err := os.Stat(clonedPath); err != nil {
		t.Fatalf("cloned agy conversation DB missing at %s: %v", clonedPath, err)
	}
	if forkState.Cleanup != nil {
		if err := forkState.Cleanup(); err != nil {
			t.Fatalf("fork cleanup failed: %v", err)
		}
	}
}
