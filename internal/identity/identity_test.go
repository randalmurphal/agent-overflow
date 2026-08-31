package identity

import (
	"os"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }

const testBackendID = "backend-under-test"

// clock is a settable time source. Every test that cares about a window
// drives one rather than sleeping, so a slow machine cannot decide whether
// a credential was still valid.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

// newFixture returns a session core over a fresh migrated store, with an
// owner account, one device, and a stopped clock.
func newFixture(t *testing.T) (*Sessions, *store.Store, *clock, store.User, store.Device) {
	t.Helper()
	st := storetest.Clone(t)
	sessions, err := NewSessions(st, testBackendID)
	if err != nil {
		t.Fatalf("NewSessions: %v", err)
	}
	c := &clock{at: time.UnixMilli(1_700_000_000_000)}
	sessions.now = c.now

	owner, err := st.EnsureOwnerUser("Owner")
	if err != nil {
		t.Fatalf("EnsureOwnerUser: %v", err)
	}
	device, err := st.CreateDevice(owner.ID, "This Desktop", string(DeviceDesktop), "linux")
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return sessions, st, c, owner, device
}

func mustMint(t *testing.T, s *Sessions, owner store.User, device store.Device, ttl time.Duration) (store.Session, string) {
	t.Helper()
	session, credential, err := s.Mint(MintRequest{
		UserID: owner.ID, DeviceID: device.ID,
		BindingClass: BindingDeviceBound,
		Scopes:       []Scope{ScopeThreadsRead, ScopeFilesRead},
		TTL:          ttl,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return session, credential
}
