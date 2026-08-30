package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/harnessclient"
)

func compatibleCapabilities() harnessclient.HarnessCapabilities {
	return harnessclient.HarnessCapabilities{
		ProtocolRevision: cliHarnessProtocolRevision,
		Methods:          []string{"HarnessSeed", "SendMessage"},
		Meters:           []string{"frames"},
		Actions:          []string{"open"},
		Queries:          []string{"viewport"},
		Workloads:        []string{"burst-stream"},
	}
}

func TestMissingCapabilitiesNamesTheControlBeforeMutation(t *testing.T) {
	caps := compatibleCapabilities()
	caps.Methods = []string{"SendMessage"}
	// This exercises the pure missing-name logic used by the command gates.
	missing := missingCapabilities([]string{"HarnessSeed", "HarnessSeed", "SendMessage"}, caps.Methods)
	if got, want := strings.Join(missing, ","), "HarnessSeed"; got != want {
		t.Fatalf("missing = %q, want %q", got, want)
	}
}

func TestCapabilitiesPreflightRefusesProtocolRevisionMismatch(t *testing.T) {
	caps := compatibleCapabilities()
	caps.ProtocolRevision++
	if err := validateHarnessCapabilities(caps, capabilityRequirements{}); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("mismatched capabilities error = %v", err)
	}
}

func TestCapabilitiesPreflightRefusesMissingRequiredControl(t *testing.T) {
	caps := compatibleCapabilities()
	err := validateHarnessCapabilities(caps, capabilityRequirements{Methods: []string{"HarnessReset"}})
	if err == nil || !strings.Contains(err.Error(), `method "HarnessReset"`) {
		t.Fatalf("missing control error = %v", err)
	}
}
