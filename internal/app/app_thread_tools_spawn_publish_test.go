package app

import (
	"database/sql"
	"slices"
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// sidebarShown replays thread:updated frames the way a client applies them
// (frontend/src/lib/stores/eventsThreadRows.ts): `listed` inserts a row the
// client lacks, `full` updates only a row it holds, `unlisted` and `deleted`
// drop it. It returns the threads the client held as a sidebar row, neither
// archived nor owned, after any frame, and the rows it holds at the end.
func sidebarShown(calls []emittedCall) (shown map[string]bool, held map[string]store.Thread) {
	shown, held = map[string]bool{}, map[string]store.Thread{}
	for _, call := range calls {
		frame, ok := call.Data.(triage.ThreadUpdateEvent)
		if !ok || call.Channel != eventchan.ThreadUpdated.String() {
			continue
		}
		switch frame.Action {
		case triage.ThreadActionListed:
			if frame.Thread != nil {
				held[frame.Thread.ID] = *frame.Thread
			}
		case triage.ThreadActionFull:
			if frame.Thread != nil {
				if _, ok := held[frame.Thread.ID]; ok {
					held[frame.Thread.ID] = *frame.Thread
				}
			}
		case triage.ThreadActionUnlisted:
			if frame.Thread != nil {
				delete(held, frame.Thread.ID)
			}
		case triage.ThreadActionDeleted:
			delete(held, frame.ID)
		}
		for id, row := range held {
			if !row.Archived && row.Placement != store.PlacementOwned {
				shown[id] = true
			}
		}
	}
	return shown, held
}

// rowFramesOf is every frame carrying threadID's row, in emission order.
func rowFramesOf(calls []emittedCall, threadID string) []triage.ThreadUpdateEvent {
	var out []triage.ThreadUpdateEvent
	for _, call := range calls {
		frame, ok := call.Data.(triage.ThreadUpdateEvent)
		if ok && call.Channel == eventchan.ThreadUpdated.String() && frame.Thread != nil && frame.Thread.ID == threadID {
			out = append(out, frame)
		}
	}
	return out
}

// An owned spawn's thread reaches clients first as its spawner's worker: no
// frame carries it before the naming write owns it, so no client shows a
// sidebar row for it at any moment. That holds for a fresh thread and for a
// fork, including a fork of a grouped source, whose group it leaves. A
// standalone spawn is listed.
func TestAnOwnedSpawnIsNeverListed(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "on it", "on it", "on it", "on it")
	source := f.forkableThread(t, "owned-spawn-source")
	group, err := f.app.store.CreateThreadGroup(source.ProjectID, "Sweep")
	if err != nil {
		t.Fatalf("CreateThreadGroup: %v", err)
	}
	if _, err := f.app.store.SetThreadGroup([]string{source.ID}, group.ID); err != nil {
		t.Fatalf("SetThreadGroup: %v", err)
	}
	rec := &emitRecorder{}
	f.app.testEmitHook = rec.capture
	spawn := func(t *testing.T, call threadtools.SpawnCall) (string, []emittedCall) {
		t.Helper()
		rec.reset()
		call.Prompt, call.WaitSeconds = "start", 20
		if call.FromThread == "" {
			call.ProjectID = f.caller.ProjectID
		}
		ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), call)
		if err != nil {
			t.Fatalf("Spawn %+v: %v", call, err)
		}
		return ack.ThreadID, rec.snapshot()
	}
	for name, call := range map[string]threadtools.SpawnCall{
		"fresh": {},
		"fork":  {FromThread: source.ID},
	} {
		t.Run(name, func(t *testing.T) {
			id, calls := spawn(t, call)
			frames := rowFramesOf(calls, id)
			if len(frames) == 0 {
				t.Fatal("no frame carried the spawned thread")
			}
			for i, frame := range frames {
				if frame.Action == triage.ThreadActionListed || frame.Thread.Placement != store.PlacementOwned || frame.Thread.SpawnedByThreadID != f.caller.ID {
					t.Errorf("frame %d of %d = %s, placement %s, spawner %q; want every frame full and owned by %s",
						i+1, len(frames), frame.Action, frame.Thread.Placement, frame.Thread.SpawnedByThreadID, f.caller.ID)
				}
			}
			if shown, _ := sidebarShown(calls); shown[id] {
				t.Error("a client applying the frames showed the owned spawn as a sidebar row")
			}
			if row, err := f.app.store.GetThread(id); err != nil || row.Placement != store.PlacementOwned || row.GroupID != "" {
				t.Errorf("spawned row = %+v, %v; want owned in no group", row, err)
			}
		})
	}
	t.Run("standalone", func(t *testing.T) {
		id, calls := spawn(t, threadtools.SpawnCall{Placement: threadtools.PlacementStandalone})
		if shown, held := sidebarShown(calls); !shown[id] || held[id].SpawnedByThreadID != f.caller.ID {
			t.Errorf("standalone spawn shown = %v, last row %+v; want a sidebar row naming its spawner", shown[id], held[id])
		}
	})
}

// Accepting a spawn announces its thread exactly once, after the naming
// write and before any message is sent: an owned worker as `full` and
// owned, and a paired computer's spawn, which has no spawn row here to own
// it, as `listed` and standalone. A spawn whose message never goes out
// still reaches every client.
func TestAcceptingASpawnAnnouncesItsThreadOnceNamed(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	adapter := f.adapter()
	local, err := adapter.localOrigin(f.callerIdentity())
	if err != nil {
		t.Fatalf("localOrigin: %v", err)
	}
	peer := local
	peer.caller = threadtools.Caller{ThreadID: "peer-caller", ComputerID: "peer", ComputerName: "Peer"}
	peer.foreign, peer.ownerDevice = true, "peer-device"
	rec := &emitRecorder{}
	f.app.testEmitHook = rec.capture
	for name, tc := range map[string]struct {
		origin         threadRequestOrigin
		wantAction     string
		wantPlacement  string
		wantSidebarRow bool
	}{
		"owned":  {local, triage.ThreadActionFull, store.PlacementOwned, false},
		"paired": {peer, triage.ThreadActionListed, store.PlacementStandalone, true},
	} {
		t.Run(name, func(t *testing.T) {
			call := threadtools.SpawnCall{Prompt: "start", ProjectID: f.caller.ProjectID, Placement: threadtools.PlacementOwned}
			create, err := adapter.spawnThreadOptions(t.Context(), tc.origin, call)
			if err != nil {
				t.Fatalf("spawnThreadOptions: %v", err)
			}
			create.owned = !tc.origin.foreign
			rec.reset()
			id, _, err := adapter.acceptRequest(tc.origin, store.ThreadRequest{
				Token: newThreadRequestToken(), Kind: store.ThreadRequestSpawn,
			}, func() (store.Thread, error) {
				return adapter.createSpawnedThread(t.Context(), call, create)
			}, create.owned)
			if err != nil {
				t.Fatalf("acceptRequest: %v", err)
			}
			calls := rec.snapshot()
			frames := rowFramesOf(calls, id)
			if len(frames) != 1 || frames[0].Action != tc.wantAction || frames[0].Thread.Placement != tc.wantPlacement {
				t.Fatalf("frames of the spawn's thread = %+v, want one %s frame, %s", frames, tc.wantAction, tc.wantPlacement)
			}
			if shown, _ := sidebarShown(calls); shown[id] != tc.wantSidebarRow {
				t.Errorf("a client showed a sidebar row: %v, want %v", shown[id], tc.wantSidebarRow)
			}
		})
	}
}

// A spawn whose naming write fails after its thread exists takes the thread
// back: the caller reads a refusal, the request is settled refused, and no
// thread is left that no request names. An ask's copy goes the same way.
func TestASpawnWhoseNamingFailsLeavesNoThread(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "on it")
	source := f.forkableThread(t, "naming-failure-source")
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER refuse_naming BEFORE UPDATE OF target_thread_id ON thread_request_receipts
		WHEN NEW.target_thread_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'naming refused'); END`); err != nil {
		t.Fatal(err)
	}
	rec := &emitRecorder{}
	f.app.testEmitHook = rec.capture
	cases := map[string]func() error{
		"owned fresh": func() error {
			_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "start", ProjectID: f.caller.ProjectID})
			return err
		},
		"standalone fresh": func() error {
			_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
				Prompt: "start", ProjectID: f.caller.ProjectID, Placement: threadtools.PlacementStandalone,
			})
			return err
		},
		"owned fork": func() error {
			_, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "start", FromThread: source.ID})
			return err
		},
		"ask": func() error {
			_, err := f.adapter().Ask(t.Context(), f.callerIdentity(), threadtools.AskCall{ThreadID: source.ID, Question: "why?"})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			before := threadIDsInStore(t, f.app)
			requests, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 100, 0)
			if err != nil {
				t.Fatal(err)
			}
			rec.reset()
			if err := call(); err == nil {
				t.Fatal("the request was accepted though its naming write failed")
			}
			if after := threadIDsInStore(t, f.app); !slices.Equal(after, before) {
				t.Errorf("threads after the failed naming = %v, want the %v it started with", after, before)
			}
			latest, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 100, 0)
			if err != nil || len(latest) != len(requests)+1 {
				t.Fatalf("requests = %d (%v), want one more than %d", len(latest), err, len(requests))
			}
			for _, row := range latest {
				if row.State != store.ThreadRequestRefused {
					t.Errorf("request %s (%s) = %s, want refused", row.Token, row.Kind, row.State)
				}
			}
			calls := rec.snapshot()
			shown, held := sidebarShown(calls)
			for id := range held {
				if !slices.Contains(before, id) {
					t.Errorf("a client still holds thread %s after its spawn was refused", id)
				}
			}
			if name == "owned fresh" || name == "owned fork" {
				for id := range shown {
					if !slices.Contains(before, id) {
						t.Errorf("a client showed thread %s of a refused owned spawn", id)
					}
				}
			}
		})
	}
}
