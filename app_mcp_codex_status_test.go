package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
)

// TestListThreadMcpServers_Codex_LiveSession_SettledProbeAndStartupTruth
// is the 2026-08-15 incident regression. Codex answered
// `mcpServerStatus/list` for atlassian with authStatus "oAuth", no
// serverInfo and zero tools, while the thread's own startup
// notification had already reported `failed` with
// "invalid_grant: Invalid refresh token" and a null failureReason. AO
// rendered "Starting…" forever: the list projector treated
// credentialed-but-toolless as a booting server, and nothing consulted
// the startup notification the session had already seen.
//
// The merge's precedence is pinned by the DISAGREEMENT cases: a
// terminal retained state (failed/cancelled) wins over any probe answer
// — the thread still holds the manager that failed, and the retained
// state carries the cause string the probe cannot — while every
// non-terminal retained state (starting, unrecognized) defers to the
// probe, because the list describes settled attempts and is by
// construction the newer observation.
func TestListThreadMcpServers_Codex_LiveSession_SettledProbeAndStartupTruth(t *testing.T) {
	const invalidGrant = "invalid_grant: Invalid refresh token"
	// The two probe shapes the cases combine with retained states:
	// settled-and-failed (credential present, no initialize evidence) and
	// settled-and-connected (serverInfo + one tool).
	const listFailed = `{"data":[{"name":"atlassian","authStatus":"oAuth","tools":{}}]}`
	const listConnected = `{"data":[{"name":"atlassian","authStatus":"oAuth","serverInfo":{"name":"atlassian","version":"1.4.0"},"tools":{"fetchTicket":{}}}]}`
	notif := func(status, errStr, failureReason string) string {
		e := "null"
		if errStr != "" {
			e = `"` + errStr + `"`
		}
		fr := "null"
		if failureReason != "" {
			fr = `"` + failureReason + `"`
		}
		return `{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"threadId":"codex-thread-mcp","name":"atlassian","status":"` + status + `","error":` + e + `,"failureReason":` + fr + `}}`
	}

	cases := []struct {
		name          string
		listResult    string
		notifications []string
		wantStatus    string
		wantError     string
		wantToolCount int
	}{
		{
			name:       "failed startup notification wins and carries the cause",
			listResult: listFailed,
			notifications: []string{
				notif("starting", "", ""),
				notif("failed", invalidGrant, ""),
			},
			wantStatus: "failed",
			wantError:  invalidGrant,
		},
		{
			// Same probe answer with no notification at all — the
			// inactive-thread shape, and what a resumed session sees
			// when the startup round happened before AO attached.
			name:       "settled probe with no initialize evidence is failed on its own",
			listResult: listFailed,
			wantStatus: "failed",
		},
		{
			// Post-reload recovery: the sign-in landed, Codex re-ran
			// startup, and both the notification and the fresh probe
			// agree. A retained failure must not latch.
			name:       "a later ready update releases the row back to the probe",
			listResult: listConnected,
			notifications: []string{
				notif("failed", invalidGrant, ""),
				notif("ready", "", ""),
			},
			wantStatus:    "connected",
			wantToolCount: 1,
		},
		{
			// The probe builds a FRESH connection set, so a repaired
			// credential can make it succeed while the thread still holds
			// the manager that failed. The thread's truth wins — and the
			// probe's tool list must not ride on a row reporting a failed
			// manager.
			name:          "a retained failure outranks a probe that would now succeed",
			listResult:    listConnected,
			notifications: []string{notif("failed", invalidGrant, "")},
			wantStatus:    "failed",
			wantError:     invalidGrant,
		},
		{
			name:          "a retained cancellation outranks a connected probe",
			listResult:    listConnected,
			notifications: []string{notif("cancelled", "", "")},
			wantStatus:    "failed",
		},
		{
			// The non-terminal side of the precedence: a retained
			// "starting" must defer to the settled probe, or a lost
			// terminal notification re-creates the "Starting… forever"
			// incident through the merge.
			name:          "a retained starting defers to a connected probe",
			listResult:    listConnected,
			notifications: []string{notif("starting", "", "")},
			wantStatus:    "connected",
			wantToolCount: 1,
		},
		{
			name:          "a retained starting defers to a failed probe",
			listResult:    listFailed,
			notifications: []string{notif("starting", "", "")},
			wantStatus:    "failed",
		},
		{
			// An unknown observation must not outrank a settled one.
			name:          "an unrecognized retained state defers to the probe",
			listResult:    listConnected,
			notifications: []string{notif("warming-up", "", "")},
			wantStatus:    "connected",
			wantToolCount: 1,
		},
		{
			// When the wire DOES classify the failure, the row resolves
			// to needs-auth and the menu's Sign in action.
			name:          "a reauthenticationRequired failure projects needs-auth",
			listResult:    listFailed,
			notifications: []string{notif("failed", "token expired", "reauthenticationRequired")},
			wantStatus:    "needs-auth",
			wantError:     "token expired",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, _, codexPath := newMCPTestApp(t)
			workspace := t.TempDir()
			writeCodexConfig(t, codexPath, `
[mcp_servers.atlassian]
url = "https://mcp.atlassian.com/v1/sse"
`)
			thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
			if err != nil {
				t.Fatalf("createTestThread: %v", err)
			}

			// The ghost notification rides along in every case: the list
			// is the membership answer, so a retained state for a server
			// it doesn't return must not create a row.
			notifications := append([]string{
				`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"threadId":"codex-thread-mcp","name":"ghost","status":"failed","error":"gone from config"}}`,
			}, tc.notifications...)
			binary := writeCodexMcpStatusResponderBinary(t, "", tc.listResult, notifications)
			sess := newCodexMcpStatusSession(t, app, thread.ID, binary, workspace, "codex-mcp-status-token")
			_ = sess

			rows, err := app.ListThreadMcpServers(thread.ID)
			if err != nil {
				t.Fatalf("ListThreadMcpServers: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want only atlassian (a startup state for a server absent from the list must not create a row): %#v", len(rows), rows)
			}
			row := findServer(rows, "atlassian")
			if row.Name == "" {
				t.Fatalf("atlassian row missing from %#v", rows)
			}
			if row.Status == "starting" {
				t.Fatalf("atlassian rendered %q — a settled list probe can never mean booting", row.Status)
			}
			if row.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", row.Status, tc.wantStatus)
			}
			if row.Error != tc.wantError {
				t.Errorf("error = %q, want %q", row.Error, tc.wantError)
			}
			if len(row.Tools) != tc.wantToolCount {
				t.Errorf("tools = %v, want %d entries", row.Tools, tc.wantToolCount)
			}
			// The frontend needs the auth enum to decide whether a failed
			// row can offer "Sign in again".
			if row.AuthStatus != "oAuth" {
				t.Errorf("authStatus = %q, want oAuth", row.AuthStatus)
			}
			if row.Source != mcpRowSourceSession {
				t.Errorf("source = %q, want %q", row.Source, mcpRowSourceSession)
			}
		})
	}
}

// TestHandleCodexMCPOAuthCompleted_ForgetsRetainedFailure is the
// post-sign-in half of the incident: the retained `failed` describes the
// run the sign-in just invalidated, and Codex's fresh startupStatus
// round only arrives at the next turn boundary — so without the forget,
// the row would keep reading "Failed · invalid_grant / Sign in again"
// over a sign-in that succeeded, until the user happened to send a turn.
func TestHandleCodexMCPOAuthCompleted_ForgetsRetainedFailure(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	workspace := t.TempDir()
	writeCodexConfig(t, codexPath, `
[mcp_servers.atlassian]
url = "https://mcp.atlassian.com/v1/sse"
`)
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	captureDir := t.TempDir()
	binary := writeCodexMcpStatusResponderBinary(t, captureDir,
		`{"data":[{"name":"atlassian","authStatus":"oAuth","serverInfo":{"name":"atlassian","version":"1.4.0"},"tools":{"fetchTicket":{}}}]}`,
		[]string{`{"jsonrpc":"2.0","method":"mcpServer/startupStatus/updated","params":{"threadId":"codex-thread-mcp","name":"atlassian","status":"failed","error":"invalid_grant: Invalid refresh token","failureReason":null}}`},
	)
	newCodexMcpStatusSession(t, app, thread.ID, binary, workspace, "codex-oauth-forget-token")

	rows, err := app.ListThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("ListThreadMcpServers before sign-in: %v", err)
	}
	if row := findServer(rows, "atlassian"); row.Status != "failed" {
		t.Fatalf("pre-sign-in status = %q, want the retained failure", row.Status)
	}

	app.handleCodexMCPOAuthCompleted(thread.ID, "atlassian", true, "")
	if method := readCodexReloadCapture(t, captureDir, 3*time.Second); method != "config/mcpServer/reload" {
		t.Fatalf("captured Codex method = %q, want config/mcpServer/reload", method)
	}

	rows, err = app.ListThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("ListThreadMcpServers after sign-in: %v", err)
	}
	if row := findServer(rows, "atlassian"); row.Status != "connected" || row.Error != "" {
		t.Fatalf("post-sign-in row = %+v, want the settled probe's connected answer", row)
	}
}

// TestHandleCodexMCPOAuthCompleted_SuccessReloadsLiveSession pins the
// post-OAuth reload. A loaded thread keeps the MCP manager it started
// with, so without `config/mcpServer/reload` a server that failed
// startup on an expired grant stays failed for the rest of the session
// no matter how the browser hop went. One completion, one reload.
func TestHandleCodexMCPOAuthCompleted_SuccessReloadsLiveSession(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	workspace := t.TempDir()
	writeCodexConfig(t, codexPath, `
[mcp_servers.atlassian]
url = "https://mcp.atlassian.com/v1/sse"
`)
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	captureDir := t.TempDir()
	binary := writeCodexRefreshCaptureBinary(t, captureDir, "codex-thread-mcp", "")
	newCodexMcpStatusSession(t, app, thread.ID, binary, workspace, "codex-oauth-reload-token")

	app.handleCodexMCPOAuthCompleted(thread.ID, "atlassian", true, "")

	if method := readCodexReloadCapture(t, captureDir, 3*time.Second); method != "config/mcpServer/reload" {
		t.Fatalf("captured Codex method = %q, want config/mcpServer/reload", method)
	}
	// A duplicate would be issued by the same handler call, so it would
	// already be in flight; a short settle is enough to catch it.
	time.Sleep(300 * time.Millisecond)
	if n := countCaptureLines(t, captureDir); n != 1 {
		t.Fatalf("captured %d reload RPCs, want exactly 1", n)
	}
}

// TestHandleCodexMCPOAuthCompleted_ReloadsSiblingSessions pins the provider-
// global ownership of the OAuth grant. A completion received on one
// app-server must clear retained startup failures in every live Codex process,
// or sibling panes stay on "Sign in again" until their sessions restart.
func TestHandleCodexMCPOAuthCompleted_ReloadsSiblingSessions(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	writeCodexConfig(t, codexPath, `
[mcp_servers.atlassian]
url = "https://mcp.atlassian.com/v1/sse"
`)
	first, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatalf("create first thread: %v", err)
	}
	second, err := createTestThread(t, app, string(provider.Codex), t.TempDir(), "gpt-5", "chat")
	if err != nil {
		t.Fatalf("create second thread: %v", err)
	}
	firstCapture := t.TempDir()
	secondCapture := t.TempDir()
	newCodexMcpStatusSession(
		t,
		app,
		first.ID,
		writeCodexRefreshCaptureBinary(t, firstCapture, "codex-thread-first", ""),
		first.WorkspacePath,
		"codex-oauth-first-token",
	)
	newCodexMcpStatusSession(
		t,
		app,
		second.ID,
		writeCodexRefreshCaptureBinary(t, secondCapture, "codex-thread-second", ""),
		second.WorkspacePath,
		"codex-oauth-second-token",
	)

	app.handleCodexMCPOAuthCompleted(first.ID, "atlassian", true, "")

	if method := readCodexReloadCapture(t, firstCapture, 3*time.Second); method != "config/mcpServer/reload" {
		t.Fatalf("initiating session method = %q, want config/mcpServer/reload", method)
	}
	if method := readCodexReloadCapture(t, secondCapture, 3*time.Second); method != "config/mcpServer/reload" {
		t.Fatalf("sibling session method = %q, want config/mcpServer/reload", method)
	}
}

// TestCodexMCPReloadRequestsCoalesce: `config/mcpServer/reload` is a
// level trigger — it re-reads the whole config — so N completions
// landing while one reload is in flight must collapse into a single
// follow-up run, not N stacked round-trips. The mock's reload arm
// blocks on a gate file, which is what makes "in flight" deterministic.
func TestCodexMCPReloadRequestsCoalesce(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	workspace := t.TempDir()
	writeCodexConfig(t, codexPath, `
[mcp_servers.atlassian]
url = "https://mcp.atlassian.com/v1/sse"
`)
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	captureDir := t.TempDir()
	gate := filepath.Join(captureDir, "gate")
	binary := writeCodexRefreshCaptureBinary(t, captureDir, "codex-thread-mcp", gate)
	newCodexMcpStatusSession(t, app, thread.ID, binary, workspace, "codex-reload-coalesce-token")

	// First completion starts a reload that blocks on the gate.
	app.handleCodexMCPOAuthCompleted(thread.ID, "atlassian", true, "")
	waitForCaptureLineCount(t, captureDir, 1, 3*time.Second)

	// Two more completions while it is in flight — both must fold into
	// ONE follow-up run.
	app.handleCodexMCPOAuthCompleted(thread.ID, "atlassian", true, "")
	app.handleCodexMCPOAuthCompleted(thread.ID, "atlassian", true, "")

	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	waitForCaptureLineCount(t, captureDir, 2, 3*time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := countCaptureLines(t, captureDir); n != 2 {
		t.Fatalf("captured %d reload RPCs, want exactly 2 (one in flight + one coalesced follow-up)", n)
	}
}

// TestHandleCodexMCPOAuthCompleted_FailureDoesNotReload: a failed
// sign-in changes nothing on disk, so reloading the thread's config
// would only re-run a startup round that is already known to fail.
func TestHandleCodexMCPOAuthCompleted_FailureDoesNotReload(t *testing.T) {
	app, _, codexPath := newMCPTestApp(t)
	workspace := t.TempDir()
	writeCodexConfig(t, codexPath, `
[mcp_servers.atlassian]
url = "https://mcp.atlassian.com/v1/sse"
`)
	thread, err := createTestThread(t, app, string(provider.Codex), workspace, "gpt-5", "chat")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	captureDir := t.TempDir()
	binary := writeCodexRefreshCaptureBinary(t, captureDir, "codex-thread-mcp", "")
	newCodexMcpStatusSession(t, app, thread.ID, binary, workspace, "codex-oauth-reload-token")

	app.handleCodexMCPOAuthCompleted(thread.ID, "atlassian", false, "user cancelled")

	if waitForMcpCapture(captureDir, 500*time.Millisecond) {
		t.Fatalf("failed sign-in must not reload; capture observed at %s", captureDir)
	}
}

// newCodexMcpStatusSession spawns a codex.Session against a mock
// app-server binary and registers it on the app under threadID.
func newCodexMcpStatusSession(t *testing.T, app *App, threadID, binary, workspace, token string) *codex.Session {
	t.Helper()
	sess, err := codex.NewSession(
		context.Background(),
		threadID,
		codex.Config{Binary: binary, Model: "gpt-5", WorkDir: workspace},
		func(provider.ProviderEvent) {},
	)
	if err != nil {
		t.Fatalf("codex.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	app.sessions[threadID] = session{
		provider: string(provider.Codex),
		token:    token,
		codex:    sess,
	}
	return sess
}

// writeCodexMcpStatusResponderBinary emits a fake codex app-server that
// answers `mcpServerStatus/list` with listResult, preceded — on the
// FIRST list call only, like the real startup round — by the given raw
// notification lines. Writing the notifications ahead of the response
// is what makes the test deterministic: the session's read loop
// dispatches lines in order, so every notification is retained before
// the list reply reaches the waiting caller. captureDir may be empty
// when the test does not exercise reload RPCs; when set, reload
// requests append to capture.jsonl and answer success.
func writeCodexMcpStatusResponderBinary(t *testing.T, captureDir, listResult string, notifications []string) string {
	t.Helper()
	if strings.Contains(listResult, "'") {
		t.Fatalf("list result must not contain a single quote (it is embedded in a shell literal): %s", listResult)
	}
	var notifLines strings.Builder
	for _, n := range notifications {
		if strings.Contains(n, "'") {
			t.Fatalf("notification must not contain a single quote: %s", n)
		}
		notifLines.WriteString("            printf '%s\\n' '" + n + "'\n")
	}
	capture := "/dev/null"
	if captureDir != "" {
		capture = filepath.Join(captureDir, "capture.jsonl")
	}
	scriptDir := t.TempDir()
	notifSentMarker := filepath.Join(scriptDir, "notifications-sent")
	script := `#!/bin/sh
set -u
while IFS= read -r line; do
    id=$(printf '%s' "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
    if [ -z "$id" ]; then
        continue
    fi
    case "$line" in
      *'"method":"initialize"'*)
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
        ;;
      *'"method":"thread/start"'*)
        printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"codex-thread-mcp","turns":[]}}}\n' "$id"
        ;;
      *'"method":"mcpServerStatus/list"'*)
        if [ ! -f ` + shellQuote(notifSentMarker) + ` ]; then
            : > ` + shellQuote(notifSentMarker) + `
` + notifLines.String() + `        fi
        printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" '` + listResult + `'
        ;;
      *'"method":"config/mcpServer/reload"'*)
        printf '%s\n' "$line" >> ` + shellQuote(capture) + `
        printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
        ;;
    esac
done
`
	path := filepath.Join(scriptDir, "codex-mcp-status-responder.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex mcp status responder: %v", err)
	}
	return path
}

func countCaptureLines(t *testing.T, captureDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(captureDir, "capture.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read capture: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func waitForCaptureLineCount(t *testing.T, captureDir string, want int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if countCaptureLines(t, captureDir) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("capture never reached %d lines (have %d)", want, countCaptureLines(t, captureDir))
}
