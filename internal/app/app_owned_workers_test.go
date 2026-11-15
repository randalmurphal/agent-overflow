package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// Owned workers (docs/specs/standing-agents.md, "Owned workers"): a thread
// a local thread spawned is owned by it unless placed, has no sidebar row,
// moves in and out of the sidebar by placement with a frame that keeps its
// panes open, follows its owner's archive, and joins the sidebar when its
// owner is deleted. An assistant's own thread joins no group.

// recordSpawn is what a finished thread_spawn with the default placement
// leaves: a local spawn request from spawner naming child, owned by it.
func recordSpawn(t *testing.T, app *App, token, spawner, child string) {
	t.Helper()
	if err := app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: spawner, Kind: store.ThreadRequestSpawn,
		TargetThreadID: child, State: store.ThreadRequestFinished,
		Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal,
		SettledAt: 2, CreatedAt: 1, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("record spawn %s: %v", token, err)
	}
	if _, err := app.store.SetThreadPlacement([]string{child}, store.PlacementOwned); err != nil {
		t.Fatalf("own %s: %v", child, err)
	}
}

// withMessage gives a thread a persisted message, which is what puts a
// thread in the sidebar's catalog at all (ListThreadsWithItems).
func withMessage(t *testing.T, app *App, threadID string) {
	t.Helper()
	if err := app.store.InsertItem(store.Item{
		ID: threadID + "-msg", ThreadID: threadID, Kind: "user_text", Role: "user", Summary: "work",
	}); err != nil {
		t.Fatalf("message on %s: %v", threadID, err)
	}
}

// catalogIDs is what the sidebar's catalog read lists.
func catalogIDs(t *testing.T, app *App) []string {
	t.Helper()
	rows, err := app.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// fullFrameFor returns the last `full` thread:updated frame for threadID,
// or nil when none was emitted.
func fullFrameFor(frames []triage.ThreadUpdateEvent, threadID string) *store.Thread {
	return lastFrameFor(frames, triage.ThreadActionFull, threadID)
}

// lastFrameFor returns the last thread:updated frame of action for
// threadID, or nil when none was emitted.
func lastFrameFor(frames []triage.ThreadUpdateEvent, action, threadID string) *store.Thread {
	var found *store.Thread
	for _, frame := range frames {
		if frame.Action == action && frame.Thread != nil && frame.Thread.ID == threadID {
			found = frame.Thread
		}
	}
	return found
}

// placementFrames checks the frames a placement move emitted for threadID:
// `listed` when it gained a sidebar row, `full` when it lost one, and never
// the archive's `unlisted`, which closes the panes showing the thread.
func placementFrames(t *testing.T, step string, frames []triage.ThreadUpdateEvent, threadID, wantPlacement string) {
	t.Helper()
	if lastFrameFor(frames, triage.ThreadActionUnlisted, threadID) != nil {
		t.Errorf("%s: a placement move unlisted %s, which closes its panes", step, threadID)
	}
	action := triage.ThreadActionListed
	if wantPlacement == store.PlacementOwned {
		action = triage.ThreadActionFull
	}
	if got := lastFrameFor(frames, action, threadID); got == nil || got.Placement != wantPlacement {
		t.Errorf("%s: %s frame of %s = %+v, want placement %s", step, action, threadID, got, wantPlacement)
	}
}

// runningSession opens a turn on threadID and registers a session for it,
// as a live provider leaves a thread at work.
func runningSession(t *testing.T, app *App, threadID string) {
	t.Helper()
	if err := app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: threadID + "-turn", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("open turn on %s: %v", threadID, err)
	}
	app.sessionManager().put(threadID, session{
		Provider: string(provider.Claude), Token: "tok-" + threadID, Liveness: newSessionLiveness(time.Now()),
	})
}

// An owned thread is absent from the sidebar's catalog and its project's
// thread count, and present again once placed standalone; the user's two
// actions (Show in Sidebar, Remove from Sidebar) move it each way with a
// frame that keeps a pane showing it open. Owned is refused for a thread no
// local thread spawned, for an assistant's own thread and for a helper.
func TestShowAndRemoveFromSidebar(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	spawner := f.thread(t, "spawner")
	worker := f.thread(t, "worker")
	plain := f.thread(t, "plain")
	for _, id := range []string{spawner.ID, worker.ID, plain.ID} {
		withMessage(t, f.app, id)
	}
	recordSpawn(t, f.app, "tok-worker", spawner.ID, worker.ID)
	count := func() int {
		t.Helper()
		projects, err := f.app.store.ListProjectsWithThreadCounts()
		if err != nil {
			t.Fatal(err)
		}
		for _, project := range projects {
			if project.Project.ID == f.project.ID {
				return project.ThreadCount
			}
		}
		t.Fatalf("project %s is not listed", f.project.ID)
		return 0
	}
	if got := catalogIDs(t, f.app); slices.Contains(got, worker.ID) || !slices.Contains(got, spawner.ID) {
		t.Fatalf("catalog = %v, want the owned worker left out", got)
	}
	if got := count(); got != 2 {
		t.Errorf("project thread count = %d, want 2", got)
	}

	f.events.reset()
	shown, err := f.app.SetThreadPlacement([]string{worker.ID}, store.PlacementStandalone)
	if err != nil || len(shown) != 1 || shown[0].Placement != store.PlacementStandalone {
		t.Fatalf("Show in Sidebar = %+v, %v", shown, err)
	}
	placementFrames(t, "show", f.threadFrames(t), worker.ID, store.PlacementStandalone)
	if got := catalogIDs(t, f.app); !slices.Contains(got, worker.ID) {
		t.Errorf("catalog after Show in Sidebar = %v, want the worker", got)
	}
	if got := count(); got != 3 {
		t.Errorf("project thread count after Show in Sidebar = %d, want 3", got)
	}

	f.events.reset()
	removed, err := f.app.SetThreadPlacement([]string{worker.ID}, store.PlacementOwned)
	if err != nil || len(removed) != 1 || removed[0].Placement != store.PlacementOwned {
		t.Fatalf("Remove from Sidebar = %+v, %v", removed, err)
	}
	placementFrames(t, "remove", f.threadFrames(t), worker.ID, store.PlacementOwned)
	if got := catalogIDs(t, f.app); slices.Contains(got, worker.ID) {
		t.Errorf("catalog after Remove from Sidebar = %v, want the worker out", got)
	}

	// Joining a group from the sidebar gives it a row in the group.
	group, err := f.app.store.CreateThreadGroup(f.project.ID, "Sweep")
	if err != nil {
		t.Fatal(err)
	}
	f.events.reset()
	if _, err := f.app.SetThreadGroup([]string{worker.ID}, group.ID); err != nil {
		t.Fatal(err)
	}
	placementFrames(t, "group", f.threadFrames(t), worker.ID, store.PlacementGrouped)

	testAssistant(t, f.app, spawner.ID)
	helper := f.thread(t, "helper", func(row *store.Thread) {
		row.ProjectID, row.WorkspacePath, row.HelperOfAgentID = "", t.TempDir(), spawner.ID
	})
	for _, tc := range []struct {
		id   string
		want error
	}{
		{plain.ID, store.ErrThreadNotSpawned},
		{spawner.ID, store.ErrStandingAgentPlaced},
		{helper.ID, store.ErrPlacementOutsideProjects},
	} {
		if _, err := f.app.SetThreadPlacement([]string{tc.id}, store.PlacementOwned); !errors.Is(err, tc.want) {
			t.Errorf("own %s = %v, want %v", tc.id, err, tc.want)
		}
	}
}

func TestGroupMovesRefuseAStandingAgentButPlaceItsSpawnedThreads(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	agent := f.thread(t, "agent")
	child := f.thread(t, "child")
	plain := f.thread(t, "plain")
	recordSpawn(t, f.app, "tok-child", agent.ID, child.ID)
	testAssistant(t, f.app, agent.ID)
	group, err := f.app.store.CreateThreadGroup(f.project.ID, "Release")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.app.SetThreadGroup([]string{agent.ID}, group.ID); !errors.Is(err, store.ErrStandingAgentGrouped) {
		t.Errorf("group the agent: %v", err)
	}
	moved, err := f.app.SetThreadGroup([]string{plain.ID, child.ID}, group.ID)
	if err != nil || len(moved) != 2 {
		t.Fatalf("group the plain and the spawned thread = %+v, %v", moved, err)
	}
	if got := f.reload(t, child.ID); got.Placement != store.PlacementGrouped || got.GroupID != group.ID {
		t.Errorf("grouped child = %s in %q", got.Placement, got.GroupID)
	}

	update := func(ids []string, call threadtools.UpdateCall) threadtools.UpdateReport {
		t.Helper()
		call.ThreadIDs = ids
		report, err := f.adapter.UpdateThreads(t.Context(), caller("someone"), call)
		if err != nil {
			t.Fatalf("UpdateThreads %+v: %v", call, err)
		}
		return report
	}
	placementIs := func(step, id, want string) {
		t.Helper()
		if got := f.reload(t, id); got.Placement != want {
			t.Errorf("%s: %s placement = %s, want %s", step, id, got.Placement, want)
		}
	}
	f.events.reset()
	report := update([]string{child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementOwned)})
	if row := updateResultFor(t, report, child.ID); !row.Updated {
		t.Fatalf("tool own = %+v", row)
	}
	placementIs("tool own", child.ID, store.PlacementOwned)
	placementFrames(t, "tool own", f.threadFrames(t), child.ID, store.PlacementOwned)
	if got := fullFrameFor(f.threadFrames(t), child.ID); got == nil || got.GroupID != "" {
		t.Errorf("tool own frame = %+v, want it out of the group", got)
	}
	f.events.reset()
	update([]string{child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementStandalone)})
	placementIs("tool standalone", child.ID, store.PlacementStandalone)
	placementFrames(t, "tool standalone", f.threadFrames(t), child.ID, store.PlacementStandalone)
	update([]string{child.ID}, threadtools.UpdateCall{Group: stringPtr("Release")})
	placementIs("tool group", child.ID, store.PlacementGrouped)
	// Ungrouping leaves the child standalone; grouped with no group keeps
	// a grouped thread where it is.
	update([]string{child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementGrouped)})
	placementIs("tool grouped in place", child.ID, store.PlacementGrouped)
	update([]string{child.ID}, threadtools.UpdateCall{Group: stringPtr("")})
	placementIs("tool ungroup", child.ID, store.PlacementStandalone)
	// A rename that moves no placement is a plain row update.
	f.events.reset()
	update([]string{child.ID}, threadtools.UpdateCall{Title: stringPtr("Renamed")})
	if frames := f.threadFrames(t); fullFrameFor(frames, child.ID) == nil || lastFrameFor(frames, triage.ThreadActionListed, child.ID) != nil {
		t.Errorf("rename frames = %+v, want one full frame", frames)
	}

	// Per id: owned for a thread nothing spawned, grouped with no group
	// for an ungrouped one, any placement for the assistant's own thread.
	report = update([]string{plain.ID, agent.ID, child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementOwned)})
	if row := updateResultFor(t, report, plain.ID); row.Updated || !strings.Contains(row.Error, "No thread on this computer spawned that thread") {
		t.Errorf("own a plain thread = %+v", row)
	}
	if row := updateResultFor(t, report, agent.ID); row.Updated || !strings.Contains(row.Error, "takes no placement") {
		t.Errorf("own the agent = %+v", row)
	}
	if row := updateResultFor(t, report, child.ID); !row.Updated {
		t.Errorf("own the child beside the refusals = %+v", row)
	}
	placementIs("refused own", plain.ID, store.PlacementGrouped)
	report = update([]string{child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementGrouped)})
	if row := updateResultFor(t, report, child.ID); row.Updated || !strings.Contains(row.Error, "needs group") {
		t.Errorf("grouped without a group on an owned thread = %+v", row)
	}
	// A patch that contradicts itself is refused for the whole call.
	if _, err := f.adapter.UpdateThreads(t.Context(), caller("someone"), threadtools.UpdateCall{
		ThreadIDs: []string{child.ID}, Group: stringPtr("Release"), Placement: stringPtr(store.PlacementStandalone),
	}); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Errorf("group with standalone = %v, want the whole call refused", err)
	}
	placementIs("contradiction", child.ID, store.PlacementOwned)
}

// Archiving a thread archives the workers it owns, down the lineage, each
// as an ordinary archive: a running worker's session stops and its turn
// settles. A standalone worker, one another thread owns and an archived one
// are left as they are; unarchiving the owner unarchives none of them. A
// worker whose archive fails is reported, and its siblings are archived.
func TestArchivingAnOwnerArchivesItsWorkers(t *testing.T) {
	t.Parallel()
	doors := map[string]func(app *App, threadID string) error{
		"binding": func(app *App, threadID string) error { return archiveThread(app, threadID) },
		"thread_update": func(app *App, threadID string) error {
			report, err := threadToolsApp{app: app}.UpdateThreads(context.Background(), caller("someone"),
				threadtools.UpdateCall{ThreadIDs: []string{threadID}, Archived: boolPtr(true)})
			if err != nil {
				return err
			}
			if row := report.Results[0]; !row.Updated || len(row.WorkersNotArchived) > 0 || row.Warning != "" {
				return fmt.Errorf("%+v", row)
			}
			return nil
		},
	}
	for name, archive := range doors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, _ := setupE2EApp(t)
			events := &emitRecorder{}
			app.testEmitHook = events.capture
			for _, id := range []string{"owner", "worker", "deep", "placed", "other", "elsewhere"} {
				if err := app.store.CreateThread(e2eThread(id, string(provider.Claude), t.TempDir())); err != nil {
					t.Fatalf("CreateThread %s: %v", id, err)
				}
			}
			recordSpawn(t, app, "tok-worker", "owner", "worker")
			recordSpawn(t, app, "tok-deep", "worker", "deep")
			recordSpawn(t, app, "tok-placed", "owner", "placed")
			recordSpawn(t, app, "tok-elsewhere", "other", "elsewhere")
			if _, err := app.SetThreadPlacement([]string{"placed"}, store.PlacementStandalone); err != nil {
				t.Fatal(err)
			}
			runningSession(t, app, "worker")
			runningSession(t, app, "deep")

			events.reset()
			if err := archive(app, "owner"); err != nil {
				t.Fatalf("archive the owner: %v", err)
			}
			archived := func(id string) bool {
				t.Helper()
				row, err := app.store.GetThread(id)
				if err != nil {
					t.Fatal(err)
				}
				return row.Archived
			}
			for id, want := range map[string]bool{"owner": true, "worker": true, "deep": true, "placed": false, "other": false, "elsewhere": false} {
				if got := archived(id); got != want {
					t.Errorf("%s archived = %v, want %v", id, got, want)
				}
			}
			var frames []triage.ThreadUpdateEvent
			for _, call := range events.snapshot() {
				if frame, ok := call.Data.(triage.ThreadUpdateEvent); ok && call.Channel == eventchan.ThreadUpdated.String() {
					frames = append(frames, frame)
				}
			}
			for _, id := range []string{"worker", "deep"} {
				if _, live := app.sessionManager().get(id); live {
					t.Errorf("worker %s still holds a session after its owner's archive", id)
				}
				turns, err := app.store.ListRecentTurns(id, 1)
				if err != nil || len(turns) != 1 || turns[0].CompletedAt == nil {
					t.Errorf("worker %s turn = %+v, %v; want it settled", id, turns, err)
				}
				if lastFrameFor(frames, triage.ThreadActionUnlisted, id) == nil {
					t.Errorf("worker %s was archived without its unlisted frame", id)
				}
			}

			if _, err := app.UnarchiveThread("owner"); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"worker", "deep"} {
				if !archived(id) {
					t.Errorf("unarchiving the owner unarchived %s", id)
				}
			}
		})
	}
}

// A worker whose archive is refused is named beside the owner's archive,
// which still succeeds, through either door; the owner and its other
// workers are archived. Archiving the owner again archives the worker once
// it can be, and replays nothing of the owner's own archive.
func TestAWorkerThatCannotBeArchivedIsReported(t *testing.T) {
	t.Parallel()
	// Each door archives and returns the workers it reports not archived.
	doors := map[string]func(t *testing.T, app *App, threadID string) []string{
		"binding": func(t *testing.T, app *App, threadID string) []string {
			result, err := app.ArchiveThread(threadID)
			if err != nil {
				t.Fatalf("ArchiveThread: %v", err)
			}
			if result.CleanupError != "" {
				t.Errorf("cleanup error = %q", result.CleanupError)
			}
			var ids []string
			for _, worker := range result.WorkersNotArchived {
				if worker.Error == "" || worker.Title == "" {
					t.Errorf("worker failure = %+v, want its title and reason", worker)
				}
				ids = append(ids, worker.ThreadID)
			}
			return ids
		},
		"thread_update": func(t *testing.T, app *App, threadID string) []string {
			report, err := threadToolsApp{app: app}.UpdateThreads(t.Context(), caller("someone"),
				threadtools.UpdateCall{ThreadIDs: []string{threadID}, Archived: boolPtr(true)})
			if err != nil {
				t.Fatalf("UpdateThreads: %v", err)
			}
			row := report.Results[0]
			if !row.Updated || row.Error != "" || row.Warning != "" {
				t.Errorf("owner's row = %+v, want updated", row)
			}
			var ids []string
			for _, worker := range row.WorkersNotArchived {
				if worker.Error == "" {
					t.Errorf("worker failure = %+v, want its reason", worker)
				}
				ids = append(ids, worker.ThreadID)
			}
			return ids
		},
	}
	// The stuck worker is the owner's own, or beneath a worker the first
	// archive did archive, so the retry has to pass through that worker
	// without replaying its archive. Beside it there, a worker that refused
	// too and was moved to the sidebar before the retry is no longer the
	// owner's to archive.
	for _, depth := range []string{"direct", "beneath an archived worker"} {
		for name, archive := range doors {
			t.Run(depth+"/"+name, func(t *testing.T) {
				t.Parallel()
				f := newOrganizeFixture(t)
				owner := f.thread(t, "owner")
				stuckOwner := owner.ID
				var middle, moved store.Thread
				if depth != "direct" {
					middle = f.thread(t, "middle")
					recordSpawn(t, f.app, "tok-middle", owner.ID, middle.ID)
					stuckOwner = middle.ID
					moved = f.thread(t, "moved", func(row *store.Thread) { row.ForkPreparing = true })
					recordSpawn(t, f.app, "tok-moved", middle.ID, moved.ID)
				}
				stuck := f.thread(t, "stuck", func(row *store.Thread) { row.ForkPreparing = true })
				sibling := f.thread(t, "sibling")
				recordSpawn(t, f.app, "tok-stuck", stuckOwner, stuck.ID)
				recordSpawn(t, f.app, "tok-sibling", owner.ID, sibling.ID)
				want := []string{stuck.ID}
				if moved.ID != "" {
					want = []string{moved.ID, stuck.ID}
				}
				got := archive(t, f.app, owner.ID)
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Fatalf("workers not archived = %v, want %v", got, want)
				}
				for id, want := range map[string]bool{owner.ID: true, sibling.ID: true, stuck.ID: false} {
					if got := f.reload(t, id).Archived; got != want {
						t.Errorf("%s archived = %v, want %v", id, got, want)
					}
				}
				if middle.ID != "" && !f.reload(t, middle.ID).Archived {
					t.Errorf("the middle worker was left unarchived")
				}

				if _, err := f.app.store.FinishForkPreparation(stuck.ID); err != nil {
					t.Fatal(err)
				}
				if moved.ID != "" {
					if _, err := f.app.store.FinishForkPreparation(moved.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := f.app.SetThreadPlacement([]string{moved.ID}, store.PlacementStandalone); err != nil {
						t.Fatal(err)
					}
				}
				f.events.reset()
				if got := archive(t, f.app, owner.ID); len(got) != 0 {
					t.Fatalf("workers not archived on the retry = %v, want none", got)
				}
				if !f.reload(t, stuck.ID).Archived {
					t.Fatal("the retry left the worker unarchived")
				}
				if moved.ID != "" && f.reload(t, moved.ID).Archived {
					t.Error("the retry archived a worker moved to the sidebar")
				}
				frames := f.threadFrames(t)
				if len(frames) != 1 || frames[0].Thread.ID != stuck.ID || frames[0].Action != triage.ThreadActionUnlisted {
					t.Errorf("frames on the retry = %+v, want only the worker's unlisted frame", frames)
				}
			})
		}
	}
}

// Deleting a thread leaves the workers it owned standalone, announced as
// rows the sidebar now lists, and in the catalog. Nothing is deleted with
// it.
func TestADeletedThreadsWorkersSurfaceStandalone(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	spawner := f.thread(t, "spawner")
	worker := f.thread(t, "worker")
	withMessage(t, f.app, worker.ID)
	recordSpawn(t, f.app, "tok-worker", spawner.ID, worker.ID)
	f.events.reset()
	if err := f.app.DeleteThread(spawner.ID); err != nil {
		t.Fatal(err)
	}
	got := f.reload(t, worker.ID)
	if got.Placement != store.PlacementStandalone || got.SpawnedByThreadID != "" {
		t.Errorf("worker after its owner's delete = placement %s spawnedBy %q, want standalone and nobody's", got.Placement, got.SpawnedByThreadID)
	}
	placementFrames(t, "delete", f.threadFrames(t), worker.ID, store.PlacementStandalone)
	if ids := catalogIDs(t, f.app); !slices.Contains(ids, worker.ID) {
		t.Errorf("catalog after the delete = %v, want the worker", ids)
	}
}

// A spawn is owned by the thread that made it, whoever that is, unless it
// asks for a sidebar row: owned threads stay out of the catalog, placed
// ones are in it. Its thread goes out again once the spawn request names
// it, carrying its spawner and its placement.
func TestASpawnIsOwnedUnlessPlaced(t *testing.T) {
	t.Parallel()
	for _, assistant := range []bool{false, true} {
		name := "ordinary thread"
		if assistant {
			name = "assistant"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaude(t, "on it", "on it", "on it", "on it")
			if assistant {
				f.assistantServingAll(t)
			}
			rec := &emitRecorder{}
			f.app.testEmitHook = rec.capture
			project := f.caller.ProjectID
			if assistant {
				project = f.forkableThread(t, "project-anchor").ProjectID
			}
			spawn := func(call threadtools.SpawnCall) store.Thread {
				t.Helper()
				call.Prompt, call.ProjectID, call.WaitSeconds = "start", project, 20
				ack, err := f.adapter().Spawn(context.Background(), f.callerIdentity(), call)
				if err != nil {
					t.Fatalf("Spawn %+v: %v", call, err)
				}
				row, err := f.app.store.GetThread(ack.ThreadID)
				if err != nil {
					t.Fatal(err)
				}
				if row.SpawnedByThreadID != f.caller.ID {
					t.Fatalf("spawned row names spawner %q, want %s", row.SpawnedByThreadID, f.caller.ID)
				}
				return row
			}
			owned := spawn(threadtools.SpawnCall{})
			if owned.Placement != store.PlacementOwned {
				t.Errorf("a spawn with no placement = %s, want owned", owned.Placement)
			}
			var named bool
			for _, event := range rec.snapshot() {
				frame, ok := event.Data.(triage.ThreadUpdateEvent)
				if !ok || event.Channel != eventchan.ThreadUpdated.String() || frame.Thread == nil || frame.Thread.ID != owned.ID {
					continue
				}
				if frame.Action == triage.ThreadActionFull && frame.Thread.SpawnedByThreadID == f.caller.ID && frame.Thread.Placement == store.PlacementOwned {
					named = true
				}
			}
			if !named {
				t.Error("no full frame of the spawned thread named its spawner and its placement")
			}
			standalone := spawn(threadtools.SpawnCall{Placement: threadtools.PlacementStandalone})
			grouped := spawn(threadtools.SpawnCall{Group: "Sweep"})
			if standalone.Placement != store.PlacementStandalone || grouped.Placement != store.PlacementGrouped {
				t.Errorf("placed spawns = %s and %s, want standalone and grouped", standalone.Placement, grouped.Placement)
			}
			catalog := catalogIDs(t, f.app)
			if slices.Contains(catalog, owned.ID) || !slices.Contains(catalog, standalone.ID) || !slices.Contains(catalog, grouped.ID) {
				t.Errorf("catalog = %v, want %s left out and %s, %s listed", catalog, owned.ID, standalone.ID, grouped.ID)
			}
			hits, err := f.app.store.SearchThreadMessages("start", 50)
			if err != nil || !slices.ContainsFunc(hits, func(hit store.ThreadMessageHit) bool { return hit.ThreadID == owned.ID }) {
				t.Errorf("search = %+v, %v; want the owned worker found", hits, err)
			}
		})
	}
}

// A contradiction is refused before any thread exists, inside the API as
// well as in the tools layer.
func TestAContradictorySpawnLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "on it")
	before, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	requestsBefore, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.adapter().Spawn(context.Background(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "start", Group: "Sweep", Placement: threadtools.PlacementStandalone,
	}); err == nil {
		t.Fatal("a spawn with group and standalone was accepted")
	}
	after, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused spawn left a thread behind: %d rows, had %d", len(after), len(before))
	}
	if requestsAfter, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 100, 0); err != nil || len(requestsAfter) != len(requestsBefore) {
		t.Fatalf("a refused spawn left a request behind: %d rows, had %d (%v)", len(requestsAfter), len(requestsBefore), err)
	}
}

// A handoff moves the workers' spawn rows to the successor, and their rows
// go out again naming it.
func TestAHandOffReannouncesTheWorkers(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	agent := f.thread(t, "agent")
	child := f.thread(t, "child")
	recordSpawn(t, f.app, "tok-child", agent.ID, child.ID)
	testAssistant(t, f.app, agent.ID)
	// The handoff starts the successor's session for its opening block.
	installMockClaudeReplies(t, f.app, "ready")
	f.events.reset()
	successor, err := f.app.StartNextAssistantThread(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := fullFrameFor(f.threadFrames(t), child.ID); got == nil || got.SpawnedByThreadID != successor.ID || got.Placement != store.PlacementOwned {
		t.Errorf("child frame = %+v, want it owned by the successor %s", got, successor.ID)
	}
}

// A handoff takes the old row out of the list and lists the successor as
// the assistant's row; the old thread is its past thread, read from the
// Previous Threads page. The successor is listed first, and the old row's
// frame names it: a client moves the panes that showed the old thread to
// it when the old row leaves. The successor's session starts for its
// opening block.
func TestAHandOffBroadcastsBothRows(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	started := stubNoticeStarts(f.app)
	agent := f.thread(t, "agent")
	testAssistant(t, f.app, agent.ID)
	f.events.reset()
	successor, err := f.app.StartNextAssistantThread(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var unlisted, listed *store.Thread
	for _, frame := range f.threadFrames(t) {
		switch {
		case frame.Thread == nil:
		case frame.Action == triage.ThreadActionUnlisted && frame.Thread.ID == agent.ID:
			if listed == nil {
				t.Error("the old row was unlisted before the successor was listed")
			}
			if frame.HandOffTo != successor.ID {
				t.Errorf("the old row's frame names %q as its successor, want %s", frame.HandOffTo, successor.ID)
			}
			unlisted = frame.Thread
		case frame.Action == triage.ThreadActionListed && frame.Thread.ID == successor.ID:
			listed = frame.Thread
		}
	}
	if unlisted == nil || !unlisted.Archived || unlisted.PastAgentID != agent.ID || unlisted.AgentID != "" {
		t.Errorf("old row frame = %+v, want it unlisted as the assistant's past thread", unlisted)
	}
	if listed == nil || listed.AgentID != agent.ID || listed.Title != agent.Title {
		t.Errorf("successor frame = %+v, want the assistant's row", listed)
	}
	page, err := f.app.ListAssistantPastThreads(agent.ID, store.PastThreadsCursor{}, 20)
	if err != nil || len(page.Threads) != 1 || page.Threads[0].ID != agent.ID || page.Threads[0].AgentSupersededAt == nil || page.Next != nil {
		t.Errorf("past threads = %+v, %v; want the old thread alone", page, err)
	}
	if _, err := f.app.ListAssistantPastThreads(agent.ID, store.PastThreadsCursor{}, store.MaxPastThreadsPage+1); !errors.Is(err, store.ErrPastThreadsPage) {
		t.Errorf("an oversized page = %v, want ErrPastThreadsPage", err)
	}
	if got := started(); !slices.Equal(got, []string{successor.ID}) {
		t.Errorf("the handoff started sessions on %v, want its successor %s", got, successor.ID)
	}
}

// An unarchive that lands between a handoff's commit and its announcement
// leaves the old thread listed: the handoff's frame follows the row it
// reads, `full` rather than `unlisted`, and still names the successor so a
// client moves what showed the old thread.
func TestAHandOffAnnouncedAfterAnUnarchiveKeepsTheOldThreadListed(t *testing.T) {
	t.Parallel()
	f := newAssistantFixture(t)
	f.mockClaude(t, "ok", "ok", "ok")
	f.assistantCaller(t)
	old := f.caller.ID
	var mu sync.Mutex
	var frames []triage.ThreadUpdateEvent
	f.app.testEmitHook = func(name string, data any) {
		if event, ok := data.(triage.ThreadUpdateEvent); ok && name == eventchan.ThreadUpdated.String() {
			mu.Lock()
			frames = append(frames, event)
			mu.Unlock()
		}
	}
	var unarchiveErr error
	f.app.handOffCommittedForTest = func(string, string) {
		_, unarchiveErr = f.app.UnarchiveThread(old)
	}
	successor, err := f.app.StartNextAssistantThread(t.Context(), old)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if unarchiveErr != nil {
		t.Fatalf("unarchive in the handoff window: %v", unarchiveErr)
	}
	mu.Lock()
	defer mu.Unlock()
	listed := map[string]bool{old: true}
	var last *triage.ThreadUpdateEvent
	for i, frame := range frames {
		if frame.Thread == nil {
			continue
		}
		switch frame.Action {
		case triage.ThreadActionListed:
			listed[frame.Thread.ID] = true
		case triage.ThreadActionUnlisted:
			delete(listed, frame.Thread.ID)
		}
		if frame.Thread.ID == old {
			last = &frames[i]
		}
	}
	if !listed[old] || !listed[successor.ID] {
		t.Fatalf("listed after the frames = %v, want the unarchived old thread and the successor", listed)
	}
	if last == nil || last.Action != triage.ThreadActionFull || last.Thread.Archived || last.HandOffTo != successor.ID {
		t.Fatalf("old thread's last frame = %+v, want a full unarchived row naming %s", last, successor.ID)
	}
}

// An archive's cascade acts on the workers its owner owned when the
// archive committed, and on each only while that still holds: a worker
// spawned after the owner was reopened, one moved to the sidebar, and every
// worker of an owner reopened before the cascade reached it are left alone.
func TestAnArchiveCascadeActsOnlyOnWhatItsArchiveOwned(t *testing.T) {
	t.Parallel()
	t.Run("owner reopened", func(t *testing.T) {
		t.Parallel()
		f := newOrganizeFixture(t)
		owner := f.thread(t, "owner")
		before := f.thread(t, "before")
		recordSpawn(t, f.app, "tok-before", owner.ID, before.ID)
		work, err := f.app.archiveThreadLocked(owner.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		// The owner is reopened and spawns again while the archive's cleanup
		// is still on its way.
		if _, err := f.app.UnarchiveThread(owner.ID); err != nil {
			t.Fatal(err)
		}
		after := f.thread(t, "after")
		recordSpawn(t, f.app, "tok-after", owner.ID, after.ID)
		if outcome := f.app.finishArchive(work); outcome.cleanup != nil || len(outcome.workers) > 0 {
			t.Fatalf("cleanup = %+v", outcome)
		}
		for _, id := range []string{before.ID, after.ID} {
			if f.reload(t, id).Archived {
				t.Errorf("%s was archived by an archive its reopened owner undid", id)
			}
		}
	})
	t.Run("owner reopened and archived again", func(t *testing.T) {
		t.Parallel()
		f := newOrganizeFixture(t)
		owner := f.thread(t, "owner")
		before := f.thread(t, "before")
		recordSpawn(t, f.app, "tok-before", owner.ID, before.ID)
		stale, err := f.app.archiveThreadLocked(owner.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.UnarchiveThread(owner.ID); err != nil {
			t.Fatal(err)
		}
		after := f.thread(t, "after")
		recordSpawn(t, f.app, "tok-after", owner.ID, after.ID)
		current, err := f.app.archiveThreadLocked(owner.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		// The first archive's cleanup reaches only what that archive owned.
		if outcome := f.app.finishArchive(stale); outcome.cleanup != nil || len(outcome.workers) > 0 {
			t.Fatalf("stale cleanup = %+v", outcome)
		}
		if !f.reload(t, before.ID).Archived || f.reload(t, after.ID).Archived {
			t.Fatalf("after the first archive's cleanup: before archived %v, after archived %v; want only before",
				f.reload(t, before.ID).Archived, f.reload(t, after.ID).Archived)
		}
		if outcome := f.app.finishArchive(current); outcome.cleanup != nil || len(outcome.workers) > 0 {
			t.Fatalf("current cleanup = %+v", outcome)
		}
		if !f.reload(t, after.ID).Archived {
			t.Errorf("the second archive left its worker unarchived")
		}
	})
	t.Run("worker moved", func(t *testing.T) {
		t.Parallel()
		f := newOrganizeFixture(t)
		owner := f.thread(t, "owner")
		moved := f.thread(t, "moved")
		kept := f.thread(t, "kept")
		recordSpawn(t, f.app, "tok-moved", owner.ID, moved.ID)
		recordSpawn(t, f.app, "tok-kept", owner.ID, kept.ID)
		work, err := f.app.archiveThreadLocked(owner.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.SetThreadPlacement([]string{moved.ID}, store.PlacementStandalone); err != nil {
			t.Fatal(err)
		}
		if outcome := f.app.finishArchive(work); outcome.cleanup != nil || len(outcome.workers) > 0 {
			t.Fatalf("cleanup = %+v", outcome)
		}
		if f.reload(t, moved.ID).Archived {
			t.Errorf("a worker moved to the sidebar was archived with its former owner")
		}
		if !f.reload(t, kept.ID).Archived {
			t.Errorf("a worker the owner still owns was left unarchived")
		}
	})
}
