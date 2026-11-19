package app

// A worker's question or approval goes to its spawner only while it works
// in a run its spawner started (store.SpawnOwnerOf): its spawn, or a send of
// the spawner's, with the run still open. In any other run, the person's or
// one another thread's send started, the prompt is the person's.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// acceptRequest writes caller's request of target as the tools accept one
// on this computer: the source row and its receipt in one write.
func acceptRequest(t *testing.T, a *App, token, caller, kind, target string) {
	t.Helper()
	origin := threadRequestOrigin{caller: threadtools.Caller{ThreadID: caller}, ownerDevice: threadReceiptLocalOwner}
	if _, err := a.store.AcceptLocalThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: kind, TargetThreadID: target, Notify: true, State: store.ThreadRequestAccepted,
	}, threadReceiptFor(origin, token, kind, target, false)); err != nil {
		t.Fatalf("accept %s: %v", token, err)
	}
}

// settleTaken settles token replied with its answer taken, so nothing is on
// its way to its caller.
func settleTaken(t *testing.T, a *App, token string) {
	t.Helper()
	got, err := a.store.SettleLocalThreadRequest(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptReplied, Answer: []byte("done"), AnswerKind: store.ThreadAnswerReply,
	}, store.ThreadRequestReplied)
	if err != nil || !got.Settled {
		t.Fatalf("settle %s: %+v, %v", token, got, err)
	}
	if _, err := a.store.SetThreadRequestNotify(token, false); err != nil {
		t.Fatal(err)
	}
}

// A middle thread a lead spawned spawns a worker, whose spawn's run ends at
// its rest. A later run asks an approval and a question: the person's run,
// whether the person's message lands before or after the rest pass ended
// the spawn's run, and one another thread's send started, put both to the
// person, wake nothing in the middle thread, and the worker reads blocked
// on the person; a run the middle thread's send started puts both to it.
func TestAWorkersPromptsGoToItsSpawnerOnlyInItsSpawnersRun(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// passEnded ends the spawn's run before the later run starts.
		passEnded bool
		// starts is the request, if any, that starts the later run.
		starts    func(t *testing.T, a *App, middle, worker string)
		toSpawner bool
	}{
		{name: "the person before the rest pass", passEnded: false},
		{name: "the person", passEnded: true},
		{name: "another thread's send", passEnded: true, starts: func(t *testing.T, a *App, middle, worker string) {
			other := noticeTestThread(t, a, "prompt-runs-other")
			acceptRequest(t, a, "other-send", other.ID, store.ThreadRequestSend, worker)
		}},
		{name: "the spawner's send", passEnded: true, toSpawner: true, starts: func(t *testing.T, a *App, middle, worker string) {
			acceptRequest(t, a, "middle-send", middle, store.ThreadRequestSend, worker)
			if row := requestRow(t, a, "middle-send"); !row.RunOpen() {
				t.Fatalf("the middle thread's send %+v; want its run open", row)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, bus := setupE2EApp(t)
			app.triage.SetPromptRouter(app.routeWorkerPrompt)
			bus.mu.Lock()
			bus.tap = app.tapWorkerPrompt
			bus.mu.Unlock()
			// The rest passes are held back: the test ends the spawn's run itself.
			r := &app.threadRequests
			r.mu.Lock()
			r.rest.checking = true
			r.mu.Unlock()
			t.Cleanup(func() {
				r.mu.Lock()
				r.rest.checking = false
				r.mu.Unlock()
			})
			var threads []store.Thread
			for range 3 {
				thread, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
				if err != nil {
					t.Fatal(err)
				}
				threads = append(threads, thread)
			}
			lead, middle, worker := threads[0], threads[1], threads[2]
			if _, err := app.UpdateThreadRuntimeMode(context.Background(), worker.ID, string(provider.RuntimeApprovalRequired)); err != nil {
				t.Fatal(err)
			}
			acceptRequest(t, app, "lead-middle", lead.ID, store.ThreadRequestSpawn, middle.ID)
			acceptRequest(t, app, "middle-worker", middle.ID, store.ThreadRequestSpawn, worker.ID)
			settleTaken(t, app, "middle-worker")
			if c.passEnded {
				app.endRunsAtRest(worker.ID)
				if row := requestRow(t, app, "middle-worker"); !row.RunEnded() {
					t.Fatalf("the spawn %+v; want its run ended at its worker's rest", row)
				}
			}
			if c.starts != nil {
				c.starts(t, app, middle.ID, worker.ID)
			}

			var mu sync.Mutex
			var woken []string
			app.startSessionFn = func(threadID string) error {
				if threadID == worker.ID {
					return app.startSessionNow(threadID)
				}
				mu.Lock()
				woken = append(woken, threadID)
				mu.Unlock()
				return errors.New("no provider for the spawner in this test")
			}
			binary := testutil.WriteMockClaudeScript(t, t.TempDir(), [][]string{{
				`{"type":"system","subtype":"init","session_id":"sess-worker","model":"claude-opus-4-7","cwd":"/tmp","tools":[],"claude_code_version":"1.0"}`,
				`{"type":"control_request","request_id":"req-approve","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"make deploy"}}}`,
				`{"type":"control_request","request_id":"req-ask","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Which module?","header":"Scope","options":[{"label":"parser","description":""},{"label":"lexer","description":""}],"multiSelect":false}]}}}`,
			}})
			if _, err := app.settings.Update(map[string]any{"claudeBinaryPath": binary}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.StopSession(worker.ID) })
			if err := app.SendMessage(context.Background(), worker.ID, "go", nil); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			if row := requestRow(t, app, "middle-worker"); !row.RunEnded() {
				t.Fatalf("the spawn %+v after the later run started; want its run ended first", row)
			}
			waitUntil(t, 10*time.Second, func() bool {
				pending := app.triage.PendingInteractiveRequests(worker.ID)
				return len(pending.Approvals) == 1 && len(pending.UserInputs) == 1
			})
			var tokens []string
			waitUntil(t, 10*time.Second, func() bool {
				if err := app.drainWorkerPrompts(context.Background(), 5*time.Second); err != nil {
					t.Fatal(err)
				}
				app.waitAgentNoticeWork()
				tokens = workerPromptTokens(t, app, middle.ID)
				return !c.toSpawner || len(tokens) == 2
			})

			pending := app.triage.PendingInteractiveRequests(worker.ID)
			waiting := app.threadWaitingOn(worker.ID)
			if !c.toSpawner {
				for _, route := range []*provider.PromptRoute{pending.Approvals[0].RoutedTo, pending.UserInputs[0].RoutedTo} {
					if route != nil {
						t.Fatalf("a prompt is routed to %+v; want the person's", route)
					}
				}
				if len(tokens) != 0 {
					t.Fatalf("the middle thread holds prompt wakes %q; want none", tokens)
				}
				mu.Lock()
				defer mu.Unlock()
				if slices.Contains(woken, middle.ID) {
					t.Fatalf("the middle thread was started for %q", woken)
				}
				if waiting.on == "" || waiting.route != nil {
					t.Fatalf("the worker waits on %q routed to %+v; want a prompt the person answers", waiting.on, waiting.route)
				}
				return
			}
			for _, route := range []*provider.PromptRoute{pending.Approvals[0].RoutedTo, pending.UserInputs[0].RoutedTo} {
				if route == nil || route.ThreadID != middle.ID {
					t.Fatalf("a prompt is routed to %+v; want the middle thread %s", route, middle.ID)
				}
			}
			if waiting.route == nil || waiting.route.ThreadID != middle.ID {
				t.Fatalf("the worker waits on %q routed to %+v; want the middle thread", waiting.on, waiting.route)
			}
		})
	}
}

// A worker's async question follows the same rule: asked in a run its
// spawner did not start, it goes to the person, who is notified, and no
// wake reaches the spawner.
func TestAWorkersAsyncQuestionOutsideItsSpawnersRunIsThePersons(t *testing.T) {
	t.Parallel()
	a, dbPath := newTestAppWithStorePath(t)
	notified := wireAsyncQuestionApp(a)
	spawner := noticeTestThread(t, a, "async-runs-spawner")
	worker := testThread("async-runs-worker")
	worker.WorkspacePath, worker.Title = t.TempDir(), "Release worker"
	if err := a.store.CreateThread(worker); err != nil {
		t.Fatal(err)
	}
	acceptRequest(t, a, "async-runs-spawn", spawner.ID, store.ThreadRequestSpawn, worker.ID)
	if _, err := a.store.SetThreadPlacement([]string{worker.ID}, store.PlacementOwned); err != nil {
		t.Fatal(err)
	}
	settleTaken(t, a, "async-runs-spawn")
	a.endRunsAtRest(worker.ID)
	if row := requestRow(t, a, "async-runs-spawn"); !row.RunEnded() {
		t.Fatalf("the spawn %+v; want its run ended at its worker's rest", row)
	}
	f := asyncQuestionFixture{app: a, dbPath: dbPath, spawner: spawner, worker: worker, notified: notified}
	f.ask(t, "question:later", twoAsyncQuestions)
	for _, row := range durableQueueRows(t, a, spawner.ID) {
		if strings.HasPrefix(row.SendID, workerPromptSendIDRoot) {
			t.Fatalf("the spawner holds a prompt wake %+v; want none", row)
		}
	}
	if waiting := a.threadWaitingOn(worker.ID); waiting.on != "question" || waiting.route != nil {
		t.Fatalf("the worker waits on %q routed to %+v; want its question the person's", waiting.on, waiting.route)
	}
	if sends := f.questionSends("question:later"); len(sends) == 0 || sends[len(sends)-1].Retract {
		t.Fatalf("question notifications = %+v; want the person notified", sends)
	}
}

// An edit-and-resend in a worker its spawner's run left at rest starts the
// person's run as a message does: a spawn's run the rest pass has not ended
// yet ends first, so the run the replacement starts is not the spawner's.
func TestAnEditAndResendInARestedWorkerEndsItsSpawnersRunFirst(t *testing.T) {
	t.Parallel()
	app, _ := newResendTestApp(t)
	worker, _ := seedResendThread(t, app, "t-resend-runs")
	r := &app.threadRequests
	r.mu.Lock()
	r.rest.checking = true
	r.mu.Unlock()
	t.Cleanup(func() {
		r.mu.Lock()
		r.rest.checking = false
		r.mu.Unlock()
	})
	spawner := noticeTestThread(t, app, "resend-runs-spawner")
	acceptRequest(t, app, "resend-spawn", spawner.ID, store.ThreadRequestSpawn, worker.ID)
	settleTaken(t, app, "resend-spawn")
	if row := requestRow(t, app, "resend-spawn"); !row.RunOpen() {
		t.Fatalf("the spawn %+v; the test needs its run open with the rest pass held back", row)
	}
	if err := revertAndResendForTest(app, context.Background(), worker.ID, "user:1", RevertAndResendOptions{Content: "rewritten prompt"}); err != nil {
		t.Fatalf("revert and resend: %v", err)
	}
	if row := requestRow(t, app, "resend-spawn"); !row.RunEnded() {
		t.Fatalf("the spawn %+v after the replacement was sent; want its run ended first", row)
	}
}
