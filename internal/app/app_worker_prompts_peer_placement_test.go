package app

import (
	"errors"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A worker a paired computer's thread spawned here asks that thread as a
// local worker asks its spawner (store.SpawnOwnerOf): always while it is
// owned, and with a sidebar row in a run that thread started. Its prompts
// follow its placement as a local worker's do.

// place gives worker i placement, as Show in Sidebar and Remove from Sidebar
// do, and drains what its frame moves.
func (f *linkedWorkers) place(t *testing.T, i int, placement string) {
	t.Helper()
	moves, err := f.app.store.SetThreadPlacement([]string{f.workers[i].ID}, placement)
	if err != nil {
		t.Fatal(err)
	}
	f.app.announceMoves(moves)
	f.drain(t)
}

// spawnWithRow adds a worker the lead there spawned with a sidebar row: its
// spawn carries no level, so it links and tracks nothing but its run. It
// returns the worker's index.
func (f *linkedWorkers) spawnWithRow(t *testing.T) int {
	t.Helper()
	worker := noticeTestThread(t, f.app, "sidebar-worker")
	token := "sidebar-spawn"
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: f.device, SourceComputerID: linkedLeadComputer, SourceComputerName: linkedLeadComputerName,
		SourceThreadID: f.lead, SourceThreadTitle: "the lead", Kind: store.ThreadRequestSpawn,
		State: store.ThreadReceiptRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if named, _, err := f.app.store.SetThreadReceiptTarget(token, worker.ID, false); err != nil || !named {
		t.Fatalf("name the spawn: %v, %v", named, err)
	}
	f.workers, f.spawns = append(f.workers, worker), append(f.spawns, token)
	return len(f.workers) - 1
}

// endSpawnRun settles worker i's spawn there and ends its run, as the
// worker's rest after it does.
func (f *linkedWorkers) endSpawnRun(t *testing.T, i int) {
	t.Helper()
	execOnStore(t, f.dbPath, `UPDATE thread_request_receipts SET state = 'finished', settled_at = 20,
		run_open = 0, run_tool_calls = 0, run_tokens = 0, run_turn_ms = 0 WHERE token = ?`, f.spawns[i])
}

// recordQuestions records worker i's async questions as its provider does.
func (f *linkedWorkers) recordQuestions(t *testing.T, i int) {
	t.Helper()
	worker := f.workers[i].ID
	if _, err := f.app.store.RecordAsyncQuestions(store.Item{
		ID: "question:ask", ThreadID: worker, Kind: "assistant_text", Role: "assistant", Status: "completed", Meta: twoAsyncQuestions,
	}); err != nil {
		t.Fatal(err)
	}
	f.app.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": worker})
	f.drain(t)
}

// In its spawner's run a worker with a sidebar row asks that thread there:
// its approval is held for it, listed to it and answered by it, whether
// its link was shown in the sidebar or its spawn made it with a row.
func TestASidebarWorkerAsksItsSpawnerThereInItsRun(t *testing.T) {
	t.Parallel()
	for name, sidebar := range map[string]func(t *testing.T, f *linkedWorkers) int{
		"a link shown in the sidebar": func(t *testing.T, f *linkedWorkers) int {
			f.place(t, 0, store.PlacementStandalone)
			return 0
		},
		"a spawn made with a sidebar row": func(t *testing.T, f *linkedWorkers) int { return f.spawnWithRow(t) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLinkedWorkers(t, 1)
			i := sidebar(t, f)
			worker := f.workers[i].ID
			token := f.ask(t, i)
			if route := f.route(t, worker); route == nil || route.ThreadID != f.lead || route.ComputerID != linkedLeadComputer {
				t.Fatalf("the approval is routed to %+v, want the lead on Desk", route)
			}
			if f.handedToPerson(worker) {
				t.Fatal("the person was asked about a prompt of its spawner's run")
			}
			if listed := f.list(t, i); len(listed) != 1 || listed[0].Token != token {
				t.Fatalf("the spawner's listing = %+v, want the approval under %s", listed, token)
			}
			// The binding admits the spawner's answer: it is written to the
			// worker, which this fixture's provider does not take.
			var answered string
			f.app.workerPromptAnsweringForTest = func(token string) error {
				answered = token
				return errors.New("no provider in this test")
			}
			if err := f.call(t, f.device, f.lead, threadAgentPeerPromptReply,
				threadPromptPeerArgs{Spawn: f.spawns[i], Token: token, Decision: threadtools.DecisionAllow}); errors.Is(err, errPeerPromptNotYours) {
				t.Fatalf("the spawner's answer was refused: %v", err)
			}
			if answered != token {
				t.Fatalf("the answer wrote %q, want the held prompt", answered)
			}
			// Once no run of the spawner's is open on it, the binding admits
			// nothing of a worker with a sidebar row, held or not.
			f.endSpawnRun(t, i)
			if listed := f.list(t, i); len(listed) != 0 {
				t.Fatalf("after the run the spawner's listing = %+v, want nothing", listed)
			}
			for _, tool := range []string{threadAgentPeerPromptReply, threadAgentPeerPromptReturn} {
				if err := f.call(t, f.device, f.lead, tool, threadPromptPeerArgs{
					Spawn: f.spawns[i], Token: token, Decision: threadtools.DecisionAllow,
				}); !errors.Is(err, errPeerPromptNotYours) {
					t.Errorf("after the run, %s: %v; want it refused as not yours", tool, err)
				}
			}
		})
	}
}

// A held prompt follows its worker's placement once its spawner's run
// ended, as a local worker's does: Show in Sidebar gives it to the person
// here, and Remove from Sidebar puts the person's to the spawner there.
// The same holds for a Codex worker's async questions.
func TestAHeldPromptFollowsItsWorkersPlacementOutsideItsSpawnersRun(t *testing.T) {
	t.Parallel()
	t.Run("an approval", func(t *testing.T) {
		t.Parallel()
		f := newLinkedWorkers(t, 1)
		worker := f.workers[0].ID
		f.endSpawnRun(t, 0)
		f.ask(t, 0)
		f.place(t, 0, store.PlacementStandalone)
		if route := f.route(t, worker); route != nil || !f.handedToPerson(worker) {
			t.Fatalf("with a sidebar row the approval is routed to %+v, want the person here", route)
		}
		if listed := f.list(t, 0); len(listed) != 0 {
			t.Fatalf("the spawner's listing = %+v, want nothing", listed)
		}
		f.place(t, 0, store.PlacementOwned)
		if route := f.route(t, worker); route == nil || route.ThreadID != f.lead || route.ComputerID != linkedLeadComputer {
			t.Fatalf("owned again the approval is routed to %+v, want the lead on Desk", route)
		}
		if listed := f.list(t, 0); len(listed) != 1 {
			t.Fatalf("owned again the spawner's listing = %+v, want the approval", listed)
		}
	})
	t.Run("async questions", func(t *testing.T) {
		t.Parallel()
		f := newLinkedWorkers(t, 1)
		worker := f.workers[0].ID
		f.endSpawnRun(t, 0)
		f.recordQuestions(t, 0)
		if _, route, open := f.app.asyncQuestionBlocked(worker); !open || route == nil || route.ComputerID != linkedLeadComputer {
			t.Fatalf("the questions are routed to %+v (open %v), want the lead on Desk", route, open)
		}
		f.place(t, 0, store.PlacementStandalone)
		if _, route, open := f.app.asyncQuestionBlocked(worker); !open || route != nil {
			t.Fatalf("with a sidebar row the questions are routed to %+v (open %v), want the person here", route, open)
		}
		if listed := f.list(t, 0); len(listed) != 0 {
			t.Fatalf("the spawner's listing = %+v, want nothing", listed)
		}
		f.place(t, 0, store.PlacementOwned)
		if _, route, open := f.app.asyncQuestionBlocked(worker); !open || route == nil || route.ComputerID != linkedLeadComputer {
			t.Fatalf("owned again the questions are routed to %+v (open %v), want the lead on Desk", route, open)
		}
		if listed := f.list(t, 0); len(listed) != 1 || len(listed[0].Questions) != 2 {
			t.Fatalf("owned again the spawner's listing = %+v, want the questions", listed)
		}
	})
}
