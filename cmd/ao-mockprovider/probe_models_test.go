package main

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/claudemodels"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
)

// TestClaudeProbeReportsMergeableModels drives the account probe over the wire
// — the same control_request the real probe sends — and folds the `models`
// array out of its answer through the production merge in
// internal/claudemodels.
//
// Two things are pinned. First, that the payload is READABLE by the real wire
// type: claude.ProbeConfig.OnModels reporting an error is the "array present
// but unreadable" answer, which a consumer must treat as no information at all,
// so a mock whose payload only looked right would leave the enrichment path
// permanently inert under harness while every test still passed.
//
// Second, that the merge is OBSERVABLE. The mock claims fast-mode support for
// claude-haiku-4-5, which the shipped catalog does not, so a working merge has
// to report exactly one capability drift and hand back a Haiku carrying
// ModelCapabilityFastMode. A payload that agreed with the catalog on every axis
// would let this test pass with the merge never having run.
//
// The envelope unwrap below mirrors claude/probe.go's
// tryParseControlInitResponse rather than calling it: that function is
// unexported, and the exported entry point (claude.ProbeAccount) is
// deliberately unreachable from here — a repo-wide guard reserves every
// claude.ProbeConfig literal for the one app-side constructor that arms the
// credential-rotation watch. The two JSON keys this reads (`response.response`
// then `models`) are the only shared surface, and claude.WireModel below is the
// real decode target with the real tags.
func TestClaudeProbeReportsMergeableModels(t *testing.T) {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--max-turns", "0"}
	p := startMock(t, args, nil, t.TempDir())
	p.expectLine(testTimeout) // system/init, asserted by TestClaudeProbeInvocation

	p.send(`{"type":"control_request","request_id":"ao-probe-init","request":{"subtype":"initialize"}}`)
	line := p.expectLine(testTimeout)
	p.closeStdinAndExpectExit(0, testTimeout)

	var envelope struct {
		Type     string `json:"type"`
		Response struct {
			Subtype   string          `json:"subtype"`
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("initialize response is not JSON: %v\n  line: %s", err, line)
	}
	if envelope.Type != "control_response" || envelope.Response.RequestID != "ao-probe-init" {
		t.Fatalf("unexpected envelope: %s", line)
	}

	var payload struct {
		Models []claude.WireModel `json:"models"`
	}
	if err := json.Unmarshal(envelope.Response.Response, &payload); err != nil {
		t.Fatalf("the mock's models array did not decode into claude.WireModel: %v", err)
	}
	if len(payload.Models) == 0 {
		t.Fatal("probe reported an empty models array; the enrichment path would never run")
	}

	merged, drift := claudemodels.Merge(provider.ClaudeModels, payload.Models)

	var haiku *provider.ModelInfo
	for i := range merged {
		if merged[i].Slug == "claude-haiku-4-5" {
			haiku = &merged[i]
		}
	}
	if haiku == nil {
		t.Fatal("merged catalog lost claude-haiku-4-5")
	}
	if !hasCapability(haiku.Capabilities, provider.ModelCapabilityFastMode) {
		t.Errorf("haiku capabilities = %v, want the wire's fast-mode claim applied", haiku.Capabilities)
	}

	if len(drift) != 1 {
		t.Fatalf("drift = %v, want exactly the one deliberate divergence", drift)
	}
	if drift[0].Model != "claude-haiku-4-5" || drift[0].Kind != claudemodels.DriftCapability {
		t.Errorf("drift[0] = %v, want a capability drift on claude-haiku-4-5", drift[0])
	}

	// The pointer row must resolve to a catalog model rather than being added
	// as a wire-only one; a "default" entry in the picker is the failure a
	// broken CanonicalSlug produces.
	for _, model := range merged {
		if model.Slug == "default" {
			t.Fatal("the default pointer row leaked into the catalog as a model")
		}
	}
	// The pointer row is also the one that proves the 1M marker survives the
	// round trip; without it the resolved model would be a different slug.
	if !strings.Contains(line, `"resolvedModel":"claude-opus-5[1m]"`) {
		t.Errorf("probe payload lost the extended-context pointer row: %s", line)
	}
}

func hasCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}
