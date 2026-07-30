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

// The recovery is bookkeeping on an outcome the engine has already accepted, so
// its two refusals matter as much as its one write: it never replaces a file that
// is already there, and it never invents one for a session that said nothing.
func TestRecoverAttemptNarrativeRefusesToOverwriteOrInvent(t *testing.T) {
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
		runner.recoverAttemptNarrative(attempt, envelope)
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
		runner.recoverAttemptNarrative(attempt, envelope)
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
		runner.recoverAttemptNarrative(attempt, envelope)
		contents, err := os.ReadFile(attempt.narrativePath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(contents), workflowrunner.RecoveredNarrativeHeader) ||
			!strings.Contains(string(contents), "I read the callers and found two") {
			t.Fatalf("narrative = %q", contents)
		}
	})
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
