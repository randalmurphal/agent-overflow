package app

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/attachment"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/agy"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil/mockexec"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/usermessage"

	_ "modernc.org/sqlite"

	"github.com/google/uuid"
)

func writeSharedPoolAgyBinary(t *testing.T, logPath string) string {
	t.Helper()
	return writePoolAgyBinary(t, logPath, "")
}

// writePoolAgyBinary is writeSharedPoolAgyBinary that, given a launchRecord
// path, appends there one line per launch with its argv and the variables
// through which agy chooses its login store and home ("<unset>" for an unset
// one), and answers `agy mcp list` and `agy -p` too.
func writePoolAgyBinary(t *testing.T, logPath, launchRecord string) string {
	t.Helper()
	record := ""
	if launchRecord != "" {
		// One line per launch: its argv and the variables agy chooses its
		// login store and home from. `mcp list` and `-p` answer at once.
		record = `printf 'args=%s|store=%s|gemini_home=%s|app_data_dir=%s\n' "$*" "${AGY_ACP_FORCE_FILE_STORAGE-<unset>}" "${GEMINI_HOME-<unset>}" "${AGY_APP_DATA_DIR-<unset>}" >> '` + launchRecord + `'
case "$1" in
mcp) echo 'No MCP servers configured.'; exit 0 ;;
-p|--print*) echo '{"result":"{}"}'; exit 0 ;;
esac
`
	}
	script := `#!/bin/bash
` + record + `LOG_FILE="` + logPath + `"
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
	attachFileAccountStoresForTest(t, app)
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

	if err := app.startSessionTakingLock(context.Background(), threadA.ID); err != nil {
		t.Fatalf("StartSession(A): %v", err)
	}
	if err := app.startSessionTakingLock(context.Background(), threadB.ID); err != nil {
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
	if _, err := app.SendMessageWithOptions(context.Background(), threadA.ID, "hello agy", pageSendOptions(uuid.NewString())); err != nil {
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

	// borrowAgyConnForCurrentAccount must hit the warm pooled connection matching
	// the live session's full pool key instead of silently falling back to a cold spawn.
	home, err := app.ensureProviderAccountManager().AgyCanonicalHome(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	borrowed, releaseBorrowed := app.borrowAgyConnForCurrentAccount(binary, home)
	if borrowed == nil {
		t.Fatal("borrowAgyConnForCurrentAccount missed warm pooled agy connection")
	}
	if borrowed.PID() != pidA {
		releaseBorrowed()
		t.Fatalf("borrowed agy conn PID = %d, want pooled PID %d", borrowed.PID(), pidA)
	}
	releaseBorrowed()

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
	forkState, err := app.resolveForkResumeState(context.Background(), source, "", nil, nil, nil, "")
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

func TestAgyGeneratedImageImportContainment(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	attStore, err := attachment.NewStore(attachment.Config{RootDir: filepath.Join(t.TempDir(), "attachments")}, app.store)
	if err != nil {
		t.Fatalf("attachment.NewStore: %v", err)
	}
	app.attachments = attStore
	ws := t.TempDir()
	thread, err := createTestThread(t, app, string(provider.Agy), ws, "gemini-4-neon", "chat")
	if err != nil {
		t.Fatal(err)
	}
	// Minimal valid 1x1 PNG header+IHDR+IDAT+IEND bytes.
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}

	// 1. Image inside <appDataDir>/brain/<session>/ is allowed.
	brainImg := filepath.Join(app.agyAppDataDir(), "brain", "sess-1", "nebula.png")
	if err := os.MkdirAll(filepath.Dir(brainImg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brainImg, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	imported, err := app.importCodexGeneratedImage(thread.ID, brainImg)
	if err != nil {
		t.Fatalf("import brain image failed: %v", err)
	}
	if imported.AttachmentID == "" || imported.MimeType != "image/png" {
		t.Fatalf("unexpected imported brain image: %+v", imported)
	}

	// 2. Image inside thread workspace is allowed.
	wsImg := filepath.Join(ws, "out.png")
	if err := os.WriteFile(wsImg, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	importedWS, err := app.importCodexGeneratedImage(thread.ID, wsImg)
	if err != nil {
		t.Fatalf("import workspace image failed: %v", err)
	}
	if importedWS.AttachmentID == "" {
		t.Fatalf("unexpected imported workspace image: %+v", importedWS)
	}

	// 3. Image outside both brain and workspace is rejected.
	outsideImg := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outsideImg, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := app.importCodexGeneratedImage(thread.ID, outsideImg); err == nil {
		t.Fatal("expected outside image path to be rejected")
	}
}

func TestAgyModelPickerReconnectTurnUniquenessAndAskQuestionForkOrdinal(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	attachFileAccountStoresForTest(t, app)
	app.configureTriageQueueCallbacks()
	binary := writeSharedPoolAgyBinary(t, filepath.Join(t.TempDir(), "wire.log"))
	if _, err := app.settings.Update(map[string]any{"agyBinaryPath": binary}); err != nil {
		t.Fatal(err)
	}

	// 1. Start from a Claude thread and switch to Antigravity through the real
	// model picker RPC (UpdateThreadModelSelection -> threadapp.ValidProvider).
	thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-sonnet-4-6", "chat")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := app.UpdateThreadModelSelection(thread.ID, string(provider.Agy), "gemini-3.1-pro-high")
	if err != nil {
		t.Fatalf("UpdateThreadModelSelection(agy): %v", err)
	}
	if updated.Provider != string(provider.Agy) {
		t.Fatalf("updated provider = %q, want agy", updated.Provider)
	}

	// 2. Run Turn 1, stop/disconnect session, reconnect, and run Turn 2.
	// Both turns MUST land as distinct turn rows in SQLite (no acp-turn-1 collision).
	if err := app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
		t.Fatalf("StartSession(1): %v", err)
	}
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "turn one", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage(1): %v", err)
	}
	waitAssistantCount := func(want int) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			items, listErr := app.store.ListItems(thread.ID)
			_, activeTurn, turnErr := app.store.GetActiveTurn(thread.ID)
			if listErr == nil && turnErr == nil && !activeTurn {
				count := 0
				for _, it := range items {
					if it.Kind == "assistant_text" && it.Status == "completed" {
						count++
					}
				}
				if count >= want {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %d completed assistant items with idle turn", want)
	}
	waitAssistantCount(1)

	if err := app.StopSession(thread.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if err := app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
		t.Fatalf("StartSession(2): %v", err)
	}
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "turn two", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessage(2): %v", err)
	}
	waitAssistantCount(2)

	// Verify queued flush delivery confirms receipt so UnconfirmedFlushSends drains to 0.
	queueHeldMessage(t, app, thread.ID, "queued follow-up")
	app.triage.FlushQueuedItems(thread.ID)
	waitAssistantCount(3)
	if unconfirmed := app.triage.UnconfirmedFlushSends(thread.ID); len(unconfirmed) != 0 {
		t.Fatalf("expected 0 unconfirmed flush sends on agy after delivery, got %d: %+v", len(unconfirmed), unconfirmed)
	}

	turns, err := app.store.ListRecentTurns(thread.ID, 10)
	if err != nil {
		t.Fatalf("ListRecentTurns: %v", err)
	}
	if len(turns) < 3 {
		t.Fatalf("turns after reconnect + queued flush = %d (%+v), want >= 3 distinct turns", len(turns), turns)
	}
	if turns[0].TurnID == turns[1].TurnID || turns[1].TurnID == turns[2].TurnID {
		t.Fatalf("reconnected turn overwrote prior turn ID: %q / %q / %q", turns[0].TurnID, turns[1].TurnID, turns[2].TurnID)
	}

	// Verify direct sends also confirm their pending-send marker so an idle
	// agy session can revert and resend without hitting "awaiting provider confirmation".
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("expected 0 pending sends on idle agy thread after direct sends completed")
	}
	itemsBeforeRevert, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems before revert: %v", err)
	}
	var userIDs []string
	for _, it := range itemsBeforeRevert {
		if it.Kind == "user_text" && it.Role == "user" && !store.IsWireOnlyUserItem(it) {
			userIDs = append(userIDs, it.ID)
		}
	}
	if len(userIDs) < 2 {
		t.Fatalf("expected at least 2 user_text items to revert, got %d", len(userIDs))
	}

	// Seed a native agy conversation SQLite DB for the current SessionRef so
	// reverting the 2nd user message exercises rollbackAgyThreadToMessage's
	// ForkConversationBeforeTarget slicing path end-to-end.
	curThread, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread before revert: %v", err)
	}
	if curThread.SessionRef == "" {
		t.Fatal("expected non-empty SessionRef on active agy thread")
	}
	origRef := curThread.SessionRef
	convDBPath := filepath.Join(app.agyAppDataDir(), "conversations", origRef+".db")
	if err := os.MkdirAll(filepath.Dir(convDBPath), 0o700); err != nil {
		t.Fatal(err)
	}
	convDB, err := sql.Open("sqlite", convDBPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = convDB.Exec(`
		CREATE TABLE trajectory_meta (trajectory_id TEXT, cascade_id TEXT);
		INSERT INTO trajectory_meta VALUES ('traj-orig', '` + origRef + `');
		CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, error_details BLOB, step_payload BLOB);
	`)
	if err == nil {
		for _, row := range []struct {
			idx      int
			stepType int
			payload  []byte
		}{
			{0, 14, encodeAgyUserStepPayload("turn one", "h0", 1000)},
			{1, 15, []byte("reply one")},
			{2, 14, encodeAgyUserStepPayload("turn two", "h2", 2000)},
			{3, 15, []byte("reply two")},
			{4, 14, encodeAgyUserStepPayload("queued follow-up", "h4", 3000)},
			{5, 15, []byte("reply three")},
		} {
			if _, err = convDB.Exec(`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, ?, ?)`, row.idx, row.stepType, row.payload); err != nil {
				break
			}
		}
	}
	_ = convDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Revert the 2nd user message (userOrdinal == 1): keeps turn 1 prefix in a
	// cloned conversation DB and resends on the new SessionRef.
	if _, err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, userIDs[1], RevertAndResendOptions{
		Content: "edited turn two",
	}); err != nil {
		t.Fatalf("RevertConversationAndResendMessage(2nd user message) on idle agy session failed: %v", err)
	}
	waitAssistantCount(2)
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("expected 0 pending sends after agy mid-thread revert-and-resend completed")
	}
	afterMidRevert, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread after mid-thread revert: %v", err)
	}
	if afterMidRevert.SessionRef == "" || afterMidRevert.SessionRef == origRef {
		t.Fatalf("expected rolled-back agy thread to repoint SessionRef, got %q (orig %q)", afterMidRevert.SessionRef, origRef)
	}
	slicedDBPath := filepath.Join(app.agyAppDataDir(), "conversations", afterMidRevert.SessionRef+".db")
	slicedDB, err := sql.Open("sqlite", slicedDBPath)
	if err != nil {
		t.Fatalf("open sliced agy conversation DB %s: %v", slicedDBPath, err)
	}
	var stepCount, maxIdx int
	scanErr := slicedDB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(idx), -1) FROM steps`).Scan(&stepCount, &maxIdx)
	_ = slicedDB.Close()
	if scanErr != nil {
		t.Fatalf("query sliced agy conversation DB: %v", scanErr)
	}
	if stepCount != 2 || maxIdx != 1 {
		t.Fatalf("sliced conversation DB has count=%d maxIdx=%d, want count=2 maxIdx=1", stepCount, maxIdx)
	}

	// Revert the 1st user message (userOrdinal == 0): clears SessionRef and
	// starts a fresh agy session.
	if _, err := app.RevertConversationAndResendMessage(context.Background(), thread.ID, userIDs[0], RevertAndResendOptions{
		Content: "edited turn one",
	}); err != nil {
		t.Fatalf("RevertConversationAndResendMessage(1st user message) on idle agy session failed: %v", err)
	}
	waitAssistantCount(1)
	if app.triage.HasPendingSendForThread(thread.ID) {
		t.Fatal("expected 0 pending sends after agy first-message revert-and-resend completed")
	}
	stopSessionChecked(t, app, thread.ID)

	// 3. isAgyNativePromptItem counts both visible user_text and AskUserQuestion with prompt_dispatched:true.
	if !isAgyNativePromptItem(store.Item{Kind: "tool_call", ToolName: "AskUserQuestion", Meta: `{"prompt_dispatched":true}`}) {
		t.Fatal("expected AskUserQuestion with prompt_dispatched:true to count as native prompt")
	}
	if isAgyNativePromptItem(store.Item{Kind: "tool_call", ToolName: "AskUserQuestion", Meta: `{}`}) {
		t.Fatal("AskUserQuestion without prompt_dispatched must not count as native prompt")
	}
}

func TestAgyBackgroundedCommandEntersTrayAndTailsLog(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	attachFileAccountStoresForTest(t, app)

	// Prepare a real task log inside <agyAppDataDir>/brain/<sess>/.system_generated/tasks/
	// and a gate file the test touches once it has verified the live tray + tail.
	sessID := "agy-bg-sess-1"
	logPath := filepath.Join(app.agyAppDataDir(), "brain", sessID, ".system_generated", "tasks", "task-42.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("go test running...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseFile := filepath.Join(t.TempDir(), "release")

	script := `#!/usr/bin/env python3
import json, os, sys, time
log_path = ` + `"` + filepath.ToSlash(logPath) + `"` + `
release_file = ` + `"` + filepath.ToSlash(releaseFile) + `"` + `
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    msg = json.loads(raw)
    mid = msg.get("id")
    method = msg.get("method")
    if method == "initialize":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"protocolVersion":1,"agentInfo":{"name":"antigravity-mock","version":"1.5.13"}}}), flush=True)
    elif method == "session/new":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"sessionId":"` + sessID + `","modes":{"currentModeId":"auto","availableModes":[{"id":"auto","name":"Auto"}]}}}), flush=True)
    elif method == "session/prompt":
        print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessID + `","update":{"sessionUpdate":"tool_call","toolCallId":"call-bg-42","name":"run_command","kind":"execute","status":"in_progress","rawInput":{"CommandLine":"make go-test","WaitMsBeforeAsync":10000}}}}), flush=True)
        bg_out = "Tool is running as a background task with task id: ` + sessID + `/task-42\nTask logs are available at: file://" + log_path + "\n"
        print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessID + `","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-bg-42","name":"run_command","kind":"execute","status":"in_progress","rawOutput":{"output":bg_out}}}}), flush=True)
        deadline = time.time() + 10.0
        while time.time() < deadline and not os.path.exists(release_file):
            time.sleep(0.02)
        print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessID + `","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-bg-42","name":"run_command","kind":"execute","status":"completed","rawOutput":{"output":"PASS\n","exit_code":0}}}}), flush=True)
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":5}}}), flush=True)
    elif method == "session/close":
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{}}), flush=True)
    elif mid is not None:
        print(json.dumps({"jsonrpc":"2.0","id":mid,"result":{}}), flush=True)
`
	bin := filepath.Join(t.TempDir(), "agy-bg-mock.py")
	mockexec.Write(t, bin, script)
	if _, err := app.settings.Update(map[string]any{"agyBinaryPath": bin}); err != nil {
		t.Fatal(err)
	}

	thread, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-4-neon", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "run background test", pageSendOptions(uuid.NewString())); err != nil {
		t.Fatalf("SendMessageWithOptions: %v", err)
	}

	// 1. Wait until the backgrounded run_command enters ListLiveBackgroundTasks.
	var liveTask store.Item
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, listErr := app.store.ListLiveBackgroundTasks(thread.ID, 0)
		if listErr == nil && len(rows) > 0 {
			liveTask = rows[0]
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if liveTask.ID == "" {
		t.Fatal("timed out waiting for backgrounded agy run_command to enter ListLiveBackgroundTasks")
	}
	if !liveTask.IsBackground || liveTask.Status != "running" {
		t.Fatalf("unexpected live background task row: %+v", liveTask)
	}

	// 2. WatchBackgroundTaskOutput must tail the agy task log file.
	watchCtx, _ := transport.WithConnState(context.Background(), transport.ConnPrincipal{})
	snap, err := app.WatchBackgroundTaskOutput(watchCtx, thread.ID, liveTask.ID)
	if err != nil {
		t.Fatalf("WatchBackgroundTaskOutput(agy): %v", err)
	}
	if !strings.Contains(snap.Data, "go test running...") {
		t.Fatalf("WatchBackgroundTaskOutput data = %q, want 'go test running...'", snap.Data)
	}
	if err := app.UnwatchBackgroundTaskOutput(watchCtx, thread.ID, snap.WatchID); err != nil {
		t.Fatalf("UnwatchBackgroundTaskOutput: %v", err)
	}

	// Containment: an agy path outside <appDataDir>/brain/<sess>/.system_generated/tasks/ must be rejected.
	outsideLog := filepath.Join(t.TempDir(), "secret.log")
	if err := os.WriteFile(outsideLog, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.resolveAgyTaskOutputPath(outsideLog); err == nil {
		t.Fatal("expected resolveAgyTaskOutputPath to reject path outside agy brain tasks dir")
	}

	// 3. Release the command and verify it settles via a Claude-style
	// tool_completion sibling (complete:<launchID>) while keeping the launch
	// row immutable and dropping the live running count to 0.
	if err := os.WriteFile(releaseFile, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	completionID := "complete:" + liveTask.ID
	var comp store.Item
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		item, found, getErr := app.store.GetThreadItem(thread.ID, completionID)
		if getErr == nil && found {
			comp = item
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if comp.ID != completionID || comp.Kind != "tool_completion" || comp.CompletionOf != liveTask.ID || comp.Status != "completed" {
		t.Fatalf("expected completed tool_completion sibling %q, got %+v", completionID, comp)
	}
	runningCount, err := app.store.CountLiveRunningBackgroundToolCalls(thread.ID)
	if err != nil {
		t.Fatalf("CountLiveRunningBackgroundToolCalls: %v", err)
	}
	if runningCount != 0 {
		t.Fatalf("CountLiveRunningBackgroundToolCalls = %d, want 0 after completion sibling lands", runningCount)
	}
	t.Cleanup(func() { stopSessionChecked(t, app, thread.ID) })
}

func TestAgyMidTurnFlushPlacementDefersUntilEcho(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertTurn(store.Turn{
		TurnID:    "t-active",
		ThreadID:  thread.ID,
		TurnIndex: 2,
		StartedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	placement, err := app.resolveUserMessagePlacement(thread, messageFlush)
	if err != nil {
		t.Fatalf("resolveUserMessagePlacement: %v", err)
	}
	if placement.persistence != messageDeferUntilEcho || placement.displayTurn != 2 || placement.responseTurn != 2 || !placement.joinsActiveTurn {
		t.Fatalf("unexpected agy mid-turn flush placement: %+v", placement)
	}
}

func TestStopBackgroundTasksAgy(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := app.store.InsertItem(store.Item{
		ID:           "bg-run-1",
		ThreadID:     thread.ID,
		TurnIndex:    1,
		Kind:         "tool_call",
		Role:         "assistant",
		ToolName:     "Bash",
		Status:       "running",
		IsBackground: true,
		Summary:      "sleep 30",
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	results, err := app.StopBackgroundTasks(thread.ID, []string{"bg-run-1", "already-done"})
	if err != nil {
		t.Fatalf("StopBackgroundTasks(agy): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if results[0].Outcome != BackgroundStopFailed || results[0].Error == "" {
		t.Fatalf("results[0] = %+v, want BackgroundStopFailed with explanation", results[0])
	}
	if results[1].Outcome != BackgroundStopEnded {
		t.Fatalf("results[1] = %+v, want BackgroundStopEnded", results[1])
	}
}

func TestResolveAgyTaskOutputPathHandlesSymlinkedNonExistentLog(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	appDataDir := app.agyAppDataDir()
	realBrain := filepath.Join(t.TempDir(), "real-brain")
	tasksDir := filepath.Join(realBrain, "sess-1", ".system_generated", "tasks")
	if err := os.MkdirAll(tasksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appDataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	brainLink := filepath.Join(appDataDir, "brain")
	if err := os.Symlink(realBrain, brainLink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	missingLog := filepath.Join(brainLink, "sess-1", ".system_generated", "tasks", "task-1.log")
	_, err := app.resolveAgyTaskOutputPath(missingLog)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolveAgyTaskOutputPath(missing under symlink) = %v, want os.ErrNotExist", err)
	}
}

func TestForkAgyThreadBeforeMessageErrorsWithoutSessionRefWhenHistoryExists(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-3.1-pro-high", "chat")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i, id := range []string{"u1", "u2"} {
		if err := app.store.InsertItem(store.Item{
			ID:        id,
			ThreadID:  thread.ID,
			TurnIndex: i + 1,
			Kind:      "user_text",
			Role:      "user",
			Status:    "completed",
			Summary:   id,
			CreatedAt: now + int64(i),
			UpdatedAt: now + int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	anchorItem, _, err := app.store.GetThreadItem(thread.ID, "u2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.forkAgyThreadBeforeMessage(thread, store.MessageAnchor{}, anchorItem); err == nil {
		t.Fatal("expected error when forking before Turn 2 with empty SessionRef")
	}
}

func encodeAgyUserStepPayload(text, stepHash string, createdAtMs int64) []byte {
	appendVarint := func(dst []byte, fieldNum, val uint64) []byte {
		dst = binary.AppendUvarint(dst, fieldNum<<3)
		return binary.AppendUvarint(dst, val)
	}
	appendBytes := func(dst []byte, fieldNum uint64, data []byte) []byte {
		dst = binary.AppendUvarint(dst, (fieldNum<<3)|2)
		dst = binary.AppendUvarint(dst, uint64(len(data)))
		return append(dst, data...)
	}
	var pl []byte
	pl = appendVarint(pl, 1, 14)
	pl = appendVarint(pl, 4, 3)
	var meta []byte
	if createdAtMs > 0 {
		var ts []byte
		ts = appendVarint(ts, 1, uint64(createdAtMs/1000))
		ts = appendVarint(ts, 2, uint64((createdAtMs%1000)*1_000_000))
		meta = appendBytes(meta, 1, ts)
	}
	if stepHash != "" {
		meta = appendBytes(meta, 35, []byte(stepHash))
	}
	if len(meta) > 0 {
		pl = appendBytes(pl, 5, meta)
	}
	var ui []byte
	if text != "" {
		ui = appendBytes(ui, 2, []byte(text))
		ui = appendBytes(ui, 3, appendBytes(nil, 1, []byte(text)))
	}
	if len(ui) > 0 {
		pl = appendBytes(pl, 19, ui)
	}
	return pl
}

func TestAgyRevertAndForkImmuneToOrdinalDrift(t *testing.T) {
	t.Parallel()
	app, _ := setupE2EApp(t)
	thread, err := createTestThread(t, app, string(provider.Agy), t.TempDir(), "gemini-4-neon", "chat")
	if err != nil {
		t.Fatal(err)
	}
	srcRef := "agy-drift-src-1"
	if err := app.writeSessionRef(thread.ID, srcRef); err != nil {
		t.Fatal(err)
	}
	thread, err = app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Seed an agy conversation DB with 7 extra historical user steps NOT present
	// in AO's timeline (the exact failure mode from dogfooding), followed by 3
	// AO-visible turns (including a duplicate "Ok restarted").
	baseMs := time.Now().UnixMilli() - 60_000
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
		INSERT INTO trajectory_meta VALUES ('orig-traj', '` + srcRef + `');
		CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, error_details BLOB, step_payload BLOB, metadata BLOB);
	`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	stepIdx := 0
	for i := 0; i < 7; i++ {
		if _, err := db.Exec(`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, 14, ?), (?, 15, X'02')`,
			stepIdx, encodeAgyUserStepPayload("historical pre-AO prompt", "hist", baseMs+int64(i*500)),
			stepIdx+1,
		); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		stepIdx += 2
	}
	// Step 14: "Ok restarted" (turn 0), Step 16: "Ok now go" (turn 1), Step 18: "fork target" (turn 2).
	u0Idx, u1Idx, u2Idx := stepIdx, stepIdx+2, stepIdx+4
	for _, entry := range []struct {
		idx  int
		text string
		hash string
		ts   int64
	}{
		{u0Idx, "Ok restarted", "hash-u0", baseMs + 10_000},
		{u1Idx, "Ok now go", "hash-u1", baseMs + 20_000},
		{u2Idx, "fork target", "hash-u2", baseMs + 30_000},
	} {
		if _, err := db.Exec(`INSERT INTO steps (idx, step_type, step_payload) VALUES (?, 14, ?), (?, 15, X'02')`,
			entry.idx, encodeAgyUserStepPayload(entry.text, entry.hash, entry.ts),
			entry.idx+1,
		); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	_ = db.Close()

	// Insert AO items: u0 (no anchor), u1 (stored native anchor), u2 (no anchor).
	u1Meta, err := usermessage.MergeProviderIDs("", "ao-uuid-1", agy.FormatStepAnchor(u1Idx, "hash-u1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []store.Item{
		{ID: "u0", ThreadID: thread.ID, TurnIndex: 0, Kind: "user_text", Role: "user", Status: "completed", Summary: "Ok restarted", CreatedAt: baseMs + 10_050, UpdatedAt: baseMs + 10_050},
		{ID: "u1", ThreadID: thread.ID, TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed", Summary: "Ok now go", Meta: u1Meta, CreatedAt: baseMs + 20_050, UpdatedAt: baseMs + 20_050},
		{ID: "u2", ThreadID: thread.ID, TurnIndex: 2, Kind: "user_text", Role: "user", Status: "completed", Summary: "fork target", CreatedAt: baseMs + 30_050, UpdatedAt: baseMs + 30_050},
	} {
		if err := app.store.InsertItem(it); err != nil {
			t.Fatal(err)
		}
	}

	readMaxIdx := func(sessionRef string) int {
		t.Helper()
		p := filepath.Join(app.agyAppDataDir(), "conversations", sessionRef+".db")
		d, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		defer func() { _ = d.Close() }()
		var maxIdx int
		if err := d.QueryRow(`SELECT MAX(idx) FROM steps`).Scan(&maxIdx); err != nil {
			t.Fatal(err)
		}
		return maxIdx
	}

	// 1. forkAgyThread(atTurnIndex=0) cuts before turn 1 ("Ok now go" via stored anchor -> u1Idx-1 = 15, NOT ordinal 1 -> step 1).
	turn0 := 0
	forkedTurn, err := app.forkAgyThread(thread, "", &turn0, nil)
	if err != nil {
		t.Fatalf("forkAgyThread(atTurnIndex=0): %v", err)
	}
	defer func() { _ = forkedTurn.Cleanup() }()
	if got := readMaxIdx(forkedTurn.SessionRef); got != u1Idx-1 {
		t.Fatalf("forkAgyThread(atTurnIndex=0) maxIdx = %d, want %d (drift-free stored anchor)", got, u1Idx-1)
	}

	// 2. forkAgyThreadBeforeMessage on unstamped u2 ("fork target") cuts at u2Idx-1 = 17 despite 7-step ordinal drift.
	u2Item, _, err := app.store.GetThreadItem(thread.ID, "u2")
	if err != nil {
		t.Fatal(err)
	}
	forkedMsg, err := app.forkAgyThreadBeforeMessage(thread, store.MessageAnchor{UserItemID: "u2"}, u2Item)
	if err != nil {
		t.Fatalf("forkAgyThreadBeforeMessage(u2): %v", err)
	}
	defer func() { _ = forkedMsg.Cleanup() }()
	if got := readMaxIdx(forkedMsg.SessionRef); got != u2Idx-1 {
		t.Fatalf("forkAgyThreadBeforeMessage(u2) maxIdx = %d, want %d (drift-free content match)", got, u2Idx-1)
	}

	// 3. rollbackAgyThreadToMessage on unstamped u2 repoints SessionRef to a DB cut at u2Idx-1 = 17.
	if err := app.rollbackAgyThreadToMessage(thread, store.MessageAnchor{UserItemID: "u2"}, u2Item); err != nil {
		t.Fatalf("rollbackAgyThreadToMessage(u2): %v", err)
	}
	afterRollback, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := readMaxIdx(afterRollback.SessionRef); got != u2Idx-1 {
		t.Fatalf("rollbackAgyThreadToMessage(u2) maxIdx = %d, want %d", got, u2Idx-1)
	}
}
