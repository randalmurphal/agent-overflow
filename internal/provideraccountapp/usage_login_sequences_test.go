package provideraccountapp

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude/claudefake"
	"agent-overflow/internal/provideraccounts"
	"agent-overflow/internal/testutil/mockexec"
)

// The sequences below are the ways one account's saved login came to hold
// another login's: an outside sign-in landing between two steps of a usage
// reading, a slot that already holds another workspace's login, and outside
// logins nothing can attribute.

// hookedSink is recordingAccountSink that runs onAccount at each
// PublishAccount, the moment reconciliation has decided on a login.
type hookedSink struct {
	*recordingAccountSink
	onAccount func()
}

func (s *hookedSink) PublishAccount(providerName string, account provideraccounts.Account, info provider.AccountInfo, generation uint64) {
	s.recordingAccountSink.PublishAccount(providerName, account, info, generation)
	if s.onAccount != nil {
		onAccount := s.onAccount
		s.onAccount = nil
		onAccount()
	}
}

// limitsSink records every reading published to the rings.
type limitsSink struct{ published []provider.RateLimitsSnapshot }

func (s *limitsSink) Publish(snapshot provider.RateLimitsSnapshot) {
	s.published = append(s.published, snapshot)
}
func (s *limitsSink) Forget(string, string) {}

// loginRig is a Codex app-server script whose answers are files a test
// rewrites: canonical-account.json answers account/read in the canonical
// home (reconciliation's identity probe) and home-account.json in a
// temporary home (a usage reading). At the rate-limit read it copies
// rotate.json into its home, as a rotation, and outside.json over the
// canonical credential, as a sign-in from outside the app landing during
// the reading. With slot-path naming a slot's credential file it writes
// slot-new.json there too, or deletes the file when there is none. Every start is logged to starts.log: "canonical", or "home"
// with the refresh token the temporary home was seeded with.
type loginRig struct {
	dir     string
	manager *Manager
	store   *provideraccounts.Store
	creds   *provideraccounts.Credentials
	sink    *hookedSink
	limits  *limitsSink
}

const loginRigRateLimits = `{"jsonrpc":"2.0","id":2,"result":{"rateLimits":{"limitId":"codex","planType":"team",` +
	`"primary":{"usedPercent":10,"windowDurationMins":300,"resetsAt":4102444800}}}}`

func newLoginRig(t *testing.T) loginRig {
	t.Helper()
	manager, store, creds := newTestManager(t)
	rig := loginRig{
		dir: t.TempDir(), manager: manager, store: store, creds: creds,
		sink: &hookedSink{recordingAccountSink: &recordingAccountSink{}}, limits: &limitsSink{},
	}
	manager.deps.Accounts = rig.sink
	manager.deps.RateLimits = rig.limits
	canonicalPath, err := creds.ActiveCredentialPath(codexName)
	if err != nil {
		t.Fatal(err)
	}
	rig.write(t, "canonical-path", canonicalPath)
	rig.write(t, "ratelimits.json", loginRigRateLimits+"\n")
	binary := mockexec.Write(t, filepath.Join(rig.dir, "mock-codex"), "#!/bin/bash\n"+
		"dir="+shellQuoteForTest(rig.dir)+"\n"+
		"if [ -n \"$CODEX_HOME\" ]; then\n"+
		"  printf 'home:%s\\n' \"$(grep -o '\"refresh_token\":\"[^\"]*\"' \"$CODEX_HOME/auth.json\" | head -1 | cut -d'\"' -f4)\" >> \"$dir/starts.log\"\n"+
		"  account=\"$dir/home-account.json\"\n"+
		"else\n"+
		"  printf 'canonical\\n' >> \"$dir/starts.log\"\n"+
		"  account=\"$dir/canonical-account.json\"\n"+
		"fi\n"+
		"while IFS= read -r line; do\n"+
		`case "$line" in`+"\n"+
		`*'"account/read"'*) cat "$account" ;;`+"\n"+
		`*'"account/rateLimits/read"'*)`+"\n"+
		`  [ -f "$dir/rotate.json" ] && cp "$dir/rotate.json" "$CODEX_HOME/auth.json"`+"\n"+
		`  [ -f "$dir/outside.json" ] && cp "$dir/outside.json" "$(cat "$dir/canonical-path")"`+"\n"+
		`  if [ -f "$dir/slot-path" ]; then`+"\n"+
		`    if [ -f "$dir/slot-new.json" ]; then cp "$dir/slot-new.json" "$(cat "$dir/slot-path")"; else rm -f "$(cat "$dir/slot-path")"; fi`+"\n"+
		`  fi`+"\n"+
		`  [ -s "$dir/ratelimits.json" ] || exit 0`+"\n"+
		`  cat "$dir/ratelimits.json" ;;`+"\n"+
		"esac\ndone\n")
	manager.deps.ProviderBinary = func(string) string { return binary }
	return rig
}

func (r loginRig) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// answer sets who account/read names in the canonical home and in a
// temporary home.
func (r loginRig) answer(t *testing.T, canonicalFrame, homeFrame string) {
	t.Helper()
	r.write(t, "canonical-account.json", canonicalFrame+"\n")
	r.write(t, "home-account.json", homeFrame+"\n")
}

func (r loginRig) starts(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, "starts.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func (r loginRig) count(t *testing.T, start string) int {
	t.Helper()
	n := 0
	for _, got := range r.starts(t) {
		if got == start {
			n++
		}
	}
	return n
}

func (r loginRig) credential(t *testing.T, accountID string, active bool) string {
	t.Helper()
	snapshot, err := r.creds.ReadCredentialSnapshot(codexName, accountID, active)
	if err != nil {
		t.Fatal(err)
	}
	return string(snapshot.Bytes())
}

func (r loginRig) save(t *testing.T, account provideraccounts.Account, credential []byte) {
	t.Helper()
	account.Provider = codexName
	if _, err := r.store.Add(account); err != nil {
		t.Fatal(err)
	}
	if credential != nil {
		if err := r.creds.WriteAccountCredential(codexName, account.ID, credential); err != nil {
			t.Fatal(err)
		}
	}
}

// selectA saves acct-a (a@example.com, ws-a) and selects it on its own login,
// recorded the way an activation leaves it.
func (r loginRig) selectA(t *testing.T) []byte {
	t.Helper()
	aLogin := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	r.save(t, provideraccounts.Account{ID: "acct-a", Email: "a@example.com", OrgID: "ws-a"}, aLogin)
	if err := r.creds.WriteNativeCredentialForTest(codexName, aLogin); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Activate(codexName, "acct-a"); err != nil {
		t.Fatal(err)
	}
	r.manager.BlessCredentialForTest(codexName, aLogin)
	return aLogin
}

// Sequence 1. The CLI rotated A's login in place, so a reading of A starts by
// identifying it; as that identification lands, someone signs in to B from
// outside the app. The reading runs on the login reconciliation accepted,
// A's rotation, never on B's, and A's slot holds A's.
func TestAReadingRunsOnTheLoginReconciliationAccepted(t *testing.T) {
	rig := newLoginRig(t)
	rig.selectA(t)
	bLogin := codexWorkspaceLogin(t, "b@example.com", "ws-b", "rt-b1", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-b", Email: "b@example.com", OrgID: "ws-b"}, nil)
	aRotated := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a2", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err := rig.creds.WriteNativeCredentialForTest(codexName, aRotated); err != nil {
		t.Fatal(err)
	}
	aFrame := chatgptAccountFrame("a@example.com", "team", "")
	rig.answer(t, aFrame, aFrame)
	rig.sink.onAccount = func() {
		if err := rig.creds.WriteNativeCredentialForTest(codexName, bLogin); err != nil {
			t.Error(err)
		}
	}

	_ = rig.manager.RefreshProviderAccountUsage(codexName, "acct-a")
	if got := rig.starts(t); !slices.Equal(got, []string{"canonical", "home:rt-a2"}) {
		t.Fatalf("Codex starts = %v, want the identity probe, then a reading seeded with A's rotation", got)
	}
	if got := rig.credential(t, "acct-a", false); got != string(aRotated) {
		t.Fatalf("A's slot = %s, want A's rotation", got)
	}
	if got := rig.credential(t, "", true); got != string(bLogin) {
		t.Fatalf("canonical = %s, want B's outside login left for reconciliation", got)
	}
}

// Sequence 2. A reading of the selected A rotates A's login in its temporary
// home while someone signs in to B from outside the app. A's slot takes A's
// rotation, the canonical credential keeps B's login, the reading says the
// login changed, and nothing is recorded as A's: the next reconciliation
// identifies B. The reading itself ran on A's login and is A's.
func TestASignInDuringASelectedReadingLeavesItAndSavesTheRotation(t *testing.T) {
	rig := newLoginRig(t)
	rig.selectA(t)
	rig.save(t, provideraccounts.Account{ID: "acct-b", Email: "b@example.com", OrgID: "ws-b"}, nil)
	aRotated := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a2", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	bLogin := codexWorkspaceLogin(t, "b@example.com", "ws-b", "rt-b1", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	rig.write(t, "rotate.json", string(aRotated))
	rig.write(t, "outside.json", string(bLogin))
	aFrame := chatgptAccountFrame("a@example.com", "team", "")
	rig.answer(t, aFrame, aFrame)

	err := rig.manager.RefreshProviderAccountUsage(codexName, "acct-a")
	if err == nil || !strings.Contains(err.Error(), "changed while refreshing usage") {
		t.Fatalf("refresh = %v, want the change named", err)
	}
	if got := rig.credential(t, "acct-a", false); got != string(aRotated) {
		t.Fatalf("A's slot = %s, want A's rotation", got)
	}
	if got := rig.credential(t, "", true); got != string(bLogin) {
		t.Fatalf("canonical = %s, want B's outside login", got)
	}
	if _, ok := rig.manager.CredentialFingerprintForTest(codexName); ok {
		t.Fatal("a credential is still recorded as A's after the canonical store moved on")
	}
	// The reading ran on A's recorded login, and Codex named A: it is A's.
	if len(rig.limits.published) != 1 || rig.limits.published[0].AccountID != "acct-a" {
		t.Fatalf("published readings = %+v, want A's one", rig.limits.published)
	}
}

// An inactive account whose slot holds another workspace's login is never
// read for it: Codex does not start, the slot is left as it is, and the card
// says to sign in again.
func TestAnInactiveSlotHoldingAnotherWorkspacesLoginIsNotRead(t *testing.T) {
	rig := newLoginRig(t)
	rig.selectA(t)
	foreign := codexWorkspaceLogin(t, "b@example.com", "ws-b", "rt-b1", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-c", Email: "c@example.com", OrgID: "ws-c"}, foreign)
	rig.answer(t, chatgptAccountFrame("a@example.com", "team", ""), chatgptAccountFrame("b@example.com", "team", ""))

	err := rig.manager.RefreshProviderAccountUsage(codexName, "acct-c")
	if code := publicCode(err); code != "account_login_mismatch" || !strings.Contains(err.Error(), "Sign in to c@example.com again") {
		t.Fatalf("refresh = %v (code %q), want the mismatch named", err, code)
	}
	if got := rig.starts(t); len(got) != 0 {
		t.Fatalf("Codex starts = %v, want none", got)
	}
	if got := rig.credential(t, "acct-c", false); got != string(foreign) {
		t.Fatalf("C's slot = %s, want it as it was", got)
	}
}

// A login saved under two accounts, the old defect's result, whose bytes name
// no workspace: a reading of the wrong account rotates it, and account/read
// names the other. The rotation reaches every slot that held the login, so
// neither is left on a spent token; the reading names the login's owner and
// publishes nothing for the account it was asked about.
func TestARotationOfALoginSavedUnderTwoAccountsReachesBoth(t *testing.T) {
	rig := newLoginRig(t)
	rig.selectA(t)
	shared := []byte(`{"tokens":{"access_token":"owner","refresh_token":"rt-owner1"},"last_refresh":"2029-01-01T00:00:00Z"}`)
	rotated := `{"tokens":{"access_token":"owner2","refresh_token":"rt-owner2"},"last_refresh":"2030-01-01T00:00:00Z"}`
	rig.save(t, provideraccounts.Account{ID: "owner", Email: "owner@example.com", OrgID: "ws-owner"}, shared)
	rig.save(t, provideraccounts.Account{ID: "damaged", Email: "damaged@example.com", OrgID: "ws-damaged"}, shared)
	rig.write(t, "rotate.json", rotated)
	rig.answer(t, chatgptAccountFrame("a@example.com", "team", ""), chatgptAccountFrame("owner@example.com", "team", ""))

	err := rig.manager.RefreshProviderAccountUsage(codexName, "damaged")
	if code := publicCode(err); code != "account_login_mismatch" || !strings.Contains(err.Error(), "owner@example.com") {
		t.Fatalf("refresh = %v (code %q), want the owner's login named", err, code)
	}
	for _, id := range []string{"owner", "damaged"} {
		if got := rig.credential(t, id, false); got != rotated {
			t.Fatalf("%s's slot = %s, want the rotation of the login it held", id, got)
		}
	}
	if len(rig.limits.published) != 0 {
		t.Fatalf("published readings = %d, want none for an account that is not the login's", len(rig.limits.published))
	}
}

// A reading whose rotation no store can take, because the account's slot
// disappeared during it, says so and keeps the rotation rather than deleting
// it: the next boot's sweep places it once the slot holds the login again.
func TestALiveRotationNothingCanTakeIsKeptForTheNextBoot(t *testing.T) {
	rig := newLoginRig(t)
	rig.selectA(t)
	cLogin := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c1", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	cRotated := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c2", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-c", Email: "c@example.com", OrgID: "ws-c"}, cLogin)
	slotPath, err := rig.creds.AccountCredentialPath(codexName, "acct-c")
	if err != nil {
		t.Fatal(err)
	}
	rig.write(t, "rotate.json", string(cRotated))
	rig.write(t, "slot-path", slotPath)
	rig.answer(t, chatgptAccountFrame("a@example.com", "team", ""), chatgptAccountFrame("c@example.com", "team", ""))

	err = rig.manager.RefreshProviderAccountUsage(codexName, "acct-c")
	if code := publicCode(err); code != "account_login_kept" {
		t.Fatalf("refresh = %v (code %q), want the kept rotation named", err, code)
	}
	if len(rig.limits.published) != 0 {
		t.Fatalf("published readings = %d, want none", len(rig.limits.published))
	}

	if err := rig.creds.WriteAccountCredential(codexName, "acct-c", cLogin); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.creds.SweepEphemeralCredentials(time.Now(), map[string]string{codexName: "acct-a"}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := rig.credential(t, "acct-c", false); got != string(cRotated) {
		t.Fatalf("C's slot after the sweep = %s, want the kept rotation", got)
	}
}

// codexAPIKeyLogin is a Codex API-key sign-in: no account, no refresh token.
const codexAPIKeyLogin = `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-outside"}`

const codexAPIKeyFrame = `{"jsonrpc":"2.0","id":3,"result":{"account":{"type":"apiKey"},"requiresOpenaiAuth":true}}`

// An outside sign-in that names no account (an API key) while a named
// account is selected is nobody's for certain. The selection stays, the
// login reaches no slot, the reading does not run, and the sentence says
// what replaces it.
func TestARefreshUnderAnIdentitylessOutsideLoginReadsNothing(t *testing.T) {
	rig := newLoginRig(t)
	aLogin := rig.selectA(t)
	if err := rig.creds.WriteNativeCredentialForTest(codexName, []byte(codexAPIKeyLogin)); err != nil {
		t.Fatal(err)
	}
	rig.answer(t, codexAPIKeyFrame, codexAPIKeyFrame)

	err := rig.manager.RefreshProviderAccountUsage(codexName, "acct-a")
	code, message, _ := errorsx.PublicDetails(err)
	if code != "account_login_unresolved" || !strings.Contains(message, "names no account, such as an API key") ||
		!strings.Contains(message, "Sign in from the app, or switch to another saved account") {
		t.Fatalf("refresh = %v (code %q), want the identity-less login named", err, code)
	}
	if got := rig.starts(t); !slices.Equal(got, []string{"canonical"}) {
		t.Fatalf("Codex starts = %v, want the identity probe only", got)
	}
	if got := rig.credential(t, "acct-a", false); got != string(aLogin) {
		t.Fatalf("A's slot = %s, want A's own login", got)
	}
	if active, ok := rig.store.Active(codexName, time.Now()); !ok || active.ID != "acct-a" {
		t.Fatalf("active = %+v, %v; want the selection kept", active, ok)
	}
}

// A selected account saved without any identity takes a login that names
// none: nothing says it is anyone else's.
func TestASelectedAccountWithoutAnIdentityTakesAnIdentitylessLogin(t *testing.T) {
	rig := newLoginRig(t)
	keyLogin := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-old"}`)
	rig.save(t, provideraccounts.Account{ID: "keyed"}, keyLogin)
	if err := rig.creds.WriteNativeCredentialForTest(codexName, keyLogin); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.Activate(codexName, "keyed"); err != nil {
		t.Fatal(err)
	}
	rig.manager.BlessCredentialForTest(codexName, keyLogin)
	if err := rig.creds.WriteNativeCredentialForTest(codexName, []byte(codexAPIKeyLogin)); err != nil {
		t.Fatal(err)
	}
	rig.answer(t, codexAPIKeyFrame, codexAPIKeyFrame)

	if err := rig.manager.ReconcileExternalProviderAccount(codexName); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := rig.credential(t, "keyed", false); got != codexAPIKeyLogin {
		t.Fatalf("slot = %s, want the new key", got)
	}
	if active, ok := rig.store.Active(codexName, time.Now()); !ok || active.ID != "keyed" {
		t.Fatalf("active = %+v, %v; want the account kept", active, ok)
	}
}

// ambiguousRig selects A beside B, both shared@example.com on two
// workspaces, then signs in outside the app with a login of that email whose
// bytes name no workspace: nothing says which of the two it is.
func ambiguousRig(t *testing.T) (rig loginRig, aLogin []byte) {
	t.Helper()
	rig = newLoginRig(t)
	aLogin = codexWorkspaceLogin(t, "shared@example.com", "ws-a", "rt-a1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-a", Email: "shared@example.com", OrgID: "ws-a"}, aLogin)
	rig.save(t, provideraccounts.Account{ID: "acct-b", Email: "shared@example.com", OrgID: "ws-b"}, nil)
	if err := rig.creds.WriteNativeCredentialForTest(codexName, aLogin); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.Activate(codexName, "acct-a"); err != nil {
		t.Fatal(err)
	}
	rig.manager.BlessCredentialForTest(codexName, aLogin)
	rig.setOutside(t, "rt-x1")
	frame := chatgptAccountFrame("shared@example.com", "team", "")
	rig.answer(t, frame, frame)
	return rig, aLogin
}

// outsideLoginX1 is the no-workspace login of shared@example.com that
// ambiguousRig signs in outside the app.
const outsideLoginX1 = `{"tokens":{"access_token":"x","refresh_token":"rt-x1"},"last_refresh":"2029-01-01T00:00:00Z"}`

// setOutside puts a no-workspace login of shared@example.com in the canonical
// store, as an outside sign-in.
func (r loginRig) setOutside(t *testing.T, refreshToken string) string {
	t.Helper()
	login := `{"tokens":{"access_token":"x","refresh_token":"` + refreshToken + `"},"last_refresh":"2029-01-01T00:00:00Z"}`
	if err := r.creds.WriteNativeCredentialForTest(codexName, []byte(login)); err != nil {
		t.Fatal(err)
	}
	return login
}

func assertUnresolved(t *testing.T, err error) {
	t.Helper()
	if code := publicCode(err); code != "account_login_unresolved" {
		t.Fatalf("refresh = %v (code %q), want the unresolved login named", err, code)
	}
}

// Polls, sends and readings under a login nothing attributes start Codex to
// identify it once; every later one answers from the verdict.
func TestAnUnresolvedLoginIsIdentifiedOnce(t *testing.T) {
	rig, aLogin := ambiguousRig(t)
	for range 2 {
		if err := rig.manager.ReconcileExternalProviderAccount(codexName); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	assertUnresolved(t, rig.manager.RefreshProviderAccountUsage(codexName, "acct-a"))
	if got := rig.starts(t); !slices.Equal(got, []string{"canonical"}) {
		t.Fatalf("Codex starts = %v, want one identity probe and nothing else", got)
	}
	if got := rig.credential(t, "acct-a", false); got != string(aLogin) {
		t.Fatalf("A's slot = %s, want A's own login", got)
	}
}

// The verdict holds only for the bytes and the saved identities it was
// judged against: another canonical credential, a saved account's identity
// changing, or a saved account's removal sends the next reconciliation to
// identify the login again.
func TestAnUnresolvedVerdictIsForgottenWhenItsInputsChange(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, rig loginRig){
		"another canonical credential": func(t *testing.T, rig loginRig) {
			rig.setOutside(t, "rt-x2")
		},
		"a saved identity changed": func(t *testing.T, rig loginRig) {
			if _, err := rig.store.UpdateMetadata(provideraccounts.Account{
				ID: "acct-b", Provider: codexName, Email: "b@example.com", OrgID: "ws-b",
			}); err != nil {
				t.Fatal(err)
			}
		},
		"a saved account removed": func(t *testing.T, rig loginRig) {
			if err := rig.manager.RemoveProviderAccount(codexName, "acct-b"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig, aLogin := ambiguousRig(t)
			if err := rig.manager.ReconcileExternalProviderAccount(codexName); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := rig.count(t, "canonical"); got != 1 {
				t.Fatalf("identity probes before the change = %d, want 1", got)
			}
			change(t, rig)
			if err := rig.manager.ReconcileExternalProviderAccount(codexName); err != nil {
				t.Fatalf("reconcile after the change: %v", err)
			}
			if got := rig.count(t, "canonical"); got != 2 {
				t.Fatalf("identity probes after the change = %d, want 2", got)
			}
			// Identified again, a login of an email only A now has is A's;
			// another unattributable login leaves A's slot alone.
			want := outsideLoginX1
			if name == "another canonical credential" {
				want = string(aLogin)
			}
			if got := rig.credential(t, "acct-a", false); got != want {
				t.Fatalf("A's slot = %s, want %s", got, want)
			}
		})
	}
}

// claudeUsageRejecting is the fake Claude API whose usage endpoint rejects
// one bearer, running onReject first: what a selected reading sees when its
// access token has expired.
type claudeUsageRejecting struct {
	*claudeEndpoints
	bearer   string
	onReject func()
}

func (e *claudeUsageRejecting) RoundTrip(req *http.Request) (*http.Response, error) {
	if claudefake.IsUsage(req) && req.Header.Get("Authorization") == "Bearer "+e.bearer {
		if e.onReject != nil {
			onReject := e.onReject
			e.onReject = nil
			onReject()
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"authentication_error"}}`)),
			Request:    req,
		}, nil
	}
	return e.claudeEndpoints.RoundTrip(req)
}

// The selected Claude account's token is rejected, so the reading refreshes it
// with the CLI in the canonical home. An outside sign-in to another account
// lands before that refresh starts: the CLI is not started on it, the reading
// says the login changed, and neither slot takes the outside login.
func TestASelectedClaudeRefreshDoesNotStartTheCLIOnAnotherLogin(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, credentials, endpoints, spawns, current := claudeOutsideLoginRig(t)
	outside := claudeLoginAs("other@example.com", "other-new", time.Now().Add(30*24*time.Hour))
	rejecting := &claudeUsageRejecting{claudeEndpoints: endpoints, bearer: claudeAccessToken(t, current)}
	rejecting.onReject = func() {
		if err := credentials.WriteNativeCredentialForTest(claudeName, outside); err != nil {
			t.Error(err)
		}
	}
	manager.deps.HTTPClient = func() *http.Client { return &http.Client{Transport: rejecting} }

	err := manager.RefreshProviderAccountUsage(claudeName, "current")
	if err == nil || !strings.Contains(err.Error(), "changed while refreshing usage") {
		t.Fatalf("refresh = %v, want the change named", err)
	}
	if got := readSpawns(t, spawns); len(got) != 0 {
		t.Fatalf("CLI starts = %v, want none", got)
	}
	if got := readCredential(t, credentials, "current"); string(got) != string(current) {
		t.Fatal("the selected account's slot no longer holds its own login")
	}
	if got := readCredential(t, credentials, "other"); string(got) == string(outside) {
		t.Fatal("the outside login reached a slot without being identified")
	}
}

// The CLI's refresh of the selected Claude account leaves a credential the
// profile endpoint cannot be asked about (no user:profile scope). Nothing
// says it is the selected account's login, so it is not saved as that
// account's: the slot keeps what it had and the canonical store keeps the
// new credential for reconciliation.
func TestASelectedClaudeRefreshToALoginThatNamesNobodyIsNotSaved(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, _, credentials, endpoints, _, current := claudeOutsideLoginRig(t)
	unnamed := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"unscoped-rt","expiresAt":%d,"scopes":["user:inference"]}}`,
		claudefake.AccessToken("current@example.com", testClaudeOrgID, "unscoped"), time.Now().Add(time.Hour).UnixMilli())
	canonicalPath, err := credentials.ActiveCredentialPath(claudeName)
	if err != nil {
		t.Fatal(err)
	}
	cli := mockexec.Write(t, filepath.Join(t.TempDir(), "mock-claude"), "#!/bin/bash\n"+
		"read -r _ || true\n"+
		"printf '%s' "+shellQuoteForTest(unnamed)+" > "+shellQuoteForTest(canonicalPath)+"\n"+
		`printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"ao-probe-init","response":{"account":{"email":"current@example.com"}}}}'`+"\n")
	manager.deps.ProviderBinary = func(string) string { return cli }
	rejecting := &claudeUsageRejecting{claudeEndpoints: endpoints, bearer: claudeAccessToken(t, current)}
	manager.deps.HTTPClient = func() *http.Client { return &http.Client{Transport: rejecting} }

	err = manager.RefreshProviderAccountUsage(claudeName, "current")
	code, message, _ := errorsx.PublicDetails(err)
	if code != "account_login_unresolved" || !strings.Contains(message, "names no account, so the app can't confirm it is current@example.com's") {
		t.Fatalf("refresh = %v (code %q), want the login refused as naming nobody", err, code)
	}
	// The bytes are the CLI's refresh of the account's own login: nothing
	// says anyone signed in outside the app.
	if strings.Contains(message, "outside the app") {
		t.Fatalf("refusal = %q, want no claim of an outside sign-in", message)
	}
	if got := readCredential(t, credentials, "current"); string(got) != string(current) {
		t.Fatalf("slot = %s, want the account's own login kept", got)
	}
	live, err := credentials.ReadCredential(claudeName, "", true)
	if err != nil || string(live) != unnamed {
		t.Fatalf("canonical = %s, %v; want the refreshed credential left in place", live, err)
	}
}

// The selected Claude refresh accepts the refreshed credential by the rule
// reconciliation uses: the identity the profile endpoint gives for it must
// resolve to the selected account among the saved ones.
func TestASelectedClaudeRefreshIsAttributedByTheReconciliationRule(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, store, _ := newTestManager(t)
	if _, err := store.UpsertAndActivate(provideraccounts.Account{
		ID: "acct-a", Provider: claudeName, Email: "e@x.com", OrgID: "org-1", OrgName: "Alpha",
	}); err != nil {
		t.Fatal(err)
	}
	selection := Selection{AccountID: "acct-a", Account: provider.AccountInfo{Email: "e@x.com", OrgID: "org-1", OrgName: "Alpha"}}
	for _, tc := range []struct {
		name    string
		info    provider.AccountInfo
		wantErr bool
	}{
		// A credential the profile endpoint was not asked about names nobody;
		// the selected account has an identity, so nothing says it is its.
		{"no identity", provider.AccountInfo{}, true},
		{"same login", provider.AccountInfo{Email: "E@X.com", OrgID: "org-1"}, false},
		{"same email different org id", provider.AccountInfo{Email: "e@x.com", OrgID: "org-2"}, true},
		// A differing display name is a rename, never another login.
		{"same email different org name without ids", provider.AccountInfo{Email: "e@x.com", OrgName: "Beta"}, false},
		{"different email", provider.AccountInfo{Email: "other@x.com"}, true},
		{"rename under the same org id", provider.AccountInfo{Email: "e@x.com", OrgID: "org-1", OrgName: "Renamed"}, false},
	} {
		if err := manager.assertSelectedClaudeIdentity(selection, tc.info); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}

// A selected account saved with a workspace and no email takes a login that
// names an email only while nothing the login carries contradicts it. An
// outside sign-in on another workspace is another login: it is added as its
// own account and selected, and the selected account's slot keeps its own.
func TestALoginOfAnotherWorkspaceNeverLandsInABlankEmailAccountsSlot(t *testing.T) {
	rig := newLoginRig(t)
	aLogin := codexWorkspaceLogin(t, "a@example.com", "ws-a", "rt-a1", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-a", OrgID: "ws-a"}, aLogin)
	if err := rig.creds.WriteNativeCredentialForTest(codexName, aLogin); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.Activate(codexName, "acct-a"); err != nil {
		t.Fatal(err)
	}
	rig.manager.BlessCredentialForTest(codexName, aLogin)
	newLogin := codexWorkspaceLogin(t, "new@example.com", "ws-b", "rt-n1", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err := rig.creds.WriteNativeCredentialForTest(codexName, newLogin); err != nil {
		t.Fatal(err)
	}
	frame := chatgptAccountFrame("new@example.com", "team", "")
	rig.answer(t, frame, frame)

	if err := rig.manager.ReconcileExternalProviderAccount(codexName); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := rig.credential(t, "acct-a", false); got != string(aLogin) {
		t.Fatalf("A's slot = %s, want A's own login", got)
	}
	active, ok := rig.store.Active(codexName, time.Now())
	if !ok || active.ID == "acct-a" || active.Email != "new@example.com" || active.OrgID != "ws-b" {
		t.Fatalf("active = %+v, %v; want the new login as its own account", active, ok)
	}
	if got := rig.credential(t, active.ID, false); got != string(newLogin) {
		t.Fatalf("new account's slot = %s, want the new login", got)
	}
}

// The same rule attributes a live session's or a probe's observation: an
// expected account saved without an email is not named by a login its known
// organization contradicts.
func TestAnObservationContradictingABlankEmailAccountIsNotAttributedToIt(t *testing.T) {
	claudeName := string(provider.Claude)
	manager, store, _ := newTestManager(t)
	if _, err := store.UpsertAndActivate(provideraccounts.Account{ID: "acct-a", Provider: claudeName, OrgID: "org-1"}); err != nil {
		t.Fatal(err)
	}
	// Refused as another login, before any write to the account.
	if _, _, err := manager.ResolveObservedAccount(claudeName, "acct-a", provider.AccountInfo{Email: "x@example.com", OrgID: "org-2"}); err == nil ||
		!strings.Contains(err.Error(), "returned unsaved account x@example.com") {
		t.Fatalf("resolve = %v, want the observation refused as another login", err)
	}
	if id, _, err := manager.ResolveObservedAccount(claudeName, "acct-a", provider.AccountInfo{Email: "x@example.com", OrgID: "org-1"}); err != nil || id != "acct-a" {
		t.Fatalf("observation on the account's organization = %q, %v; want acct-a", id, err)
	}
}

// foreignSlotRig selects A beside C, whose slot holds a login of another
// workspace (the old defect's result), and D, after C in card order.
func foreignSlotRig(t *testing.T) (rig loginRig, aLogin, foreign, dLogin []byte) {
	t.Helper()
	rig = newLoginRig(t)
	aLogin = rig.selectA(t)
	foreign = codexWorkspaceLogin(t, "b@example.com", "ws-b", "rt-b1", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	dLogin = codexWorkspaceLogin(t, "d@example.com", "ws-d", "rt-d1", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-c", Email: "c@example.com", OrgID: "ws-c"}, foreign)
	rig.save(t, provideraccounts.Account{ID: "acct-d", Email: "d@example.com", OrgID: "ws-d"}, dLogin)
	return rig, aLogin, foreign, dLogin
}

// A slot whose credential names another workspace than its account is never
// activated: a switch to it is refused unwritten with the sign-in-again
// sentence, an auto-switch sees an account that needs a sign-in, and a
// removal passes over it for the next usable card.
func TestASlotHoldingAnotherWorkspacesLoginIsNeverActivated(t *testing.T) {
	assertUntouched := func(t *testing.T, rig loginRig, aLogin, foreign []byte) {
		t.Helper()
		if active, ok := rig.store.Active(codexName, time.Now()); !ok || active.ID != "acct-a" {
			t.Fatalf("active = %+v, %v; want acct-a kept", active, ok)
		}
		if got := rig.credential(t, "", true); got != string(aLogin) {
			t.Fatalf("canonical = %s, want A's login", got)
		}
		if got := rig.credential(t, "acct-c", false); got != string(foreign) {
			t.Fatalf("C's slot = %s, want it as it was", got)
		}
		if fingerprint, ok := rig.manager.CredentialFingerprintForTest(codexName); !ok || fingerprint != sha256.Sum256(aLogin) {
			t.Fatal("the record no longer names A's login")
		}
	}
	t.Run("switch", func(t *testing.T) {
		rig, aLogin, foreign, _ := foreignSlotRig(t)
		_, err := rig.manager.SwitchProviderAccount(codexName, "acct-c")
		if code := publicCode(err); code != "account_login_mismatch" || !strings.Contains(err.Error(), "Sign in to c@example.com again") {
			t.Fatalf("switch = %v (code %q), want the mismatch named", err, code)
		}
		assertUntouched(t, rig, aLogin, foreign)
	})
	t.Run("auto-switch", func(t *testing.T) {
		rig, aLogin, foreign, _ := foreignSlotRig(t)
		_, err := rig.manager.AutoSwitchProviderAccount(codexName, "acct-c")
		if !IsNeedsLogin(err) {
			t.Fatalf("auto-switch = %v, want an account that needs a sign-in", err)
		}
		assertUntouched(t, rig, aLogin, foreign)
	})
	t.Run("removal", func(t *testing.T) {
		rig, _, foreign, dLogin := foreignSlotRig(t)
		if err := rig.manager.RemoveProviderAccount(codexName, "acct-a"); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if active, ok := rig.store.Active(codexName, time.Now()); !ok || active.ID != "acct-d" {
			t.Fatalf("active = %+v, %v; want the next usable card", active, ok)
		}
		if got := rig.credential(t, "", true); got != string(dLogin) {
			t.Fatalf("canonical = %s, want D's login", got)
		}
		if got := rig.credential(t, "acct-c", false); got != string(foreign) {
			t.Fatalf("C's slot = %s, want it as it was", got)
		}
	})
}

// A sign-in to an inactive account while its reading runs replaces the slot
// the reading started from. The rotation of the old login is then nobody's
// to keep: the slot moved past it, and the reading's home is deleted.
func TestASignInDuringAnInactiveReadingRetiresItsRotation(t *testing.T) {
	rig := newLoginRig(t)
	rig.selectA(t)
	cLogin := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c1", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	cRotated := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c2", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	cSignIn := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c9", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-c", Email: "c@example.com", OrgID: "ws-c"}, cLogin)
	slotPath, err := rig.creds.AccountCredentialPath(codexName, "acct-c")
	if err != nil {
		t.Fatal(err)
	}
	rig.write(t, "rotate.json", string(cRotated))
	rig.write(t, "slot-path", slotPath)
	rig.write(t, "slot-new.json", string(cSignIn))
	rig.answer(t, chatgptAccountFrame("a@example.com", "team", ""), chatgptAccountFrame("c@example.com", "team", ""))

	if err := rig.manager.RefreshProviderAccountUsage(codexName, "acct-c"); err != nil {
		t.Fatalf("refresh = %v, want the reading, with the old login's rotation retired", err)
	}
	if got := rig.credential(t, "acct-c", false); got != string(cSignIn) {
		t.Fatalf("C's slot = %s, want the sign-in", got)
	}
	results, err := rig.creds.SweepEphemeralCredentials(time.Now(), map[string]string{codexName: "acct-a"})
	if err != nil || len(results) != 0 {
		t.Fatalf("sweep = %+v, %v; want nothing left for it", results, err)
	}
}

// A rotation no store took is kept while another saved slot that may hold
// the token it spent cannot be read: an unread slot is no evidence that
// nothing needs it.
func TestAnUnreadableOtherSlotKeepsARotationNothingTook(t *testing.T) {
	rig := newLoginRig(t)
	rig.selectA(t)
	cLogin := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c1", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	cRotated := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c2", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	cSignIn := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c9", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-c", Email: "c@example.com", OrgID: "ws-c"}, cLogin)
	rig.save(t, provideraccounts.Account{ID: "acct-0", Email: "zero@example.com", OrgID: "ws-0"}, cLogin)
	zeroPath, err := rig.creds.AccountCredentialPath(codexName, "acct-0")
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.Rename(zeroPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, zeroPath); err != nil {
		t.Fatal(err)
	}
	slotPath, err := rig.creds.AccountCredentialPath(codexName, "acct-c")
	if err != nil {
		t.Fatal(err)
	}
	// C's slot moves on during the probe, so C's slot does not take the
	// rotation and only the unread slot could still need it.
	rig.write(t, "rotate.json", string(cRotated))
	rig.write(t, "slot-path", slotPath)
	rig.write(t, "slot-new.json", string(cSignIn))
	rig.answer(t, chatgptAccountFrame("a@example.com", "team", ""), chatgptAccountFrame("c@example.com", "team", ""))

	err = rig.manager.RefreshProviderAccountUsage(codexName, "acct-c")
	if code, _, _ := errorsx.PublicDetails(err); code != "account_login_kept" || !strings.Contains(err.Error(), "acct-0") {
		t.Fatalf("refresh = %v (code %q), want the rotation kept and the unread slot named", err, code)
	}
	if got := rig.credential(t, "acct-c", false); got != string(cSignIn) {
		t.Fatalf("C's slot = %s, want the sign-in", got)
	}
	results, err := rig.creds.SweepEphemeralCredentials(time.Now(), map[string]string{codexName: "acct-a"})
	if err == nil || !strings.Contains(err.Error(), "acct-0") || len(results) != 0 {
		t.Fatalf("sweep = %+v, %v; want the kept rotation's unread slot reported again", results, err)
	}
}

// Another saved slot whose credential cannot be read does not stop a
// reading's rotation from reaching the account's own slot. The reading lands
// and says which slot it could not check, and the rotation is kept for the
// next start, which checks that slot again.
func TestAnUnreadableOtherSlotDoesNotStopTheOwnersRotation(t *testing.T) {
	rig := newLoginRig(t)
	rig.selectA(t)
	cLogin := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c1", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	cRotated := codexWorkspaceLogin(t, "c@example.com", "ws-c", "rt-c2", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	rig.save(t, provideraccounts.Account{ID: "acct-c", Email: "c@example.com", OrgID: "ws-c"}, cLogin)
	// acct-0 sorts first: a slot whose credential is a link, which every
	// credential read refuses.
	rig.save(t, provideraccounts.Account{ID: "acct-0", Email: "zero@example.com", OrgID: "ws-0"}, cLogin)
	zeroPath, err := rig.creds.AccountCredentialPath(codexName, "acct-0")
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.Rename(zeroPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, zeroPath); err != nil {
		t.Fatal(err)
	}
	rig.write(t, "rotate.json", string(cRotated))
	rig.answer(t, chatgptAccountFrame("a@example.com", "team", ""), chatgptAccountFrame("c@example.com", "team", ""))

	err = rig.manager.RefreshProviderAccountUsage(codexName, "acct-c")
	if code, _, _ := errorsx.PublicDetails(err); code != "account_login_unchecked" || !strings.Contains(err.Error(), "acct-0") {
		t.Fatalf("refresh = %v (code %q), want the saved rotation and the unchecked slot named", err, code)
	}
	if got := rig.credential(t, "acct-c", false); got != string(cRotated) {
		t.Fatalf("C's slot = %s, want the rotation", got)
	}
	if len(rig.limits.published) != 1 || rig.limits.published[0].AccountID != "acct-c" {
		t.Fatalf("published readings = %+v, want C's", rig.limits.published)
	}
	results, err := rig.creds.SweepEphemeralCredentials(time.Now(), map[string]string{codexName: "acct-a"})
	if err == nil || len(results) != 0 {
		t.Fatalf("sweep = %+v, %v; want the kept rotation's slot reported again", results, err)
	}
}
