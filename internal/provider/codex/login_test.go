package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake app-server, answering only what a sign-in client asks. It omits the
// `jsonrpc` field on every response on purpose — that is what the real
// app-server does, and a decoder that required it would pass here and fail
// against the CLI.
const mockCodexLoginScript = `#!/bin/bash
printf '%s\n' "$*" > "$AO_CODEX_LOGIN_ARGV"
while IFS= read -r line; do
  if [ -n "$AO_CODEX_LOGIN_LOG" ]; then printf '%s\n' "$line" >> "$AO_CODEX_LOGIN_LOG"; fi
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"id":%s,"result":{"userAgent":"codex_cli_rs/0.151.0 (fake)"}}\n' "$id"
      ;;
    *'"type":"chatgptDeviceCode"'*)
      printf '{"id":%s,"result":{"type":"chatgptDeviceCode","loginId":"login-device","verificationUrl":"https://chatgpt.com/device","userCode":"ABCD-EFGH"}}\n' "$id"
      case "$AO_CODEX_LOGIN_COMPLETE" in
        success) printf '{"method":"account/login/completed","params":{"loginId":"login-device","success":true}}\n' ;;
        fail) printf '{"method":"account/login/completed","params":{"loginId":"login-device","success":false,"error":"Login was not completed"}}\n' ;;
        other) printf '{"method":"account/login/completed","params":{"loginId":"a-different-login","success":true}}\n' ;;
        *) : ;;
      esac
      ;;
    *'"method":"account/login/start"'*)
      printf '{"id":%s,"result":{"type":"chatgpt","loginId":"login-browser","authUrl":"https://auth.example.test/authorize?state=secret"}}\n' "$id"
      ;;
    *'"method":"account/login/cancel"'*)
      printf '{"id":%s,"result":{"status":"%s"}}\n' "$id" "${AO_CODEX_CANCEL_STATUS:-canceled}"
      ;;
  esac
done
exit 0
`

type codexLoginFake struct {
	binary   string
	home     string
	argvPath string
	logPath  string
}

func newCodexLoginFake(t *testing.T) *codexLoginFake {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "mock-codex-login.sh")
	if err := os.WriteFile(binary, []byte(mockCodexLoginScript), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &codexLoginFake{
		binary:   binary,
		home:     filepath.Join(dir, "home"),
		argvPath: filepath.Join(dir, "argv"),
		logPath:  filepath.Join(dir, "requests"),
	}
	t.Setenv("AO_CODEX_LOGIN_ARGV", fake.argvPath)
	t.Setenv("AO_CODEX_LOGIN_LOG", fake.logPath)
	t.Setenv("AO_CODEX_LOGIN_COMPLETE", "")
	t.Setenv("AO_CODEX_CANCEL_STATUS", "")
	return fake
}

func (f *codexLoginFake) start(t *testing.T) *LoginSession {
	t.Helper()
	session, err := StartLogin(t.Context(), LoginConfig{
		Binary: f.binary,
		Env:    map[string]string{"CODEX_HOME": f.home},
	})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func (f *codexLoginFake) requests(t *testing.T) []string {
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

// The credential-store pin is what makes account switching an atomic file
// replacement, and it is needed at PERSIST time — which is here, on a
// headless host where Codex's `auto` mode would otherwise pick a keyring.
func TestSignInPinsTheFileCredentialStore(t *testing.T) {
	fake := newCodexLoginFake(t)
	fake.start(t)
	data, err := os.ReadFile(fake.argvPath)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.TrimSpace(string(data))
	if !strings.Contains(argv, `cli_auth_credentials_store="file"`) || !strings.Contains(argv, "app-server") {
		t.Fatalf("argv = %q", argv)
	}
}

// The one notification a sign-in client depends on is named at the call site;
// everything else in the catalogue is opted out, account/updated included.
func TestSignInSubscribesOnlyToTheCompletionNotification(t *testing.T) {
	fake := newCodexLoginFake(t)
	fake.start(t)
	requests := fake.requests(t)
	if len(requests) == 0 {
		t.Fatal("the fake saw no initialize")
	}
	initialize := requests[0]
	if strings.Contains(initialize, `"account/login/completed"`) {
		t.Errorf("initialize opted out of the completion notification: %s", initialize)
	}
	if !strings.Contains(initialize, `"account/updated"`) {
		t.Errorf("initialize did not opt out of account/updated: %s", initialize)
	}
}

func TestStartDeviceLoginReturnsTheCodeAndItsPage(t *testing.T) {
	fake := newCodexLoginFake(t)
	before := time.Now()
	start, err := fake.start(t).StartDeviceLogin(t.Context())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if start.LoginID != "login-device" {
		t.Errorf("LoginID = %q", start.LoginID)
	}
	if start.UserCode != "ABCD-EFGH" {
		t.Errorf("UserCode = %q", start.UserCode)
	}
	if start.VerificationURL != "https://chatgpt.com/device" {
		t.Errorf("VerificationURL = %q", start.VerificationURL)
	}
	if start.AuthURL != "" {
		t.Errorf("AuthURL = %q, want empty on the device variant", start.AuthURL)
	}
	// The wire carries no expiry, so the countdown is ours and it has to be
	// there — a device screen with no timer is how a user sits on a dead code.
	if start.ExpiresAt.Before(before.Add(DeviceCodeLifetime - time.Minute)) {
		t.Errorf("ExpiresAt = %v, want about %v out", start.ExpiresAt, DeviceCodeLifetime)
	}
}

// Upstream matches the discriminant literally and answers a wrong spelling by
// listing every variant, which reads as a protocol failure rather than a typo.
func TestStartDeviceLoginSendsTheExactVariantSpelling(t *testing.T) {
	fake := newCodexLoginFake(t)
	if _, err := fake.start(t).StartDeviceLogin(t.Context()); err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	var found bool
	for _, request := range fake.requests(t) {
		if strings.Contains(request, `"type":"chatgptDeviceCode"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no request carried the exact variant spelling: %#v", fake.requests(t))
	}
}

func TestStartBrowserLoginReturnsTheAuthorizeURL(t *testing.T) {
	fake := newCodexLoginFake(t)
	start, err := fake.start(t).StartBrowserLogin(t.Context())
	if err != nil {
		t.Fatalf("StartBrowserLogin: %v", err)
	}
	if start.AuthURL != "https://auth.example.test/authorize?state=secret" {
		t.Errorf("AuthURL = %q", start.AuthURL)
	}
	if start.UserCode != "" || start.VerificationURL != "" {
		t.Errorf("browser start carried device fields: %+v", start)
	}
}

func TestWaitForCompletionResolvesOnItsOwnLogin(t *testing.T) {
	fake := newCodexLoginFake(t)
	t.Setenv("AO_CODEX_LOGIN_COMPLETE", "success")
	session := fake.start(t)
	start, err := session.StartDeviceLogin(t.Context())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if err := session.WaitForCompletion(t.Context(), start.LoginID); err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
}

// The completion can be dispatched before anyone waits for it, so it is
// buffered rather than dropped. Without the buffer this flow hangs until its
// deadline on a fast provider.
func TestWaitForCompletionAcceptsACompletionThatArrivedFirst(t *testing.T) {
	fake := newCodexLoginFake(t)
	t.Setenv("AO_CODEX_LOGIN_COMPLETE", "success")
	session := fake.start(t)
	start, err := session.StartDeviceLogin(t.Context())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := session.WaitForCompletion(t.Context(), start.LoginID); err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
}

// A superseded or cancelled login emits its OWN failed completion, so a
// client that matched on the method alone would report somebody else's
// outcome as this one's.
func TestWaitForCompletionIgnoresAnotherLoginsCompletion(t *testing.T) {
	fake := newCodexLoginFake(t)
	t.Setenv("AO_CODEX_LOGIN_COMPLETE", "other")
	session := fake.start(t)
	start, err := session.StartDeviceLogin(t.Context())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err = session.WaitForCompletion(ctx, start.LoginID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForCompletion = %v, want it to keep waiting for its own login", err)
	}
}

func TestWaitForCompletionSurfacesAFailedCompletion(t *testing.T) {
	fake := newCodexLoginFake(t)
	t.Setenv("AO_CODEX_LOGIN_COMPLETE", "fail")
	session := fake.start(t)
	start, err := session.StartDeviceLogin(t.Context())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	err = session.WaitForCompletion(t.Context(), start.LoginID)
	if err == nil {
		t.Fatal("WaitForCompletion returned nil on a failed completion")
	}
	if !strings.Contains(err.Error(), "Login was not completed") {
		t.Fatalf("WaitForCompletion error = %q", err)
	}
}

// notFound is the outcome we asked for, reached a different way.
func TestCancelTreatsNotFoundAsDone(t *testing.T) {
	for _, status := range []string{"canceled", "notFound"} {
		t.Run(status, func(t *testing.T) {
			fake := newCodexLoginFake(t)
			t.Setenv("AO_CODEX_CANCEL_STATUS", status)
			session := fake.start(t)
			start, err := session.StartDeviceLogin(t.Context())
			if err != nil {
				t.Fatalf("StartDeviceLogin: %v", err)
			}
			if err := session.Cancel(t.Context(), start.LoginID); err != nil {
				t.Fatalf("Cancel(%s) = %v", status, err)
			}
		})
	}
}

func TestCallAfterTheAppServerExitsReportsWhy(t *testing.T) {
	fake := newCodexLoginFake(t)
	session := fake.start(t)
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := session.StartDeviceLogin(t.Context()); err == nil {
		t.Fatal("StartDeviceLogin on a dead app-server returned nil error")
	}
}
