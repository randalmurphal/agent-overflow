package claude

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// TestPlanLiveUpdateNeverCarriesASystemPrompt pins that a system prompt
// change is never live: every transition, including turning a prompt on or
// editing one, falls through to the restart path. A prompt change applies
// at the thread's next session start (docs/decisions.md 2026-10-14); the
// app pins the settings-level override before this diff, so only a
// feature-owned prompt reaches it, and that one converges by restart.
func TestPlanLiveUpdateNeverCarriesASystemPrompt(t *testing.T) {
	tests := []struct {
		name   string
		prev   string
		next   string
		wantOK bool
	}{
		{name: "edited", prev: "old prompt", next: "new prompt", wantOK: false},
		{name: "turned off", prev: "old prompt", next: "", wantOK: false},
		{name: "turned on", prev: "", next: "new prompt", wantOK: false},
		{name: "unchanged", prev: "same prompt", next: "same prompt", wantOK: true},
		{name: "both empty", prev: "", next: "", wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := liveUpdateBaseOptions()
			prev.SystemPrompt = tc.prev
			next := liveUpdateBaseOptions()
			next.SystemPrompt = tc.next

			update, ok := PlanLiveUpdate(prev, next)
			if ok != tc.wantOK {
				t.Fatalf("PlanLiveUpdate ok = %v, want %v (update %+v)", ok, tc.wantOK, update)
			}
			if ok && !update.Empty() {
				t.Fatalf("PlanLiveUpdate update = %+v, want empty", update)
			}
		})
	}
}

// TestPlanLiveUpdateIgnoresOverrideSourceProvenance pins that
// SessionOptions.SystemPromptOverrideSource, the app-layer record of WHICH
// stored override produced the rendered prompt, never reaches the diff. It
// is deliberately absent from Config, so two sessions running the identical
// rendered prompt must plan as unchanged even when the provenance differs.
func TestPlanLiveUpdateIgnoresOverrideSourceProvenance(t *testing.T) {
	prev := liveUpdateBaseOptions()
	prev.SystemPrompt = "rendered prompt"
	prev.SystemPromptOverrideSource = "stored {{PLATFORM}} prompt"
	next := liveUpdateBaseOptions()
	next.SystemPrompt = "rendered prompt"
	next.SystemPromptOverrideSource = "a completely different stored prompt"

	update, ok := PlanLiveUpdate(prev, next)
	if !ok || !update.Empty() {
		t.Fatalf("PlanLiveUpdate = (%+v, %v), want an empty live update", update, ok)
	}
}

// TestClaudeCLIVersionAtLeast pins the comparison, including the two forms
// the version reaches us in: bare (`system/init.claude_code_version`) and
// suffixed (`claude --version` prints "2.1.237 (Claude Code)").
func TestClaudeCLIVersionAtLeast(t *testing.T) {
	tests := []struct {
		have string
		want bool
	}{
		{"2.1.214", true},
		{"2.1.219", true},
		{"2.1.237", true},
		{"2.1.237 (Claude Code)", true},
		{"2.2.0", true},
		{"3.0.0", true},
		{"2.1.213", false},
		{"2.0.999", false},
		{"1.9.9", false},
		{"1.0", false},
		{"", false},
		{"garbage", false},
		{"2.1", false},
	}
	for _, tc := range tests {
		if got := claudeCLIVersionAtLeast(tc.have, minLiveThinkingCLIVersion); got != tc.want {
			t.Errorf("claudeCLIVersionAtLeast(%q, %q) = %v, want %v",
				tc.have, minLiveThinkingCLIVersion, got, tc.want)
		}
	}
}

// TestSessionCLIVersionFromWireInit ties the version gate to the wire: the
// gate is only conservative-by-default if the version actually arrives, and
// `system/init.claude_code_version` is the sole in-session source.
func TestSessionCLIVersionFromWireInit(t *testing.T) {
	line := fixtureLines(t, effortLiveFixture)[2]
	events, err := (&Parser{}).ParseLine("thread-1", line)
	if err != nil {
		t.Fatalf("ParseLine(init): %v", err)
	}
	if len(events) != 1 || events[0].Kind != provider.EventInit {
		t.Fatalf("events = %+v, want one init", events)
	}
	var info provider.SessionInfo
	if err := json.Unmarshal(events[0].Meta, &info); err != nil {
		t.Fatalf("unmarshal session info: %v", err)
	}
	s := &Session{}
	s.noteCLIVersion(info.Version)
	if got := s.CLIVersion(); got != "2.1.219" {
		t.Fatalf("CLIVersion = %q, want the fixture's 2.1.219", got)
	}
	if !s.supportsLiveThinking() {
		t.Fatal("2.1.219 must clear the live thinking floor")
	}
	// A later init that omits the key must not un-learn the version.
	s.noteCLIVersion("")
	if got := s.CLIVersion(); got != "2.1.219" {
		t.Fatalf("CLIVersion = %q after an empty note, want it unchanged", got)
	}
}
