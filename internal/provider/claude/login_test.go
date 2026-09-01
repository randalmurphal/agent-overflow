package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fake speaks the sign-in control channel and nothing else, exactly as
// the real CLI does under these flags. Behaviour is chosen by environment so
// one script serves every case; a test that spawns it never reaches a real
// binary, a real provider home, or the network.
const mockLoginScript = `#!/bin/bash
printf '%s\n' "$*" > "$AO_LOGIN_ARGV"
printf '%s\n' "$CLAUDE_CONFIG_DIR" >> "$AO_LOGIN_ARGV"
printf '%s\n' "${CLAUDE_SECURESTORAGE_CONFIG_DIR-<unset>}" >> "$AO_LOGIN_ARGV"
printf '%s\n' "${ANTHROPIC_BASE_URL-<unset>}" >> "$AO_LOGIN_ARGV"
auth=0
while IFS= read -r line; do
  if [ -n "$AO_LOGIN_LOG" ]; then printf '%s\n' "$line" >> "$AO_LOGIN_LOG"; fi
  reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
  sub=$(printf '%s' "$line" | sed -n 's/.*"subtype":"\([^"]*\)".*/\1/p')
  case "$sub" in
    claude_authenticate)
      auth=$((auth+1))
      if [ "$auth" = "1" ] && [ -n "$AO_LOGIN_AUTH_ERROR" ]; then
        printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"%s"}}\n' "$reqid" "$AO_LOGIN_AUTH_ERROR"
      elif [ "$auth" = "2" ] && [ -n "$AO_LOGIN_AUTH_ERROR2" ]; then
        printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"%s"}}\n' "$reqid" "$AO_LOGIN_AUTH_ERROR2"
      else
        printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"manualUrl":"https://claude.ai/manual/%s","automaticUrl":"http://127.0.0.1:9999/callback/%s"}}}\n' "$reqid" "$auth" "$auth"
      fi
      ;;
    claude_oauth_callback)
      code=$(printf '%s' "$line" | sed -n 's/.*"authorizationCode":"\([^"]*\)".*/\1/p')
      if [ -n "$AO_LOGIN_GOOD_CODE" ] && [ "$code" = "$AO_LOGIN_GOOD_CODE" ]; then
        printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{"account":{"email":"user@example.test"}}}}\n' "$reqid"
      else
        printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"Request failed with status code 400"}}\n' "$reqid"
      fi
      ;;
    claude_oauth_wait_for_completion)
      case "$AO_LOGIN_WAIT" in
        success) printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid" ;;
        burned) printf '{"type":"control_response","response":{"subtype":"error","request_id":"%s","error":"No active claude_authenticate flow"}}\n' "$reqid" ;;
        *) : ;;
      esac
      ;;
  esac
done
exit 0
`

type loginFake struct {
	binary    string
	configDir string
	argvPath  string
	logPath   string
}

func newLoginFake(t *testing.T) *loginFake {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "mock-claude-login.sh")
	if err := os.WriteFile(binary, []byte(mockLoginScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &loginFake{
		binary:    binary,
		configDir: filepath.Join(dir, "home"),
		argvPath:  filepath.Join(dir, "argv"),
		logPath:   filepath.Join(dir, "requests"),
	}
	t.Setenv("AO_LOGIN_ARGV", fake.argvPath)
	t.Setenv("AO_LOGIN_LOG", fake.logPath)
	t.Setenv("AO_LOGIN_AUTH_ERROR", "")
	t.Setenv("AO_LOGIN_AUTH_ERROR2", "")
	t.Setenv("AO_LOGIN_GOOD_CODE", "")
	t.Setenv("AO_LOGIN_WAIT", "")
	return fake
}

func (f *loginFake) start(t *testing.T) *LoginSession {
	t.Helper()
	session, err := StartLogin(t.Context(), LoginConfig{Binary: f.binary, ConfigDir: f.configDir})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// capture is the fake's argv-and-environment record: argv, CLAUDE_CONFIG_DIR,
// CLAUDE_SECURESTORAGE_CONFIG_DIR, ANTHROPIC_BASE_URL, in that order.
func (f *loginFake) capture(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.argvPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("capture = %q", data)
	}
	return lines
}

func (f *loginFake) requests(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// The argv is the whole mechanism: --verbose is not optional under
// stream-json output, and the config dir is what keeps a sign-in out of the
// canonical home.
func TestStartLoginUsesTheHeadlessControlChannelArgvAndIsolatedHome(t *testing.T) {
	fake := newLoginFake(t)
	session := fake.start(t)
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	lines := fake.capture(t)
	wantArgv := "-p --input-format stream-json --output-format stream-json --verbose"
	if lines[0] != wantArgv {
		t.Errorf("argv = %q, want %q", lines[0], wantArgv)
	}
	if lines[1] != fake.configDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", lines[1], fake.configDir)
	}
	if lines[2] != "<unset>" {
		t.Errorf("CLAUDE_SECURESTORAGE_CONFIG_DIR = %q, want it cleared", lines[2])
	}
}

// A sign-in runs with the same environment an account probe does, because the
// environment picks which backend answers: signing in against the default one
// and then adopting whoever a custom ANTHROPIC_BASE_URL reports is two
// different accounts wearing one row.
func TestStartLoginCarriesTheConfiguredEnvironmentUnderTheHomePin(t *testing.T) {
	fake := newLoginFake(t)
	session, err := StartLogin(t.Context(), LoginConfig{
		Binary:    fake.binary,
		ConfigDir: fake.configDir,
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://backend.example.test",
			// The pin outranks whatever the caller's map says, so a stale
			// value here can never land a credential in the canonical home.
			"CLAUDE_CONFIG_DIR": "/should/not/win",
		},
	})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	lines := fake.capture(t)
	if lines[1] != fake.configDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want the pin %q", lines[1], fake.configDir)
	}
	if lines[3] != "https://backend.example.test" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the configured value", lines[3])
	}
}

func TestAuthenticateReturnsBothURLForms(t *testing.T) {
	fake := newLoginFake(t)
	urls, err := fake.start(t).Authenticate(t.Context())
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if urls.ManualURL != "https://claude.ai/manual/1" {
		t.Errorf("ManualURL = %q", urls.ManualURL)
	}
	if urls.AutomaticURL != "http://127.0.0.1:9999/callback/1" {
		t.Errorf("AutomaticURL = %q", urls.AutomaticURL)
	}
}

func TestStartLoginRefusesWithoutAnIsolatedConfigDir(t *testing.T) {
	if _, err := StartLogin(t.Context(), LoginConfig{Binary: "claude"}); err == nil {
		t.Fatal("StartLogin with no config dir returned nil error")
	}
}

func TestSubmitCallbackSucceedsWithTheMatchingCode(t *testing.T) {
	fake := newLoginFake(t)
	t.Setenv("AO_LOGIN_GOOD_CODE", "good-code")
	session := fake.start(t)
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := session.SubmitCallback(t.Context(), "good-code", "state-1"); err != nil {
		t.Fatalf("SubmitCallback: %v", err)
	}
}

// A bad paste is prose with no code beside it, and the sentence names the
// status the exchange came back with — which is the only thing that tells the
// user "the code, not the network".
func TestSubmitCallbackSurfacesTheCLIProse(t *testing.T) {
	fake := newLoginFake(t)
	t.Setenv("AO_LOGIN_GOOD_CODE", "good-code")
	session := fake.start(t)
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	err := session.SubmitCallback(t.Context(), "wrong-code", "state-1")
	if err == nil {
		t.Fatal("SubmitCallback with a bad code returned nil error")
	}
	if !strings.Contains(err.Error(), "Request failed with status code 400") {
		t.Fatalf("SubmitCallback error = %q, want the CLI prose", err)
	}
}

// Refused before the CLI is touched, because the CLI forwards an absent half
// as undefined and answers with an opaque 400 that also burns the flow.
func TestSubmitCallbackRefusesAnIncompletePasteWithoutWriting(t *testing.T) {
	fake := newLoginFake(t)
	session := fake.start(t)
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	before := len(fake.requests(t))
	for _, pair := range [][2]string{{"", "state"}, {"code", ""}, {"  ", " "}} {
		if err := session.SubmitCallback(t.Context(), pair[0], pair[1]); err == nil {
			t.Fatalf("SubmitCallback(%q, %q) returned nil error", pair[0], pair[1])
		}
	}
	if got := len(fake.requests(t)); got != before {
		t.Fatalf("incomplete pastes wrote %d requests to the CLI, want 0", got-before)
	}
}

// A second flow is a NEW link. The test reads it off the response rather than
// off internal state, because "the user is holding a dead link" is the whole
// failure this exists to avoid.
func TestAuthenticateAgainSupersedesWithAFreshLink(t *testing.T) {
	fake := newLoginFake(t)
	session := fake.start(t)
	first, err := session.Authenticate(t.Context())
	if err != nil {
		t.Fatalf("first Authenticate: %v", err)
	}
	second, err := session.Authenticate(t.Context())
	if err != nil {
		t.Fatalf("second Authenticate: %v", err)
	}
	if first.ManualURL == second.ManualURL {
		t.Fatalf("second Authenticate reused the first link %q", first.ManualURL)
	}
}

// The leak this pins: the CLI never answers a superseded wait, so a
// supersede that did not abandon it would park a goroutine per retry until
// the whole flow's ten-minute deadline.
func TestSupersedingReleasesTheOutstandingWait(t *testing.T) {
	fake := newLoginFake(t)
	t.Setenv("AO_LOGIN_WAIT", "never")
	session := fake.start(t)
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- session.WaitForCompletion(t.Context()) }()
	// The wait has to be registered before the supersede, or the test would
	// pass by racing rather than by the abandon working.
	waitForRequestCount(t, fake, 2)

	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("second Authenticate: %v", err)
	}
	select {
	case err := <-waited:
		if !errors.Is(err, errLoginSuperseded) {
			t.Fatalf("abandoned wait resolved with %v, want errLoginSuperseded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseded wait was never released")
	}
}

func TestWaitForCompletionResolvesOnTheLoopbackListenersSuccess(t *testing.T) {
	fake := newLoginFake(t)
	t.Setenv("AO_LOGIN_WAIT", "success")
	session := fake.start(t)
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := session.WaitForCompletion(t.Context()); err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
}

// The one string this package matches, because it is the one condition with a
// different remedy: the link is dead and only a fresh flow recovers it.
func TestBurnedFlowIsTyped(t *testing.T) {
	fake := newLoginFake(t)
	t.Setenv("AO_LOGIN_WAIT", "burned")
	session := fake.start(t)
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := session.WaitForCompletion(t.Context()); !errors.Is(err, ErrLoginFlowBurned) {
		t.Fatalf("WaitForCompletion error = %v, want ErrLoginFlowBurned", err)
	}
}

// Managed settings pin the account type in prose with no machine-readable
// code, and the refusal is raised before anything starts — so the flip is
// free, happens once, and is invisible to the caller.
func TestManagedSettingsRefusalRetriesWithTheOtherAccountType(t *testing.T) {
	fake := newLoginFake(t)
	t.Setenv("AO_LOGIN_AUTH_ERROR",
		"forceLoginMethod is 'console' in settings; log in with an Anthropic Console account instead.")
	session := fake.start(t)
	if _, err := session.Authenticate(t.Context()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	requests := fake.requests(t)
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want exactly one retry", requests)
	}
	if !strings.Contains(requests[0], `"loginWithClaudeAi":true`) {
		t.Errorf("first attempt = %q, want loginWithClaudeAi true", requests[0])
	}
	if !strings.Contains(requests[1], `"loginWithClaudeAi":false`) {
		t.Errorf("retry = %q, want loginWithClaudeAi flipped", requests[1])
	}
}

// One flip, never a loop: a second refusal is the answer, not a third try —
// and it is the RETRY's prose that surfaces, because by then the account type
// the settings demanded is the one that was tried.
func TestManagedSettingsRefusalSurfacesTheRetryProseAndStopsThere(t *testing.T) {
	fake := newLoginFake(t)
	t.Setenv("AO_LOGIN_AUTH_ERROR", "forceLoginMethod is 'console' in settings.")
	t.Setenv("AO_LOGIN_AUTH_ERROR2", "This organization has no seats left.")
	session := fake.start(t)
	_, err := session.Authenticate(t.Context())
	if err == nil {
		t.Fatal("Authenticate returned nil error, want the retry refusal")
	}
	if err.Error() != "This organization has no seats left." {
		t.Fatalf("Authenticate error = %q, want the retry prose", err)
	}
	if got := len(fake.requests(t)); got != 2 {
		t.Fatalf("the CLI saw %d attempts, want exactly two", got)
	}
}

func TestAuthenticateAfterTheProcessDiesReportsWhy(t *testing.T) {
	fake := newLoginFake(t)
	session := fake.start(t)
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := session.Authenticate(t.Context()); err == nil {
		t.Fatal("Authenticate on a dead process returned nil error")
	}
}

func waitForRequestCount(t *testing.T, fake *loginFake, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.requests(t)) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the fake never saw %d requests (saw %#v)", want, fake.requests(t))
}
