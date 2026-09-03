package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Codex's account sign-in, which unlike Claude's runs over an ordinary
// `app-server` connection: same argv, same handshake, so it arrives at the
// normal adapter rather than at an invocation mode of its own.
//
// The device-code variant is the one this exists for. Its completion is a
// NOTIFICATION the server sends when the person finishes on another screen, so
// there is nothing a test can write to stdin to reach it — the trigger is the
// control channel's login_complete command, which pairs the notification with
// the credential adoption then reads out of the login home.

const (
	// mockCodexLoginID is constant because one app-server serves one login,
	// and a completion is addressed by this and by nothing else. A driver that
	// had to discover it would be discovering a value the mock invented.
	mockCodexLoginID = "mock-codex-login"
	// mockCodexUserCode is what the fake device page would show. Grouped like
	// upstream's, since the UI renders it at the size of something read off
	// another screen and a single run of characters would not exercise that.
	mockCodexUserCode        = "MOCK-CODE"
	mockCodexVerificationURL = "https://mock.agent-overflow.test/device"
	mockCodexAuthURL         = "https://mock.agent-overflow.test/oauth/authorize?state=mock-codex"
)

// codexLoginState is the adapter's sign-in half: the live login id, or empty.
// Guarded by its own mutex because a login_complete command arrives on the
// control poll goroutine while account/login/* arrive on the stdin one.
type codexLoginState struct {
	mu      sync.Mutex
	loginID string
	device  bool
}

// handleLoginRequest answers account/login/start and account/login/cancel.
// Returns false for anything else, so the caller falls through to the read
// methods and then to -32601.
func (a *codexAdapter) handleLoginRequest(id json.RawMessage, method string, params json.RawMessage) bool {
	switch method {
	case "account/login/start":
		// Upstream matches the discriminant LITERALLY and answers a wrong
		// spelling by listing every variant it knows, so the mock branches on
		// the exact string too — a mock that accepted any spelling would let a
		// typo pass here and fail against the real app-server.
		device := readParamString(params, "type") == "chatgptDeviceCode"
		a.login.mu.Lock()
		a.login.loginID = mockCodexLoginID
		a.login.device = device
		a.login.mu.Unlock()
		if device {
			a.writeRPCResult(id, fmt.Sprintf(
				`{"type":"chatgptDeviceCode","loginId":%s,"verificationUrl":%s,"userCode":%s}`,
				mustJSON(mockCodexLoginID), mustJSON(mockCodexVerificationURL), mustJSON(mockCodexUserCode)))
			return true
		}
		a.writeRPCResult(id, fmt.Sprintf(
			`{"type":"chatgpt","loginId":%s,"authUrl":%s}`,
			mustJSON(mockCodexLoginID), mustJSON(mockCodexAuthURL)))
		return true
	case "account/login/cancel":
		// `notFound` is a real answer rather than an error: after a settled or
		// already-cancelled login there is nothing left to cancel, and the
		// client treats both as the outcome it asked for.
		status := "notFound"
		a.login.mu.Lock()
		if a.login.loginID != "" && readParamString(params, "loginId") == a.login.loginID {
			a.login.loginID = ""
			status = "canceled"
		}
		a.login.mu.Unlock()
		a.writeRPCResult(id, fmt.Sprintf(`{"status":%s}`, mustJSON(status)))
		return true
	}
	return false
}

// completeLogin settles the live sign-in from a control-channel command. A
// success writes the credential FIRST and announces afterwards, because the
// coordinator's adoption starts the moment the notification lands and reads
// the login home immediately.
func (a *codexAdapter) completeLogin(success bool, message string) {
	a.login.mu.Lock()
	loginID := a.login.loginID
	a.login.loginID = ""
	a.login.mu.Unlock()
	if loginID == "" {
		log.Printf("codex: login_complete with no sign-in in progress (ignored)")
		return
	}
	if success {
		if err := writeMockCodexCredential(); err != nil {
			log.Printf("codex: %v", err)
			success = false
			message = err.Error()
		}
	}
	if success {
		a.w.writeLine(fmt.Sprintf(
			`{"method":"account/login/completed","params":{"loginId":%s,"success":true}}`,
			mustJSON(loginID)), 0, 0)
		return
	}
	if message == "" {
		// Upstream's own wording for a login that ended without completing,
		// which is what a cancel or a supersede produces.
		message = "Login was not completed"
	}
	a.w.writeLine(fmt.Sprintf(
		`{"method":"account/login/completed","params":{"loginId":%s,"success":false,"error":%s}}`,
		mustJSON(loginID), mustJSON(message)), 0, 0)
}

// writeMockCodexCredential lands `auth.json` in the isolated login home. Same
// guard as the Claude half: an invocation that named no CODEX_HOME writes
// nothing, which is what keeps this out of a developer's real home — AO always
// names an ephemeral one for a sign-in.
func writeMockCodexCredential() error {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		return fmt.Errorf("this mock writes a credential only into CODEX_HOME, and none was set")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create mock login home: %w", err)
	}
	// `tokens.account_id` is the fallback workspace axis CredentialOrgID reads
	// when there is no id_token claim; the mock ships no JWT, so this is the
	// one that answers.
	credential := fmt.Sprintf(
		`{"OPENAI_API_KEY":null,"tokens":{"access_token":"mock-access","refresh_token":"mock-refresh",`+
			`"account_id":"mock-workspace"},"last_refresh":%s}`,
		mustJSON(time.Now().UTC().Format(time.RFC3339)))
	return writeMockLoginFile(filepath.Join(home, "auth.json"), credential)
}
