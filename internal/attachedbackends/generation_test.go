package attachedbackends

import (
	"context"
	"errors"
	"testing"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/errorsx"

	"github.com/google/uuid"
)

// A profile an older build saved carries no generation. Read twice, by
// every reader, it names one, which nothing had to write; a session that
// continues another pairing names that one's.
func TestAPairingNamesOneGenerationHoweverItIsRead(t *testing.T) {
	m, dir := newManager(t)
	legacy, carried := uuid.NewString(), uuid.NewString()
	seed(t, dir, deviceclient.Session{BackendID: legacy, SessionID: "first-session", Credential: "c", Endpoint: "https://127.0.0.1:1"})
	seed(t, dir, deviceclient.Session{BackendID: carried, SessionID: "upgraded-session", PairingGeneration: "first-session-there",
		Credential: "c", Endpoint: "https://127.0.0.1:1"})
	for id, want := range map[string]string{legacy: "first-session", carried: "first-session-there"} {
		for range 2 {
			if got, err := m.Grant(id); err != nil || got != want {
				t.Fatalf("Grant(%s) = %q, %v; want %q", id, got, err, want)
			}
		}
		if row, _, err := m.Lookup(id); err != nil || row.Generation != want {
			t.Fatalf("Lookup(%s) names generation %q, %v; want %q", id, row.Generation, err, want)
		}
	}
	if raw, err := deviceclient.LoadSession(dir, legacy); err != nil || raw.PairingGeneration != "" {
		t.Fatalf("reading the older profile wrote %q, %v", raw.PairingGeneration, err)
	}
	if got, err := m.Grant(uuid.NewString()); err != nil || got != "" {
		t.Fatalf("Grant of a computer never paired = %q, %v", got, err)
	}
}

// A call made for a record of another generation is refused where its
// carrier is chosen, before anything is dialled; one of the generation held
// goes on to the network.
func TestACallForAnotherGenerationIsRefusedBeforeItIsSent(t *testing.T) {
	m, dir := newManager(t)
	id := uuid.NewString()
	seed(t, dir, deviceclient.Session{BackendID: id, SessionID: "held", Credential: "c", Endpoint: "https://127.0.0.1:1"})
	if err := m.SetAgentAccess(id, true); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(generation string) error{
		"thread": func(generation string) error {
			return m.CallThreadPeerUnder(context.Background(), id, generation, "ThreadToolRequestStatus", nil)
		},
		"command": func(generation string) error {
			return m.CallAgentPeerUnder(context.Background(), id, generation, "RemoteCommandStatus", nil)
		},
	} {
		err := call("replaced")
		if code, _, public := errorsx.PublicDetails(err); !errors.Is(err, ErrPairingNotHeld) || !public || code != "remote_not_paired" {
			t.Fatalf("%s call for a replaced generation = %v", name, err)
		}
		if err := call("held"); err == nil || errors.Is(err, ErrPairingNotHeld) {
			t.Fatalf("%s call for the generation held = %v, want it dialled", name, err)
		}
	}
	if err := m.CallThreadPeerUnder(context.Background(), uuid.NewString(), "held", "ThreadToolRequestStatus", nil); !errors.Is(err, ErrPairingNotHeld) {
		t.Fatalf("a call for a computer paired no longer = %v", err)
	}
}
