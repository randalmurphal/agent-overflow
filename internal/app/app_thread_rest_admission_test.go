package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflowtools"
	"github.com/google/uuid"
)

// While a thread is being brought to rest it starts no new work: no spawn,
// send, ask or reminder, and no loop or watch. Once the rest is over it may
// again.
func TestAThreadBeingStoppedStartsNoThreadWork(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	other := f.plainThread(t)
	caller := f.callerIdentity()
	release := f.app.resting.close(f.caller.ID)
	for name, admit := range map[string]func() error{
		"spawn": func() error {
			_, err := f.adapter().Spawn(t.Context(), caller, threadtools.SpawnCall{Prompt: "go"})
			return err
		},
		"send": func() error {
			_, err := f.adapter().Send(t.Context(), caller, threadtools.SendCall{ThreadID: other.ID, Message: "hi"})
			return err
		},
		"ask": func() error {
			_, err := f.adapter().Ask(t.Context(), caller, threadtools.AskCall{ThreadID: other.ID, Question: "why?"})
			return err
		},
		"remind": func() error {
			_, err := f.adapter().Remind(t.Context(), caller, threadtools.RemindCall{DueAtUnixMs: time.Now().Add(time.Hour).UnixMilli(), Note: "n"})
			return err
		},
		"loop": func() error {
			_, err := f.app.registerAgentWake(store.AgentWake{
				ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board", Note: "n",
				IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli(),
			})
			return err
		},
	} {
		if err := admit(); !errors.Is(err, errThreadResting) {
			release()
			t.Fatalf("%s while the thread is being stopped = %v, want it refused", name, err)
		}
	}
	if rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0); err != nil || len(rows) != 0 {
		release()
		t.Fatalf("requests made while stopping = %+v, %v; want none", rows, err)
	}
	release()
	if _, err := f.adapter().Remind(t.Context(), caller, threadtools.RemindCall{DueAtUnixMs: time.Now().Add(time.Hour).UnixMilli(), Note: "n"}); err != nil {
		t.Fatalf("a reminder after the rest: %v", err)
	}
}

// While a thread is being brought to rest it starts no remote command.
func TestAThreadBeingStoppedStartsNoRemoteCommand(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, _, ctx, input := remoteAdmissionFixture(t)
	scope, _ := transport.CallerScopeFrom(ctx)
	release := source.resting.close(scope.ThreadID)
	_, err := source.AgentRemoteStart(ctx, input)
	if !errors.Is(err, errThreadResting) {
		release()
		t.Fatalf("a remote start while the thread is being stopped = %v, want it refused", err)
	}
	if _, err := source.store.GetRemoteWatch(input.ComputerID, input.Request.ID); err == nil {
		release()
		t.Fatal("the refused remote start left a watch")
	}
	release()
	input.Request.ID = uuid.NewString()
	if _, err := source.AgentRemoteStart(ctx, input); err != nil {
		t.Fatalf("a remote start after the rest: %v", err)
	}
}

// While a thread is being brought to rest it starts nothing on a paired
// computer.
func TestAThreadBeingStoppedSpawnsNothingOnAPairedComputer(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	release := pair.source.resting.close(pair.caller.ID)
	defer release()
	_, err := pair.adapter().Spawn(t.Context(), pair.callerIdentity(), threadtools.SpawnCall{Prompt: "go", ComputerID: pair.computer, ProjectID: pair.project})
	if !errors.Is(err, errThreadResting) {
		t.Fatalf("a remote spawn while the thread is being stopped = %v, want it refused", err)
	}
	if rows, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 10, 0); err != nil || len(rows) != 0 {
		t.Fatalf("requests after the refused spawn = %+v, %v; want none", rows, err)
	}
}

// While an assistant's thread is being brought to rest it creates no
// assistant.
func TestAThreadBeingStoppedCreatesNoAssistant(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.assistantCaller(t)
	before, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	release := f.app.resting.close(f.caller.ID)
	defer release()
	_, err = f.adapter().NewAssistant(t.Context(), f.callerIdentity(), threadtools.NewAssistantCall{Name: "Scout", Role: "Watch."})
	if !errors.Is(err, errThreadResting) {
		t.Fatalf("a new assistant while the thread is being stopped = %v, want it refused", err)
	}
	if after, err := f.app.store.ListThreads(); err != nil || len(after) != len(before) {
		t.Fatalf("threads after the refused creation = %d, %v; had %d", len(after), err, len(before))
	}
}

// While a thread is being brought to rest it starts and binds no workflow
// run, and its agent sets no run going again; it may still pause or cancel
// one.
func TestAThreadBeingStoppedStartsNoWorkflowRun(t *testing.T) {
	t.Parallel()
	fixture := newCLIFixture(t)
	thread, err := fixture.app.CreateThread(t.Context(), CreateThreadOptions{
		ProjectID: fixture.project.ID, Provider: "claude", Model: "claude-opus-4-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := transport.WithCallerScope(context.Background(), interactiveScope(fixture, thread.ID))
	release := fixture.app.resting.close(thread.ID)
	if _, err := fixture.app.workflowAgentStartRun(ctx, WorkflowAgentStartInput{WorkflowID: "tool-flow", Goal: "while stopping"}); !errors.Is(err, errThreadResting) {
		release()
		t.Fatalf("a run start while the thread is being stopped = %v, want it refused", err)
	}
	if runs, err := fixture.app.store.ListWorkItems(store.WorkItemListFilter{}); err != nil || len(runs) != 0 {
		release()
		t.Fatalf("runs after the refused start = %d, %v; want none", len(runs), err)
	}
	release()
	result, err := fixture.app.workflowAgentStartRun(ctx, WorkflowAgentStartInput{WorkflowID: "tool-flow", Goal: "after"})
	if err != nil || result.BoundThreadID != thread.ID {
		t.Fatalf("a run start after the rest = %+v, %v", result, err)
	}
	release = fixture.app.resting.close(thread.ID)
	defer release()
	if _, err := fixture.app.WorkflowBindThread(result.ItemID, thread.ID); !errors.Is(err, errThreadResting) {
		t.Fatalf("a bind while the thread is being stopped = %v, want it refused", err)
	}
	tools := workflowToolsAdapter{a: fixture.app}
	for _, action := range []string{"resume", "rerun", "retry_unit", "retry_failed_units"} {
		if _, err := tools.ControlRun(ctx, workflowtools.ControlInput{Action: action, RunID: result.ItemID}); !errors.Is(err, errThreadResting) {
			t.Fatalf("run %s while the thread is being stopped = %v, want it refused", action, err)
		}
	}
	if err := tools.ResolveRun(ctx, workflowtools.ResolveInput{Decision: "answer", RunID: result.ItemID, Answer: "yes"}); !errors.Is(err, errThreadResting) {
		t.Fatalf("an answer while the thread is being stopped = %v, want it refused", err)
	}
	if _, err := tools.ControlRun(ctx, workflowtools.ControlInput{Action: "pause", RunID: result.ItemID}); errors.Is(err, errThreadResting) {
		t.Fatalf("a pause while the thread is being stopped was refused: %v", err)
	}
}

// Closing a thread's admission waits for the admissions already under way,
// so what a rest reads afterwards is all the thread started.
func TestClosingAdmissionWaitsForTheAdmissionsUnderWay(t *testing.T) {
	t.Parallel()
	var fence threadRestFence
	done, err := fence.admit("t")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan func())
	go func() { closed <- fence.close("t") }()
	select {
	case <-closed:
		t.Fatal("admission closed while an admission was under way")
	case <-time.After(50 * time.Millisecond):
	}
	done()
	release := <-closed
	if _, err := fence.admit("t"); !errors.Is(err, errThreadResting) {
		t.Fatalf("admission while closed = %v, want refused", err)
	}
	if _, err := fence.admit("u"); err != nil {
		t.Fatalf("another thread's admission = %v", err)
	}
	release()
	if done, err := fence.admit("t"); err != nil {
		t.Fatalf("admission after the release = %v", err)
	} else {
		done()
	}
}

// An archive closes the thread's admission from before its capture until its
// session is stopped, through the archive binding and thread_update alike,
// and opens it again afterwards.
func TestAnArchivedThreadStartsNoWorkWhileItsSessionStops(t *testing.T) {
	t.Parallel()
	for name, archive := range map[string]func(*requestFixture, string) error{
		"binding": func(f *requestFixture, id string) error {
			_, err := f.app.ArchiveThread(id)
			return err
		},
		"thread_update": func(f *requestFixture, id string) error {
			archived := true
			_, err := f.app.applyThreadOrganizePatch(t.Context(), id, threadapp.OrganizePatch{Archived: &archived})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaudeHoldingTheTurn(t, "working")
			ack := f.runningRequest(t, "work on it")
			var admitErr error
			tried := false
			f.app.queueDropReadForTest = func(threadID string) {
				if threadID != ack.ThreadID || tried {
					return
				}
				tried = true
				done, err := f.app.resting.admit(ack.ThreadID)
				if err == nil {
					done()
				}
				admitErr = err
			}
			if err := archive(f, ack.ThreadID); err != nil {
				t.Fatal(err)
			}
			if !tried || !errors.Is(admitErr, errThreadResting) {
				t.Fatalf("an admission while the session stopped: tried %v, %v; want it refused", tried, admitErr)
			}
			done, err := f.app.resting.admit(ack.ThreadID)
			if err != nil {
				t.Fatalf("an admission after the archive: %v", err)
			}
			done()
		})
	}
}
