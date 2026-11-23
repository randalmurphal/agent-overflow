package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/testutil/mockexec"
)

// writeCodexMarkerBinary is a Codex that records each start in marker and
// answers nothing: a send that reaches a provider process leaves the file.
func writeCodexMarkerBinary(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex-must-not-run")
	mockexec.Write(t, path, "#!/bin/bash\nprintf 'started\\n' >> "+shellQuote(marker)+"\nexit 1\n")
	return path
}

// writeCodexTurnBinary is an app-server signed in as email that runs every
// turn to completion and records each turn/start line in capture.
func writeCodexTurnBinary(t *testing.T, email, capture string) string {
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
    printf '{"jsonrpc":"2.0","id":%%s,"result":{"account":{"type":"chatgpt","email":"%s","planType":"pro"},"requiresOpenaiAuth":true}}\n' "$id"
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
`, email, providerThread, capture, providerThread, providerThread, providerThread)
	path := filepath.Join(t.TempDir(), "codex-turns")
	mockexec.Write(t, path, script)
	return path
}

// While the canonical Codex login is one reconciliation could not attribute
// to one saved account, a send is refused with that verdict's sentence
// before any provider process starts. A switch to another saved account
// still goes through, and the next send runs on it.
func TestASendUnderAnUnresolvedLoginIsRefusedUntilASwitch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// firstEmail and firstOrg are the selected account's; the other
		// saved account is second@example.com in ws-2 unless sharedEmail.
		firstEmail, firstOrg string
		secondEmail          string
		// observed is the outside login's account/read answer.
		observed string
		want     string
	}{
		{
			name:       "a login that names no account",
			firstEmail: "first@example.com", firstOrg: "ws-1",
			secondEmail: "second@example.com",
			observed:    `{"type":"chatgpt","planType":"pro"}`,
			want:        "names no account",
		},
		{
			name:       "a login whose email two saved accounts share",
			firstEmail: "shared@example.com", firstOrg: "ws-1",
			secondEmail: "shared@example.com",
			observed:    `{"type":"chatgpt","email":"shared@example.com","planType":"pro"}`,
			want:        "an email more than one saved account uses",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestAppWithStore(t)
			app.settings = settings.NewService(t.TempDir())
			app.ensureTriageRouter()
			codexName := string(provider.Codex)
			firstLogin := []byte(`{"tokens":{"access_token":"first","refresh_token":"rt-first"}}`)
			secondLogin := []byte(`{"tokens":{"access_token":"second","refresh_token":"rt-second"}}`)
			accounts, err := provideraccounts.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			credentials := newTestProviderCredentials(t, t.TempDir())
			for _, saved := range []struct {
				account provideraccounts.Account
				login   []byte
			}{
				{provideraccounts.Account{ID: "second", Provider: codexName, Email: tc.secondEmail, OrgID: "ws-2"}, secondLogin},
				{provideraccounts.Account{ID: "first", Provider: codexName, Email: tc.firstEmail, OrgID: tc.firstOrg}, firstLogin},
			} {
				if err := credentials.WriteAccountCredential(codexName, saved.account.ID, saved.login); err != nil {
					t.Fatal(err)
				}
				if _, err := accounts.UpsertAndActivate(saved.account); err != nil {
					t.Fatal(err)
				}
			}
			if err := credentials.WriteNativeCredentialForTest(codexName, firstLogin); err != nil {
				t.Fatal(err)
			}
			attachProviderAccountStoresForTest(t, app, accounts, credentials)
			app.providerAccounts.BlessCredentialForTest(codexName, firstLogin)

			thread := testThread("unresolved-send")
			thread.WorkspacePath = t.TempDir()
			if err := app.store.CreateThread(thread); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "started")
			setCodex := func(binary string) {
				t.Helper()
				if _, err := app.settings.Update(map[string]any{"codexBinaryPath": binary}); err != nil {
					t.Fatal(err)
				}
			}

			// The recorded login is not refused.
			setCodex(writeCodexMarkerBinary(t, marker))
			if err := app.ensureProviderAccountReadyForSendLocked(thread); err != nil {
				t.Fatalf("send readiness on the recorded login = %v, want none", err)
			}

			// A login from outside the app replaces the canonical one, and
			// reconciliation leaves it unattributed.
			if err := credentials.WriteNativeCredentialForTest(codexName, []byte(`{"tokens":{"access_token":"outside","refresh_token":"rt-outside"}}`)); err != nil {
				t.Fatal(err)
			}
			setCodex(writeCodexAccountReadProbeBinary(t, tc.observed))
			if err := app.providerAccounts.ReconcileExternalProviderAccount(codexName); err != nil {
				t.Fatalf("reconcile = %v, want the login left unattributed quietly", err)
			}

			setCodex(writeCodexMarkerBinary(t, marker))
			err = app.SendMessage(context.Background(), thread.ID, "hello", nil)
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

			if _, err := app.providerAccounts.SwitchProviderAccount(codexName, "second"); err != nil {
				t.Fatalf("switch = %v, want it to go through", err)
			}
			capture := filepath.Join(t.TempDir(), "turns")
			setCodex(writeCodexTurnBinary(t, tc.secondEmail, capture))
			t.Cleanup(func() { _ = app.StopSession(thread.ID) })
			if err := app.SendMessage(context.Background(), thread.ID, "hello", nil); err != nil {
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
