package aocli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type pairingScript struct {
	methods             []string
	confirmed, canceled bool
	states              []string
}

func (s *pairingScript) Call(_ context.Context, method string, result any, _ ...any) error {
	s.methods = append(s.methods, method)
	switch method {
	case "MintDevicePairing":
		*(result.(*consoleInvite)) = consoleInvite{LinkID: "test-link", URL: "https://example.test/#pair=one-time"}
	case "DevicePairingStatus":
		state := "redeemed"
		if len(s.states) > 0 {
			state, s.states = s.states[0], s.states[1:]
		}
		*(result.(*consolePairingStatus)) = consolePairingStatus{State: state, VerificationNumber: "123456", DeviceLabel: "Phone"}
	case "ConfirmDevicePairing":
		s.confirmed = true
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
