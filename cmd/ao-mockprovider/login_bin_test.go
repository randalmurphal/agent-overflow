package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/control"
	"agent-overflow/internal/harness/scenario"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/provideraccountapp"
)

// The sign-in mocks, driven by the REAL drivers rather than by hand-written
// frames. That is the whole value of these cases: a mock whose answers only
// satisfied a test written beside it would pass while AO's own decoder
// refused every one of them, which is the failure this file exists to catch.

// startClaudeLoginMock spawns the mock in its sign-in mode through the
// production driver, with an isolated config dir standing in for the
// ephemeral home the coordinator cuts.
func startClaudeLoginMock(t *testing.T) (*claude.LoginSession, string) {
	t.Helper()
	configDir := t.TempDir()
	session, err := claude.StartLogin(t.Context(), claude.LoginConfig{
		Binary:    mockBin,
		ConfigDir: configDir,
	})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, configDir
}

func TestClaudeSignInMockAnswersBothLinks(t *testing.T) {
	session, _ := startClaudeLoginMock(t)
	urls, err := session.Authenticate(t.Context())
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if urls.ManualURL == "" || urls.AutomaticURL == "" {
		t.Fatalf("Authenticate = %+v, want both links", urls)
	}
	// The two forms carry the SAME exchange and differ only in redirect_uri.
	// A mock that minted two independent states would let a client pick the
	// wrong one and still pass.
	if state := mockLoginState(t, urls.ManualURL); state != mockLoginState(t, urls.AutomaticURL) {
		t.Fatalf("the two links carry different state: %q vs %q",
			state, mockLoginState(t, urls.AutomaticURL))
	}
}

func TestClaudeSignInMockCompletesOnThePastedCode(t *testing.T) {
	session, configDir := startClaudeLoginMock(t)
	urls, err := session.Authenticate(t.Context())
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := session.SubmitCallback(t.Context(), mockClaudeLoginCode, mockLoginState(t, urls.ManualURL)); err != nil {
		t.Fatalf("SubmitCallback: %v", err)
	}

	// The credential is what adoption actually consumes, so the assertion is
	// the app's own policy read rather than the file's presence.
	data, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		t.Fatalf("read mock credential: %v", err)
	}
	if provideraccountapp.CredentialSignedOut("claude", data) {
		t.Fatal("the mock wrote a credential that reads as signed out")
	}
	if _, ok := provideraccountapp.CredentialChainPosition("claude", data); !ok {
		t.Fatal("the mock wrote a credential with no rotation-chain position")
	}
	if _, err := os.Stat(filepath.Join(configDir, ".claude.json")); err != nil {
		t.Fatalf("the mock wrote no identity record beside the credential: %v", err)
	}
}

// One bad paste burns the flow: upstream closes its listener and clears the
// slot, so everything after answers the no-active-flow sentence. That rule is
// the reason AO restarts rather than re-prompts, and a mock that let a second
// paste succeed would hide a coordinator that never restarted.
func TestClaudeSignInMockBurnsTheFlowOnABadCode(t *testing.T) {
	session, configDir := startClaudeLoginMock(t)
	urls, err := session.Authenticate(t.Context())
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	state := mockLoginState(t, urls.ManualURL)
	err = session.SubmitCallback(t.Context(), "not-the-code", state)
	if err == nil {
		t.Fatal("SubmitCallback accepted a wrong code")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("SubmitCallback error = %q, want the provider's own refusal", err)
	}
	if err := session.SubmitCallback(t.Context(), mockClaudeLoginCode, state); !errors.Is(err, claude.ErrLoginFlowBurned) {
		t.Fatalf("second SubmitCallback = %v, want the flow to be burned", err)
	}
	if _, err := os.Stat(filepath.Join(configDir, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a burned flow left a credential behind: %v", err)
	}

	// The recovery is a FRESH flow, and its link must not be the dead one.
	fresh, err := session.Authenticate(t.Context())
	if err != nil {
		t.Fatalf("Authenticate after a burned flow: %v", err)
	}
	if mockLoginState(t, fresh.ManualURL) == state {
		t.Fatal("the replacement link carries the burned flow's state")
	}
	if err := session.SubmitCallback(t.Context(), mockClaudeLoginCode, mockLoginState(t, fresh.ManualURL)); err != nil {
		t.Fatalf("SubmitCallback on the replacement flow: %v", err)
	}
}

// mockLoginState reads the exchange's state out of a mock sign-in URL.
func mockLoginState(t *testing.T, link string) string {
	t.Helper()
	_, query, ok := strings.Cut(link, "?")
	if !ok {
		t.Fatalf("sign-in link %q carries no query", link)
	}
	for _, pair := range strings.Split(query, "&") {
		if value, found := strings.CutPrefix(pair, "state="); found {
			return value
		}
	}
	t.Fatalf("sign-in link %q carries no state", link)
	return ""
}

// startCodexLoginMock spawns the mock as an app-server with a live control
// channel, because the device flow's completion has no other trigger.
func startCodexLoginMock(t *testing.T) (*codex.LoginSession, *control.Server, string, chan string) {
	t.Helper()
	scenarioJSON, err := json.Marshal(&scenario.Scenario{
		Version:  scenario.CurrentVersion,
		Name:     "codex-login",
		Provider: scenario.ProviderCodex,
		Turns:    []scenario.Turn{{Label: "idle", Steps: []scenario.Step{}}},
	})
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}
	mockIDs := make(chan string, 8)
	srv, err := control.NewServer(control.ServerConfig{
		Resolve: func(control.Registration) (control.Assignment, error) {
			return control.Assignment{ScenarioName: "codex-login", ScenarioJSON: scenarioJSON}, nil
		},
		OnReport: func(info control.MockInfo, _ control.Report) {
			select {
			case mockIDs <- info.MockID:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	home := t.TempDir()
	session, err := codex.StartLogin(t.Context(), codex.LoginConfig{
		Binary:  mockBin,
		WorkDir: t.TempDir(),
		Env: map[string]string{
			"CODEX_HOME":     home,
			control.EnvAddr:  srv.Addr(),
			control.EnvToken: srv.Token(),
		},
	})
	if err != nil {
		t.Fatalf("codex StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, srv, home, mockIDs
}

func TestCodexDeviceSignInMockCompletesOnTheControlTrigger(t *testing.T) {
	session, srv, home, mockIDs := startCodexLoginMock(t)
	start, err := session.StartDeviceLogin(t.Context())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if start.UserCode != mockCodexUserCode || start.VerificationURL != mockCodexVerificationURL {
		t.Fatalf("StartDeviceLogin = %+v", start)
	}
	if start.ExpiresAt.IsZero() {
		t.Fatal("the device flow carries no expiry, so nothing can show a countdown")
	}

	mockID := awaitMockID(t, mockIDs)
	if err := srv.Command(mockID, control.Command{Type: control.CommandLoginComplete}); err != nil {
		t.Fatalf("login_complete: %v", err)
	}
	if err := session.WaitForCompletion(t.Context(), start.LoginID); err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Fatalf("a completed sign-in wrote no credential: %v", err)
	}
}

func TestCodexDeviceSignInMockSurfacesAFailedCompletion(t *testing.T) {
	session, srv, home, mockIDs := startCodexLoginMock(t)
	start, err := session.StartDeviceLogin(t.Context())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	mockID := awaitMockID(t, mockIDs)
	if err := srv.Command(mockID, control.Command{
		Type:  control.CommandLoginComplete,
		Error: "Login was not completed",
	}); err != nil {
		t.Fatalf("login_complete: %v", err)
	}
	err = session.WaitForCompletion(t.Context(), start.LoginID)
	if err == nil || !strings.Contains(err.Error(), "Login was not completed") {
		t.Fatalf("WaitForCompletion = %v, want the failure text", err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed sign-in left a credential behind: %v", err)
	}
}

func TestCodexSignInMockCancels(t *testing.T) {
	session, _, _, _ := startCodexLoginMock(t)
	start, err := session.StartDeviceLogin(t.Context())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if err := session.Cancel(t.Context(), start.LoginID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Cancelling twice is `notFound`, which the driver treats as the outcome
	// it asked for rather than as an error.
	if err := session.Cancel(t.Context(), start.LoginID); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
}

func TestCodexBrowserSignInMockAnswersAnAuthorizeURL(t *testing.T) {
	session, _, _, _ := startCodexLoginMock(t)
	start, err := session.StartBrowserLogin(t.Context())
	if err != nil {
		t.Fatalf("StartBrowserLogin: %v", err)
	}
	if start.AuthURL != mockCodexAuthURL || start.UserCode != "" {
		t.Fatalf("StartBrowserLogin = %+v", start)
	}
}

func awaitMockID(t *testing.T, ids chan string) string {
	t.Helper()
	select {
	case id := <-ids:
		return id
	case <-time.After(testTimeout):
		t.Fatal("the sign-in mock never registered on the control channel")
		return ""
	}
}
