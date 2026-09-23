// forge.go is the fake forge's harness surface: seeding the forge state
// ao-mockforge answers from, and reading back what the app asked it.
// Each invocation also fans out as a harness:forge event so a spec can
// await the call a UI action causes instead of polling.
package harnessrpc

import (
	"encoding/json"
	"fmt"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/harness/forgefake"
)

func (h *Harness) onForgeInvocation(inv forgefake.Invocation) {
	if h.config.Host == nil {
		return
	}
	h.config.Host.Emit(eventchan.HarnessForge, inv)
}

// HarnessForgeSeed adds repositories, pull or merge requests, comments,
// review threads, CI and attachments to the fake forge (the fixture
// format is forgefake.Fixture). A repository already seeded under the
// same forge and project is replaced. Returns the fixture with every
// generated id filled in. HarnessReset clears it.
func (h *Harness) HarnessForgeSeed(raw json.RawMessage) (forgefake.Fixture, error) {
	var fixture forgefake.Fixture
	if err := decodeStrictJSON("forge fixture", raw, &fixture); err != nil {
		return forgefake.Fixture{}, err
	}
	seeded, err := h.forge.Seed(fixture)
	if err != nil {
		return forgefake.Fixture{}, fmt.Errorf("seed forge: %w", err)
	}
	return seeded, nil
}

// HarnessForgeInvocations returns every recorded gh or glab invocation
// with a sequence number above since: argv, cwd, stdin, the route that
// answered (or unhandled) and the exit status.
func (h *Harness) HarnessForgeInvocations(since int) forgefake.InvocationLog {
	return h.forge.Invocations(since)
}
