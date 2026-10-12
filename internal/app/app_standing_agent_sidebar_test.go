package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
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

func TestPromoteStandingAgentUngroupsItAndItsSpawnedThreads(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	agent := f.thread(t, "agent")
	childA := f.thread(t, "child-a")
	childB := f.thread(t, "child-b")
	stranger := f.thread(t, "stranger")
	recordSpawn(t, f.app, "tok-a", agent.ID, childA.ID)
	recordSpawn(t, f.app, "tok-b", agent.ID, childB.ID)
	groupA, err := f.app.store.CreateThreadGroup(f.project.ID, "Shared")
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := f.app.store.CreateThreadGroup(f.project.ID, "Only children")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.SetThreadGroup([]string{agent.ID, childA.ID, stranger.ID}, groupA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.SetThreadGroup([]string{childB.ID}, groupB.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.app.store.PinThread(childA.ID); err != nil {
		t.Fatal(err)
	}
	f.events.reset()

	row, err := f.app.PromoteStandingAgent(agent.ID)
	if err != nil {
		t.Fatalf("PromoteStandingAgent: %v", err)
	}
	if row.ThreadID != agent.ID {
		t.Fatalf("promoted row = %+v", row)
	}
	for _, id := range []string{agent.ID, childA.ID, childB.ID} {
		if got := f.reload(t, id); got.GroupID != "" || got.PinnedAt != nil {
			t.Errorf("%s after the promotion = group %q pinned %v, want ungrouped and unpinned", id, got.GroupID, got.PinnedAt)
		}
	}
	if got := f.reload(t, stranger.ID); got.GroupID != groupA.ID {
		t.Errorf("the stranger left its group: %+v", got)
	}
	if got := f.reload(t, agent.ID); got.AgentPromotedAt == nil {
		t.Errorf("the agent row carries no promotion time: %+v", got)
	}
	if _, err := f.app.store.GetThreadGroup(groupB.ID); err == nil {
		t.Errorf("the group the promotion emptied still exists")
	}
	if _, err := f.app.store.GetThreadGroup(groupA.ID); err != nil {
		t.Errorf("the group with a stranger left was deleted: %v", err)
	}

	frames := f.threadFrames(t)
	if got := fullFrameFor(frames, agent.ID); got == nil || got.AgentPromotedAt == nil || got.GroupID != "" {
		t.Errorf("agent frame = %+v, want a full frame of the promoted, ungrouped row", got)
	}
	for _, id := range []string{childA.ID, childB.ID} {
		if got := fullFrameFor(frames, id); got == nil || got.GroupID != "" || got.SpawnedByThreadID != agent.ID {
			t.Errorf("%s frame = %+v, want the ungrouped row naming its spawner", id, got)
		}
	}
	if got := fullFrameFor(frames, stranger.ID); got != nil {
		t.Errorf("the stranger was broadcast: %+v", got)
	}
	if groups := f.groupFrames(t); len(groups) != 1 || groups[0].Action != "delete" || groups[0].Group.ID != groupB.ID {
		t.Errorf("group frames = %+v, want one delete of the emptied group", groups)
	}

	// A second promotion changes nothing and says nothing.
	f.events.reset()
	if again, err := f.app.PromoteStandingAgent(agent.ID); err != nil || again.PromotedAt != row.PromotedAt {
		t.Fatalf("second promotion = %+v, %v", again, err)
	}
	if frames := f.threadFrames(t); len(frames) != 0 {
		t.Errorf("a repeated promotion broadcast %+v", frames)
	}
}

func TestPromoteStandingAgentRefusesAThreadThatIsNotChatOrPlan(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	terminal := f.thread(t, "terminal", func(thread *store.Thread) { thread.Mode = threadmode.ModeTerminal })
	if _, err := f.app.PromoteStandingAgent(terminal.ID); err == nil || !errors.Is(err, errNotAStandingAgentThread) || !strings.Contains(err.Error(), "chat or plan") {
		t.Fatalf("promote a terminal thread: %v", err)
	}
	plan := f.thread(t, "plan", func(thread *store.Thread) { thread.Mode = threadmode.ModePlan })
	if _, err := f.app.PromoteStandingAgent(plan.ID); err != nil {
		t.Fatalf("promote a plan thread: %v", err)
	}
}

func TestGroupMovesRefuseAStandingAgentAndItsSpawnedThreads(t *testing.T) {
	t.Parallel()
	f := newOrganizeFixture(t)
	agent := f.thread(t, "agent")
	child := f.thread(t, "child")
	plain := f.thread(t, "plain")
	recordSpawn(t, f.app, "tok-child", agent.ID, child.ID)
	if _, err := f.app.PromoteStandingAgent(agent.ID); err != nil {
		t.Fatal(err)
	}
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
	if row := updateResultFor(t, report, child.ID); row.Updated || !strings.Contains(row.Error, "nests under the agent") {
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
	if _, err := f.app.PromoteStandingAgent(agent.ID); err != nil {
		t.Fatal(err)
	}
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

// A spawned thread's row says whether its spawner is a standing agent, so
// every change of that fact re-emits the row: promotion, demotion, a
// committed move and the agent's delete (which takes the spawn rows too).
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
	childFrame := func(step string, wantBond bool) {
		t.Helper()
		frames := f.threadFrames(t)
		got := fullFrameFor(frames, child.ID)
		if got == nil || got.SpawnedByAgent != wantBond || got.SpawnedByThreadID != agent.ID {
			t.Errorf("%s: child frame = %+v, want spawnedByAgent %v spawnedBy %q", step, got, wantBond, agent.ID)
		}
		// An archived child is in no client's list: no frame for it.
		if fullFrameFor(frames, archived.ID) != nil {
			t.Errorf("%s: the archived child was broadcast", step)
		}
	}

	f.events.reset()
	if _, err := f.app.PromoteStandingAgent(agent.ID); err != nil {
		t.Fatal(err)
	}
	childFrame("promote", true)

	f.events.reset()
	if err := f.app.DemoteStandingAgent(agent.ID); err != nil {
		t.Fatal(err)
	}
	childFrame("demote", false)

	if _, err := f.app.PromoteStandingAgent(agent.ID); err != nil {
		t.Fatal(err)
	}
	transfer, err := f.app.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: uuid.NewString(), ThreadID: agent.ID, PeerBackendID: uuid.NewString(), Kind: "move", Direction: "outgoing",
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
	childFrame("move committed", false)

	// Deleting an agent takes its spawn rows: the child is nobody's.
	other := f.thread(t, "other-agent")
	plain := f.thread(t, "plain-child")
	recordSpawn(t, f.app, "tok-plain", other.ID, plain.ID)
	if _, err := f.app.PromoteStandingAgent(other.ID); err != nil {
		t.Fatal(err)
	}
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
	if _, err := f.app.PromoteStandingAgent(f.caller.ID); err != nil {
		t.Fatal(err)
	}
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
