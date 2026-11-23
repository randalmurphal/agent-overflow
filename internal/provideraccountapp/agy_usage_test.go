package provideraccountapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// agyRequest is one request an Antigravity test answered: its URL and the
// access token it carried.
type agyRequest struct{ url, bearer string }

// agyQuota answers Antigravity's HTTP requests and records each one with the
// bearer it carried. Google's userinfo is answered by userinfo, and when that
// is nil with a body that names nobody, so identity comes from the
// credential's id_token_email. A quota request is answered by quota, and when
// that is nil with one bucket.
type agyQuota struct {
	mu       sync.Mutex
	log      []agyRequest
	userinfo func(*http.Request) (*http.Response, error)
	quota    func(*http.Request) (*http.Response, error)
}

func (q *agyQuota) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		q.mu.Lock()
		q.log = append(q.log, agyRequest{url: req.URL.String(), bearer: strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")})
		answer := q.quota
		if req.URL.Host == "www.googleapis.com" {
			answer = q.userinfo
		}
		q.mu.Unlock()
		if answer != nil {
			return answer(req)
		}
		if req.URL.Host == "www.googleapis.com" {
			return agyAnswer(http.StatusOK, `{}`), nil
		}
		return agyAnswer(http.StatusOK, `{"buckets":[{"bucketId":"gemini-argon-5h","window":"5h","remainingFraction":0.8}]}`), nil
	})}
}

func agyAnswer(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body))}
}

func (q *agyQuota) answerUserinfo(fn func(*http.Request) (*http.Response, error)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.userinfo = fn
}

func (q *agyQuota) answerQuota(fn func(*http.Request) (*http.Response, error)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.quota = fn
}

// quotaBearers is the access token of every quota request since the last
// call, in order. Userinfo lookups are not quota requests.
func (q *agyQuota) quotaBearers() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var bearers []string
	for _, request := range q.log {
		if !strings.Contains(request.url, "www.googleapis.com") {
			bearers = append(bearers, request.bearer)
		}
	}
	q.log = nil
	return bearers
}

// wantQuotaBearers fails unless every quota request since the last check
// carried one of want's tokens, in want's order, each at least once.
func (r agyReadingRig) wantQuotaBearers(t *testing.T, want ...string) {
	t.Helper()
	got := r.quota.quotaBearers()
	var distinct []string
	for _, bearer := range got {
		if len(distinct) == 0 || distinct[len(distinct)-1] != bearer {
			distinct = append(distinct, bearer)
		}
	}
	if !slices.Equal(distinct, want) {
		t.Fatalf("quota requests carried %v, want %v", got, want)
	}
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
	quota.quotaBearers()
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
	rig.wantQuotaBearers(t, "ya29.dev-new")
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

	err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), string(provider.Agy), rig.devID)
	code, message, _ := errorsx.PublicDetails(err)
	if code != "account_login_unresolved" || !strings.Contains(message, "The Antigravity login in use names no account") {
		t.Fatalf("reading = %v (code %q), want the unresolved sentence", err, code)
	}
	rig.wantQuotaBearers(t)
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
	rig.wantQuotaBearers(t, "ya29.dev-old")
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

func (r agyReadingRig) recorded(t *testing.T, login []byte) {
	t.Helper()
	if fingerprint, ok := r.manager.CredentialFingerprintForTest(string(provider.Agy)); !ok || fingerprint != sha256.Sum256(login) {
		t.Fatalf("the record is not on %s", login)
	}
}

func (r agyReadingRig) canonicalIs(t *testing.T, login []byte) {
	t.Helper()
	got, err := r.credentials.ReadCredential(string(provider.Agy), "", true)
	if err != nil || !bytes.Equal(got, login) {
		t.Fatalf("canonical = %s, %v; want %s", got, err, login)
	}
}

// refuseBearer answers 401 to every quota request made with token, running
// during the first one, and answers the others with one bucket.
func refuseBearer(token string, during func()) func(*http.Request) (*http.Response, error) {
	var once sync.Once
	return func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "Bearer "+token {
			if during != nil {
				once.Do(during)
			}
			return agyAnswer(http.StatusUnauthorized, `{}`), nil
		}
		return agyAnswer(http.StatusOK, `{"buckets":[{"bucketId":"gemini-argon-5h","window":"5h","remainingFraction":0.8}]}`), nil
	}
}

const agyRetrySentence = "agy credentials changed while refreshing usage; retry"

// A second sign-in to dev's Google account from outside the app, landing
// while dev's quota request is refused, carries dev's email and another
// refresh token. It is not dev's refreshed login: the reading fails with a
// retryable answer, and neither dev's slot nor the record moves to it.
func TestARefusedAgyReadingNeverTakesAnotherLoginOfTheSameEmail(t *testing.T) {
	rig := newAgyReadingRig(t)
	devOld := agyCredential("dev-old", "dev@example.com")
	secondSignIn := agyCredential("devb-1", "dev@example.com")
	var installErr error
	rig.quota.answerQuota(refuseBearer("ya29.dev-old", func() {
		installErr = rig.credentials.WriteNativeCredentialForTest(string(provider.Agy), secondSignIn)
	}))

	err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), string(provider.Agy), rig.devID)
	if installErr != nil {
		t.Fatal(installErr)
	}
	if err == nil || !strings.Contains(err.Error(), agyRetrySentence) {
		t.Fatalf("reading = %v, want the retryable answer", err)
	}
	rig.wantQuotaBearers(t, "ya29.dev-old")
	if got := rig.slot(t, rig.devID); got != string(devOld) {
		t.Fatalf("dev's slot = %s, want dev's login", got)
	}
	rig.recorded(t, devOld)
	rig.canonicalIs(t, secondSignIn)
	if len(*rig.published) != 0 {
		t.Fatalf("published readings = %+v, want none", *rig.published)
	}
}

// agyAccountProbeThatRefreshes is agyAccountProbe whose first run replaces
// the file at canonicalPath with refreshed, as agy's own token refresh does.
func agyAccountProbeThatRefreshes(t *testing.T, canonicalPath string, refreshed []byte) string {
	t.Helper()
	dir := t.TempDir()
	pending := filepath.Join(dir, "refreshed.json")
	if err := os.WriteFile(pending, refreshed, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mock-agy")
	mockexec.Write(t, path, `#!/bin/bash
if [ -f '`+pending+`' ]; then /bin/mv -f '`+pending+`' '`+canonicalPath+`'; fi
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

// Google may rotate the refresh token when agy refreshes an access token
// the quota endpoint refused. The refreshed login then proves nothing about
// whose it is, so that reading fails with the retryable answer and writes
// nothing; the next reading reconciles the canonical login first, finds it
// dev's by the email it carries, and lands on it.
func TestAnAgyRefreshThatRotatedTheRefreshTokenFailsOnceThenLands(t *testing.T) {
	rig := newAgyReadingRig(t)
	agyName := string(provider.Agy)
	devOld := agyCredential("dev-old", "dev@example.com")
	rotated := agyCredential("devr-1", "dev@example.com")
	canonicalPath, err := rig.credentials.ActiveCredentialPath(agyName)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk, err := os.ReadFile(canonicalPath); err != nil || !bytes.Equal(onDisk, devOld) {
		t.Fatalf("canonical file %s = %s, %v; want dev's login there", canonicalPath, onDisk, err)
	}
	binary := agyAccountProbeThatRefreshes(t, canonicalPath, rotated)
	rig.manager.deps.ProviderBinary = func(string) string { return binary }
	rig.quota.answerQuota(refuseBearer("ya29.dev-old", nil))

	err = rig.manager.RefreshProviderAccountUsageContext(context.Background(), agyName, rig.devID)
	if err == nil || !strings.Contains(err.Error(), agyRetrySentence) {
		t.Fatalf("reading across agy's refresh = %v, want the retryable answer", err)
	}
	rig.wantQuotaBearers(t, "ya29.dev-old")
	rig.canonicalIs(t, rotated)
	rig.recorded(t, devOld)
	if got := rig.slot(t, rig.devID); got != string(devOld) {
		t.Fatalf("dev's slot = %s, want it unchanged", got)
	}
	if len(*rig.published) != 0 {
		t.Fatalf("published readings = %+v, want none", *rig.published)
	}

	if err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), agyName, rig.devID); err != nil {
		t.Fatalf("the next reading = %v, want it to land", err)
	}
	rig.wantQuotaBearers(t, "ya29.devr-1")
	if selection := rig.manager.Selection(agyName); selection.AccountID != rig.devID {
		t.Fatalf("selection = %+v, want dev kept", selection)
	}
	rig.recorded(t, rotated)
	if got := rig.slot(t, rig.devID); got != string(rotated) {
		t.Fatalf("dev's slot = %s, want the rotated login", got)
	}
	if len(*rig.published) != 1 || (*rig.published)[0].AccountID != rig.devID {
		t.Fatalf("published readings = %+v, want dev's one", *rig.published)
	}
}

// foreignAgySlotRig selects dev beside carol, whose slot holds bob's login
// (an older build's result), and dave.
func foreignAgySlotRig(t *testing.T) (rig agyReadingRig, foreign []byte) {
	t.Helper()
	rig = newAgyReadingRig(t,
		provideraccounts.Account{ID: "carol", Email: "carol@example.com"},
		provideraccounts.Account{ID: "dave", Email: "dave@example.com"})
	foreign = agyCredential("bob-1", "bob@example.com")
	if err := rig.credentials.WriteAccountCredential(string(provider.Agy), "carol", foreign); err != nil {
		t.Fatal(err)
	}
	return rig, foreign
}

// A saved Antigravity slot whose credential carries another email than its
// account's is never read or activated: its reading is refused before the
// quota endpoint is asked, a switch to it is refused unwritten, auto-switch
// sees an account that needs a sign-in, and removal passes over it.
func TestAnAgySlotHoldingAnotherAccountsLoginIsNeverReadOrActivated(t *testing.T) {
	agyName := string(provider.Agy)
	devOld := agyCredential("dev-old", "dev@example.com")
	wantMismatch := func(t *testing.T, what string, err error) {
		t.Helper()
		code, message, _ := errorsx.PublicDetails(err)
		if code != "account_login_mismatch" || message != "The login saved for carol@example.com is another account's. Sign in to carol@example.com again." {
			t.Fatalf("%s = %v (code %q), want the mismatch sentence", what, err, code)
		}
	}
	untouched := func(t *testing.T, rig agyReadingRig, foreign []byte) {
		t.Helper()
		if selection := rig.manager.Selection(agyName); selection.AccountID != rig.devID {
			t.Fatalf("selection = %+v, want dev kept", selection)
		}
		rig.canonicalIs(t, devOld)
		rig.recorded(t, devOld)
		if got := rig.slot(t, "carol"); got != string(foreign) {
			t.Fatalf("carol's slot = %s, want it as it was", got)
		}
		rig.wantQuotaBearers(t)
	}
	t.Run("reading", func(t *testing.T) {
		rig, foreign := foreignAgySlotRig(t)
		err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), agyName, "carol")
		wantMismatch(t, "reading", err)
		untouched(t, rig, foreign)
		if len(*rig.published) != 0 {
			t.Fatalf("published readings = %+v, want none", *rig.published)
		}
	})
	t.Run("switch", func(t *testing.T) {
		rig, foreign := foreignAgySlotRig(t)
		_, err := rig.manager.SwitchProviderAccount(agyName, "carol")
		wantMismatch(t, "switch", err)
		untouched(t, rig, foreign)
	})
	t.Run("auto-switch", func(t *testing.T) {
		rig, foreign := foreignAgySlotRig(t)
		_, err := rig.manager.AutoSwitchProviderAccount(agyName, "carol")
		if !IsNeedsLogin(err) {
			t.Fatalf("auto-switch = %v, want an account that needs a sign-in", err)
		}
		untouched(t, rig, foreign)
	})
	t.Run("removal", func(t *testing.T) {
		rig, foreign := foreignAgySlotRig(t)
		if err := rig.manager.RemoveProviderAccount(agyName, rig.devID); err != nil {
			t.Fatalf("remove dev: %v", err)
		}
		if selection := rig.manager.Selection(agyName); selection.AccountID != "dave" {
			t.Fatalf("selection = %+v, want dave, the next usable card", selection)
		}
		rig.canonicalIs(t, agyCredential("dave", "dave@example.com"))
		if got := rig.slot(t, "carol"); got != string(foreign) {
			t.Fatalf("carol's slot = %s, want it as it was", got)
		}
	})
}

// A lookup of Google's userinfo that fails, for a login whose credential
// carries no email, identified nothing. Reconciliation fails with that
// failure for a reading and a send alike, and keeps no verdict: once the
// lookup answers, the next reading identifies the login and lands.
func TestAFailedLookupOfANamelessAgyLoginIsAnErrorNotAVerdict(t *testing.T) {
	rig := newAgyReadingRig(t)
	agyName := string(provider.Agy)
	nameless := agyCredential("nameless", "")
	rig.canonical(t, nameless)
	rig.quota.answerUserinfo(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network is unreachable")
	})

	err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), agyName, rig.devID)
	if code := publicCode(err); code == "account_login_unresolved" || err == nil ||
		!strings.Contains(err.Error(), "look up the Google account of the Antigravity login") || !strings.Contains(err.Error(), "network is unreachable") {
		t.Fatalf("reading = %v (code %q), want the failed lookup named", err, code)
	}
	err = rig.manager.ReconcileBeforeSend(agyName)
	if code := publicCode(err); code == "account_login_unresolved" || err == nil || !strings.Contains(err.Error(), "network is unreachable") {
		t.Fatalf("send = %v (code %q), want the failed lookup named", err, code)
	}
	rig.wantQuotaBearers(t)
	if len(*rig.published) != 0 {
		t.Fatalf("published readings = %+v, want none", *rig.published)
	}

	rig.quota.answerUserinfo(func(*http.Request) (*http.Response, error) {
		return agyAnswer(http.StatusOK, `{"email":"dev@example.com"}`), nil
	})
	if err := rig.manager.RefreshProviderAccountUsageContext(context.Background(), agyName, rig.devID); err != nil {
		t.Fatalf("reading once the lookup answers = %v, want it to land", err)
	}
	rig.wantQuotaBearers(t, "ya29.nameless")
	rig.recorded(t, nameless)
	if len(*rig.published) != 1 || (*rig.published)[0].AccountID != rig.devID {
		t.Fatalf("published readings = %+v, want dev's one", *rig.published)
	}
}

// A lookup that never answers ends at its own bound, under the reconcile
// lock, as a failed lookup: the reading answers with it.
func TestAnAgyLookupThatNeverAnswersEndsAtItsBound(t *testing.T) {
	bound := agyIdentifyTimeout
	agyIdentifyTimeout = 100 * time.Millisecond
	t.Cleanup(func() { agyIdentifyTimeout = bound })
	rig := newAgyReadingRig(t)
	agyName := string(provider.Agy)
	rig.canonical(t, agyCredential("nameless", ""))
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	rig.quota.answerUserinfo(func(req *http.Request) (*http.Response, error) {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-released:
			return nil, errors.New("test ended")
		}
	})

	done := make(chan error, 1)
	go func() {
		done <- rig.manager.RefreshProviderAccountUsageContext(context.Background(), agyName, rig.devID)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "look up the Google account of the Antigravity login") {
			t.Fatalf("reading = %v, want the lookup ended at its bound", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lookup ran past its bound")
	}
	rig.wantQuotaBearers(t)
}
