package attachedbackends

import (
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/deviceclient"
)

// seed writes one stored session into a temporary profile directory, the
// way a completed pairing would have. Nothing here reaches a network: the
// endpoint is only ever parsed.
func seed(t *testing.T, dir string, session deviceclient.Session) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, deviceclient.SessionsDirName), 0o700); err != nil {
		t.Fatalf("create the sessions dir: %v", err)
	}
	if err := deviceclient.SaveSession(dir, session); err != nil {
		t.Fatalf("seed %s: %v", session.BackendID, err)
	}
}

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	if _, err := deviceclient.EnrollDeviceKey(dir); err != nil {
		t.Fatalf("enroll the device key: %v", err)
	}
	manager, err := New(dir, "this-machine", "linux")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return manager, dir
}

// TestAttachedNamesEveryStoredProfile — the manifest is built from the
// profile directory on every read, which is what makes an attach and a
// removal take effect with no invalidation to get wrong.
func TestAttachedNamesEveryStoredProfile(t *testing.T) {
	manager, dir := newManager(t)
	seed(t, dir, deviceclient.Session{
		BackendID: "aaa", BackendName: "workshop-mini", Endpoint: "https://mini.local:8443",
		SessionID: "s1", Credential: "c1",
	})
	seed(t, dir, deviceclient.Session{
		BackendID: "bbb", Endpoint: "https://loft.local:8443", SessionID: "s2", Credential: "c2",
	})

	profiles := manager.Attached()
	if len(profiles) != 2 {
		t.Fatalf("attached = %+v, want two", profiles)
	}
	if profiles[0].ID != "aaa" || profiles[0].BackendID != "aaa" {
		t.Errorf("first profile = %+v, want the stored backend id", profiles[0])
	}
	if profiles[0].Name != "workshop-mini" {
		t.Errorf("name = %q, want what the machine called itself", profiles[0].Name)
	}
	// A machine that published no name falls back to where it is, never
	// to an empty label nobody can act on.
	if profiles[1].Name != "https://loft.local:8443" {
		t.Errorf("nameless machine labelled %q, want its address", profiles[1].Name)
	}
}

// TestRenameWinsOverWhatTheMachineCallsItself — two machines that both
// answer "mac-mini" are told apart by what the owner typed, and the far
// side is told nothing.
func TestRenameWinsOverWhatTheMachineCallsItself(t *testing.T) {
	manager, dir := newManager(t)
	seed(t, dir, deviceclient.Session{
		BackendID: "aaa", BackendName: "mac-mini", Endpoint: "https://mini.local:8443",
		SessionID: "s1", Credential: "c1",
	})

	if err := manager.Rename("aaa", "The Loft Mini"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := manager.Attached()[0].Name; got != "The Loft Mini" {
		t.Errorf("name = %q, want the nickname", got)
	}
	stored, err := deviceclient.LoadSession(dir, "aaa")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Nickname != "The Loft Mini" {
		t.Errorf("stored nickname = %q, want it persisted", stored.Nickname)
	}
	if stored.BackendName != "mac-mini" {
		t.Errorf("BackendName = %q, want the machine's own name untouched", stored.BackendName)
	}

	// Clearing is a rename to nothing, and the machine's own name comes
	// back rather than the row going blank.
	if err := manager.Rename("aaa", ""); err != nil {
		t.Fatalf("clear the nickname: %v", err)
	}
	if got := manager.Attached()[0].Name; got != "mac-mini" {
		t.Errorf("name after clearing = %q, want the machine's own", got)
	}
}

// TestRemoveForgetsTheSessionAndKeepsTheDeviceKey — the key names this
// DEVICE, and the far side adopts its row by thumbprint if this
// installation ever pairs with that machine again.
func TestRemoveForgetsTheSessionAndKeepsTheDeviceKey(t *testing.T) {
	manager, dir := newManager(t)
	seed(t, dir, deviceclient.Session{
		BackendID: "aaa", Endpoint: "https://mini.local:8443", SessionID: "s1", Credential: "c1",
	})
	// Build the carrier first, so the removal has a cached one to drop.
	if manager.Carrier("aaa") == nil {
		t.Fatal("no carrier for a stored profile")
	}
	old := manager.carriers["aaa"].client
	if err := manager.Remove("aaa"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if profiles := manager.Attached(); len(profiles) != 0 {
		t.Errorf("attached after removal = %+v, want none", profiles)
	}
	if manager.Carrier("aaa") != nil {
		t.Error("a removed backend still has a carrier")
	}
	if old.Session().Credential != "" {
		t.Error("removed carrier still authorizes requests")
	}
	if _, err := deviceclient.DeviceKey(dir); err != nil {
		t.Errorf("the device key did not survive the removal: %v", err)
	}
}

// TestCarrierIsNilForAnUnknownBackend — nil is an ordinary answer, and it
// has to be an UNTYPED one: a typed nil returned through an interface is
// not nil, and the transport registers its refusal on exactly that test.
func TestCarrierIsNilForAnUnknownBackend(t *testing.T) {
	manager, _ := newManager(t)
	if carrier := manager.Carrier("never-paired"); carrier != nil {
		t.Fatalf("carrier for an unknown backend = %#v, want an untyped nil", carrier)
	}
}

// TestListReportsNoReachabilityBeforeAnythingAnswered — reachability is
// last-known and never probed, so a machine nothing has spoken to this
// launch reports zero rather than a guess.
func TestListReportsNoReachabilityBeforeAnythingAnswered(t *testing.T) {
	manager, dir := newManager(t)
	seed(t, dir, deviceclient.Session{
		BackendID: "aaa", BackendName: "mini", Endpoint: "https://mini.local:8443",
		SessionID: "s1", Credential: "c1", Nickname: "Mini",
	})
	rows, err := manager.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	if rows[0].LastReachedMs != 0 {
		t.Errorf("lastReachedMs = %d, want zero before anything answered", rows[0].LastReachedMs)
	}
	if rows[0].Nickname != "Mini" || rows[0].Endpoint != "https://mini.local:8443" {
		t.Errorf("row = %+v, want the stored profile", rows[0])
	}
}
