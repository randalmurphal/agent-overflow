package identity

import (
	"os"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/testutil/providerpath"
)

func TestMain(m *testing.M) { os.Exit(providerpath.Guard(func() int { return storetest.Run(m) })) }

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

// bearerProof is a presentation from a device with no signing key: the
// enrollment thumbprint itself, which is what §15 constraint 6's
// plain-HTTP LAN browser has and all it will ever have. Method and Path
// are deliberately empty — nothing reads them on this path, and a test
// that filled them would suggest otherwise.
func bearerProof(thumbprint string) DeviceProof {
	return DeviceProof{Value: thumbprint}
}

// mintOn writes the live session req describes on an existing device, with
// its first credential pair, through the builder and transaction a passkey
// sign-in uses.
func mintOn(s *Sessions, device store.Device, req MintRequest) (store.Session, TokenSet, error) {
	written, tokens, err := s.newDeviceSession(req, device, false, s.now().UnixMilli())
	if err != nil {
		return store.Session{}, TokenSet{}, err
	}
	if err := s.store.CreateDeviceSession(written); err != nil {
		return store.Session{}, TokenSet{}, err
	}
	return written.Session, tokens, nil
}

func mustMint(t *testing.T, s *Sessions, owner store.User, device store.Device, ttl time.Duration) (store.Session, string) {
	t.Helper()
	session, tokens, err := mintOn(s, device, MintRequest{
		UserID:       owner.ID,
		BindingClass: BindingDeviceBound,
		Scopes:       []Scope{ScopeThreadsRead, ScopeFilesRead},
		TTL:          ttl,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return session, tokens.Credential
}
