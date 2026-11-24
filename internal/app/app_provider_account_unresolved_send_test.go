package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/claudefake"
	"agent-overflow/internal/provideraccountapp"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/testutil/mockexec"

	"github.com/google/uuid"
)

// writeCodexMarkerBinary is a Codex that records each start in marker and
// answers nothing: a send that reaches a provider process leaves the file.
func writeCodexMarkerBinary(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex-must-not-run")
	mockexec.Write(t, path, "#!/bin/bash\nprintf 'started\\n' >> "+shellQuote(marker)+"\nexit 1\n")
	return path
}

// writeCodexTurnBinary is an app-server whose account/read answers
// accountJSON, that runs every turn to completion and records each
// turn/start line in capture.
func writeCodexTurnBinary(t *testing.T, accountJSON, capture string) string {
	t.Helper()
	const providerThread = "codex-unresolved-send"
	script := fmt.Sprintf(`#!/bin/bash
turn=0
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"userAgent":"codex_cli_rs/0.149.0 (Ubuntu 24.04; x86_64) app_server"}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"account/read"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"account":%s,"requiresOpenaiAuth":true}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"thread/start"'; then
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"thread":{"id":"%s"}}}\n' "$id"
    continue
  fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"turn/start"'; then
    printf '%%s\n' "$line" >> %q
    turn=$((turn+1))
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"turn":{"id":"turn-%%s"}}}\n' "$id" "$turn"
    printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"%s","turn":{"id":"turn-%%s"}}}\n' "$turn"
    printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%s","turnId":"turn-%%s","item":{"id":"msg-%%s","type":"agentMessage","text":"done"}}}\n' "$turn" "$turn"
    printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%s","turn":{"id":"turn-%%s","status":"completed"}}}\n' "$turn"
    continue
  fi
  printf '{"jsonrpc":"2.0","id":%%s,"result":{}}\n' "$id"
done
`, accountJSON, providerThread, capture, providerThread, providerThread, providerThread)
	path := filepath.Join(t.TempDir(), "codex-turns")
	mockexec.Write(t, path, script)
	return path
}

// agyLogin is an Antigravity OAuth credential; email, when set, is the
// id_token_email the CLI persists. Google's userinfo is unreachable in an
// isolated boot, so a test of a login that names nobody answers it with
// googleUserinfoNamesNobody.
func agyLogin(token, email string) []byte {
	if email == "" {
		return []byte(`{"access_token":"ya29.` + token + `","refresh_token":"1//` + token + `"}`)
	}
	return []byte(`{"access_token":"ya29.` + token + `","refresh_token":"1//` + token + `","id_token_email":"` + email + `"}`)
}

// googleUserinfoNamesNobody answers Google's userinfo with a body that
// names nobody, and every other request the way an isolated boot does.
var googleUserinfoNamesNobody = &http.Client{Transport: userinfoNamesNobody{}}

type userinfoNamesNobody struct{}

func (userinfoNamesNobody) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "www.googleapis.com" && req.URL.Path == "/oauth2/v2/userinfo" {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: req}, nil
	}
	return isolatedHTTPClient.Transport.RoundTrip(req)
}

// unresolvedSendCase is one provider's half of the unresolved-send test:
// its saved logins, the outside login, the binary that identifies the
// outside login, and the binary that runs a turn.
type unresolvedSendCase struct {
	name, provider, settingsKey, model string
	// firstEmail and firstOrg are the selected account's; the other saved
	// account is secondEmail in secondOrg.
	firstEmail, firstOrg, secondEmail, secondOrg string
	firstLogin, secondLogin, outsideLogin        []byte
	identify                                     func(t *testing.T) string
	turns                                        func(t *testing.T, capture string) string
	want                                         string
}

// While the canonical login is one reconciliation could not attribute to
// one saved account, a send is refused with that verdict's sentence before
// any provider process starts. A switch to another saved account still goes
// through, and the next send runs on it.
func TestASendUnderAnUnresolvedLoginIsRefusedUntilASwitch(t *testing.T) {
	t.Parallel()
	codexLogin := func(token string) []byte {
		return []byte(`{"tokens":{"access_token":"` + token + `","refresh_token":"rt-` + token + `"}}`)
	}
	codexTurns := func(email string) func(*testing.T, string) string {
		return func(t *testing.T, capture string) string {
			return writeCodexTurnBinary(t, `{"type":"chatgpt","email":"`+email+`","planType":"pro"}`, capture)
		}
	}
	agyIdentify := func(t *testing.T) string { return writeSharedPoolAgyBinary(t, "") }
	cases := []unresolvedSendCase{
		{
			name: "a Codex login that names no account", provider: string(provider.Codex), settingsKey: "codexBinaryPath",
			firstEmail: "first@example.com", firstOrg: "ws-1", secondEmail: "second@example.com", secondOrg: "ws-2",
			firstLogin: codexLogin("first"), secondLogin: codexLogin("second"), outsideLogin: codexLogin("outside"),
			identify: func(t *testing.T) string {
				return writeCodexAccountReadProbeBinary(t, `{"type":"chatgpt","planType":"pro"}`)
			},
			turns: codexTurns("second@example.com"),
			want:  "The Codex login in use names no account",
		},
		{
			name: "a Codex login whose email two saved accounts share", provider: string(provider.Codex), settingsKey: "codexBinaryPath",
			firstEmail: "shared@example.com", firstOrg: "ws-1", secondEmail: "shared@example.com", secondOrg: "ws-2",
			firstLogin: codexLogin("first"), secondLogin: codexLogin("second"), outsideLogin: codexLogin("outside"),
			identify: func(t *testing.T) string {
				return writeCodexAccountReadProbeBinary(t, `{"type":"chatgpt","email":"shared@example.com","planType":"pro"}`)
			},
			turns: codexTurns("shared@example.com"),
			want:  "an email more than one saved account uses",
		},
		{
			name: "an Antigravity login that names no account", provider: string(provider.Agy), settingsKey: "agyBinaryPath",
			model:      "gemini-3.1-pro-high",
			firstEmail: "first@example.com", firstOrg: "g-1", secondEmail: "second@example.com", secondOrg: "g-2",
			firstLogin: agyLogin("first", "first@example.com"), secondLogin: agyLogin("second", "second@example.com"),
			outsideLogin: agyLogin("outside", ""),
			identify:     agyIdentify, turns: writeSharedPoolAgyBinary,
			want: "The Antigravity login in use names no account",
		},
		{
			name: "an Antigravity login whose email two saved accounts share", provider: string(provider.Agy), settingsKey: "agyBinaryPath",
			model:      "gemini-3.1-pro-high",
			firstEmail: "shared@example.com", firstOrg: "g-1", secondEmail: "shared@example.com", secondOrg: "g-2",
			firstLogin: agyLogin("first", "shared@example.com"), secondLogin: agyLogin("second", "shared@example.com"),
			outsideLogin: agyLogin("outside", "shared@example.com"),
			identify:     agyIdentify, turns: writeSharedPoolAgyBinary,
			want: "The Antigravity login in use is shared@example.com, an email more than one saved account uses",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.rateLimitProbeClientOverride = googleUserinfoNamesNobody
			app.settings = settings.NewService(t.TempDir())
			app.ensureTriageRouter()
			accounts, err := provideraccounts.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			credentials := newTestProviderCredentials(t, t.TempDir())
			for _, saved := range []struct {
				account provideraccounts.Account
				login   []byte
			}{
				{provideraccounts.Account{ID: "second", Provider: tc.provider, Email: tc.secondEmail, OrgID: tc.secondOrg}, tc.secondLogin},
				{provideraccounts.Account{ID: "first", Provider: tc.provider, Email: tc.firstEmail, OrgID: tc.firstOrg}, tc.firstLogin},
			} {
				if err := credentials.WriteAccountCredential(tc.provider, saved.account.ID, saved.login); err != nil {
					t.Fatal(err)
				}
				if _, err := accounts.UpsertAndActivate(saved.account); err != nil {
					t.Fatal(err)
				}
			}
			if err := credentials.WriteNativeCredentialForTest(tc.provider, tc.firstLogin); err != nil {
				t.Fatal(err)
			}
			attachProviderAccountStoresForTest(t, app, accounts, credentials)
			app.providerAccounts.BlessCredentialForTest(tc.provider, tc.firstLogin)

			thread := testThread("unresolved-send")
			thread.Provider = tc.provider
			if tc.model != "" {
				thread.Model = tc.model
			}
			thread.WorkspacePath = t.TempDir()
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "started")
			setBinary := func(binary string) {
				t.Helper()
				if _, err := app.settings.Update(map[string]any{tc.settingsKey: binary}); err != nil {
					t.Fatal(err)
				}
			}

			// The recorded login is not refused.
			setBinary(writeCodexMarkerBinary(t, marker))
			if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
				t.Fatalf("send readiness on the recorded login = %v, want none", err)
			}

			// A login from outside the app replaces the canonical one, and
			// reconciliation leaves it unattributed.
			if err := credentials.WriteNativeCredentialForTest(tc.provider, tc.outsideLogin); err != nil {
				t.Fatal(err)
			}
			setBinary(tc.identify(t))
			if err := app.providerAccounts.ReconcileExternalProviderAccount(tc.provider); err != nil {
				t.Fatalf("reconcile = %v, want the login left unattributed quietly", err)
			}

			setBinary(writeCodexMarkerBinary(t, marker))
			_, err = app.SendMessageWithOptions(context.Background(), thread.ID, "hello", pageSendOptions(uuid.NewString()))
			code, message, _ := errorsx.PublicDetails(err)
			if code != "account_login_unresolved" || !strings.Contains(message, tc.want) ||
				!strings.Contains(message, "Sign in from the app, or switch to another saved account") {
				t.Fatalf("send = %v (code %q), want the unresolved sentence", err, code)
			}
			if started, readErr := os.ReadFile(marker); !os.IsNotExist(readErr) {
				t.Fatalf("provider starts = %q, %v; want none", started, readErr)
			}
			if _, live := app.sessionManager().get(thread.ID); live {
				t.Fatal("a session started for the refused send")
			}
			if items, err := app.store.ListItems(thread.ID); err != nil || len(items) != 0 {
				t.Fatalf("thread rows = %+v, %v; want none", items, err)
			}

			if _, err := app.providerAccounts.SwitchProviderAccount(tc.provider, "second"); err != nil {
				t.Fatalf("switch = %v, want it to go through", err)
			}
			capture := filepath.Join(t.TempDir(), "turns")
			setBinary(tc.turns(t, capture))
			t.Cleanup(func() { _ = app.StopSession(thread.ID) })
			if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "hello", pageSendOptions(uuid.NewString())); err != nil {
				t.Fatalf("send after the switch = %v, want it sent", err)
			}
			waitForCondition(t, "the turn on the switched account", func() bool {
				text, err := os.ReadFile(capture)
				return err == nil && strings.Contains(string(text), "hello")
			})
			if sess, _ := app.sessionManager().get(thread.ID); sess.CredentialAccountID != "second" {
				t.Fatalf("session account = %q, want second", sess.CredentialAccountID)
			}
		})
	}
}

// writeClaudeTurnBinary is a Claude that answers every control request,
// runs every turn to completion and records each user message in capture.
func writeClaudeTurnBinary(t *testing.T, capture string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-turns")
	mockexec.Write(t, path, `#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"type":"control_request"'*)
      reqid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '{"type":"control_response","response":{"subtype":"success","request_id":"%s","response":{}}}\n' "$reqid"
      ;;
    *'"type":"user"'*)
      printf '%s\n' "$line" >> `+shellQuote(capture)+`
      printf '%s\n' '`+mockClaudeInitLine+`'
      printf '{"isReplay":true,%s\n' "${line#\{}"
      printf '%s\n' '{"type":"assistant","message":{"id":"msg-done","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"done"}]}}'
      printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'
      ;;
  esac
done
`)
	return path
}

// With no account saved, a login that names nobody (a Codex API key, a
// Claude token without the profile scope) is no verdict: reconciliation
// has nothing to attribute it to, so the send goes out on it and nothing
// is saved under any name.
func TestASendWithNoSavedAccountGoesOutOnALoginThatNamesNobody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, provider, settingsKey, model string
		login                              string
		binary                             func(t *testing.T, capture string) string
	}{
		{
			name: "a Codex API key", provider: string(provider.Codex), settingsKey: "codexBinaryPath",
			login: `{"OPENAI_API_KEY":"sk-test-key"}`,
			binary: func(t *testing.T, capture string) string {
				return writeCodexTurnBinary(t, `{"type":"apiKey"}`, capture)
			},
		},
		{
			name: "a Claude token without the profile scope", provider: string(provider.Claude), settingsKey: "claudeBinaryPath",
			login: fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"unscoped-rt","expiresAt":%d,"scopes":["user:inference"]}}`,
				claudefake.AccessToken("someone@example.com", "org-1", "unscoped"), time.Now().Add(time.Hour).UnixMilli()),
			binary: writeClaudeTurnBinary,
		},
		{
			name: "an Antigravity login that names no account", provider: string(provider.Agy), settingsKey: "agyBinaryPath",
			model:  "gemini-3.1-pro-high",
			login:  string(agyLogin("nameless", "")),
			binary: writeSharedPoolAgyBinary,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.rateLimitProbeClientOverride = googleUserinfoNamesNobody
			app.settings = settings.NewService(t.TempDir())
			app.ensureTriageRouter()
			accounts, err := provideraccounts.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			credentials := newTestProviderCredentials(t, t.TempDir())
			if err := credentials.WriteNativeCredentialForTest(tc.provider, []byte(tc.login)); err != nil {
				t.Fatal(err)
			}
			attachProviderAccountStoresForTest(t, app, accounts, credentials)
			capture := filepath.Join(t.TempDir(), "turns")
			if _, err := app.settings.Update(map[string]any{tc.settingsKey: tc.binary(t, capture)}); err != nil {
				t.Fatal(err)
			}
			thread := testThread("nameless-send")
			thread.Provider = tc.provider
			if tc.model != "" {
				thread.Model = tc.model
			}
			thread.WorkspacePath = t.TempDir()
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.StopSession(thread.ID) })

			if _, err := app.SendMessageWithOptions(context.Background(), thread.ID, "hello", pageSendOptions(uuid.NewString())); err != nil {
				t.Fatalf("send = %v, want it sent", err)
			}
			waitForCondition(t, "the turn on the nameless login", func() bool {
				text, err := os.ReadFile(capture)
				return err == nil && strings.Contains(string(text), "hello")
			})
			if saved := app.providerAccounts.List(tc.provider, time.Now()); len(saved) != 0 {
				t.Fatalf("saved accounts = %+v, want none", saved)
			}
			slots, err := credentials.AccountCredentialPath(tc.provider, "probe")
			if err != nil {
				t.Fatal(err)
			}
			if entries, err := os.ReadDir(filepath.Dir(filepath.Dir(slots))); err == nil && len(entries) != 0 {
				t.Fatalf("saved slots = %d entries, want none", len(entries))
			} else if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}

			// The account probe is what saves such a login: with nothing
			// selected it adopts it as a new account saved without an email,
			// selects it, and stores the login in its slot.
			if _, err := app.providerAccounts.RunAccountProbe(provideraccountapp.ProbeRequest{
				ProviderName: tc.provider,
				Probe:        func(context.Context) (provider.AccountInfo, error) { return provider.AccountInfo{}, nil },
			}); err != nil {
				t.Fatalf("account probe = %v", err)
			}
			saved := app.providerAccounts.List(tc.provider, time.Now())
			active, hasActive := app.providerAccounts.Active(tc.provider, time.Now())
			if len(saved) != 1 || saved[0].Email != "" || !hasActive || active.ID != saved[0].ID {
				t.Fatalf("after the probe: saved = %+v, active = %+v (%v); want one selected account without an email", saved, active, hasActive)
			}
			if slot, err := credentials.ReadCredential(tc.provider, saved[0].ID, false); err != nil || string(slot) != tc.login {
				t.Fatalf("the adopted account's slot = %s, %v; want the login", slot, err)
			}
		})
	}
}

// A lookup of Google's userinfo that fails, for an Antigravity login that
// carries no email, identified nothing: a send on it is refused with that
// failure, not as a login that names nobody, and no turn starts.
func TestASendOnAnAgyLoginWhoseLookupFailedIsRefusedWithTheFailure(t *testing.T) {
	t.Parallel()
	agyName := string(provider.Agy)
	app := newTestAppWithStore(t)
	app.settings = settings.NewService(t.TempDir())
	app.ensureTriageRouter()
	accounts, err := provideraccounts.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	credentials := newTestProviderCredentials(t, t.TempDir())
	if err := credentials.WriteNativeCredentialForTest(agyName, agyLogin("nameless", "")); err != nil {
		t.Fatal(err)
	}
	attachProviderAccountStoresForTest(t, app, accounts, credentials)
	capture := filepath.Join(t.TempDir(), "turns")
	if _, err := app.settings.Update(map[string]any{"agyBinaryPath": writeSharedPoolAgyBinary(t, capture)}); err != nil {
		t.Fatal(err)
	}
	thread := testThread("lookup-failed-send")
	thread.Provider = agyName
	thread.Model = "gemini-3.1-pro-high"
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.StopSession(thread.ID) })

	_, err = app.SendMessageWithOptions(context.Background(), thread.ID, "hello", pageSendOptions(uuid.NewString()))
	if code, _, _ := errorsx.PublicDetails(err); code == "account_login_unresolved" || err == nil ||
		!strings.Contains(err.Error(), "look up the Google account of the Antigravity login") {
		t.Fatalf("send = %v (code %q), want the failed lookup named", err, code)
	}
	// Reconciliation's identity probe is the only agy run; the message
	// never reaches one.
	if text, readErr := os.ReadFile(capture); strings.Contains(string(text), "hello") || (readErr != nil && !os.IsNotExist(readErr)) {
		t.Fatalf("agy saw %q, %v; want no turn", text, readErr)
	}
	if _, live := app.sessionManager().get(thread.ID); live {
		t.Fatal("a session started for the refused send")
	}
	if saved := app.providerAccounts.List(agyName, time.Now()); len(saved) != 0 {
		t.Fatalf("saved accounts = %+v, want none", saved)
	}
}
