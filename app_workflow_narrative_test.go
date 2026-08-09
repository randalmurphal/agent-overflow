package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/engine"
	workflowrunner "agent-overflow/internal/workflow/runner"
)

// A `read-only` phase runs in a session that denies every file write, so the
// narrative it is asked for arrives as prose and the runner lifts it into the
// file. Before this, a completed read-only run left every attempt directory
// empty, the wake pointed at a path nothing had created, and the triage seed read
// "narrative unavailable".
//
// The fixture mixes access levels in ONE run so both halves of the rule are
// proven against the same binary: the mock writes the narrative file whenever the
// prompt names a path, which only a writing phase's suffix does. So the read-only
// phase's file can only come from recovery, and the writing phase's can only come
// from the agent — and the recovery must leave that one alone.
func TestWorkflowNarrativeIsRecoveredOnlyWhenTheAgentWroteNone(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeMixedAccessWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeNarrativeClaude(t),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "mixed-access", "shared", "exercise narratives",
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() {
		if item.WorktreePath != "" {
			_ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true)
		}
	})

	recovered := readAttemptNarrative(t, app, item.ID, "survey")
	if !strings.HasPrefix(recovered, workflowrunner.RecoveredNarrativeHeader+"\n\n") {
		t.Fatalf("read-only phase narrative is not marked as recovered:\n%s", recovered)
	}
	if !strings.Contains(recovered, narrativeMockProse) {
		t.Fatalf("read-only phase narrative lost the session's final message:\n%s", recovered)
	}

	authored := readAttemptNarrative(t, app, item.ID, "apply")
	if authored != narrativeMockAuthored {
		t.Fatalf("writing phase narrative = %q, want the agent's own file %q", authored, narrativeMockAuthored)
	}
	if strings.Contains(authored, workflowrunner.RecoveredNarrativeHeader) {
		t.Fatalf("recovery overwrote the agent's own narrative:\n%s", authored)
	}
}

// Settling is bookkeeping on an outcome the engine has already accepted, so its
// two refusals matter as much as its writes: it never replaces a file that is
// already there, and it never invents one for a turn that produced no account at
// any tier. The three sources are ordered — the element's own file, then the
// `narrative` it authored into its envelope, then the D39 recovery — and only
// the last one is marked as recovered.
func TestSettleAttemptNarrativeRefusesToOverwriteOrInvent(t *testing.T) {
	app := newTestAppWithStore(t)
	runner := newWorkflowAppRunner(app, t.TempDir(), nil)
	envelope := json.RawMessage(`{"status":"done","outputs":{"report":"ok"}}`)

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
		seedAssistantText(t, app, attempt.threadID, "prose the recovery must not use")
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
		seedAssistantText(t, app, attempt.threadID, "I read the callers and found two")
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
		seedAssistantText(t, app, attempt.threadID, "prose the envelope field must beat")
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
	app := newTestAppWithStore(t)
	runner := newWorkflowAppRunner(app, t.TempDir(), nil)
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

// The whole reason `narrative` is a control field: Codex constrains EVERY
// assistant message of a schema'd turn, so a read-only element cannot send prose
// at all and the account has to ride in the envelope. End to end, the field
// becomes the attempt's narrative file — authored, not recovered — and never
// reaches the persisted envelope the gate and the wake read.
func TestWorkflowEnvelopeNarrativeBecomesTheAttemptNarrative(t *testing.T) {
	app, _ := setupE2EApp(t)
	configRoot := t.TempDir()
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)
	projectRow = mustReloadProject(t, app.store, projectRow.ID)
	writeUndeclaredAccessWorkflow(t, configRoot)
	writeWorkspaceProfile(t, configRoot, projectRow.Slug, `
base_branch: main
reliability:
  watchdog: 1h
  backoff: [5ms]
`)
	if _, err := app.settings.Update(map[string]any{
		"claudeBinaryPath": writeEnvelopeNarrativeClaude(t),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "undeclared-access", "shared", "exercise envelope narratives",
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")
	t.Cleanup(func() {
		if item.WorktreePath != "" {
			_ = app.gitCore().RemoveWorktreeForce(repo, item.WorktreePath, true)
		}
	})

	narrative := readAttemptNarrative(t, app, item.ID, "survey")
	if narrative != envelopeNarrativeAccount+"\n" {
		t.Fatalf("narrative = %q, want the envelope's own account", narrative)
	}
	if strings.Contains(narrative, workflowrunner.RecoveredNarrativeHeader) {
		t.Fatalf("an authored envelope narrative was marked as recovered:\n%s", narrative)
	}
	if strings.Contains(narrative, narrativeMockProse) {
		t.Fatalf("the envelope field lost to the session's prose:\n%s", narrative)
	}
	phases := listWorkflowPhases(t, app, item.ID)
	if len(phases) != 1 {
		t.Fatalf("phases = %+v", phases)
	}
	persisted := string(phases[0].OutputEnvelope)
	if strings.Contains(persisted, "narrative") || strings.Contains(persisted, envelopeNarrativeAccount) {
		t.Fatalf("the persisted envelope carried prose: %s", persisted)
	}
	if outputs := decodeEnvelopeOutputs(t, phases[0].OutputEnvelope); outputs["report"] != "deliverable.md" {
		t.Fatalf("stripping damaged the outputs = %v", outputs)
	}
}

const envelopeNarrativeAccount = "I surveyed the resolver and found one binding"

// writeEnvelopeNarrativeClaude is a mock read-only element that does what the
// suffix now asks: it puts its account in the envelope's `narrative` field. It
// also speaks prose, so the authored field has to beat the D39 recovery rather
// than merely fill in for it.
func writeEnvelopeNarrativeClaude(t *testing.T) string {
	t.Helper()
	status := `{"status":"done","outputs":{"report":"deliverable.md"},"question":null,"reason":null,` +
		`"narrative":"` + envelopeNarrativeAccount + `"}`
	script := `#!/bin/bash
while IFS= read -r line; do
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"envelope-narrative","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"` + narrativeMockProse + `"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + status + `}'
done
`
	return writeExecutable(t, "envelope-narrative-claude.sh", script)
}

func seedAssistantText(t *testing.T, app *App, threadID, text string) {
	t.Helper()
	if err := app.store.CreateThread(store.Thread{
		ID: threadID, ProjectID: defaultTestProjectID, ProjectPath: "/tmp/project",
		Title: threadID, Provider: "claude", Model: "sonnet", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.store.InsertItem(store.Item{
		ID: threadID + ":text", ThreadID: threadID, TurnIndex: 0, ItemIndex: 1,
		Kind: workflowAssistantTextKind, Role: "assistant", Status: "completed",
		Summary: text, CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}
}

func readAttemptNarrative(t *testing.T, app *App, itemID, phaseID string) string {
	t.Helper()
	path, err := workflowrunner.NarrativePath(app.workflowDataRoot(), itemID, phaseID, 1)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s narrative: %v", phaseID, err)
	}
	return string(contents)
}

const (
	narrativeMockProse    = "I surveyed the callers and found two"
	narrativeMockAuthored = "the agent wrote this itself\n"
)

// writeNarrativeClaude is a mock that always speaks prose and writes the
// narrative file only when the prompt names one — which is exactly the
// difference between the write-access suffix and the read-only one.
func writeNarrativeClaude(t *testing.T) string {
	t.Helper()
	status := `{"status":"done","outputs":{"report":"deliverable.md"},"question":null,"reason":null}`
	script := `#!/bin/bash
while IFS= read -r line; do
  narrative=$(printf '%s' "$line" | grep -o '/[^" ]*/narrative\.md' | head -1)
  if [ -n "$narrative" ]; then
    printf '` + narrativeMockAuthored + `' > "$narrative"
  fi
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"narrative","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}'
  printf '%s\n' '{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"` + narrativeMockProse + `"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + status + `}'
done
`
	return writeExecutable(t, "narrative-claude.sh", script)
}
