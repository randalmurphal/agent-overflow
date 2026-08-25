package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVars is the ONE substitution environment the library checks run every
// shipped scenario against. It lives in a _test.go file (the scenario package
// is linked into ao-mockprovider and must stay import- and payload-light) but
// is EXPORTED, because the parser check lives in the external scenario_test
// package and needs the same bindings.
//
// One environment, not two: the checks answer different questions about the
// same lines — is every emit line JSON, and does the app's own parser accept
// it — so a variable bound in one and not the other means the stricter check
// silently runs against a line no scenario would ever emit. That is how
// ${USER_INPUT} and ${CLIENT_ID} came to be substituted in the integrity
// check and left literal in the parser check.
//
// A variable a scenario can reference must be bound here, and bound to a value
// shaped like the real one. USER_INPUT / CLIENT_ID are bound by the codex
// adapter from the `turn/start` it is answering, and CLIENT_ID is empty when
// the caller supplied none — the values below are the populated case, which is
// the one that has to parse.
var TestVars = Vars{
	"SESSION_ID": "sess-test",
	"THREAD_ID":  "thread-test",
	"TURN":       "1",
	"TURN_ID":    "turn-1",
	"REQUEST_ID": "7",
	"CWD":        "/tmp/workspace",
	"ITER":       "1",
	"USER_INPUT": "steered message",
	"CLIENT_ID":  "user:1:flush:1",
}

// TestLibraryIntegrity enforces the shipped-library invariants: every
// file parses + validates, its Name matches its filename, and every
// inline emit line is itself valid JSON after ${VAR} substitution
// (provider wire frames are always JSON objects on both protocols).
func TestLibraryIntegrity(t *testing.T) {
	entries, err := Library()
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("embedded library is empty")
	}
	vars := TestVars
	for _, entry := range entries {
		raw, s, err := LoadLibrary(entry.Name)
		if err != nil {
			t.Fatalf("LoadLibrary(%s): %v", entry.Name, err)
		}
		if len(raw) == 0 || s.Name != entry.Name {
			t.Fatalf("scenario %q: name mismatch with filename", entry.Name)
		}
		var steps []Step
		steps = append(steps, s.OnStart...)
		for _, turn := range s.Turns {
			steps = append(steps, turn.Steps...)
		}
		for _, step := range steps {
			checkStepLines(t, entry.Name, step, vars)
		}
		if s.Provider == ProviderClaude {
			checkClaudeFraming(t, entry.Name, steps, vars)
		}
		for method, tmpl := range codexResponses(s) {
			line := vars.Substitute(tmpl)
			if !json.Valid([]byte(line)) {
				t.Errorf("scenario %q: codex response for %s is not valid JSON: %s", entry.Name, method, line)
			}
		}
	}
}

func checkStepLines(t *testing.T, name string, step Step, vars Vars) {
	t.Helper()
	var lines []string
	if step.Emit != nil {
		lines = step.Emit.Lines
	}
	if step.Approval != nil {
		for _, sub := range append(append([]Step{}, step.Approval.OnAllow...), step.Approval.OnDeny...) {
			checkStepLines(t, name, sub, vars)
		}
	}
	for _, sub := range repeatBody(step) {
		checkStepLines(t, name, sub, vars)
	}
	for _, line := range lines {
		substituted := vars.Substitute(line)
		if strings.Contains(substituted, "${") {
			t.Errorf("scenario %q: line has an unknown ${VAR} token: %s", name, substituted)
		}
		if !json.Valid([]byte(substituted)) {
			t.Errorf("scenario %q: emit line is not valid JSON: %s", name, substituted)
		}
	}
}

// checkClaudeFraming enforces the two protocol invariants Claude
// scenarios must respect because the mock's claude adapter owns the
// per-turn init + user echo frames:
//
//  1. No scenario line may be a system/init envelope — the adapter
//     emits one per user turn; a scenario copy would double it.
//  2. Every assistant envelope carrying text/thinking must have a
//     preceding stream_event message_start with the same message id —
//     that id registration is what makes the app's parser dedupe the
//     coalesced envelope instead of rendering its content twice.
func checkClaudeFraming(t *testing.T, name string, steps []Step, vars Vars) {
	t.Helper()
	started := map[string]bool{}
	var walk func(steps []Step)
	walk = func(steps []Step) {
		for _, step := range steps {
			if step.Approval != nil {
				walk(step.Approval.OnAllow)
				walk(step.Approval.OnDeny)
			}
			walk(repeatBody(step))
			if step.Emit == nil {
				continue
			}
			for _, raw := range step.Emit.Lines {
				line := vars.Substitute(raw)
				var env struct {
					Type    string `json:"type"`
					Subtype string `json:"subtype"`
					Event   string `json:"event"`
					Data    struct {
						Message struct {
							ID string `json:"id"`
						} `json:"message"`
					} `json:"data"`
					Message struct {
						ID      string `json:"id"`
						Content []struct {
							Type string `json:"type"`
						} `json:"content"`
					} `json:"message"`
				}
				if json.Unmarshal([]byte(line), &env) != nil {
					continue // structural validity is checked elsewhere
				}
				if env.Type == "system" && env.Subtype == "init" {
					t.Errorf("scenario %q: system/init is adapter-emitted; remove the scenario line: %s", name, line)
				}
				if env.Type == "stream_event" && env.Event == "message_start" {
					started[env.Data.Message.ID] = true
				}
				if env.Type == "assistant" {
					for _, block := range env.Message.Content {
						if (block.Type == "text" || block.Type == "thinking") && !started[env.Message.ID] {
							t.Errorf("scenario %q: assistant envelope %q carries %s but no prior message_start registered its id — the app would render its content twice", name, env.Message.ID, block.Type)
						}
					}
				}
			}
		}
	}
	walk(steps)
}

// repeatBody returns a repeat step's body (nil for every other step), so
// the library walkers reach lines nested inside a loop. A scenario whose
// only emits live in a repeat must not slip past these invariants.
func repeatBody(step Step) []Step {
	if step.Repeat == nil {
		return nil
	}
	return step.Repeat.Steps
}

func codexResponses(s *Scenario) map[string]string {
	if s.Codex == nil {
		return nil
	}
	return s.Codex.Responses
}

func TestDefaultNameCoversBothProviders(t *testing.T) {
	for _, provider := range []string{ProviderClaude, ProviderCodex} {
		name, err := DefaultName(provider)
		if err != nil {
			t.Fatalf("DefaultName(%s): %v", provider, err)
		}
		if _, s, err := LoadLibrary(name); err != nil || s.Provider != provider {
			t.Fatalf("default %q for %s: %v (provider %q)", name, provider, err, s.Provider)
		}
	}
	if _, err := DefaultName("gemini"); err == nil {
		t.Fatal("DefaultName accepted an unknown provider")
	}
}
