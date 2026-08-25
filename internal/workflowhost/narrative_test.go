package workflowhost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// Settling is bookkeeping on an outcome the engine has already accepted, so its
// two refusals matter as much as its writes: it never replaces a file that is
// already there, and it never invents one for a turn that produced no account at
// any tier. The three sources are ordered — the element's own file, then the
// `narrative` it authored into its envelope, then the D39 recovery — and only
// the last one is marked as recovered.
func TestSettleAttemptNarrativeRefusesToOverwriteOrInvent(t *testing.T) {
	host := &fakeHost{}
	runner := newTestRunner(t, host, nil, nil)
	envelope := json.RawMessage(`{"status":"done","outputs":{"report":"ok"}}`)

	// The session's own prose reaches the recovery through the one seam that
	// reads it, so a subtest states what its thread said by naming it here.
	said := map[string]string{}
	host.assistantTexts = func(threadID string) ([]string, error) {
		if text, ok := said[threadID]; ok {
			return []string{text}, nil
		}
		return nil, nil
	}

	newAttempt := func(t *testing.T, threadID string) *workflowAttempt {
		t.Helper()
		path := filepath.Join(t.TempDir(), "attempt", "narrative.md")
		return &workflowAttempt{
			workflowCompletion: workflowCompletion{
				key: engine.RunKey{ItemID: "item", PhaseID: "survey", Attempt: 1}, narrativePath: path,
			},
			threadID: threadID,
		}
	}

	t.Run("a silent session leaves no file", func(t *testing.T) {
		attempt := newAttempt(t, "silent-thread")
		runner.settleAttemptNarrative(attempt, "", envelope)
		if _, err := os.Stat(attempt.narrativePath); !os.IsNotExist(err) {
			t.Fatalf("stat(%q) = %v, want the file to be absent", attempt.narrativePath, err)
		}
	})

	t.Run("an existing file survives", func(t *testing.T) {
		attempt := newAttempt(t, "authored-thread")
		said[attempt.threadID] = "prose the recovery must not use"
		if err := os.MkdirAll(filepath.Dir(attempt.narrativePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(attempt.narrativePath, []byte("the agent's own account"), 0o600); err != nil {
			t.Fatal(err)
		}
		runner.settleAttemptNarrative(attempt, "the envelope field must not win either", envelope)
		contents, err := os.ReadFile(attempt.narrativePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != "the agent's own account" {
			t.Fatalf("narrative = %q, want the agent's own file untouched", contents)
		}
	})

	t.Run("prose becomes the narrative", func(t *testing.T) {
		attempt := newAttempt(t, "speaking-thread")
		said[attempt.threadID] = "I read the callers and found two"
		runner.settleAttemptNarrative(attempt, "", envelope)
		contents, err := os.ReadFile(attempt.narrativePath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(contents), workflowrunner.RecoveredNarrativeHeader) ||
			!strings.Contains(string(contents), "I read the callers and found two") {
			t.Fatalf("narrative = %q", contents)
		}
	})

	// The element deliberately put this account in its envelope, so it is
	// authored exactly as a file it wrote would be — no recovered header — and it
	// beats whatever the session happened to say.
	t.Run("an authored envelope narrative is not marked as recovered", func(t *testing.T) {
		attempt := newAttempt(t, "envelope-thread")
		said[attempt.threadID] = "prose the envelope field must beat"
		runner.settleAttemptNarrative(attempt, "I surveyed the resolver and found one binding", envelope)
		contents, err := os.ReadFile(attempt.narrativePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(contents) != "I surveyed the resolver and found one binding\n" {
			t.Fatalf("narrative = %q, want the authored account verbatim", contents)
		}
		if strings.Contains(string(contents), workflowrunner.RecoveredNarrativeHeader) {
			t.Fatalf("an authored envelope narrative was marked as recovered:\n%s", contents)
		}
	})
}

// The `narrative` control field is lifted out at the one seam every agent-backed
// turn reports through, so nothing downstream — the gate, a join's `units`
// results, the persisted attempt envelope — can ever see prose in an envelope.
func TestWorkflowFinishStripsTheEnvelopeNarrative(t *testing.T) {
	runner := newTestRunner(t, nil, nil, nil)
	narrativePath := filepath.Join(t.TempDir(), "attempt", "narrative.md")
	delivered := make(chan engine.Outcome, 1)
	attempt := &workflowAttempt{
		workflowCompletion: workflowCompletion{
			key: engine.RunKey{ItemID: "item", PhaseID: "survey", Attempt: 1}, narrativePath: narrativePath,
		},
		threadID: "stripping-thread",
		complete: func(outcome engine.Outcome) { delivered <- outcome },
	}
	runKey := workflowRunKey(attempt.key)
	runner.mu.Lock()
	runner.runs[runKey] = attempt
	runner.mu.Unlock()

	runner.finish(runKey, engine.Outcome{
		Kind: engine.OutcomeQuestion,
		Envelope: json.RawMessage(
			`{"status":"question","outputs":null,"question":"which branch?","reason":null,"narrative":"I got as far as the resolver"}`,
		),
	})
	outcome := <-delivered
	if strings.Contains(string(outcome.Envelope), "narrative") ||
		strings.Contains(string(outcome.Envelope), "I got as far as the resolver") {
		t.Fatalf("the engine was handed prose in the envelope: %s", outcome.Envelope)
	}
	if !strings.Contains(string(outcome.Envelope), `"which branch?"`) {
		t.Fatalf("stripping damaged the envelope: %s", outcome.Envelope)
	}
	contents, err := os.ReadFile(narrativePath)
	if err != nil {
		t.Fatalf("a question envelope's narrative was not written: %v", err)
	}
	if string(contents) != "I got as far as the resolver\n" {
		t.Fatalf("narrative = %q", contents)
	}
}
