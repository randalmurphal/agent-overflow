package provideraccountapp

import (
	"crypto/sha256"
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
	"agent-overflow/internal/provider/claude/claudefake"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil/mockexec"
)

// claudeEndpoints is the fake Claude API: the profile endpoint names each
// credential by its token (claudefake), the usage endpoint answers the
// identities it holds, and every request is logged with its bearer.
type claudeEndpoints struct {
	usage    *claudefake.Usage
	mu       sync.Mutex
	requests []string
}

func (e *claudeEndpoints) RoundTrip(req *http.Request) (*http.Response, error) {
	bearer := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	kind := "other"
	switch {
	case claudefake.IsProfile(req):
		kind = "profile"
	case claudefake.IsUsage(req):
		kind = "usage"
	}
	e.mu.Lock()
	e.requests = append(e.requests, kind+" "+bearer)
	e.mu.Unlock()
	if kind == "profile" {
		return claudefake.ServeProfile(req)
	}
	if resp, ok, err := e.usage.Serve(req); ok || err != nil {
		return resp, err
	}
	return claudefake.Transport{}.RoundTrip(req)
}

func (e *claudeEndpoints) log() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.requests)
}

// claudeAccessToken is the access token in Claude credential bytes.
func claudeAccessToken(t *testing.T, credential []byte) string {
	t.Helper()
	_, after, ok := strings.Cut(string(credential), `"accessToken":"`)
	token, _, closed := strings.Cut(after, `"`)
	if !ok || !closed {
		t.Fatalf("no access token in %s", credential)
	}
	return token
}

// spawnLog wraps binary so every start of it appends its CODEX_HOME (empty
// for a run in the canonical home) to the returned log file.
func spawnLog(t *testing.T, binary string) (wrapped, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "spawns.log")
	wrapped = mockexec.Write(t, filepath.Join(dir, "logged-"+filepath.Base(binary)), "#!/bin/bash\n"+
		"printf '%s\\n' \"run:${CODEX_HOME}\" >> "+shellQuoteForTest(log)+"\n"+
		"exec "+shellQuoteForTest(binary)+" \"$@\"\n")
	return wrapped, log
}

func readSpawns(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// claudeOutsideLoginRig selects "current" on its own login, recorded the way
// an activation leaves it, beside a saved "other", with the usage endpoint
// answering both logins.
func claudeOutsideLoginRig(t *testing.T) (manager *Manager, store *provideraccounts.Store, credentials *provideraccounts.Credentials, endpoints *claudeEndpoints, spawns string, current []byte) {
	t.Helper()
	manager, store, credentials = newTestManager(t)
	endpoints = &claudeEndpoints{usage: claudefake.NewUsage()}
	for _, email := range []string{"current@example.com", "other@example.com"} {
		if err := endpoints.usage.Set(email, []claudefake.UsageLimit{{Kind: "session", Group: "session", Percent: 34, ResetsAt: "2099-01-01T00:00:00Z"}}); err != nil {
			t.Fatal(err)
		}
	}
	manager.deps.HTTPClient = func() *http.Client { return &http.Client{Transport: endpoints} }
	var binary string
	binary, spawns = spawnLog(t, writeClaudeIdentityProbe(t, staleProbeEmail))
	manager.deps.ProviderBinary = func(string) string { return binary }
	current = claudeLoginAs("current@example.com", "current", time.Now().Add(20*24*time.Hour))
	seedClaudeAccounts(t, manager, store, credentials, current, "other",
		claudeLoginAs("other@example.com", "other-old", time.Now().Add(20*24*time.Hour)))
	return manager, store, credentials, endpoints, spawns, current
}

// With nothing changed outside the app, a selected Claude refresh is one
// usage request on the selected account's bearer: no identity probe, no
// profile request.
func TestASelectedClaudeRefreshWithNothingChangedSendsOnlyItsReading(t *testing.T) {
	manager, _, _, endpoints, spawns, current := claudeOutsideLoginRig(t)
	if err := manager.RefreshProviderAccountUsage(string(provider.Claude), "current"); err != nil {
		t.Fatal(err)
	}
	if got, want := endpoints.log(), []string{"usage " + claudeAccessToken(t, current)}; !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if got := readSpawns(t, spawns); len(got) != 0 {
		t.Fatalf("CLI starts = %v, want none", got)
	}
}

// After an outside `claude login` as the other saved account, a refresh of
// the selected account reconciles first: the selection moves to the login
// the CLI now runs, the other account's slot takes it, and the account the
// refresh asked about is read from its own slot. Its slot keeps its own
// login, and the fingerprint recorded is the outside login's, for the
// account it belongs to.
func TestASelectedClaudeRefreshReconcilesAnOutsideLoginFirst(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, store, credentials, endpoints, _, current := claudeOutsideLoginRig(t)
	outside := claudeLoginAs("other@example.com", "other-new", time.Now().Add(30*24*time.Hour))
	if err := credentials.WriteNativeCredentialForTest(claudeName, outside); err != nil {
		t.Fatal(err)
	}

	if err := manager.RefreshProviderAccountUsage(claudeName, "current"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := activeID(t, store); got != "other" {
		t.Fatalf("active = %q, want the account the CLI now runs", got)
	}
	if got := readCredential(t, credentials, "current"); string(got) != string(current) {
		t.Fatal("the refreshed account's slot no longer holds its own login")
	}
	if got := readCredential(t, credentials, "other"); string(got) != string(outside) {
		t.Fatal("the outside login did not reach its own account's slot")
	}
	if fingerprint, ok := manager.CredentialFingerprintForTest(claudeName); !ok || fingerprint != sha256.Sum256(outside) {
		t.Fatal("the recorded fingerprint is not the outside login now selected")
	}
	if requests := endpoints.log(); !slices.Contains(requests, "usage "+claudeAccessToken(t, current)) ||
		slices.Contains(requests, "usage "+claudeAccessToken(t, outside)) {
		t.Fatalf("requests = %v, want the reading on the refreshed account's own login only", requests)
	}
}

// codexOutsideLoginProbe is a mock `codex app-server`: in the canonical home
// (the identity probe of reconciliation) it answers account/read as the
// outside login; in a temporary home (a usage probe) it answers as whoever
// the credential there is, a login whose refresh token starts with rt-a
// being A's, then answers the usage read.
func codexOutsideLoginProbe(t *testing.T, aEmail, bEmail string) string {
	t.Helper()
	dir := t.TempDir()
	aFrame, bFrame := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	writeAccountReadFrame(t, aFrame, chatgptAccountFrame(aEmail, "team", ""))
	writeAccountReadFrame(t, bFrame, chatgptAccountFrame(bEmail, "team", ""))
	rateLimits := `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"team",` +
		`"primary":{"usedPercent":10,"windowDurationMins":300,"resetsAt":4102444800}}}}`
	return mockexec.Write(t, filepath.Join(dir, "mock-codex"), "#!/bin/bash\n"+
		"if [ -z \"$CODEX_HOME\" ]; then\n"+
		"  read -r _ || true\n  read -r _ || true\n  read -r _ || true\n"+
		"  cat "+shellQuoteForTest(bFrame)+"\n  exit 0\nfi\n"+
		"read -r _ || true\nread -r _ || true\nread -r _ || true\nread -r _ || true\n"+
		"if grep -q '\"refresh_token\":\"rt-a' \"$CODEX_HOME/auth.json\"; then cat "+shellQuoteForTest(aFrame)+
		"; else cat "+shellQuoteForTest(bFrame)+"; fi\n"+
		"printf '%s\\n' '"+rateLimits+"'\n")
}

// codexOutsideLoginRig selects acct-a on its own login, recorded the way an
// activation leaves it, beside a saved acct-b of bEmail on another workspace.
func codexOutsideLoginRig(t *testing.T, bEmail string) (manager *Manager, store *provideraccounts.Store, credentials *provideraccounts.Credentials, spawns string, aLogin, bOld []byte) {
	t.Helper()
	codexName := string(provider.Codex)
	manager, store, credentials = newTestManager(t)
	var binary string
	binary, spawns = spawnLog(t, codexOutsideLoginProbe(t, "same@example.com", bEmail))
	manager.deps.ProviderBinary = func(string) string { return binary }
	aLogin = codexWorkspaceLogin(t, "same@example.com", "ws-a", "rt-a1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	bOld = codexWorkspaceLogin(t, bEmail, "ws-b", "rt-b0", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if _, err := store.UpsertAndActivate(provideraccounts.Account{ID: "acct-a", Provider: codexName, Email: "same@example.com", OrgID: "ws-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(provideraccounts.Account{ID: "acct-b", Provider: codexName, Email: bEmail, OrgID: "ws-b"}); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(codexName, "acct-a", aLogin); err != nil {
		t.Fatal(err)
	}
	if err := credentials.CommitSelectedCredentialForTest(codexName, "acct-a", aLogin, aLogin); err != nil {
		t.Fatal(err)
	}
	if err := credentials.WriteAccountCredential(codexName, "acct-b", bOld); err != nil {
		t.Fatal(err)
	}
	manager.BlessCredentialForTest(codexName, aLogin)
	return manager, store, credentials, spawns, aLogin, bOld
}

// With nothing changed outside the app, a selected Codex refresh starts
// Codex once, for its reading in a temporary home: no identity probe.
func TestASelectedCodexRefreshWithNothingChangedStartsOnlyItsReading(t *testing.T) {
	manager, _, _, spawns, _, _ := codexOutsideLoginRig(t, "b@example.com")
	if err := manager.RefreshProviderAccountUsage(string(provider.Codex), "acct-a"); err != nil {
		t.Fatal(err)
	}
	if got := readSpawns(t, spawns); len(got) != 1 || got[0] == "run:" {
		t.Fatalf("Codex starts = %v, want one, in a temporary home", got)
	}
}

// After an outside `codex login` as the other saved account, whether it
// shares the selected account's email or not, a refresh of the selected
// account reconciles first: the selection moves to it, its slot takes the
// login, and the account the refresh asked about is read from its own slot,
// which keeps its own login.
func TestASelectedCodexRefreshReconcilesAnOutsideLoginFirst(t *testing.T) {
	codexName := string(provider.Codex)
	for _, bEmail := range []string{"b@example.com", "same@example.com"} {
		t.Run(bEmail, func(t *testing.T) {
			manager, store, credentials, spawns, aLogin, _ := codexOutsideLoginRig(t, bEmail)
			outside := codexWorkspaceLogin(t, bEmail, "ws-b", "rt-b1", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
			if err := credentials.WriteNativeCredentialForTest(codexName, outside); err != nil {
				t.Fatal(err)
			}

			if err := manager.RefreshProviderAccountUsage(codexName, "acct-a"); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if active, ok := store.Active(codexName, time.Now()); !ok || active.ID != "acct-b" {
				t.Fatalf("active = %+v, %v; want the account Codex now runs", active, ok)
			}
			if got := mustReadCredential(t, credentials, "acct-a", false); got != string(aLogin) {
				t.Fatal("the refreshed account's slot no longer holds its own login")
			}
			if got := mustReadCredential(t, credentials, "acct-b", false); got != string(outside) {
				t.Fatal("the outside login did not reach its own account's slot")
			}
			if fingerprint, ok := manager.CredentialFingerprintForTest(codexName); !ok || fingerprint != sha256.Sum256(outside) {
				t.Fatal("the recorded fingerprint is not the outside login now selected")
			}
			if got := readSpawns(t, spawns); len(got) != 2 || got[0] != "run:" || got[1] == "run:" {
				t.Fatalf("Codex starts = %v, want the identity probe, then a reading in a temporary home", got)
			}
		})
	}
}

// The commit of a selected reading lands nothing when the probe started from
// a canonical credential AO never recorded for the account: not the slot,
// not the canonical store, not the fingerprint, not the snapshot. That is an
// outside login that reached the canonical store after the reconciliation
// the reading started with.
func TestASelectedCommitRefusesACredentialItNeverRecorded(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		claudeName := string(provider.Claude)
		manager, store, credentials, _, _, current := claudeOutsideLoginRig(t)
		outside := claudeLoginAs("other@example.com", "other-new", time.Now().Add(30*24*time.Hour))
		if err := credentials.WriteNativeCredentialForTest(claudeName, outside); err != nil {
			t.Fatal(err)
		}
		assertCommitRefused(t, manager, store, credentials, claudeName, "current", current, outside, nil)
	})
	t.Run("codex", func(t *testing.T) {
		codexName := string(provider.Codex)
		manager, store, credentials, _, aLogin, _ := codexOutsideLoginRig(t, "b@example.com")
		outside := codexWorkspaceLogin(t, "b@example.com", "ws-b", "rt-b1", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
		rotated := codexWorkspaceLogin(t, "b@example.com", "ws-b", "rt-b2", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
		if err := credentials.WriteNativeCredentialForTest(codexName, outside); err != nil {
			t.Fatal(err)
		}
		assertCommitRefused(t, manager, store, credentials, codexName, "acct-a", aLogin, outside, rotated)
	})
}

func assertCommitRefused(
	t *testing.T,
	manager *Manager,
	store *provideraccounts.Store,
	credentials *provideraccounts.Credentials,
	providerName, accountID string,
	own, outside, rotated []byte,
) {
	t.Helper()
	refresh := providerUsageRefresh{
		providerName:    providerName,
		accountID:       accountID,
		generation:      store.Generation(providerName),
		isSelected:      true,
		probed:          provideraccounts.NewCredentialSnapshot(outside),
		slotAtStart:     provideraccounts.NewCredentialSnapshot(own),
		refreshed:       provideraccounts.NewCredentialSnapshot(rotated),
		readingComplete: true,
	}
	manager.mu.Lock()
	commit, err := manager.commitProviderUsageRefreshLocked(refresh)
	manager.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "changed while refreshing usage") {
		t.Fatalf("commit = %v, want it refused", err)
	}
	if commit.snapshotDescribesAccount {
		t.Fatal("a refused commit let its snapshot describe the account")
	}
	slot, readErr := credentials.ReadCredential(providerName, accountID, false)
	if readErr != nil || string(slot) != string(own) {
		t.Fatalf("slot = %s, %v; want the account's own login", slot, readErr)
	}
	canonical, readErr := credentials.ReadCredential(providerName, "", true)
	if readErr != nil || string(canonical) != string(outside) {
		t.Fatalf("canonical = %s, %v; want the outside login as it was", canonical, readErr)
	}
	if fingerprint, ok := manager.CredentialFingerprintForTest(providerName); !ok || fingerprint != sha256.Sum256(own) {
		t.Fatal("a refused commit recorded another credential as the account's")
	}
}

// pickedWithResets is selectedRig with the selected account's reset list
// read once, so a spend can be chosen from it.
func pickedWithResets(t *testing.T) (resetRig, []byte, *provideraccounts.UsageResets) {
	t.Helper()
	rig, seed := selectedRig(t)
	rig.write(t, "account.json", chatgptAccountFrame("picked@example.com", "team", "")+"\n")
	rig.holdResets(t, twoCredits)
	if err := rig.manager.RefreshProviderAccountUsage(codexName, "picked"); err != nil {
		t.Fatalf("first reading: %v", err)
	}
	resets := rig.resetsOf(t, "picked")
	if resets == nil {
		t.Fatal("the first reading recorded no reset list")
	}
	return rig, seed, resets
}

// An outside `codex login` as another saved account, seen at the start of a
// spend of the selected account's reset, is reconciled first. The selection
// moves to it, so the spend is refused unsent, saying why: nothing is
// recorded and no consume is written.
func TestASpendUnderAnOutsideLoginIsRefusedUnsent(t *testing.T) {
	rig, seed, resets := pickedWithResets(t)
	if _, err := rig.store.Add(provideraccounts.Account{ID: "other", Provider: codexName, Email: "other@example.com", OrgID: "ws-other", SubscriptionType: "team"}); err != nil {
		t.Fatal(err)
	}
	outside := `{"tokens":{"access_token":"other","refresh_token":"other-refresh","account_id":"ws-other"},"last_refresh":"2029-06-01T00:00:00Z"}`
	if err := rig.creds.WriteNativeCredentialForTest(codexName, []byte(outside)); err != nil {
		t.Fatal(err)
	}
	rig.write(t, "account.json", chatgptAccountFrame("other@example.com", "team", "")+"\n")
	rig.answerConsume(t, "reset")

	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "picked", "RateLimitResetCredit_soon", resets.Revision)
	if code := publicCode(err); code != "reset_account_switched" {
		t.Fatalf("spend = %v (code %q), want it refused for the switch", err, code)
	}
	if !strings.Contains(err.Error(), "other@example.com") || !strings.Contains(err.Error(), "No reset was used") {
		t.Fatalf("refusal = %q, want it to name the login and say nothing was used", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("consumes = %v, want none", consumes)
	}
	if pending, ok := rig.store.PendingReset(codexName, "picked"); ok {
		t.Fatalf("a refused spend left a record %+v", pending)
	}
	if active, ok := rig.store.Active(codexName, time.Now()); !ok || active.ID != "other" {
		t.Fatalf("active = %+v, %v; want the outside login selected", active, ok)
	}
	if got := rig.credential(t, "picked", false); got != string(seed) {
		t.Fatalf("picked's slot = %s, want its own login", got)
	}
}

// The reconciliation a spend starts with runs inside the spend's send
// cutoff, which bounds everything before the consume: a spend whose
// reconciliation outlasts it is refused unsent, and says so.
func TestASpendsReconciliationCountsAgainstItsCutoff(t *testing.T) {
	rig, _, resets := pickedWithResets(t)
	binary := rig.manager.deps.ProviderBinary(codexName)
	slow := mockexec.Write(t, filepath.Join(t.TempDir(), "slow-identity-codex"), "#!/bin/bash\n"+
		"[ -z \"$CODEX_HOME\" ] && sleep 1.5\n"+
		"exec "+shellQuoteForTest(binary)+" \"$@\"\n")
	rig.manager.deps.ProviderBinary = func(string) string { return slow }
	rig.manager.resetSendCutoffOverride = time.Second
	// The CLI rotated the selected account's login in place.
	rotatedInPlace := `{"tokens":{"access_token":"picked-2","refresh_token":"picked-refresh-2","account_id":"ws-picked"},"last_refresh":"2029-06-01T00:00:00Z"}`
	if err := rig.creds.WriteNativeCredentialForTest(codexName, []byte(rotatedInPlace)); err != nil {
		t.Fatal(err)
	}
	rig.answerConsume(t, "reset")

	_, err := rig.manager.SpendProviderAccountReset(t.Context(), codexName, "picked", "RateLimitResetCredit_soon", resets.Revision)
	if _, message, public := errorsx.PublicDetails(err); !public || !strings.Contains(message, "No reset was used") {
		t.Fatalf("spend = %v, want it refused unsent in words that say so", err)
	}
	if consumes := rig.consumes(t); len(consumes) != 0 {
		t.Fatalf("consumes = %v, want none past the cutoff", consumes)
	}
	if pending, ok := rig.store.PendingReset(codexName, "picked"); ok {
		t.Fatalf("a late spend left a record %+v", pending)
	}
	if active, ok := rig.store.Active(codexName, time.Now()); !ok || active.ID != "picked" {
		t.Fatalf("active = %+v, %v; want the selected account kept", active, ok)
	}
}
