package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// The sidebar agent row's backend (docs/specs/standing-agents.md, "Sidebar
// agent row"): a thread row names the agent that spawned it, a group move
// refuses the agent's own thread but not the threads it spawned, which take
// any placement (ruling 36), and what moves an agent's archived-child count
// re-broadcasts the agent's row.

// recordSpawn is what a finished thread_spawn with the default placement
// leaves: a local spawn request from spawner naming child, nested under it.
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
	if _, err := app.store.SetThreadPlacement([]string{child}, store.PlacementNested); err != nil {
		t.Fatalf("nest %s: %v", child, err)
	}
}

// fullFrameFor returns the last `full` thread:updated frame for threadID,
// or nil when none was emitted.
func fullFrameFor(frames []triage.ThreadUpdateEvent, threadID string) *store.Thread {
	var found *store.Thread
	for _, frame := range frames {
		if frame.Action == triage.ThreadActionFull && frame.Thread != nil && frame.Thread.ID == threadID {
			found = frame.Thread
		}
	}
	return found
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
	report := update([]string{child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementNested)})
	if row := updateResultFor(t, report, child.ID); !row.Updated {
		t.Fatalf("tool nest = %+v", row)
	}
	placementIs("tool nest", child.ID, store.PlacementNested)
	if got := fullFrameFor(f.threadFrames(t), child.ID); got == nil || got.Placement != store.PlacementNested || got.GroupID != "" {
		t.Errorf("tool nest frame = %+v", got)
	}
	update([]string{child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementStandalone)})
	placementIs("tool standalone", child.ID, store.PlacementStandalone)
	update([]string{child.ID}, threadtools.UpdateCall{Group: stringPtr("Release")})
	placementIs("tool group", child.ID, store.PlacementGrouped)
	// Ungrouping leaves the child standalone; grouped with no group keeps
	// a grouped thread where it is.
	update([]string{child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementGrouped)})
	placementIs("tool grouped in place", child.ID, store.PlacementGrouped)
	update([]string{child.ID}, threadtools.UpdateCall{Group: stringPtr("")})
	placementIs("tool ungroup", child.ID, store.PlacementStandalone)

	// Per id: nested for a thread nothing spawned, grouped with no group
	// for an ungrouped one, any placement for the assistant's own thread.
	report = update([]string{plain.ID, agent.ID, child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementNested)})
	if row := updateResultFor(t, report, plain.ID); row.Updated || !strings.Contains(row.Error, "No thread on this computer spawned that thread") {
		t.Errorf("nest a plain thread = %+v", row)
	}
	if row := updateResultFor(t, report, agent.ID); row.Updated || !strings.Contains(row.Error, "takes no placement") {
		t.Errorf("nest the agent = %+v", row)
	}
	if row := updateResultFor(t, report, child.ID); !row.Updated {
		t.Errorf("nest the child beside the refusals = %+v", row)
	}
	placementIs("refused nest", plain.ID, store.PlacementGrouped)
	report = update([]string{child.ID}, threadtools.UpdateCall{Placement: stringPtr(store.PlacementGrouped)})
	if row := updateResultFor(t, report, child.ID); row.Updated || !strings.Contains(row.Error, "needs group") {
		t.Errorf("grouped without a group on a nested thread = %+v", row)
	}
	// A patch that contradicts itself is refused for the whole call.
	if _, err := f.adapter.UpdateThreads(t.Context(), caller("someone"), threadtools.UpdateCall{
		ThreadIDs: []string{child.ID}, Group: stringPtr("Release"), Placement: stringPtr(store.PlacementStandalone),
	}); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Errorf("group with standalone = %v, want the whole call refused", err)
	}
	placementIs("contradiction", child.ID, store.PlacementNested)
}

func TestASpawnedThreadsArchiveMovesItsAgentsRow(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	agent := f.thread(t, "agent")
	child := f.thread(t, "child")
	plainSpawner := f.thread(t, "plain-spawner")
	plainChild := f.thread(t, "plain-child")
	recordSpawn(t, f.app, "tok-child", agent.ID, child.ID)
	recordSpawn(t, f.app, "tok-plain", plainSpawner.ID, plainChild.ID)
	testAssistant(t, f.app, agent.ID)
	agentCount := func(step string, want int) {
		t.Helper()
		got := fullFrameFor(f.threadFrames(t), agent.ID)
		if got == nil || got.ArchivedChildCount != want {
			t.Errorf("%s: agent frame = %+v, want archivedChildCount %d", step, got, want)
		}
		if row := f.reload(t, agent.ID); row.ArchivedChildCount != want {
			t.Errorf("%s: agent row = archivedChildCount %d, want %d", step, row.ArchivedChildCount, want)
		}
	}

	f.events.reset()
	if err := f.app.ArchiveThread(child.ID); err != nil {
		t.Fatal(err)
	}
	agentCount("archive", 1)

	f.events.reset()
	if _, err := f.app.UnarchiveThread(child.ID); err != nil {
		t.Fatal(err)
	}
	agentCount("unarchive", 0)

	f.events.reset()
	report, err := f.adapter.UpdateThreads(t.Context(), caller("someone"), threadtools.UpdateCall{
		ThreadIDs: []string{child.ID}, Archived: boolPtr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if row := updateResultFor(t, report, child.ID); !row.Updated {
		t.Fatalf("tool archive = %+v", row)
	}
	agentCount("tool archive", 1)

	// An archived child counts only while nested: a placement change moves
	// the count and re-broadcasts the agent's row.
	f.events.reset()
	if _, err := f.app.SetThreadPlacement([]string{child.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	agentCount("standalone", 0)
	f.events.reset()
	report, err = f.adapter.UpdateThreads(t.Context(), caller("someone"), threadtools.UpdateCall{
		ThreadIDs: []string{child.ID}, Placement: stringPtr(store.PlacementNested),
	})
	if err != nil {
		t.Fatal(err)
	}
	if row := updateResultFor(t, report, child.ID); !row.Updated {
		t.Fatalf("tool nest = %+v", row)
	}
	agentCount("tool nest", 1)

	f.events.reset()
	if err := f.app.DeleteThread(child.ID); err != nil {
		t.Fatal(err)
	}
	agentCount("delete", 0)

	// A spawner that is not a standing agent carries no count and is not
	// re-broadcast for its child.
	f.events.reset()
	if err := f.app.ArchiveThread(plainChild.ID); err != nil {
		t.Fatal(err)
	}
	if got := fullFrameFor(f.threadFrames(t), plainSpawner.ID); got != nil {
		t.Errorf("a plain spawner was broadcast: %+v", got)
	}
	if listed, err := f.app.ListArchivedSpawnedThreads(plainSpawner.ID); err != nil || len(listed) != 1 || listed[0].ID != plainChild.ID {
		t.Errorf("archived spawned threads of the plain spawner = %+v, %v", listed, err)
	}
}

// A spawned thread's row says whether its spawner is an assistant's current
// thread, so every change of that fact re-emits the row: a handoff, a
// committed move and the assistant's delete (which takes the spawn rows
// too).
func TestASpawnedThreadsBondFollowsItsAgentsRow(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	agent := f.thread(t, "agent")
	child := f.thread(t, "child")
	archived := f.thread(t, "archived-child")
	recordSpawn(t, f.app, "tok-child", agent.ID, child.ID)
	recordSpawn(t, f.app, "tok-archived", agent.ID, archived.ID)
	if err := f.app.ArchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}
	testAssistant(t, f.app, agent.ID)
	// The handoff starts the successor's session for its opening block.
	installMockClaudeReplies(t, f.app, "ready")
	childFrame := func(step, spawner string, wantBond bool) {
		t.Helper()
		frames := f.threadFrames(t)
		got := fullFrameFor(frames, child.ID)
		if got == nil || got.SpawnedByAgent != wantBond || got.SpawnedByThreadID != spawner {
			t.Errorf("%s: child frame = %+v, want spawnedByAgent %v spawnedBy %q", step, got, wantBond, spawner)
		}
		// An archived child is in no client's list: no frame for it.
		if fullFrameFor(frames, archived.ID) != nil {
			t.Errorf("%s: the archived child was broadcast", step)
		}
	}

	f.events.reset()
	successor, err := f.app.StartNextAssistantThread(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	childFrame("handoff", successor.ID, true)

	transfer, err := f.app.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: uuid.NewString(), ThreadID: successor.ID, PeerBackendID: uuid.NewString(), Kind: "move", Direction: "outgoing",
		ActivationHash: strings.Repeat("a", 64), PrivateState: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"prepared", "committed"} {
		if transfer, err = f.app.store.AdvanceThreadTransfer(transfer.ID, phase, strings.Repeat("b", 64)); err != nil {
			t.Fatal(err)
		}
	}
	f.events.reset()
	f.app.threadTransferChanged(transfer)
	childFrame("move committed", successor.ID, false)

	// Deleting an assistant takes its spawn rows: the child is nobody's.
	other := f.thread(t, "other-agent")
	plain := f.thread(t, "plain-child")
	recordSpawn(t, f.app, "tok-plain", other.ID, plain.ID)
	testAssistant(t, f.app, other.ID)
	f.events.reset()
	if err := f.app.DeleteThread(other.ID); err != nil {
		t.Fatal(err)
	}
	if got := fullFrameFor(f.threadFrames(t), plain.ID); got == nil || got.SpawnedByAgent || got.SpawnedByThreadID != "" {
		t.Errorf("delete agent: child frame = %+v, want no spawner and no bond", got)
	}
}

// A spawn's thread goes out twice: once at creation, and again once the
// spawn request names it, so the sidebar can nest it under its spawner.
func TestASpawnBroadcastsItsThreadNamingTheSpawner(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "on it")
	rec := &emitRecorder{}
	f.app.testEmitHook = rec.capture
	ack, err := f.adapter().Spawn(context.Background(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "start"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	spawned, err := f.app.store.GetThread(ack.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if spawned.SpawnedByThreadID != f.caller.ID {
		t.Fatalf("spawned row names spawner %q, want %s", spawned.SpawnedByThreadID, f.caller.ID)
	}
	var named bool
	for _, event := range rec.snapshot() {
		frame, ok := event.Data.(triage.ThreadUpdateEvent)
		if !ok || event.Channel != eventchan.ThreadUpdated.String() || frame.Thread == nil || frame.Thread.ID != ack.ThreadID {
			continue
		}
		if frame.Action == triage.ThreadActionFull && frame.Thread.SpawnedByThreadID == f.caller.ID {
			named = true
		}
	}
	if !named {
		t.Fatalf("no full frame of the spawned thread named its spawner")
	}

	if spawned.Placement != store.PlacementNested {
		t.Errorf("a spawn with no placement = %s, want nested", spawned.Placement)
	}

	// A standing agent places what it spawns: grouped by naming a group,
	// standalone on request. The spawn record names it either way.
	testAssistant(t, f.app, f.caller.ID)
	for _, tc := range []struct {
		call      threadtools.SpawnCall
		placement string
		group     string
	}{
		{threadtools.SpawnCall{Prompt: "start", Group: "Sweep"}, store.PlacementGrouped, "Sweep"},
		{threadtools.SpawnCall{Prompt: "start", Placement: threadtools.PlacementStandalone}, store.PlacementStandalone, ""},
		{threadtools.SpawnCall{Prompt: "start", Placement: threadtools.PlacementNested}, store.PlacementNested, ""},
	} {
		ack, err := f.adapter().Spawn(context.Background(), f.callerIdentity(), tc.call)
		if err != nil {
			t.Fatalf("spawn %+v: %v", tc.call, err)
		}
		row, err := f.app.store.GetThread(ack.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Placement != tc.placement || row.SpawnedByThreadID != f.caller.ID || !row.SpawnedByAgent {
			t.Errorf("spawn %+v = placement %s spawnedBy %q agent %v", tc.call, row.Placement, row.SpawnedByThreadID, row.SpawnedByAgent)
		}
		if tc.group != "" {
			group, err := f.app.store.GetThreadGroup(row.GroupID)
			if err != nil || group.Name != tc.group {
				t.Errorf("spawn %+v group = %+v, %v", tc.call, group, err)
			}
		}
	}
	// A contradiction is refused before any thread exists, inside the API
	// as well as in the tools layer.
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

// A handoff takes the old row out of the list and lists the successor as
// the assistant's row, counting the old thread among its archived children.
// The successor is listed first, and the old row's frame names it: a client
// moves the panes that showed the old thread to it when the old row leaves.
// The successor's session starts for its opening block.
func TestAHandOffBroadcastsBothRows(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	started := stubNoticeStarts(f.app)
	agent := f.thread(t, "agent")
	archived := f.thread(t, "archived-child")
	recordSpawn(t, f.app, "tok-archived", agent.ID, archived.ID)
	if err := f.app.ArchiveThread(archived.ID); err != nil {
		t.Fatal(err)
	}
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
	if listed == nil || listed.AgentID != agent.ID || listed.ArchivedChildCount != 2 || listed.Title != agent.Title {
		t.Errorf("successor frame = %+v, want the assistant's row counting the archived child and the past thread", listed)
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
