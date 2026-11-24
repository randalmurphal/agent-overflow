package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflowtools"
)

// postMCPTool posts one tools/call and returns the raw JSON-RPC reply.
func postMCPTool(endpoint, name string, args any) (string, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": name, "arguments": args,
	}})
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: 60 * time.Second}).Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(response.Body)
	return string(raw), errors.Join(err, response.Body.Close())
}

// holdAdmission holds the first call that reaches admitWork for threadID
// there until release is closed, and closes reached once it is held.
func holdAdmission(a *App, threadID string) (reached, release chan struct{}) {
	reached, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	a.admitWorkForTest = func(id string) {
		if id == threadID {
			once.Do(func() {
				close(reached)
				<-release
			})
		}
	}
	return reached, release
}

func awaitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// A thread_spawn that passed authentication and is held before admission
// while a Stop ends its session starts nothing once released, however late
// it gets there; a spawn from the session started afterwards does.
func TestASpawnFromASessionAStopEndedStartsNothing(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "done", "done", "done")
	t.Cleanup(func() { _ = f.app.threadMCPServer().Close() })
	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	live, ok := f.app.sessionManager().get(f.caller.ID)
	if !ok {
		t.Fatal("the caller has no session")
	}
	endpoint := threadMCPEndpoint(t, f.app, f.caller, live.Token)
	reached, release := holdAdmission(f.app, f.caller.ID)
	type reply struct {
		raw string
		err error
	}
	late := make(chan reply, 1)
	go func() {
		raw, err := postMCPTool(endpoint, "thread_spawn", map[string]any{"prompt": "late work"})
		late <- reply{raw, err}
	}()
	awaitClosed(t, reached, "the spawn reaching admission")

	var stop ThreadAgentStop
	f.app.restThreadHere(t.Context(), f.caller.ID, userRestStop(), map[string]bool{}, &stop)
	if len(stop.Failures) != 0 {
		t.Fatalf("the rest failed: %v", stop.Failures)
	}
	if _, ok := f.app.sessionManager().get(f.caller.ID); ok {
		t.Fatal("the rest kept the caller's session")
	}
	close(release)
	got := <-late
	if got.err != nil || !strings.Contains(got.raw, `"isError":true`) || !strings.Contains(got.raw, "The agent session that made this call has ended") {
		t.Fatalf("the late spawn = %s, %v; want it refused as from an ended session", got.raw, got.err)
	}
	if rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0); err != nil || len(rows) != 0 {
		t.Fatalf("requests after the late spawn = %+v, %v; want none", rows, err)
	}

	if err := f.app.startSessionTakingLock(context.Background(), f.caller.ID); err != nil {
		t.Fatalf("StartSession again: %v", err)
	}
	fresh, ok := f.app.sessionManager().get(f.caller.ID)
	if !ok || fresh.Token == live.Token {
		t.Fatalf("the new session = %+v, %v; want a new one", fresh, ok)
	}
	raw, err := postMCPTool(threadMCPEndpoint(t, f.app, f.caller, fresh.Token), "thread_spawn", map[string]any{"prompt": "new work"})
	if err != nil || strings.Contains(raw, `"isError":true`) {
		t.Fatalf("a spawn from the new session = %s, %v; want it admitted", raw, err)
	}
	if rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0); err != nil || len(rows) != 1 {
		t.Fatalf("requests after the new session's spawn = %d, %v; want one", len(rows), err)
	}
}

// A run_start that passed authentication and is held before admission while
// a Stop ends its session starts and binds no run once released; one from
// the session started afterwards does.
func TestARunStartFromASessionAStopEndedStartsNoRun(t *testing.T) {
	t.Parallel()
	fixture := newCLIFixture(t)
	t.Cleanup(func() { _ = fixture.app.workflowMCPServer().Close() })
	thread, err := fixture.app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: fixture.project.ID, Provider: "claude", Model: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	live, ok := fixture.app.sessionManager().get(thread.ID)
	if !ok {
		t.Fatal("the thread has no session")
	}
	endpoint := workflowMCPEndpoint(t, fixture.app, thread, live.Token)
	reached, release := holdAdmission(fixture.app, thread.ID)
	late := make(chan string, 1)
	go func() {
		raw, err := postMCPTool(endpoint, "run_start", map[string]any{"workflow_id": "tool-flow", "goal": "late"})
		if err != nil {
			raw = err.Error()
		}
		late <- raw
	}()
	awaitClosed(t, reached, "the run start reaching admission")

	var stop ThreadAgentStop
	fixture.app.restThreadHere(t.Context(), thread.ID, userRestStop(), map[string]bool{}, &stop)
	if _, ok := fixture.app.sessionManager().get(thread.ID); ok {
		t.Fatal("the rest kept the thread's session")
	}
	close(release)
	if raw := <-late; !strings.Contains(raw, `"isError":true`) || !strings.Contains(raw, "The agent session that made this call has ended") {
		t.Fatalf("the late run start = %s; want it refused as from an ended session", raw)
	}
	if runs, err := fixture.app.store.ListWorkItems(store.WorkItemListFilter{}); err != nil || len(runs) != 0 {
		t.Fatalf("runs after the late start = %d, %v; want none", len(runs), err)
	}

	if err := fixture.app.startSessionTakingLock(context.Background(), thread.ID); err != nil {
		t.Fatalf("StartSession again: %v", err)
	}
	fresh, ok := fixture.app.sessionManager().get(thread.ID)
	if !ok || fresh.Token == live.Token {
		t.Fatalf("the new session = %+v, %v; want a new one", fresh, ok)
	}
	raw, err := postMCPTool(workflowMCPEndpoint(t, fixture.app, thread, fresh.Token), "run_start", map[string]any{"workflow_id": "tool-flow", "goal": "new"})
	if err != nil || strings.Contains(raw, `"isError":true`) {
		t.Fatalf("a run start from the new session = %s, %v; want it admitted", raw, err)
	}
	runs, err := fixture.app.store.ListWorkItems(store.WorkItemListFilter{})
	if err != nil || len(runs) != 1 || runs[0].OriginThreadID != thread.ID {
		t.Fatalf("runs after the new session's start = %+v, %v; want one bound to the thread", runs, err)
	}
}

// Every admission point refuses work a call starts for a session that is no
// longer the thread's, and admits it for the thread's current session and
// for a call no session made.
func TestEveryAdmissionRefusesWorkOfAnEndedSession(t *testing.T) {
	t.Parallel()
	t.Run("thread tools", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		other := f.plainThread(t)
		f.app.sessionManager().put(f.caller.ID, session{Token: "current", Provider: string(provider.Claude)})
		ended := f.callerIdentity()
		ended.Session = "ended"
		later := time.Now().Add(time.Hour).UnixMilli()
		for name, admit := range map[string]func(threadtools.Caller) error{
			"spawn": func(c threadtools.Caller) error {
				_, err := f.adapter().Spawn(t.Context(), c, threadtools.SpawnCall{Prompt: "go"})
				return err
			},
			"send": func(c threadtools.Caller) error {
				_, err := f.adapter().Send(t.Context(), c, threadtools.SendCall{ThreadID: other.ID, Message: "hi"})
				return err
			},
			"ask": func(c threadtools.Caller) error {
				_, err := f.adapter().Ask(t.Context(), c, threadtools.AskCall{ThreadID: other.ID, Question: "why?"})
				return err
			},
			"remind": func(c threadtools.Caller) error {
				_, err := f.adapter().Remind(t.Context(), c, threadtools.RemindCall{DueAtUnixMs: later, Note: "n"})
				return err
			},
			"loop": func(c threadtools.Caller) error {
				_, err := f.adapter().Loop(t.Context(), c, threadtools.LoopCall{Label: "board", Note: "n", EverySeconds: 3600})
				return err
			},
		} {
			if err := admit(ended); !errors.Is(err, errSessionEnded) {
				t.Fatalf("%s from an ended session = %v, want it refused", name, err)
			}
		}
		if rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0); err != nil || len(rows) != 0 {
			t.Fatalf("requests from the ended session = %+v, %v; want none", rows, err)
		}
		current := f.callerIdentity()
		current.Session = "current"
		if _, err := f.adapter().Remind(t.Context(), current, threadtools.RemindCall{DueAtUnixMs: later, Note: "n"}); err != nil {
			t.Fatalf("a reminder from the current session: %v", err)
		}
		if _, err := f.adapter().Remind(t.Context(), f.callerIdentity(), threadtools.RemindCall{DueAtUnixMs: later, Note: "m"}); err != nil {
			t.Fatalf("a reminder no session made: %v", err)
		}
	})
	t.Run("assistant", func(t *testing.T) {
		t.Parallel()
		f := newAssistantFixture(t)
		f.assistantCaller(t)
		f.app.sessionManager().put(f.caller.ID, session{Token: "current", Provider: string(provider.Claude)})
		ended := f.callerIdentity()
		ended.Session = "ended"
		if _, err := f.adapter().NewAssistant(t.Context(), ended, threadtools.NewAssistantCall{Name: "Scout", Role: "Watch."}); !errors.Is(err, errSessionEnded) {
			t.Fatalf("a new assistant from an ended session = %v, want it refused", err)
		}
		if _, err := f.adapter().Watch(t.Context(), ended, threadtools.WatchCall{Label: "w", Command: "true", EverySeconds: 3600}); !errors.Is(err, errSessionEnded) {
			t.Fatalf("a watch from an ended session = %v, want it refused", err)
		}
	})
	t.Run("paired spawn", func(t *testing.T) {
		t.Parallel()
		remotetest.Require(t)
		pair := newReachPair(t)
		pair.source.sessionManager().put(pair.caller.ID, session{Token: "current", Provider: string(provider.Claude)})
		ended := pair.callerIdentity()
		ended.Session = "ended"
		if _, err := pair.adapter().Spawn(t.Context(), ended, threadtools.SpawnCall{Prompt: "go", ComputerID: pair.computer, ProjectID: pair.project}); !errors.Is(err, errSessionEnded) {
			t.Fatalf("a remote spawn from an ended session = %v, want it refused", err)
		}
		if rows, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 10, 0); err != nil || len(rows) != 0 {
			t.Fatalf("requests after the refused spawn = %+v, %v; want none", rows, err)
		}
	})
	t.Run("remote command", func(t *testing.T) {
		t.Parallel()
		remotetest.Require(t)
		source, _, ctx, input := remoteAdmissionFixture(t)
		scope, _ := transport.CallerScopeFrom(ctx)
		source.sessionManager().put(scope.ThreadID, session{Token: "current", Provider: string(provider.Codex)})
		if authenticated, err := source.remoteMCPContext(ctx, remoteMCPAccess{ThreadID: scope.ThreadID, SessionToken: "current"}); err != nil {
			t.Fatalf("remoteMCPContext: %v", err)
		} else if got, _ := transport.CallerScopeFrom(authenticated); got.Session != "current" {
			t.Fatalf("a remote tool call's scope names session %q, want the one it authenticated as", got.Session)
		}
		scope.Session = "ended"
		if _, err := source.AgentRemoteStart(transport.WithCallerScope(ctx, scope), input); !errors.Is(err, errSessionEnded) {
			t.Fatalf("a remote start from an ended session = %v, want it refused", err)
		}
		if _, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err == nil {
			t.Fatal("the refused remote start left a watch")
		}
		scope.Session = "current"
		if _, err := source.AgentRemoteStart(transport.WithCallerScope(ctx, scope), input); err != nil {
			t.Fatalf("a remote start from the current session: %v", err)
		}
	})
	t.Run("workflow", func(t *testing.T) {
		t.Parallel()
		fixture := newCLIFixture(t)
		thread, err := fixture.app.CreateThread(t.Context(), CreateThreadOptions{
			ProjectID: fixture.project.ID, Provider: "claude", Model: "claude-opus-4-7",
		})
		if err != nil {
			t.Fatal(err)
		}
		scope := interactiveScope(fixture, thread.ID)
		result, err := fixture.app.workflowAgentStartRun(transport.WithCallerScope(context.Background(), scope), WorkflowAgentStartInput{WorkflowID: "tool-flow", Goal: "before"})
		if err != nil {
			t.Fatalf("a run start no session made: %v", err)
		}
		fixture.app.sessionManager().put(thread.ID, session{Token: "current", Provider: string(provider.Claude)})
		scope.Session = "ended"
		ctx := transport.WithCallerScope(context.Background(), scope)
		if _, err := fixture.app.workflowAgentStartRun(ctx, WorkflowAgentStartInput{WorkflowID: "tool-flow", Goal: "late"}); !errors.Is(err, errSessionEnded) {
			t.Fatalf("a run start from an ended session = %v, want it refused", err)
		}
		tools := workflowToolsAdapter{a: fixture.app}
		for _, action := range []string{"resume", "rerun", "retry_unit", "retry_failed_units"} {
			if _, err := tools.ControlRun(ctx, workflowtools.ControlInput{Action: action, RunID: result.ItemID}); !errors.Is(err, errSessionEnded) {
				t.Fatalf("run %s from an ended session = %v, want it refused", action, err)
			}
		}
		if err := tools.ResolveRun(ctx, workflowtools.ResolveInput{Decision: "answer", RunID: result.ItemID, Answer: "yes"}); !errors.Is(err, errSessionEnded) {
			t.Fatalf("an answer from an ended session = %v, want it refused", err)
		}
		if runs, err := fixture.app.store.ListWorkItems(store.WorkItemListFilter{}); err != nil || len(runs) != 1 {
			t.Fatalf("runs after the refused start = %d, %v; want the one before", len(runs), err)
		}
	})
}
