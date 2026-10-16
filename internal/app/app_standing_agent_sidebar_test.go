package app

import (
	"context"
	"errors"
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
// agent row"): a thread row names the agent that spawned it, promotion
// takes the agent and its spawned threads out of their groups, a group move
// refuses them afterwards, and what moves an agent's archived-child count
// re-broadcasts the agent's row.

// recordSpawn is the row a finished thread_spawn leaves: a local spawn
// request from spawner naming child.
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

func TestGroupMovesRefuseAStandingAgentAndItsSpawnedThreads(t *testing.T) {
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

	if _, err := f.app.store.SetThreadGroup([]string{agent.ID}, group.ID); !errors.Is(err, store.ErrStandingAgentGrouped) {
		t.Errorf("group the agent: %v", err)
	}
	// One refused id fails the whole move: the plain thread stays put.
	if _, err := f.app.store.SetThreadGroup([]string{plain.ID, child.ID}, group.ID); !errors.Is(err, store.ErrSpawnedChildGrouped) {
		t.Errorf("group the spawned thread: %v", err)
	}
	if got := f.reload(t, plain.ID); got.GroupID != "" {
		t.Errorf("the plain thread moved in a refused call: %+v", got)
	}
	// Leaving a group is never refused.
	if _, err := f.app.store.SetThreadGroup([]string{agent.ID, child.ID}, ""); err != nil {
		t.Errorf("ungroup: %v", err)
	}

	report, err := f.adapter.UpdateThreads(t.Context(), caller("someone"), threadtools.UpdateCall{
		ThreadIDs: []string{child.ID}, Group: stringPtr("Release"),
	})
	if err != nil {
		t.Fatalf("UpdateThreads: %v", err)
	}
	if row := updateResultFor(t, report, child.ID); row.Updated || !strings.Contains(row.Error, "nests under the assistant") {
		t.Errorf("tool group move of a spawned thread = %+v, want the refusal", row)
	}
	if got := f.reload(t, child.ID); got.GroupID != "" {
		t.Errorf("the spawned thread joined a group: %+v", got)
	}
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

	// A standing agent's spawned threads nest under its row, so a group
	// in its spawn is refused before any thread exists.
	testAssistant(t, f.app, f.caller.ID)
	before, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.adapter().Spawn(context.Background(), f.callerIdentity(), threadtools.SpawnCall{Prompt: "start", Group: "Sweep"}); err == nil || !strings.Contains(err.Error(), "Omit group") {
		t.Fatalf("grouped spawn by a standing agent: %v", err)
	}
	after, err := f.app.store.ListThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("a refused spawn left a thread behind: %d rows, had %d", len(after), len(before))
	}
}

// A handoff takes the old row out of the list and lists the successor as
// the assistant's row, counting the old thread among its archived children.
// The successor is listed first, and the old row's frame names it: a client
// moves the panes that showed the old thread to it when the old row leaves.
func TestAHandOffBroadcastsBothRows(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
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
