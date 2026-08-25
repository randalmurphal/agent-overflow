package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

func TestWorkflowContinuationProvesColdProviderContext(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		start    func(context.Context, string) error
	}{
		{
			name: "claude transcript missing", provider: string(provider.Claude),
			start: func(context.Context, string) error {
				return fmt.Errorf("Claude start must not run after the transcript preflight failed")
			},
		},
		{
			name: "codex rejects cursor", provider: string(provider.Codex),
			start: func(context.Context, string) error {
				return fmt.Errorf("spawn Codex: %w", &codex.RPCError{
					Method: "thread/resume", Code: -32600, Message: "no rollout found for thread id provider-session",
				})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thread := store.Thread{
				ID: "cold-context-" + strings.ReplaceAll(tc.name, " ", "-"), Mode: "workflow",
				Provider: tc.provider, SessionRef: "provider-session", WorkspacePath: t.TempDir(),
			}
			runner := newTestRunner(t, &fakeHost{startSessionTakingLock: tc.start}, nil, nil)

			started, err := runner.ensureReusableWorkflowSession(
				t.Context(), thread, json.RawMessage(`{"type":"object"}`),
			)
			if started || !errors.Is(err, engine.ErrProviderContextUnavailable) {
				t.Fatalf("availability = started %v, err %v; want unavailable", started, err)
			}
			if schema := runner.schemaForThread(thread.ID); len(schema) != 0 {
				t.Fatalf("failed preflight retained temporary schema: %s", schema)
			}
		})
	}
}

func TestWorkflowContinuationAcceptsLiveContextBeforeCursorPersistence(t *testing.T) {
	thread := store.Thread{
		ID: "live-context-before-cursor", Mode: "workflow",
		Provider: string(provider.Codex), SessionRef: "",
	}
	live := true
	runner := newTestRunner(t, &fakeHost{
		sessionActive: func(string) bool { return live },
		startSessionTakingLock: func(context.Context, string) error {
			return errors.New("cold resume must not run while the provider process is live")
		},
	}, nil, nil)

	started, err := runner.ensureReusableWorkflowSession(
		t.Context(), thread, json.RawMessage(`{"type":"object"}`),
	)
	if err != nil || started {
		t.Fatalf("availability = started %v, err %v; want live context without a cold start", started, err)
	}

	live = false
	started, err = runner.ensureReusableWorkflowSession(
		t.Context(), thread, json.RawMessage(`{"type":"object"}`),
	)
	if started || !errors.Is(err, engine.ErrProviderContextUnavailable) {
		t.Fatalf("cold cursor-less availability = started %v, err %v; want unavailable", started, err)
	}
}

func TestWorkflowThreadDeletionRaceUsesLaunchSemantics(t *testing.T) {
	dataStore := newTestStore(t)
	project := testutil.EnsureProject(t, dataStore, t.TempDir())
	runner := newTestRunner(t, nil, dataStore, nil)
	spec := ThreadSpec{
		ItemID: "item", Label: "phase work", Title: "Workflow: work",
		ProviderName: string(provider.Codex), Model: "gpt-5.5", Access: def.AccessReadOnly,
		Workspace: PreparedWorkspace{Path: project.Path, Project: project},
	}

	continuation, err := engine.ContinueThread("deleted-thread")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.prepareAgentTurnThread(t.Context(), workflowAgentTurnPlan{
		request: engine.RunRequest{Launch: continuation}, thread: spec,
	})
	if !errors.Is(err, engine.ErrProviderContextUnavailable) {
		t.Fatalf("deleted continuation error = %v, want unavailable context", err)
	}

	reuse, err := engine.ReuseThread("deleted-thread")
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.prepareAgentTurnThread(t.Context(), workflowAgentTurnPlan{
		request: engine.RunRequest{Launch: reuse}, thread: spec,
	})
	if !errors.Is(err, engine.ErrProviderContextUnavailable) {
		t.Fatalf("deleted warm reuse error = %v, want the engine to reconstruct it", err)
	}
}

func TestWorkflowRunnerRejectsUnsupportedPhasesAndStopsUnknownRuns(t *testing.T) {
	runner := newTestRunner(t, nil, nil, nil)
	if _, err := runner.Stop(context.Background(), engine.RunKey{ItemID: "missing", PhaseID: "phase", Attempt: 1}); err != nil {
		t.Fatalf("Stop unknown run = %v", err)
	}
	for _, phase := range []def.Phase{
		{ID: "tool", Driver: def.DriverTool, Shape: def.ShapeSingle},
		{ID: "fan", Shape: def.ShapeFanOut},
	} {
		err := runner.Start(context.Background(), engine.RunRequest{
			Phase: phase, Launch: engine.FreshTurn(),
		}, func() {}, func(engine.Outcome) {})
		if err == nil {
			t.Fatalf("unsupported phase %+v succeeded", phase)
		}
	}

	key := engine.RunKey{ItemID: "known", PhaseID: "phase", Attempt: 1}
	runKey := workflowRunKey(key)
	unsubscribed := false
	runner.runs[runKey] = &workflowAttempt{
		workflowCompletion: workflowCompletion{key: key}, threadID: "workflow-thread", unsubscribe: func() { unsubscribed = true },
	}
	runner.schemas["workflow-thread"] = json.RawMessage(`{"type":"object"}`)
	if _, err := runner.Stop(context.Background(), key); err != nil {
		t.Fatalf("Stop known run = %v", err)
	}
	if !unsubscribed || runner.runs[runKey] != nil || len(runner.schemas["workflow-thread"]) != 0 {
		t.Fatalf("known run cleanup: unsubscribed=%v runs=%v schemas=%v", unsubscribed, runner.runs, runner.schemas)
	}
	if drop, err := runner.sendIfActive(
		context.Background(), runKey, "the late retry", "late retry", json.RawMessage(`{"type":"object"}`), 0,
	); err != nil || drop != workflowSendDropUnregistered {
		t.Fatalf("post-stop retry = drop %q, err %v", drop, err)
	}
}

func TestWorkflowTurnErrorTerminalClassification(t *testing.T) {
	if !workflowTurnErrorIsTerminal(provider.ProviderEvent{
		Kind: provider.EventError, Failure: &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryEvent},
	}) {
		t.Fatal("fatal error without a following turn completion was not terminal")
	}
	for _, event := range []provider.ProviderEvent{
		{},
		{Kind: provider.EventError},
		{Kind: provider.EventError, Failure: &provider.FailureMeta{Class: provider.FailureFatal, Boundary: provider.FailureBoundaryTurn}},
	} {
		if workflowTurnErrorIsTerminal(event) {
			t.Fatalf("non-terminal error classified terminal: %+v", event)
		}
	}
}

// The registered phase schema is what a workflow session starts bound to, so
// the read the App's session start goes through has to return it — and has to
// distinguish "registered, deliberately schema-less" (a takeover) from "not
// registered at all", which is a session that must never start.
func TestSessionSchemaForThreadSeparatesRegisteredFromAbsent(t *testing.T) {
	runner := newTestRunner(t, nil, nil, nil)
	if schema, registered := runner.SessionSchemaForThread("unknown-thread"); registered || schema != nil {
		t.Fatalf("unregistered thread = (%s, %v), want no schema and not registered", schema, registered)
	}
	if err := runner.registerTemporarySchema("phase-thread", json.RawMessage(`{"type":"object"}`)); err != nil {
		t.Fatal(err)
	}
	schema, registered := runner.SessionSchemaForThread("phase-thread")
	if !registered || string(schema) != `{"type":"object"}` {
		t.Fatalf("registered thread = (%s, %v)", schema, registered)
	}
}
