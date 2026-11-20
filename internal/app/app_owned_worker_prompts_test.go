package app

// An owned worker's prompts are its spawner's (docs/specs/standing-agents.md,
// ruling 19): one the spawner cannot take now stays routed to it, reads as
// waiting on it, starts and wakes nothing, and is put to it again at its next
// turn; a paused spawner's wake waits in its queue. The route follows the
// placement whenever it changes.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

var errFixtureNoProvider = errors.New("the provider binary is missing")

// startsOf records the session starts a makes, each failing as the
// fixture's do.
func startsOf(a *App) func() []string {
	var mu sync.Mutex
	var started []string
	a.startSessionFn = func(threadID string) error {
		mu.Lock()
		started = append(started, threadID)
		mu.Unlock()
		return errFixtureNoProvider
	}
	return func() []string {
		a.waitAgentNoticeWork()
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), started...)
	}
}

// requireRoutedTo checks the worker's one approval is open and routed to
// threadID, and that the worker reads as waiting on it.
func (f workerPromptFixture) requireRoutedTo(t *testing.T, threadID string) {
	t.Helper()
	pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals
	if len(pending) != 1 || pending[0].RoutedTo == nil || pending[0].RoutedTo.ThreadID != threadID {
		t.Fatalf("the worker's prompt = %+v, want it open and routed to %s", pending, threadID)
	}
	if waiting := f.app.threadWaitingOn(f.worker.ID); waiting.route == nil || waiting.route.ThreadID != threadID {
		t.Fatalf("the worker waits on %q routed to %+v, want %s", waiting.on, waiting.route, threadID)
	}
}

// turnStarts has threadID's provider start a turn.
func (f workerPromptFixture) turnStarts(t *testing.T, threadID, turn string) {
	t.Helper()
	workerEvent(t, f.app, threadID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: threadID + ":" + turn}, nil)
	f.drain(t)
}

// An owned worker's prompt whose wake its spawner did not take, withdrawn
// from its queue or dropped with its session, stays its spawner's: it reads
// as waiting on the spawner, nobody is started for it and nothing is queued,
// until the spawner's next turn starts, which puts it to the spawner again.
// A worker with a sidebar row asked outside its spawner's run gives it to
// the person (TestAnOrdinarySpawnersWithdrawnWakeHandsThePromptBack).
func TestAnOwnedWorkersPromptItsSpawnerDidNotTakeWaitsForItsNextTurn(t *testing.T) {
	t.Parallel()
	for name, drop := range map[string]func(*testing.T, workerPromptFixture, string){
		"withdrawn": func(t *testing.T, f workerPromptFixture, sendID string) {
			if stop, err := f.app.StopThreadAgentIncoming(t.Context(), f.assistant.ID, sendID); err != nil || stop.Outcome != threadAgentStopped {
				t.Fatalf("withdraw = %+v, %v", stop, err)
			}
		},
		"its session ended": func(t *testing.T, f workerPromptFixture, sendID string) {
			if err := f.app.StopSession(f.assistant.ID); err != nil {
				t.Fatalf("StopSession: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newPlacedWorkerPromptFixture(t, "parked", false, store.PlacementOwned, true)
			started := startsOf(f.app)
			f.drain(t)
			f.requireRoutedTo(t, f.assistant.ID)
			sendID := f.queuedPrompt(t, f.assistant.ID)
			before := len(started())
			drop(t, f, sendID)
			f.drain(t)
			f.requireRoutedTo(t, f.assistant.ID)
			if tokens := workerPromptTokens(t, f.app, f.assistant.ID); len(tokens) != 0 {
				t.Fatalf("the spawner holds prompt wakes %q; want none until its next turn", tokens)
			}
			if after := started(); len(after) != before {
				t.Fatalf("starts %q after the spawner did not take the prompt; want none", after[before:])
			}

			// The spawner's next turn runs in a session of its own.
			f.app.startSessionFn = func(string) error { return nil }
			f.app.triage.MarkThreadActive(f.assistant.ID, "next-session")
			f.turnStarts(t, f.assistant.ID, "next")
			if again := f.queuedPrompt(t, f.assistant.ID); again == sendID {
				t.Fatalf("the prompt was put to the spawner again under its old wake %s", again)
			}
			f.requireRoutedTo(t, f.assistant.ID)
		})
	}
}

// While assistants are off, an assistant's owned worker still asks the
// assistant: a prompt it had stays its, and one asked during the pause is
// queued for the assistant, which is started for it once assistants are on.
func TestAnOwnedWorkersPromptWaitsForItsPausedSpawner(t *testing.T) {
	t.Parallel()
	f := newPlacedWorkerPromptFixture(t, "paused", true, store.PlacementOwned, true)
	started := startsOf(f.app)
	f.drain(t)
	switchAssistants(t, f.app, false)
	f.app.convergeAssistants()
	f.drain(t)
	f.requireRoutedTo(t, f.assistant.ID)

	askApproval(t, f.app, f.worker.ID, "approval-2", "make release")
	f.drain(t)
	pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals
	if len(pending) != 2 {
		t.Fatalf("the worker's prompts = %+v, want two open", pending)
	}
	for _, prompt := range pending {
		if prompt.RoutedTo == nil || prompt.RoutedTo.ThreadID != f.assistant.ID {
			t.Fatalf("a prompt asked during the pause = %+v, want it the paused assistant's", prompt)
		}
	}
	if tokens := workerPromptTokens(t, f.app, f.assistant.ID); len(tokens) == 0 {
		t.Fatal("the paused assistant holds no prompt wake; want the new one queued for it")
	}
	before := len(started())
	switchAssistants(t, f.app, true)
	f.app.convergeAssistants()
	f.drain(t)
	waitUntil(t, 10*time.Second, func() bool { return len(started()) > before })
}

// The route follows the placement. A worker with no run of its spawner's
// open asks its spawner while it is owned; Show in Sidebar gives the open
// prompt to the person and withdraws the spawner's wake, and Remove from
// Sidebar puts it to the spawner again. A worker in its spawner's run asks
// the spawner in either placement.
func TestAnOpenPromptFollowsItsWorkersPlacement(t *testing.T) {
	t.Parallel()
	f := newPlacedWorkerPromptFixture(t, "placed", false, store.PlacementOwned, true)
	startsOf(f.app)
	f.drain(t)
	f.requireRoutedTo(t, f.assistant.ID)

	if _, err := f.app.SetThreadPlacement([]string{f.worker.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		f.drain(t)
		pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals
		return len(pending) == 1 && pending[0].RoutedTo == nil
	})
	if tokens := workerPromptTokens(t, f.app, f.assistant.ID); len(tokens) != 0 {
		t.Fatalf("the spawner holds prompt wakes %q after Show in Sidebar", tokens)
	}

	if _, err := f.app.SetThreadPlacement([]string{f.worker.ID}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		f.drain(t)
		pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals
		return len(pending) == 1 && pending[0].RoutedTo != nil
	})
	f.requireRoutedTo(t, f.assistant.ID)
	f.queuedPrompt(t, f.assistant.ID)

	running := newPlacedWorkerPromptFixture(t, "placed-run", false, store.PlacementStandalone, false)
	startsOf(running.app)
	running.drain(t)
	for _, placement := range []string{store.PlacementOwned, store.PlacementStandalone} {
		if _, err := running.app.SetThreadPlacement([]string{running.worker.ID}, placement); err != nil {
			t.Fatal(err)
		}
		running.drain(t)
		running.requireRoutedTo(t, running.assistant.ID)
	}
}

// A placement change while the spawner's answer is being written leaves the
// prompt to that answer; an answer that does not land ends with the prompt
// routed for the placement then.
func TestAPlacementChangeLeavesAPromptBeingAnswered(t *testing.T) {
	t.Parallel()
	f := newPlacedWorkerPromptFixture(t, "answering", false, store.PlacementOwned, true)
	startsOf(f.app)
	f.drain(t)
	f.requireRoutedTo(t, f.assistant.ID)
	tokens := workerPromptTokens(t, f.app, f.assistant.ID)
	if len(tokens) != 1 {
		t.Fatalf("the spawner holds prompt wakes %q, want one", tokens)
	}
	blocked := newBlockedAnswer(t)
	f.app.workerPromptAnsweringForTest = func(string) error { return blocked.hold() }
	done := make(chan error, 1)
	go func() {
		_, _, err := f.app.answerWorkerPrompt(t.Context(), threadtools.Caller{ThreadID: f.assistant.ID},
			threadtools.ReplyCall{Token: tokens[0], Decision: threadtools.DecisionAllow})
		done <- err
	}()
	within(t, "the answer being written", blocked.entered)
	if _, err := f.app.SetThreadPlacement([]string{f.worker.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.requireRoutedTo(t, f.assistant.ID)
	blocked.free()
	if err := within(t, "the answer", done); err == nil {
		t.Fatal("the failed answer was taken")
	}
	waitUntil(t, 10*time.Second, func() bool {
		f.drain(t)
		pending := f.app.triage.PendingInteractiveRequests(f.worker.ID).Approvals
		return len(pending) == 1 && pending[0].RoutedTo == nil
	})
}
