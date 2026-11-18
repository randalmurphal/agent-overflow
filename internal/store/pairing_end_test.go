package store

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A request or command from before pairing generations were recorded names
// none. The boot finds the computers they are on and binds each one's to
// the generation it holds then; a local request has no pairing to bind and
// stays as it is, and once bound none is listed again.
func TestRecordsThatNameNoPairingAreBoundToTheirComputers(t *testing.T) {
	s := newTestStore(t)
	lead := uuid.NewString()
	mustCreateThread(t, s, lead)
	first, second := uuid.NewString(), uuid.NewString()
	request := func(token, computer, pairing string) {
		t.Helper()
		if err := s.InsertThreadRequest(ThreadRequest{Token: token, CallerThreadID: lead, Kind: ThreadRequestSend, TargetComputerID: computer,
			TargetThreadID: "t-" + token, State: ThreadRequestRunning, Pairing: pairing}); err != nil {
			t.Fatal(err)
		}
	}
	request("legacy-first", first, "")
	request("bound-first", first, "g0")
	request("legacy-second", second, "")
	request("local", "", "")
	command := RemoteWatch{ComputerID: first, RequestID: uuid.NewString(), ThreadID: lead, Fingerprint: strings.Repeat("a", 64), Label: "build"}
	if _, err := s.RegisterRemoteWatch(command); err != nil {
		t.Fatal(err)
	}

	computers, err := s.UnboundPairingComputers()
	slices.Sort(computers)
	want := []string{first, second}
	slices.Sort(want)
	if err != nil || !slices.Equal(computers, want) {
		t.Fatalf("computers with records naming no pairing = %v, %v; want %v", computers, err, want)
	}
	if err := s.BindRecordPairing(first, "g1"); err != nil {
		t.Fatal(err)
	}
	for token, pairing := range map[string]string{"legacy-first": "g1", "bound-first": "g0", "legacy-second": "", "local": ""} {
		if row := requestRow(t, s, token); row.Pairing != pairing {
			t.Fatalf("request %s names pairing %q, want %q", token, row.Pairing, pairing)
		}
	}
	if w, err := s.GetRemoteWatch(first, command.RequestID); err != nil || w.Pairing != "g1" {
		t.Fatalf("the command names pairing %q, %v; want g1", w.Pairing, err)
	}
	if computers, err := s.UnboundPairingComputers(); err != nil || !slices.Equal(computers, []string{second}) {
		t.Fatalf("after the binding = %v, %v; want the second computer alone", computers, err)
	}
	if err := s.BindRecordPairing(second, ""); err == nil {
		t.Fatal("a binding to no generation was accepted")
	}
}
