package main

import (
	"fmt"
	"slices"
	"strings"

	"agent-overflow/internal/harnessclient"
)

const cliHarnessProtocolRevision = harnessclient.SupportedHarnessProtocolRevision

// capabilityRequirements names the controls a command will use. Keeping the
// check here lets each command fail before its first mutation while the
// backend remains the source of truth for the catalog.
type capabilityRequirements struct {
	Methods   []string
	Meters    []string
	Actions   []string
	Queries   []string
	Workloads []string
}

func requireHarnessProtocol(client *harnessclient.Client, req capabilityRequirements) error {
	caps, err := client.CachedCapabilities()
	if err != nil {
		return fmt.Errorf("harness protocol preflight: %w; this instance is too old for safe control, rebuild the harness", err)
	}
	return validateHarnessCapabilities(caps, req)
}

func validateHarnessCapabilities(caps harnessclient.HarnessCapabilities, req capabilityRequirements) error {
	if caps.ProtocolRevision != cliHarnessProtocolRevision {
		return fmt.Errorf("harness protocol revision %d is incompatible with CLI revision %d; rebuild the harness", caps.ProtocolRevision, cliHarnessProtocolRevision)
	}
	for _, check := range []struct {
		kind string
		want []string
		have []string
	}{
		{"method", req.Methods, caps.Methods},
		{"meter", req.Meters, caps.Meters},
		{"action", req.Actions, caps.Actions},
		{"query", req.Queries, caps.Queries},
		{"workload", req.Workloads, caps.Workloads},
	} {
		missing := missingCapabilities(check.want, check.have)
		if len(missing) > 0 {
			return fmt.Errorf("harness protocol preflight: instance is missing %s %s", check.kind, quoteCapabilities(missing))
		}
	}
	return nil
}

func missingCapabilities(want, have []string) []string {
	missing := make([]string, 0, len(want))
	for _, name := range want {
		if !slices.Contains(have, name) {
			missing = append(missing, name)
		}
	}
	return slices.Compact(missing)
}

func quoteCapabilities(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

func warnCapabilities(e *env, caps harnessclient.HarnessCapabilities, err error) {
	if err != nil {
		fmt.Fprintf(e.stderr, "ao-harness: WARNING: HarnessCapabilities is unavailable (%v); read-only diagnostics remain available, but mutations, arbitrary rpc, bench, profile and replay are refused\n", err)
		return
	}
	if caps.ProtocolRevision != cliHarnessProtocolRevision {
		fmt.Fprintf(e.stderr, "ao-harness: WARNING: instance speaks harness protocol revision %d, CLI requires %d; read-only diagnostics remain available, but mutations, arbitrary rpc, bench, profile and replay are refused\n", caps.ProtocolRevision, cliHarnessProtocolRevision)
	}
}
