//go:build !noremote

package attachedbackends

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
	"github.com/google/uuid"
)

// seedS1 stores session s1 for p's computer in place of whatever is stored,
// and returns it with the carrier this manager caches for it.
func seedS1(t *testing.T, m *Manager, dir string, p *introducedPeer) (deviceclient.Session, *carrier) {
	t.Helper()
	s1 := deviceclient.Session{BackendID: p.backendID, Endpoint: p.route.Endpoint, CertFingerprint: p.route.CertFingerprint,
		SessionID: "s1", Credential: "c1", RefreshSecret: "r1", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()}
	seed(t, dir, s1)
	held, err := m.carrier(context.Background(), p.backendID)
	if err != nil {
		t.Fatal(err)
	}
	return s1, held
}

// A person's re-pairing or an introduction whose session write took effect
// and still answered an error (the rename into the sessions directory
// succeeded, its sync did not) is put back: the old session, its agent
// opt-in and its carrier.
func TestARePairingWhoseWriteTookEffectWithAnErrorIsPutBack(t *testing.T) {
	t.Parallel()
	for _, commit := range []string{"person's re-pairing", "introduction"} {
		t.Run(commit, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, dir := newManager(t)
			s1, old := seedS1(t, m, dir, p)
			if err := m.SetAgentAccess(p.backendID, true); err != nil {
				t.Fatal(err)
			}
			// A write-only directory: a file can be renamed into it, and the
			// directory cannot be opened to sync it.
			sessions := filepath.Join(dir, deviceclient.SessionsDirName)
			if err := os.Chmod(sessions, 0o300); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(sessions, 0o700) })
			var answer <-chan introductionAnswer
			if commit == "introduction" {
				answer = p.introduce(context.Background(), m)
			} else {
				answer = p.pair(m)
			}
			got := within(t, "the commit", 15*time.Second, answer)
			if err := os.Chmod(sessions, 0o700); err != nil {
				t.Fatal(err)
			}
			if got.added || got.err == nil || !strings.Contains(got.err.Error(), "sync dir") {
				t.Fatalf("commit = added %t, %v; want it to fail on the directory sync", got.added, got.err)
			}
			unchangedCarrier(t, m, p.backendID, old, s1)
			if access, err := m.AgentAccess(); err != nil || !access[p.backendID] {
				t.Fatalf("agent access = %v, %v; want the old pairing's opt-in back", access, err)
			}
		})
	}
}

// A re-pairing over a session file that could not be read, which therefore
// cannot be put back, fails without giving the old opt-in to the session it
// wrote.
func TestARePairingThatCannotBePutBackKeepsTheGrantOff(t *testing.T) {
	t.Parallel()
	errBuild := errors.New("the carrier could not be built")
	p := newIntroducedPeer(t, nil)
	m, dir := newManager(t)
	seedS1(t, m, dir, p)
	if err := m.SetAgentAccess(p.backendID, true); err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(dir, deviceclient.SessionsDirName, p.backendID+".json")
	if err := os.Chmod(stored, 0o000); err != nil {
		t.Fatal(err)
	}
	m.buildCarrier = func(*deviceclient.Client, string) (*carrier, error) { return nil, errBuild }
	got := within(t, "the re-pairing", 15*time.Second, p.pair(m))
	if got.added || !errors.Is(got.err, errBuild) || !strings.Contains(got.err.Error(), "could not be put back") {
		t.Fatalf("re-pairing = added %t, %v; want %v and the session not put back", got.added, got.err, errBuild)
	}
	if access, err := m.AgentAccess(); err != nil || access[p.backendID] {
		t.Fatalf("agent access = %v, %v; want it off for the session the re-pairing left", access, err)
	}
}

// A forget whose first write, the exclusion, took effect and still answered
// an error is put back: the computer stays paired and not excluded.
func TestAForgetWhoseFirstWriteTookEffectWithAnErrorIsPutBack(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, nil)
	m, dir := newManager(t)
	s1, _ := seedS1(t, m, dir, p)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := within(t, "Forget", 15*time.Second, removeAsync(m, p.backendID))
	if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), "sync dir") {
		t.Fatalf("Remove = %v; want it to fail on the directory sync", err)
	}
	if excluded, err := m.ownDeviceExcluded(p.backendID); err != nil || excluded {
		t.Fatalf("excluded = %t, %v; want the failed forget's exclusion taken back", excluded, err)
	}
	if stored, err := deviceclient.LoadSession(dir, p.backendID); err != nil || stored.SessionID != s1.SessionID {
		t.Fatalf("stored = %+v, %v; want the pairing kept", stored, err)
	}
}

// Sixteen computers are enabled for agent commands, the most allowed, one
// of them the computer being re-paired. The re-pairing turns its opt-in off
// and then fails; meanwhile another manager tries to enable a seventeenth.
// That waits for the re-pairing's transaction, the re-paired computer gets
// its opt-in back, and the seventeenth is refused.
func TestARolledBackOptInIsNotTakenByAnotherManager(t *testing.T) {
	t.Parallel()
	errBuild := errors.New("the carrier could not be built")
	p := newIntroducedPeer(t, nil)
	m, dir := newManager(t)
	seedS1(t, m, dir, p)
	if err := m.SetAgentAccess(p.backendID, true); err != nil {
		t.Fatal(err)
	}
	for range MaxAgentComputers - 1 {
		id := uuid.NewString()
		seed(t, dir, deviceclient.Session{BackendID: id, Endpoint: "https://mini.local:8443", SessionID: "s", Credential: "c"})
		if err := m.SetAgentAccess(id, true); err != nil {
			t.Fatal(err)
		}
	}
	seventeenth := uuid.NewString()
	seed(t, dir, deviceclient.Session{BackendID: seventeenth, Endpoint: "https://mini.local:8443", SessionID: "s", Credential: "c"})

	entered, release := make(chan struct{}), make(chan struct{})
	m.buildCarrier = func(*deviceclient.Client, string) (*carrier, error) {
		close(entered)
		<-release
		return nil, errBuild
	}
	rePairing := p.pair(m)
	within(t, "the re-pairing's carrier build", 10*time.Second, entered)
	enabled := make(chan error, 1)
	go func() { enabled <- otherManager(t, dir).SetAgentAccess(seventeenth, true) }()
	stillWaiting(t, "the other manager's opt-in", enabled)
	close(release)
	if got := within(t, "the re-pairing", 10*time.Second, rePairing); got.added || !errors.Is(got.err, errBuild) {
		t.Fatalf("re-pairing = added %t, %v; want %v", got.added, got.err, errBuild)
	}
	if err := within(t, "the other manager's opt-in", 10*time.Second, enabled); err == nil || !strings.Contains(err.Error(), "at most 16") {
		t.Fatalf("the seventeenth opt-in = %v; want it refused by the limit", err)
	}
	access, err := m.AgentAccess()
	if err != nil {
		t.Fatal(err)
	}
	if !access[p.backendID] || access[seventeenth] || len(access) != MaxAgentComputers {
		t.Fatalf("agent access = %v; want the re-paired computer's opt-in back and the seventeenth refused", access)
	}
}

// An introduction whose caller left before its commit decided stores
// nothing, also when nothing has turned that into a cancellation yet: here
// no callback is registered on the caller's context at all.
func TestACommitAfterItsCallerLeftStoresNothing(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, nil)
	m, dir := newManager(t)
	link, err := deviceclient.DecodeLink(p.raw)
	if err != nil {
		t.Fatal(err)
	}
	caller, leave := context.WithCancel(context.Background())
	e, err := m.beginEnrollment(caller, link.BackendID, introductionByPeer, false)
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := deviceclient.Redeem(context.Background(), dir, link, "this-machine", "linux", m.clientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	leave()
	_, installed, err := m.commitEnrollment(e, link, redeemed)
	m.finishEnrollment(link.BackendID, e)
	if installed != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("commit after the caller left = installed %t, %v; want nothing and the caller's cancellation", installed != nil, err)
	}
	nothingEnrolled(t, m, p.backendID)
}

// A carrier a transition installs starts renewing only once the session
// file is released: a renewal holds its client's mutex while it waits for
// that file's lock.
func TestAnInstalledCarrierRenewsOnlyAfterTheSessionFileIsReleased(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, nil)
	m, dir := newManager(t)
	s1, _ := seedS1(t, m, dir, p)
	client, err := deviceclient.Open(dir, s1, m.clientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	built, err := newCarrier(client, p.backendID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := m.transact(p.backendID, func(tx *computerTx) error {
		tx.install(built)
		if built.onEnded != nil || tx.cached() == built {
			t.Error("the carrier was cached or renewing inside the transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.installed || cachedCarrier(m, p.backendID) != built {
		t.Fatal("the carrier was not installed once the transaction committed")
	}
	if built.onEnded == nil {
		t.Fatal("the installed carrier was not wired once the transaction ended")
	}
}
