package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// A cold resume is proven by the provider's start alone. Only the provider's
// report that the conversation is gone, empty or of unproven content makes
// the context unavailable, and the error names it; any other failure is the
// start's own. Neither keeps the temporary schema.
func TestWorkflowContinuationProvesColdProviderContext(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		start       func(context.Context, string) error
		unavailable bool
		cancelled   bool
	}{
		{
			name: "claude content unproven", provider: string(provider.Claude), unavailable: true,
			start: func(context.Context, string) error {
				return fmt.Errorf("%w: get_context_usage refused: Unsupported control request subtype: get_context_usage", claude.ErrContentUnproven)
			},
		},
		{
			name: "claude has no such conversation", provider: string(provider.Claude), unavailable: true,
			start: func(context.Context, string) error {
				return fmt.Errorf("%w: No conversation found with session ID: provider-session", claude.ErrSessionNotFound)
			},
		},
		{
			name: "claude conversation holds no user message", provider: string(provider.Claude), unavailable: true,
			start: func(context.Context, string) error {
				return fmt.Errorf("%w: get_context_usage counts 0 user-message tokens", claude.ErrConversationEmpty)
			},
		},
		{
			name: "codex rejects cursor", provider: string(provider.Codex), unavailable: true,
			start: func(context.Context, string) error {
				return fmt.Errorf("spawn Codex: %w", &codex.RPCError{
					Method: "thread/resume", Code: -32600, Message: "no rollout found for thread id provider-session",
				})
			},
		},
		{
			name: "claude initialized on another session", provider: string(provider.Claude),
			start: func(context.Context, string) error {
				return fmt.Errorf("%w: --resume named provider-session", claude.ErrResumedOtherSession)
			},
		},
		{
			name: "claude exited before init", provider: string(provider.Claude),
			start: func(context.Context, string) error {
				return errors.New("claude: the process exited before it initialized")
			},
		},
		{
			name: "cancelled while claude loads", provider: string(provider.Claude), cancelled: true,
			start: func(ctx context.Context, _ string) error {
				<-ctx.Done()
				return fmt.Errorf("claude: wait for the session to initialize: %w", ctx.Err())
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thread := store.Thread{
				ID: "cold-context-" + strings.ReplaceAll(tc.name, " ", "-"), Mode: "workflow",
				Provider: tc.provider, SessionRef: "provider-session", WorkspacePath: t.TempDir(),
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			start := tc.start
			if tc.cancelled {
				// Cancelled while the provider loads, after the runner's own
				// check of ctx.
				start = func(ctx context.Context, threadID string) error {
					cancel()
					return tc.start(ctx, threadID)
				}
			}
			var schemaAtStart json.RawMessage
			var runner *Runner
			runner = newTestRunner(t, &fakeHost{startSessionTakingLock: func(ctx context.Context, threadID string) error {
				schemaAtStart = runner.schemaForThread(threadID)
				return start(ctx, threadID)
			}}, nil, nil)

			started, err := runner.ensureReusableWorkflowSession(
				ctx, thread, json.RawMessage(`{"type":"object"}`),
			)
			if started || err == nil || errors.Is(err, engine.ErrProviderContextUnavailable) != tc.unavailable {
				t.Fatalf("availability = started %v, err %v; want unavailable %v", started, err, tc.unavailable)
			}
			if tc.unavailable && !strings.Contains(err.Error(), tc.start(t.Context(), thread.ID).Error()) {
				t.Fatalf("unavailable = %v, want it to name the provider's report", err)
			}
			if len(schemaAtStart) == 0 {
				t.Fatal("the start ran without the temporary schema")
			}
			if tc.cancelled && !strings.Contains(err.Error(), "startup cancelled") {
				t.Fatalf("cancelled availability = %v, want a cancelled start", err)
			}
			if schema := runner.schemaForThread(thread.ID); len(schema) != 0 {
				t.Fatalf("failed proof retained temporary schema: %s", schema)
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

// A live session is proven too, not accepted because it is registered, and
// its reuse registers no temporary schema: the runner did not start it.
func TestWorkflowContinuationProvesALiveSession(t *testing.T) {
	thread := store.Thread{
		ID: "live-context-proven", Mode: "workflow",
		Provider: string(provider.Claude), SessionRef: "provider-session",
	}
	var proofErr error
	var schemaAtProof json.RawMessage
	var runner *Runner
	runner = newTestRunner(t, &fakeHost{
		sessionActive: func(string) bool { return true },
		startSessionTakingLock: func(context.Context, string) error {
			return errors.New("a live session must not be started again")
		},
		proveSession: func(_ context.Context, threadID string, started bool) error {
			if started {
				return errors.New("the live session was not the one proven")
			}
			schemaAtProof = runner.schemaForThread(threadID)
			return proofErr
		},
	}, nil, nil)
	schema := json.RawMessage(`{"type":"object"}`)

	started, err := runner.ensureReusableWorkflowSession(t.Context(), thread, schema)
	if started || err != nil || len(schemaAtProof) != 0 {
		t.Fatalf("live reuse = started %v, err %v, schema %s; want reuse with no schema", started, err, schemaAtProof)
	}

	proofErr = fmt.Errorf("%w: No conversation found with session ID: provider-session", claude.ErrSessionNotFound)
	started, err = runner.ensureReusableWorkflowSession(t.Context(), thread, schema)
	if started || !errors.Is(err, engine.ErrProviderContextUnavailable) {
		t.Fatalf("unproven live session = started %v, err %v; want unavailable", started, err)
	}
	if registered := runner.schemaForThread(thread.ID); len(registered) != 0 {
		t.Fatalf("an unproven live session left a schema: %s", registered)
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
