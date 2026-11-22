//go:build !noremote

package attachedbackends

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/deviceclient"
)

// A commit whose carrier cannot be built after its writes leaves the
// session, the own-device exclusion and agent access as the transaction read
// them, and the cached carrier as it was: for a person's pairing with a
// forgotten computer, a person's re-pairing, and an introduction.
//
// Building a carrier fails only on an address no redemption can have
// answered on (newCarrier), so the test makes the build fail.
func TestAFailedCarrierBuildRollsBackEveryWrite(t *testing.T) {
	t.Parallel()
	errBuild := errors.New("the carrier could not be built")
	for _, commit := range []string{"pairing with a forgotten computer", "re-pairing", "introduction"} {
		t.Run(commit, func(t *testing.T) {
			t.Parallel()
			p := newIntroducedPeer(t, nil)
			m, dir := newManager(t)
			if got := within(t, "the first pairing", 5*time.Second, p.pair(m)); !got.added || got.err != nil {
				t.Fatalf("first pairing = added %t, %v", got.added, got.err)
			}
			var old *carrier
			if commit == "pairing with a forgotten computer" {
				if err := m.Remove(p.backendID); err != nil {
					t.Fatal(err)
				}
			} else {
				old = enrolled(t, m, p.backendID)
				if err := m.SetAgentAccess(p.backendID, true); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := deviceclient.LoadSession(dir, p.backendID)
			access, err := m.AgentAccess()
			if err != nil {
				t.Fatal(err)
			}
			excluded, err := m.ownDeviceExcluded(p.backendID)
			if err != nil {
				t.Fatal(err)
			}
			m.buildCarrier = func(*deviceclient.Client, string) (*carrier, error) { return nil, errBuild }

			var answer <-chan introductionAnswer
			if commit == "introduction" {
				answer = p.introduce(context.Background(), m)
			} else {
				answer = p.pair(m)
			}
			if got := within(t, "the commit", 10*time.Second, answer); got.added || !errors.Is(got.err, errBuild) {
				t.Fatalf("commit = added %t, %v; want it to fail with %v", got.added, got.err, errBuild)
			}

			after, afterErr := deviceclient.LoadSession(dir, p.backendID)
			if !errors.Is(afterErr, beforeErr) || after.SessionID != before.SessionID || after.Credential != before.Credential || after.OwnDevice != before.OwnDevice {
				t.Fatalf("stored session = %+v, %v; want %+v, %v", after, afterErr, before, beforeErr)
			}
			if got, err := m.AgentAccess(); err != nil || got[p.backendID] != access[p.backendID] {
				t.Fatalf("agent access = %v, %v; want %t as before", got, err, access[p.backendID])
			}
			if got, err := m.ownDeviceExcluded(p.backendID); err != nil || got != excluded {
				t.Fatalf("excluded = %t, %v; want %t as before", got, err, excluded)
			}
			if old == nil {
				nothingEnrolled(t, m, p.backendID)
			} else {
				unchangedCarrier(t, m, p.backendID, old, before)
			}
		})
	}
}

// A computer id may spell a shared file's lock name. Its transitions take
// that file's in-process lock as well as its own profile lock, so the two
// are separate locks and its forget returns.
func TestAComputerNamedLikeASharedFileLockCanBeForgotten(t *testing.T) {
	t.Parallel()
	for _, id := range []string{agentAccessLock, ownExclusionsLock} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			m, dir := newManager(t)
			seed(t, dir, deviceclient.Session{BackendID: id, Endpoint: "https://mini.local:8443", SessionID: "s1", Credential: "c1"})
			if err := within(t, "Forget", 5*time.Second, removeAsync(m, id)); err != nil {
				t.Fatalf("Remove(%q): %v", id, err)
			}
			if _, err := deviceclient.LoadSession(dir, id); !errors.Is(err, deviceclient.ErrNoSession) {
				t.Fatalf("stored after Forget = %v; want nothing", err)
			}
			if excluded, err := m.ownDeviceExcluded(id); err != nil || !excluded {
				t.Fatalf("excluded = %t, %v; want the forget recorded", excluded, err)
			}
		})
	}
}
