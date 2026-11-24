package aocli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/rpcclient"
)

type pairingScript struct {
	methods             []string
	confirmed, canceled bool
	states              []string
	confirmErr          error
}

func (s *pairingScript) Call(_ context.Context, method string, result any, _ ...any) error {
	s.methods = append(s.methods, method)
	switch method {
	case "MintOwnDevicePairing":
		*(result.(*consoleInvite)) = consoleInvite{LinkID: "test-link", URL: "https://example.test/#pair=one-time"}
	case "DevicePairingStatus":
		state := "redeemed"
		if len(s.states) > 0 {
			state, s.states = s.states[0], s.states[1:]
		}
		*(result.(*consolePairingStatus)) = consolePairingStatus{State: state, VerificationNumber: "123456", DeviceLabel: "Phone"}
	case "ConfirmDevicePairing":
		s.confirmed = true
		return s.confirmErr
	case "CancelDevicePairing":
		s.canceled = true
	}
	return nil
}
func TestPairingConsoleRequiresMatchingNumberAndCancelsOnEOF(t *testing.T) {
	for _, input := range []string{"123456\n", "123455\n", "", "yes\n"} {
		script := &pairingScript{}
		var output bytes.Buffer
		err := runPairingConsole(t.Context(), script, "desktop", false, false, strings.NewReader(input), &output, time.Millisecond)
		match := input == "123456\n"
		if (err == nil) != match || script.confirmed != match || script.canceled == match {
			t.Fatalf("input %q: err=%v, script=%+v", input, err, script)
		}
		if !strings.Contains(output.String(), "123456") {
			t.Fatal("verification was not shown")
		}
	}
}
func TestPairingConsoleMachineStreamHasExplicitConfirmationBoundary(t *testing.T) {
	script := &pairingScript{states: []string{"pending", "redeemed"}}
	var output bytes.Buffer
	if err := runPairingConsole(t.Context(), script, "desktop", false, true, strings.NewReader("123456\n"), &output, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	for _, expected := range []string{"invitation", "verification", "paired"} {
		var record struct {
			Type string `json:"type"`
		}
		if err := decoder.Decode(&record); err != nil || record.Type != expected {
			t.Fatalf("event=%+v, %v", record, err)
		}
	}
	if strings.Contains(output.String(), "private-launch") {
		t.Fatal("launch credential entered setup output")
	}
}

// An invitation another owner view approved whose access has since ended is
// never reported as paired, and the console says why it stopped.
func TestPairingConsoleSaysAnApprovalWhoseAccessEndedIsNotPaired(t *testing.T) {
	for state, want := range map[string]string{
		"accessEnded":   "this device was allowed, and its access has since ended or expired; run agent-overflow pair to pair it again",
		"deviceRemoved": "this device was allowed, and has since been removed in Devices; choose Restore or Forget on it there, then run agent-overflow pair again",
	} {
		script := &pairingScript{states: []string{state}}
		var output bytes.Buffer
		err := runPairingConsole(t.Context(), script, "desktop", false, false, strings.NewReader(""), &output, time.Millisecond)
		if err == nil || err.Error() != want {
			t.Fatalf("%s: the console's answer = %v", state, err)
		}
		if strings.Contains(output.String(), "Device connected") || script.confirmed {
			t.Fatalf("%s: the console reported or confirmed a pairing whose access ended: %q, %+v", state, output.String(), script)
		}
	}
}

// An invitation another owner view allowed while the console waited for the
// typed number is paired, and its link is not canceled. Any other refusal
// still fails the console and cancels.
func TestPairingConsoleTreatsAnAlreadyAllowedInvitationAsPaired(t *testing.T) {
	script := &pairingScript{confirmErr: &rpcclient.Error{Code: "pairing_already_allowed", Message: "this device is already allowed"}}
	var output bytes.Buffer
	if err := runPairingConsole(t.Context(), script, "desktop", false, false, strings.NewReader("123456\n"), &output, time.Millisecond); err != nil {
		t.Fatalf("the console's answer = %v, want paired", err)
	}
	if !strings.Contains(output.String(), "Device connected") || script.canceled {
		t.Fatalf("the console = %q, %+v, want it paired and its link kept", output.String(), script)
	}
	refused := &pairingScript{confirmErr: &rpcclient.Error{Code: "pairing_closed", Message: "this pairing is no longer open; start pairing again"}}
	if err := runPairingConsole(t.Context(), refused, "desktop", false, false, strings.NewReader("123456\n"), &output, time.Millisecond); err == nil || !refused.canceled {
		t.Fatalf("a refused approval = %v, %+v, want it failed and canceled", err, refused)
	}
}
