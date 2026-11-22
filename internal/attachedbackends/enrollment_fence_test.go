//go:build !noremote

package attachedbackends

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/atomicfile"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/filelock"
)

// otherManager is a second manager over dir, as the frontend-only desktop
// runs beside the host's.
func otherManager(t *testing.T, dir string) *Manager {
	t.Helper()
	other, err := New(dir, "this-machine", "linux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	return other
}

// holdLockFile takes the OS lock named name in dir's profile locks, as the
// manager or deviceclient holding it would, and returns its release.
func holdLockFile(t *testing.T, dir, name string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := os.MkdirAll(filepath.Join(dir, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := filelock.Acquire(ctx, filepath.Join(dir, "locks", name+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if err := filelock.Release(held); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(release)
	return release
}

// holdSessionFile holds id's session file lock, the computer's transaction
// fence across managers.
func holdSessionFile(t *testing.T, dir, id string) func() {
	t.Helper()
	return holdLockFile(t, dir, id+".json")
}

// waitInsideTransaction waits until something holds id's session file lock,
// which in these tests is the manager whose transition was started and is
// now stopped inside its transaction. It fails if that transition returns
// first.
func waitInsideTransaction[T any](t *testing.T, dir, id string, done <-chan T) {
	t.Helper()
	path := filepath.Join(dir, "locks", id+".json.lock")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		select {
		case got := <-done:
			t.Fatalf("the transition returned %+v before it was stopped inside its transaction", got)
		default:
		}
		if probe, err := filelock.Open(path); err == nil {
			free, err := filelock.TryLock(probe)
			if free {
				err = errors.Join(err, filelock.Release(probe))
			} else {
				err = errors.Join(err, probe.Close())
			}
			if err != nil {
				t.Fatal(err)
			}
			if !free {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the transition never entered its transaction")
		}
	}
}

// pairingState is a computer's durable local pairing state, as the
// transitions model it: which session is stored ("" none, "s1" the seeded
// one, "new" one a transition saved), whether it is an own-device session,
// and its agent opt-in and own-device exclusion.
type pairingState struct {
	session   string
	ownDevice bool
	access    bool
	excluded  bool
}

func (s pairingState) String() string {
	return fmt.Sprintf("session=%q own=%t access=%t excluded=%t", s.session, s.ownDevice, s.access, s.excluded)
}

// pairingOp is one transition as two managers run it, with its serial
// effect and the shared files its transaction writes in a given state.
type pairingOp struct {
	name  string
	run   func(m *Manager, p *introducedPeer) error
	apply func(pairingState) pairingState
	gates func(pairingState) []string
}

var pairingOps = []pairingOp{
	{
		name: "person's pairing",
		run: func(m *Manager, p *introducedPeer) error {
			_, err := m.Add(context.Background(), p.personRaw)
			return err
		},
		apply: func(pairingState) pairingState { return pairingState{session: "new"} },
		gates: func(s pairingState) []string {
			if s.excluded {
				return []string{agentAccessLock, ownExclusionsLock}
			}
			return []string{agentAccessLock}
		},
	},
	{
		name: "introduction",
		run: func(m *Manager, p *introducedPeer) error {
			_, err := m.AcceptOwnDeviceIntroduction(context.Background(), p.raw, []computerroute.Route{p.route})
			return err
		},
		apply: func(s pairingState) pairingState {
			if s.excluded || (s.session != "" && s.ownDevice) {
				return s
			}
			return pairingState{session: "new", ownDevice: true, access: s.access, excluded: s.excluded}
		},
		gates: func(pairingState) []string { return nil },
	},
	{
		name: "forget",
		run:  func(m *Manager, p *introducedPeer) error { return m.Remove(p.backendID) },
		apply: func(pairingState) pairingState {
			return pairingState{excluded: true}
		},
		gates: func(pairingState) []string { return []string{ownExclusionsLock, agentAccessLock} },
	},
	{
		name: "tombstone",
		run: func(m *Manager, p *introducedPeer) error {
			_, err := m.retireOwnProfile(p.backendID, "s1")
			return err
		},
		apply: func(s pairingState) pairingState {
			if s.session == "s1" && s.ownDevice {
				return pairingState{excluded: s.excluded}
			}
			return s
		},
		gates: func(s pairingState) []string {
			if s.session == "s1" && s.ownDevice {
				return []string{agentAccessLock}
			}
			return nil
		},
	},
	{
		name: "agent access on",
		run: func(m *Manager, p *introducedPeer) error {
			if err := m.SetAgentAccess(p.backendID, true); err != nil && !errors.Is(err, deviceclient.ErrNoSession) {
				return err
			}
			return nil
		},
		apply: func(s pairingState) pairingState {
			if s.session != "" {
				s.access = true
			}
			return s
		},
		gates: func(s pairingState) []string {
			if s.session != "" {
				return []string{agentAccessLock}
			}
			return nil
		},
	},
}

var initialPairingStates = []struct {
	name  string
	state pairingState
}{
	{"ordinary pairing", pairingState{session: "s1", access: true}},
	{"own-device pairing", pairingState{session: "s1", ownDevice: true, access: true}},
	{"forgotten", pairingState{excluded: true}},
}

// seedPairingState stores s for p's computer.
func seedPairingState(t *testing.T, m *Manager, dir string, p *introducedPeer, s pairingState) {
	t.Helper()
	if s.session != "" {
		seed(t, dir, deviceclient.Session{OwnDevice: s.ownDevice, BackendID: p.backendID, Endpoint: p.route.Endpoint, CertFingerprint: p.route.CertFingerprint,
			SessionID: s.session, Credential: "c", RefreshSecret: "r", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()})
	}
	if s.access {
		if err := m.SetAgentAccess(p.backendID, true); err != nil {
			t.Fatal(err)
		}
	}
	if s.excluded {
		if err := atomicfile.WriteJSON(filepath.Join(dir, ownExclusionsFile), map[string]bool{p.backendID: true}); err != nil {
			t.Fatal(err)
		}
	}
}

// readPairingState reads p's computer's state back.
func readPairingState(t *testing.T, m *Manager, p *introducedPeer) pairingState {
	t.Helper()
	var s pairingState
	stored, err := deviceclient.LoadSession(m.dir, p.backendID)
	switch {
	case err == nil && stored.SessionID == "s1":
		s.session, s.ownDevice = "s1", stored.OwnDevice
	case err == nil:
		s.session, s.ownDevice = "new", stored.OwnDevice
	case !errors.Is(err, deviceclient.ErrNoSession):
		t.Fatal(err)
	}
	access, err := m.AgentAccess()
	if err != nil {
		t.Fatal(err)
	}
	excluded, err := m.ownDeviceExcluded(p.backendID)
	if err != nil {
		t.Fatal(err)
	}
	s.access, s.excluded = access[p.backendID], excluded
	return s
}

func runPairingOp(op pairingOp, m *Manager, p *introducedPeer) <-chan error {
	done := make(chan error, 1)
	go func() { done <- op.run(m, p) }()
	return done
}

// Two managers over one profile directory run every pair of transitions on
// one computer, from each starting state: stopped inside the first one's
// transaction at each shared-file write, and in either order. The end state
// is always one of the two serial results, and a transition the first one
// is inside holds the second one off until it has finished. A tombstone and
// a person's re-pairing never leave the new pairing with the old grant.
func TestTwoManagersTransitionsEndInASerialResult(t *testing.T) {
	t.Parallel()
	for _, initial := range initialPairingStates {
		for _, first := range pairingOps {
			for _, second := range pairingOps {
				gates := append([]string{""}, first.gates(initial.state)...)
				for _, gate := range gates {
					stop := "either order"
					if gate != "" {
						stop = "first stopped at " + gate
					}
					t.Run(fmt.Sprintf("%s/%s then %s/%s", initial.name, first.name, second.name, stop), func(t *testing.T) {
						t.Parallel()
						p := newIntroducedPeer(t, nil)
						a, dir := newManager(t)
						b := otherManager(t, dir)
						seedPairingState(t, a, dir, p, initial.state)
						firstThenSecond := second.apply(first.apply(initial.state))
						secondThenFirst := first.apply(second.apply(initial.state))

						var release func()
						if gate == "" {
							release = holdSessionFile(t, dir, p.backendID)
						} else {
							release = holdLockFile(t, dir, gate)
						}
						firstDone := runPairingOp(first, a, p)
						if gate != "" {
							waitInsideTransaction(t, dir, p.backendID, firstDone)
						}
						secondDone := runPairingOp(second, b, p)
						// An introduction refused by its check before the
						// redemption returns without a transaction.
						if gate != "" && second.name != "introduction" {
							stillWaiting(t, "the second manager's "+second.name, secondDone)
						}
						release()
						if err := within(t, "the first transition", 10*time.Second, firstDone); err != nil {
							t.Fatalf("%s: %v", first.name, err)
						}
						if err := within(t, "the second transition", 10*time.Second, secondDone); err != nil {
							t.Fatalf("%s: %v", second.name, err)
						}
						got := readPairingState(t, a, p)
						switch {
						case gate != "" && second.name != "introduction":
							if got != firstThenSecond {
								t.Fatalf("end state %s; want %s, the first then the second", got, firstThenSecond)
							}
						case got != firstThenSecond && got != secondThenFirst:
							t.Fatalf("end state %s; want %s or %s", got, firstThenSecond, secondThenFirst)
						}
					})
				}
			}
		}
	}
}

// Another manager saves an own-device pairing, or forgets the computer,
// after this manager's introduction was checked and redeemed and before its
// transaction. The transaction decides on what is stored then and keeps it.
func TestAnotherManagersWriteBetweenTheCheckAndTheSaveIsKept(t *testing.T) {
	t.Parallel()
	for _, write := range []string{"forget", "own-device pairing"} {
		t.Run(write, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, dir := newManager(t)
			release := holdSessionFile(t, dir, p.backendID)
			answer := p.introduce(context.Background(), m)
			// Checked and redeemed: the transaction waits on the lock.
			for deadline := time.Now().Add(10 * time.Second); p.redemptions.Load() == 0; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the introduction never redeemed")
				}
			}
			theirs := deviceclient.Session{OwnDevice: true, BackendID: p.backendID, Endpoint: p.route.Endpoint, CertFingerprint: p.route.CertFingerprint,
				SessionID: "other-manager", Credential: "c", RefreshSecret: "r", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()}
			// What the other manager's transition writes while it holds the
			// session file, held here.
			if write == "forget" {
				if err := atomicfile.WriteJSON(filepath.Join(dir, ownExclusionsFile), map[string]bool{p.backendID: true}); err != nil {
					t.Fatal(err)
				}
			} else if err := atomicfile.WriteJSON(filepath.Join(dir, deviceclient.SessionsDirName, p.backendID+".json"), theirs); err != nil {
				t.Fatal(err)
			}
			release()
			if got := within(t, "the introduction", 5*time.Second, answer); got.added || got.err != nil {
				t.Fatalf("introduction overtaken by the other manager = added %t, %v; want not added with no error", got.added, got.err)
			}
			saved, err := deviceclient.LoadSession(dir, p.backendID)
			if write == "forget" {
				if !errors.Is(err, deviceclient.ErrNoSession) {
					t.Fatalf("forgotten computer after the introduction = %+v, %v; want nothing stored", saved, err)
				}
				return
			}
			if err != nil || saved.SessionID != theirs.SessionID {
				t.Fatalf("stored after the introduction = %+v, %v; want the other manager's pairing", saved, err)
			}
		})
	}
}

// Forget drops whatever pairing is stored, also one another manager saved
// after this manager's carrier was built from the older one.
func TestForgetDropsAPairingAnotherManagerSaved(t *testing.T) {
	t.Parallel()
	m, dir := newManager(t)
	const id = "0f6f8d3e-6f8b-4c39-9d5e-3a8f8f3f1c11"
	older := deviceclient.Session{BackendID: id, Endpoint: "https://mini.local:8443", SessionID: "older", Credential: "c1"}
	seed(t, dir, older)
	held, err := m.carrier(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	newer := older
	newer.SessionID, newer.Credential = "newer", "c2"
	seed(t, dir, newer)
	if err := m.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := deviceclient.LoadSession(dir, id); !errors.Is(err, deviceclient.ErrNoSession) {
		t.Fatalf("stored after Forget = %v; want nothing", err)
	}
	if !held.client.Retired() {
		t.Fatal("the carrier built from the older pairing was not retired")
	}
}

// unchangedCarrier fails unless id's cached carrier is still want, working,
// and the stored session is still before.
func unchangedCarrier(t *testing.T, m *Manager, id string, want *carrier, before deviceclient.Session) {
	t.Helper()
	m.mu.Lock()
	cached := m.carriers[id]
	m.mu.Unlock()
	if cached != want || want.client.Retired() {
		t.Fatalf("cached carrier is the one before %t, retired %t; want it kept and working", cached == want, want.client.Retired())
	}
	if verdict := want.peers.verdict(); verdict != nil {
		t.Fatalf("the kept carrier's connections were closed: %v", verdict)
	}
	if after, err := deviceclient.LoadSession(m.dir, id); err != nil || after.SessionID != before.SessionID || after.Credential != before.Credential {
		t.Fatalf("stored session = %+v, %v; want %+v", after, err, before)
	}
}

// An introduction refused in its transaction leaves the carrier this
// manager cached for another manager's own-device pairing as it was.
func TestARefusedIntroductionLeavesTheCarrierCachedForAnotherManagersPairing(t *testing.T) {
	t.Parallel()
	p := newIntroducedPeer(t, func(p *introducedPeer) { p.holdPair = make(chan struct{}) })
	m, dir := newManager(t)
	answer := p.introduce(context.Background(), m)
	within(t, "the redemption reaching the other computer", 5*time.Second, p.pairArrived)
	theirs := deviceclient.Session{OwnDevice: true, BackendID: p.backendID, Endpoint: p.route.Endpoint, CertFingerprint: p.route.CertFingerprint,
		SessionID: "other-manager", Credential: "c", RefreshSecret: "r", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli()}
	seed(t, dir, theirs)
	// A carried request through this manager caches the other's pairing.
	lookup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cached, err := m.carrier(lookup, p.backendID)
	if err != nil {
		t.Fatal(err)
	}
	closeOnce(p.holdPair)
	if got := within(t, "the introduction", 5*time.Second, answer); got.added || got.err != nil {
		t.Fatalf("introduction = added %t, %v; want not added with no error", got.added, got.err)
	}
	unchangedCarrier(t, m, p.backendID, cached, theirs)
}

// A person's re-pairing whose save fails leaves the old pairing, its
// carrier and its connections as they were: on the session file's lock,
// on the write, and on reading the agent opt-ins.
func TestAFailedRePairingLeavesTheOldPairingWorking(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"lock", "write", "agent access"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, dir := newManager(t)
			if got := within(t, "the first pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
				t.Fatalf("first pairing = added %t, %v", got.added, got.err)
			}
			old := enrolled(t, m, p.backendID)
			// The re-pairing turns the opt-in off before its save, and a
			// failure gives it back.
			granted := failure != "agent access"
			if granted {
				if err := m.SetAgentAccess(p.backendID, true); err != nil {
					t.Fatal(err)
				}
			}
			before, err := deviceclient.LoadSession(dir, p.backendID)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "lock":
				// Held past the transaction's bounded wait.
				holdSessionFile(t, dir, p.backendID)
			case "write":
				sessions := filepath.Join(dir, deviceclient.SessionsDirName)
				if err := os.Chmod(sessions, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(sessions, 0o700) })
			case "agent access":
				if err := os.WriteFile(filepath.Join(dir, agentAccessFile), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := within(t, "the failed re-pairing", 15*time.Second, p.pair(m)); got.added || got.err == nil {
				t.Fatalf("re-pairing = added %t, %v; want it to fail", got.added, got.err)
			}
			if failure == "write" {
				if err := os.Chmod(filepath.Join(dir, deviceclient.SessionsDirName), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			unchangedCarrier(t, m, p.backendID, old, before)
			if granted {
				if access, err := m.AgentAccess(); err != nil || !access[p.backendID] {
					t.Fatalf("agent access after the failed re-pairing = %v, %v; want it given back", access, err)
				}
			}
		})
	}
}

// A person's pairing with a forgotten computer that fails, on the session's
// write or on clearing the exclusion after it, leaves the computer
// forgotten: nothing stored, still excluded, nothing cached.
func TestAFailedPairingWithAForgottenComputerLeavesItForgotten(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"session write", "exclusion write"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, dir := newManager(t)
			if got := within(t, "the first pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
				t.Fatalf("first pairing = added %t, %v", got.added, got.err)
			}
			if err := m.Remove(p.backendID); err != nil {
				t.Fatal(err)
			}
			// The session is written in its own directory; the exclusions
			// file beside the profile's other files.
			readOnly := filepath.Join(dir, deviceclient.SessionsDirName)
			if failure == "exclusion write" {
				readOnly = dir
			}
			if err := os.Chmod(readOnly, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
			if got := within(t, "the failed pairing", 15*time.Second, p.pair(m)); got.added || got.err == nil {
				t.Fatalf("pairing = added %t, %v; want it to fail", got.added, got.err)
			}
			if err := os.Chmod(readOnly, 0o700); err != nil {
				t.Fatal(err)
			}
			if got := readPairingState(t, m, p); got != (pairingState{excluded: true}) {
				t.Fatalf("after the failed pairing %s; want the computer still forgotten", got)
			}
			nothingEnrolled(t, m, p.backendID)
		})
	}
}

// Two managers turning agent access on for two computers at once both
// write agent-access.json; its OS lock holds the second off until the first
// has written, so neither opt-in is lost.
func TestTwoManagersAgentAccessForTwoComputersKeepsBoth(t *testing.T) {
	t.Parallel()
	a, dir := newManager(t)
	b := otherManager(t, dir)
	const first, second = "0f6f8d3e-6f8b-4c39-9d5e-3a8f8f3f1c11", "1f6f8d3e-6f8b-4c39-9d5e-3a8f8f3f1c12"
	for _, id := range []string{first, second} {
		seed(t, dir, deviceclient.Session{BackendID: id, Endpoint: "https://mini.local:8443", SessionID: "s1", Credential: "c1"})
	}
	release := holdLockFile(t, dir, agentAccessLock)
	aDone := make(chan error, 1)
	go func() { aDone <- a.SetAgentAccess(first, true) }()
	bDone := make(chan error, 1)
	go func() { bDone <- b.SetAgentAccess(second, true) }()
	stillWaiting(t, "the first manager's opt-in", aDone)
	stillWaiting(t, "the second manager's opt-in", bDone)
	release()
	for _, done := range []<-chan error{aDone, bDone} {
		if err := within(t, "an opt-in", 10*time.Second, done); err != nil {
			t.Fatal(err)
		}
	}
	if access, err := a.AgentAccess(); err != nil || !access[first] || !access[second] {
		t.Fatalf("agent access = %v, %v; want both computers", access, err)
	}
}
