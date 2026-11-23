package provideraccountapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil/mockexec"
)

// agyAccountProbe is a mock `agy acp` that answers the account probe's
// initialize and session/new. The probe names no account: an Antigravity
// login's identity is its credential's, read over HTTP or from the
// credential's own id_token_email.
func agyAccountProbe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mock-agy")
	mockexec.Write(t, path, `#!/bin/bash
while IFS= read -r line; do
  id=$(/bin/echo "$line" | /usr/bin/grep -o '"id":[0-9]*' | /usr/bin/head -1 | /usr/bin/grep -o '[0-9]*')
  if [ -z "$id" ]; then continue; fi
  if /bin/echo "$line" | /usr/bin/grep -q '"method":"initialize"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentInfo":{"name":"antigravity-mock","version":"1.5.13"},"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"close":{}}}}}\n' "$id"
  elif /bin/echo "$line" | /usr/bin/grep -q '"method":"session/new"'; then
    printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"agy-probe"}}\n' "$id"
  else
    printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
  fi
done
`)
	return path
}

// agyQuota answers every Antigravity request as the quota endpoint does and
// counts them; Google's userinfo gets the same body, which names nobody, so
// identity comes from the credential's id_token_email.
type agyQuota struct{ requests atomic.Int32 }

func (q *agyQuota) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		q.requests.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(bytes.NewBufferString(`{"buckets":[{"bucketId":"gemini-argon-5h","window":"5h","remainingFraction":0.8}]}`))}, nil
	})}
}

// agyReadingRig is one manager with dev@example.com saved and selected on a
// recorded Antigravity login, a mock agy for reconciliation's probe, and a
// quota endpoint that always answers.
type agyReadingRig struct {
	manager     *Manager
	credentials *provideraccounts.Credentials
	quota       *agyQuota
	devID       string
	published   *[]provider.RateLimitsSnapshot
}

func newAgyReadingRig(t *testing.T, saved ...provideraccounts.Account) agyReadingRig {
	t.Helper()
	manager, store, credentials := newTestManager(t)
	agyName := string(provider.Agy)
	binary := agyAccountProbe(t)
	manager.deps.ProviderBinary = func(string) string { return binary }
	quota := &agyQuota{}
	manager.deps.HTTPClient = quota.client
	limits := &limitsSink{}
	manager.deps.RateLimits = limits
	for _, account := range saved {
		account.Provider = agyName
		if _, err := store.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := credentials.WriteAccountCredential(agyName, account.ID, agyCredential(account.ID, account.Email)); err != nil {
			t.Fatal(err)
		}
	}
	dev := agyCredential("dev-old", "dev@example.com")
	if err := credentials.WriteNativeCredentialForTest(agyName, dev); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RunAccountProbe(ProbeRequest{ProviderName: agyName, Context: context.Background(),
		Probe: func(context.Context) (provider.AccountInfo, error) {
			return provider.AccountInfo{Email: "dev@example.com"}, nil
		}}); err != nil {
		t.Fatalf("seed dev: %v", err)
	}
	selection := manager.Selection(agyName)
	if selection.AccountID == "" || selection.Account.Email != "dev@example.com" {
		t.Fatalf("selection = %+v, want dev", selection)
	}
	if recorded, ok := manager.CredentialFingerprintForTest(agyName); !ok || recorded != sha256.Sum256(dev) {
		t.Fatal("dev's canonical login is not recorded")
	}
	return agyReadingRig{manager: manager, credentials: credentials, quota: quota, devID: selection.AccountID, published: &limits.published}
}

// agyCredential is an Antigravity OAuth credential whose access token is
// token and whose refresh token stays the same for one account's logins.
func agyCredential(token, email string) []byte {
	refresh := strings.SplitN(token, "-", 2)[0]
	if email == "" {
		return []byte(`{"access_token":"ya29.` + token + `","refresh_token":"1//` + refresh + `"}`)
	}
	return []byte(`{"access_token":"ya29.` + token + `","refresh_token":"1//` + refresh + `","id_token_email":"` + email + `"}`)
}

func (r agyReadingRig) canonical(t *testing.T, login []byte) {
	t.Helper()
	if err := r.credentials.WriteNativeCredentialForTest(string(provider.Agy), login); err != nil {
		t.Fatal(err)
	}
}

func (r agyReadingRig) slot(t *testing.T, accountID string) string {
	t.Helper()
	got, err := r.credentials.ReadCredential(string(provider.Agy), accountID, false)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// The agy CLI refreshes its access token in the canonical home and keeps the
// refresh token. A manual reading of the selected account afterwards lands,
// with the slot and the record on the refreshed login.
func TestASelectedAgyReadingAfterTheCLIRefreshedItsTokenLands(t *testing.T) {
	rig := newAgyReadingRig(t)
	refreshed := agyCredential("dev-new", "dev@example.com")
	rig.canonical(t, refreshed)

	if err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), string(provider.Agy), rig.devID); err != nil {
		t.Fatalf("reading after the CLI's refresh = %v, want it to land", err)
	}
	if got := rig.slot(t, rig.devID); got != string(refreshed) {
		t.Fatalf("dev's slot = %s, want the refreshed login", got)
	}
	if recorded, _ := rig.manager.CredentialFingerprintForTest(string(provider.Agy)); recorded != sha256.Sum256(refreshed) {
		t.Fatal("the record is not on the refreshed login")
	}
	if len(*rig.published) != 1 || (*rig.published)[0].AccountID != rig.devID {
		t.Fatalf("published readings = %+v, want dev's", *rig.published)
	}
}

// An Antigravity login from outside the app that names nobody leaves the
// reading of the selected account with the unresolved sentence, and the
// quota endpoint is never asked.
func TestASelectedAgyReadingUnderAnUnresolvedLoginAnswersWithItsSentence(t *testing.T) {
	rig := newAgyReadingRig(t)
	slotBefore := rig.slot(t, rig.devID)
	rig.canonical(t, agyCredential("nameless", ""))
	requestsBefore := rig.quota.requests.Load()

	err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), string(provider.Agy), rig.devID)
	code, message, _ := errorsx.PublicDetails(err)
	if code != "account_login_unresolved" || !strings.Contains(message, "The Antigravity login in use names no account") {
		t.Fatalf("reading = %v (code %q), want the unresolved sentence", err, code)
	}
	// Reconciliation's identification asks Google's userinfo once; the
	// quota endpoint is never asked.
	if asked := rig.quota.requests.Load() - requestsBefore; asked > 1 {
		t.Fatalf("requests during the refused reading = %d, want only the identification", asked)
	}
	if got := rig.slot(t, rig.devID); got != slotBefore {
		t.Fatalf("dev's slot = %s, want it unchanged", got)
	}
	if len(*rig.published) != 0 {
		t.Fatalf("published readings = %+v, want none", *rig.published)
	}
}

// A reading whose reconciliation moved the selection to another saved
// account reads dev from its own slot, as an inactive account, and writes
// nothing to the canonical store or the record for dev.
func TestAnAgyReadingWhoseReconciliationMovedTheSelectionReadsTheSlot(t *testing.T) {
	rig := newAgyReadingRig(t, provideraccounts.Account{ID: "bob", Email: "bob@example.com"})
	agyName := string(provider.Agy)
	devSlot := rig.slot(t, rig.devID)
	bobOutside := agyCredential("bob-outside", "bob@example.com")
	rig.canonical(t, bobOutside)

	if err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), agyName, rig.devID); err != nil {
		t.Fatalf("reading = %v, want dev read from its slot", err)
	}
	if selection := rig.manager.Selection(agyName); selection.AccountID != "bob" {
		t.Fatalf("selection = %+v, want bob, whose login is in the canonical store", selection)
	}
	if got := rig.slot(t, rig.devID); got != devSlot {
		t.Fatalf("dev's slot = %s, want it unchanged", got)
	}
	canonical, err := rig.credentials.ReadCredential(agyName, "", true)
	if err != nil || !bytes.Equal(canonical, bobOutside) {
		t.Fatalf("canonical = %s, %v; want bob's outside login", canonical, err)
	}
	if recorded, _ := rig.manager.CredentialFingerprintForTest(agyName); recorded != sha256.Sum256(bobOutside) {
		t.Fatal("the record is not bob's login")
	}
	if len(*rig.published) != 1 || (*rig.published)[0].AccountID != rig.devID {
		t.Fatalf("published readings = %+v, want dev's", *rig.published)
	}
}
