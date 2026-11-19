package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A paired computer's owned spawn is owned on the computer that runs it,
// through its receipt, the link: its spawner's worker there as a local
// spawn is here, with no sidebar row (docs/specs/standing-agents.md, Owned
// workers).

// recordListedFrames keeps the ids of every thread app announced `listed`.
// Installed before the app does anything the test watches.
func recordListedFrames(app *App) func(threadID string) bool {
	var mu sync.Mutex
	listed := map[string]bool{}
	app.testEmitHook = func(name string, data any) {
		frame, ok := data.(triage.ThreadUpdateEvent)
		if name != string(eventchan.ThreadUpdated) || !ok || frame.Action != triage.ThreadActionListed {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if frame.Thread != nil {
			listed[frame.Thread.ID] = true
		}
		listed[frame.ID] = true
	}
	return func(threadID string) bool {
		mu.Lock()
		defer mu.Unlock()
		return listed[threadID]
	}
}

// sidebarListsThread reports whether app's sidebar catalog, its archived
// list or its project counts include threadID.
func sidebarListsThread(t *testing.T, app *App, projectID, threadID string, alone int) (listed, archived, counted bool) {
	t.Helper()
	rows, err := app.store.ListThreadsWithItems()
	if err != nil {
		t.Fatal(err)
	}
	listed = slices.ContainsFunc(rows, func(row store.Thread) bool { return row.ID == threadID })
	archivedRows, err := app.store.ListArchivedThreads()
	if err != nil {
		t.Fatal(err)
	}
	archived = slices.ContainsFunc(archivedRows, func(row store.Thread) bool { return row.ID == threadID })
	projects, err := app.store.ListProjectsWithThreadCounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range projects {
		if project.Project.ID == projectID {
			counted = project.ThreadCount > alone
		}
	}
	return listed, archived, counted
}

// A spawn left owned, the default, is its spawner's worker on the computer
// that runs it: owned through a link receipt that names the spawner's
// computer, never listed in that computer's sidebar, its archived list or
// its project's counts, and never announced listed. A spawn given a sidebar
// row or a group gets one there, as it would here, and a fork there is
// owned in no group.
func TestARemoteOwnedSpawnIsOwnedThere(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	listedThere := recordListedFrames(pair.dest)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})

	ack := pair.spawnThere(t, "work over there", nil)
	if strings.Contains(ack.Notice, "older version") {
		t.Fatalf("the spawn's reply says %q of a computer that keeps it owned", ack.Notice)
	}
	receipt := pair.receipt(t, ack.Token)
	worker, err := pair.dest.store.GetThread(receipt.TargetThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if worker.Placement != store.PlacementOwned || !receipt.SpawnLink || worker.SpawnedByComputerID != pair.sourceID {
		t.Fatalf("the worker there is %s, linked %v, spawned by computer %q; want owned through its link from %q",
			worker.Placement, receipt.SpawnLink, worker.SpawnedByComputerID, pair.sourceID)
	}
	if state, err := pair.dest.threadPeerRequestState(t.Context(), receipt.OwnerDeviceID, ack.Token); err != nil || state.Placement != store.PlacementOwned {
		t.Fatalf("the destination reports placement %q, %v", state.Placement, err)
	}
	if listed, archived, counted := sidebarListsThread(t, pair.dest, pair.project, worker.ID, 0); listed || archived || counted {
		t.Fatalf("the owned worker is in the sidebar %v, the archived list %v, the counts %v", listed, archived, counted)
	}
	if _, _, err := pair.dest.store.ArchiveThread(worker.ID); err != nil {
		t.Fatal(err)
	}
	if listed, archived, counted := sidebarListsThread(t, pair.dest, pair.project, worker.ID, 0); listed || archived || counted {
		t.Fatalf("the archived owned worker is in the sidebar %v, the archived list %v, the counts %v", listed, archived, counted)
	}
	if listedThere(worker.ID) {
		t.Fatal("the destination announced its owned worker listed")
	}

	standalone := pair.spawnThere(t, "for the user", map[string]any{"placement": threadtools.PlacementStandalone})
	grouped := pair.spawnThere(t, "for the sweep", map[string]any{"group": "Sweep"})
	for name, want := range map[string]struct {
		token, placement string
	}{
		"standalone": {standalone.Token, store.PlacementStandalone},
		"grouped":    {grouped.Token, store.PlacementGrouped},
	} {
		row := pair.receipt(t, want.token)
		thread, err := pair.dest.store.GetThread(row.TargetThreadID)
		if err != nil || thread.Placement != want.placement || row.SpawnLink {
			t.Fatalf("%s: the thread there is %s, linked %v, %v; want %s and no link", name, thread.Placement, row.SpawnLink, err, want.placement)
		}
		waitUntil(t, 10*time.Second, func() bool { return listedThere(thread.ID) })
	}

	source := forkableThreadOn(t, pair.dest, "owned-fork-source")
	group, err := pair.dest.store.CreateThreadGroup(source.ProjectID, "Forks")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pair.dest.store.SetThreadGroup([]string{source.ID}, group.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	forked, err := pair.adapter().Spawn(ctx, pair.callerIdentity(), threadtools.SpawnCall{
		Prompt: "carry on", FromThread: source.ID, FromThreadComputer: pair.computer, ComputerID: pair.computer,
	})
	if err != nil {
		t.Fatalf("a fork there: %v", err)
	}
	fork, err := pair.dest.store.GetThread(pair.receipt(t, forked.Token).TargetThreadID)
	if err != nil || fork.Placement != store.PlacementOwned || fork.GroupID != "" {
		t.Fatalf("the fork there is %s in group %q, %v; want owned in none", fork.Placement, fork.GroupID, err)
	}
	if listedThere(fork.ID) {
		t.Fatal("the destination announced its owned fork listed")
	}
}

// olderOwnershipPeer is a computer from before ownership across computers,
// or one reached by such a computer's spawn. dropLevel strips the level a
// spawn carries, as a source that predates it sends none; predates also
// strips the placement, which such a destination never reads, and leaves
// the placement out of its answers.
type olderOwnershipPeer struct {
	*App
	predates bool
}

func (p *olderOwnershipPeer) ThreadToolCall(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool != "thread_spawn" {
		return p.App.ThreadToolCall(ctx, call)
	}
	var args map[string]any
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return ThreadPeerReply{}, err
	}
	delete(args, "level")
	if p.predates {
		delete(args, "placement")
	}
	stripped, err := json.Marshal(args)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	call.Args = stripped
	reply, err := p.App.ThreadToolCall(ctx, call)
	if p.predates && reply.Request != nil {
		reply.Request.Placement = ""
	}
	return reply, err
}

// A spawn that arrives without its level is not owned there: a source from
// before ownership across computers sends none, though its omitted
// placement resolves to owned. A destination from before it keeps no spawn
// owned, and the spawn's reply says the thread has a sidebar row there.
func TestAnOwnedSpawnWithoutItsLevelIsAPlainRowThere(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for _, tc := range []struct {
		name     string
		predates bool
	}{
		{name: "the source sends no level"},
		{name: "the destination predates ownership", predates: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
				return serveAppAs(t, app, &olderOwnershipPeer{App: app, predates: tc.predates})
			})
			installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
			// As the agent calls it and reads its result.
			result := pair.call(t, "thread_spawn", `{"prompt":"work over there","computer_id":"`+pair.computer+`","project_id":"`+pair.project+`"}`)
			token, _ := result["token"].(string)
			receipt := pair.receipt(t, token)
			thread, err := pair.dest.store.GetThread(receipt.TargetThreadID)
			if err != nil || thread.Placement != store.PlacementStandalone || receipt.SpawnLink {
				t.Fatalf("the thread there is %s, linked %v, %v; want a plain row", thread.Placement, receipt.SpawnLink, err)
			}
			if err := pair.dest.RenameThread(thread.ID, "renamed standalone"); err != nil {
				t.Fatal(err)
			}
			if changes, err := pair.dest.store.PendingSpawnRevisions(receipt.OwnerDeviceID, "", 1); err != nil || len(changes) != 0 {
				t.Fatalf("legacy spawn owed structural revisions: %+v, %v", changes, err)
			}
			note, _ := result["note"].(string)
			// The notice opens the note, naming the computer as this one does.
			first, _, _ := strings.Cut(note, ". ")
			older := strings.HasSuffix(first, " runs an older version, so the thread has a sidebar row there")
			if older != tc.predates {
				t.Fatalf("the spawn's note is %q; want it to open naming the older version: %v", note, tc.predates)
			}
		})
	}
}

// revokingPeer is a destination whose spawning device loses its last
// session here once the spawn's call is admitted: before the receipt is
// written, or between that write and the naming write that would own the
// thread.
type revokingPeer struct {
	*App
	t             *testing.T
	beforeReceipt bool
}

func (p *revokingPeer) ThreadToolCall(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool != "thread_spawn" {
		return p.App.ThreadToolCall(ctx, call)
	}
	owner, err := p.remoteCommandOwner(ctx)
	if err != nil {
		return ThreadPeerReply{}, err
	}
	revoke := func() {
		sessions, err := p.store.ListSessionsForDevice(owner)
		if err != nil {
			p.t.Error(err)
		}
		for _, session := range sessions {
			if _, _, err := p.store.RevokeSession(session.ID, time.Now().UnixMilli()); err != nil {
				p.t.Error(err)
			}
		}
	}
	if p.beforeReceipt {
		revoke()
	} else {
		// Set and read on this call's goroutine, before anything reads it.
		p.spawnNamingForTest = func(string) { revoke() }
	}
	return p.runThreadPeerRequest(ctx, owner, call)
}

// A link needs its owner's device to reach this computer through an
// unrevoked session. An owned spawn whose device loses its last one before
// the receipt is written, or before the naming write owns the thread, ends
// standalone with a sidebar row and no link, as a release leaves it.
func TestAnOwnedSpawnFromADeviceWithoutASessionIsStandaloneThere(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for _, tc := range []struct {
		name          string
		beforeReceipt bool
	}{
		{name: "revoked before the receipt", beforeReceipt: true},
		{name: "revoked before the naming"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
				return serveAppAs(t, app, &revokingPeer{App: app, t: t, beforeReceipt: tc.beforeReceipt})
			})
			listedThere := recordListedFrames(pair.dest)
			installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})

			ack := pair.spawnThere(t, "work over there", nil)
			receipt := pair.receipt(t, ack.Token)
			thread, err := pair.dest.store.GetThread(receipt.TargetThreadID)
			if err != nil || thread.Placement != store.PlacementStandalone || receipt.SpawnLink {
				t.Fatalf("the thread there is %s, linked %v, %v; want standalone and no link", thread.Placement, receipt.SpawnLink, err)
			}
			waitUntil(t, 10*time.Second, func() bool { return listedThere(thread.ID) })
			if listed, _, _ := sidebarListsThread(t, pair.dest, pair.project, thread.ID, 0); !listed {
				t.Fatal("the standalone thread has no sidebar row there")
			}
		})
	}
}

// A paired computer's request from the thread whose link owns a worker
// there marks the worker read when the worker answers, as a local
// spawner's answer does (ruling 30); one the person gave a sidebar row,
// which no link owns, keeps its dot.
func TestAnAnswerToALinkedSpawnerMarksTheWorkerReadThere(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeReplies(t, pair.dest, "done over there", "done for the user")

	owned := pair.spawnThere(t, "work over there", nil)
	plain := pair.spawnThere(t, "work for the user", map[string]any{"placement": threadtools.PlacementStandalone})
	for _, tc := range []struct {
		token string
		read  bool
	}{
		{owned.Token, true},
		{plain.Token, false},
	} {
		threadID := pair.receipt(t, tc.token).TargetThreadID
		var row store.Thread
		waitUntil(t, 20*time.Second, func() bool {
			receipt := pair.receipt(t, tc.token)
			var err error
			row, err = pair.dest.store.GetThread(threadID)
			return err == nil && receipt.SettledAt != 0 && row.LatestTurnCompletedAt != nil
		})
		if tc.read {
			waitUntil(t, 10*time.Second, func() bool {
				row, _ = pair.dest.store.GetThread(threadID)
				return !threadUnread(row)
			})
			continue
		}
		// The mark runs after the settlement; give it the time it takes.
		time.Sleep(200 * time.Millisecond)
		if row, _ = pair.dest.store.GetThread(threadID); !threadUnread(row) {
			t.Fatal("an answer from a thread with a sidebar row there marked it read")
		}
	}
}
