package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// TestWorkflowPhaseAccessMapsToThreadRuntimeMode is the end-to-end proof for
// decision D22: a phase's `access` declaration is enforced at the provider
// session, not merely used to decide whether a worktree is cut.
//
// The fixture deliberately mixes access levels in ONE run so the two axes are
// visibly independent: because the write phase exists, the whole item gets a
// worktree and both phases execute in it — yet the read-only phase's session
// must still be restricted. A test with a read-only-only workflow would pass
// even if access were still wired to workspace derivation alone.
//
// Assertions run against the persisted thread row and the SessionOptions
// derived from it, because the row is the source of truth every later session
// start (restart, resume, Answer-continuation) re-derives from.
func TestWorkflowPhaseAccessMapsToThreadRuntimeMode(t *testing.T) {
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
		"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), false, "done"),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "mixed-access", "shared", "exercise access",
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

	// A writing phase in the graph means the run is provisioned a worktree —
	// which both phases share. Access is therefore NOT derivable from the
	// workspace here; it has to come from the phase declaration.
	if item.WorktreePath == "" {
		t.Fatal("mixed-access workflow did not provision a worktree for its write phase")
	}

	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	threadsByPhase := map[string]string{}
	for _, phase := range detail.Phases {
		threadsByPhase[phase.PhaseID] = phase.ThreadID
	}
	for _, phaseID := range []string{"survey", "apply"} {
		if threadsByPhase[phaseID] == "" {
			t.Fatalf("phase %q produced no thread: %+v", phaseID, detail.Phases)
		}
	}

	cases := []struct {
		phaseID            string
		wantRuntimeMode    provider.RuntimeMode
		wantPermissionMode string
		wantToolsRemoved   bool
	}{
		{"survey", provider.RuntimeReadOnly, "dontAsk", true},
		{"apply", provider.RuntimeFullAccess, "bypassPermissions", false},
	}
	for _, tc := range cases {
		t.Run(tc.phaseID, func(t *testing.T) {
			thread, err := app.store.GetThread(threadsByPhase[tc.phaseID])
			if err != nil {
				t.Fatal(err)
			}
			if thread.RuntimeMode != string(tc.wantRuntimeMode) {
				t.Fatalf("thread row runtime_mode = %q, want %q", thread.RuntimeMode, tc.wantRuntimeMode)
			}
			// Both phases run in the same worktree — proving the runtime mode
			// came from `access`, not from which workspace was provisioned.
			if thread.WorkspacePath != item.WorktreePath {
				t.Fatalf("thread workspace = %q, want the item worktree %q", thread.WorkspacePath, item.WorktreePath)
			}

			opts, _, err := app.buildSessionOptions(thread)
			if err != nil {
				t.Fatal(err)
			}
			if opts.RuntimeMode != tc.wantRuntimeMode {
				t.Fatalf("SessionOptions.RuntimeMode = %q, want %q", opts.RuntimeMode, tc.wantRuntimeMode)
			}
			cfg := claude.ConfigFromOptions(opts)
			if cfg.BasePermissionMode != tc.wantPermissionMode {
				t.Fatalf("claude BasePermissionMode = %q, want %q", cfg.BasePermissionMode, tc.wantPermissionMode)
			}
			if got := len(cfg.DisallowedTools) > 0; got != tc.wantToolsRemoved {
				t.Fatalf("claude DisallowedTools present = %v (%v), want %v", got, cfg.DisallowedTools, tc.wantToolsRemoved)
			}
		})
	}
}

// TestWorkflowUndeclaredAccessRunsReadOnly pins the default direction at the
// level that matters. A phase that says nothing about access gets no worktree
// AND a restricted session — the two halves of "unset means read-only".
func TestWorkflowUndeclaredAccessRunsReadOnly(t *testing.T) {
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
		"claudeBinaryPath": writeWorkspaceClaude(t, filepath.Join(t.TempDir(), "cwd"), false, "done"),
	}); err != nil {
		t.Fatal(err)
	}
	startWorkflowEngineForTest(t, app, configRoot)

	item, err := app.WorkflowStartRun(
		projectRow.ID, "undeclared-access", "shared", "no access field",
		json.RawMessage(`{}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	item = waitForWorkflowItem(t, app, item.ID, engine.StateDone, "")

	if item.WorktreePath != "" {
		t.Fatalf("undeclared access provisioned a worktree %q — unset must mean read-only", item.WorktreePath)
	}
	detail, err := app.WorkflowGetItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Phases) == 0 {
		t.Fatal("no phases recorded")
	}
	thread, err := app.store.GetThread(detail.Phases[0].ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.RuntimeMode != string(provider.RuntimeReadOnly) {
		t.Fatalf("undeclared-access thread runtime_mode = %q, want read-only", thread.RuntimeMode)
	}
	if thread.WorkspacePath != projectRow.Path {
		t.Fatalf("read-only phase workspace = %q, want the project root %q", thread.WorkspacePath, projectRow.Path)
	}
}

// TestWorkflowPhaseRuntimeModeMapping covers the pure mapping directly,
// including the unset case the YAML fixtures cannot express twice.
func TestWorkflowPhaseRuntimeModeMapping(t *testing.T) {
	cases := map[def.Access]provider.RuntimeMode{
		def.AccessWrite:    provider.RuntimeFullAccess,
		def.AccessReadOnly: provider.RuntimeReadOnly,
		"":                 provider.RuntimeReadOnly,
	}
	for access, want := range cases {
		if got := workflowPhaseRuntimeMode(access); got != want {
			t.Errorf("workflowPhaseRuntimeMode(%q) = %q, want %q", access, got, want)
		}
	}
}

// TestCreateWorkflowThreadRejectsProviderThatCannotEnforceAccess proves the
// phase refuses to start rather than running with an inert access
// declaration. claude-tui hands permissions to the real TUI, so its threads'
// runtime mode is never applied — starting an unattended read-only phase on
// it would silently grant full access.
func TestCreateWorkflowThreadRejectsProviderThatCannotEnforceAccess(t *testing.T) {
	app, _ := setupE2EApp(t)
	repo := testutil.InitGitRepo(t)
	projectRow := testutil.EnsureProject(t, app.store, repo)

	request := engine.RunRequest{
		Key: engine.RunKey{ItemID: "item-access", PhaseID: "survey", Attempt: 1},
		Phase: def.Phase{
			ID:       "survey",
			Driver:   def.DriverAgent,
			Provider: string(provider.ClaudeTUI),
			Model:    "claude-opus-4-7",
			Access:   def.AccessReadOnly,
		},
	}
	_, err := app.createWorkflowThread(request, repo, projectRow)
	if err == nil {
		t.Fatal("createWorkflowThread accepted a provider that cannot enforce runtime modes")
	}
	if !strings.Contains(err.Error(), "does not enforce runtime modes") {
		t.Fatalf("error = %v, want a runtime-mode enforcement message", err)
	}
	// Typed so the engine parks it as a wiring error rather than an
	// agent error — the definition is unrunnable, not the agent misbehaving.
	if !errors.Is(err, engine.ErrWiringFailed) {
		t.Fatalf("error %v is not tagged engine.ErrWiringFailed", err)
	}

	threads, err := app.store.ListThreadsByProject(projectRow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 0 {
		t.Fatalf("refused phase still created %d thread(s)", len(threads))
	}
}

func writeMixedAccessWorkflow(t *testing.T, configRoot string) {
	t.Helper()
	writeAccessWorkflowFixture(t, configRoot, "mixed-access", `id: mixed-access
name: Mixed access
phases:
  - id: survey
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: step.md
    access: read-only
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: apply
  - id: apply
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: step.md
    access: write
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: manual
`)
}

func writeUndeclaredAccessWorkflow(t *testing.T, configRoot string) {
	t.Helper()
	writeAccessWorkflowFixture(t, configRoot, "undeclared-access", `id: undeclared-access
name: Undeclared access
phases:
  - id: survey
    driver: agent
    provider: claude
    model: claude-opus-4-7
    prompt: step.md
    outputs:
      report:
        schema:
          type: string
    gate:
      routes:
        - to: done
cleanup: manual
`)
}

func writeAccessWorkflowFixture(t *testing.T, configRoot, id, definition string) {
	t.Helper()
	dir := filepath.Join(configRoot, "workflows")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "step.md"), []byte("Do the step"), 0o600); err != nil {
		t.Fatal(err)
	}
}
