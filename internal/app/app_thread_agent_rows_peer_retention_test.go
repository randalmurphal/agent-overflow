package app

// A spawn on a paired computer ties its thread there to the thread that
// made it at both ends (docs/specs/standing-agents.md, "Owned workers"):
// past the retention floor the destination keeps the receipt as its lineage
// while the thread exists, and the source keeps the spawn as its lineage
// until that computer says the thread is gone, each with its answers
// expiring as every request's do.

import (
	"database/sql"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// remoteLoopWorker spawns a thread on the pair's destination that arms a
// loop and ends its turn, and waits until the source records it open there.
func remoteLoopWorker(t *testing.T, pair *reachPair) (*agentPeerPair, string) {
	t.Helper()
	installMockClaudeReplies(t, pair.dest, "watching the deploy")
	// The caller here is told when a person stops the worker.
	installMockClaudeReplies(t, pair.source, "noted")
	ack := pair.spawnThere(t, "watch the deploy", nil)
	child := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 10*time.Second, func() bool { return pair.receipt(t, ack.Token).SettledAt != 0 })
	destID, _ := pair.dest.backendIdentity()
	there := threadtools.Caller{ThreadID: child, ComputerID: destID, ComputerName: "destination"}
	if _, err := (threadToolsApp{app: pair.dest}).Loop(t.Context(), there, threadtools.LoopCall{Label: "deploy check", Note: "check", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop there: %v", err)
	}
	pair.poll(t)
	agentPair := &agentPeerPair{reachPair: pair, spawn: pair.request(t, ack.Token)}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return threadRequestSettled(row) && row.NestedOpen })
	agentPair.spawn = pair.request(t, ack.Token)
	return agentPair, child
}

// A remote worker with nothing open below it, only an armed loop or a live
// turn, keeps its spawn's receipt there past the retention floor: the
// receipt is kept as its lineage with its answer gone and its token
// unknown, so the spawner's listing and Stop still reach the worker. The
// store's own tests hold the lineage's shape and its delete with the
// thread.
func TestRetentionKeepsTheReceiptOfARemoteWorkerThatIsNotAtRest(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for name, start := range map[string]func(*testing.T, *reachPair) (*agentPeerPair, string){
		"loop": remoteLoopWorker, "turn": remoteRunningWorker,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pair := newReachPair(t)
			agentPair, child := start(t, pair)
			floor := time.Now().Add(time.Minute).UnixMilli()
			for _, app := range []*App{pair.source, pair.dest} {
				if _, err := app.store.DeleteThreadRequestsBefore(floor); err != nil {
					t.Fatalf("sweep: %v", err)
				}
			}
			if _, found, err := pair.dest.store.GetThreadRequestReceipt(agentPair.spawn.Token); err != nil || found {
				t.Fatalf("the lineage's token reads found %v, %v; want unknown", found, err)
			}
			if !pair.request(t, agentPair.spawn.Token).NestedOpen {
				t.Fatal("the sweep here dropped the spawn's open work")
			}
			listing, err := pair.source.peerThreadAgentRows(t.Context(), pair.computer, agentPair.spawn.MadeIn(), child, threadAgentRowDepth-1)
			if err != nil || listing.Worker == "" {
				t.Fatalf("the forwarded listing after the sweeps = %+v, %v; want the worker's state", listing, err)
			}
			stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, agentPair.spawn.Token)
			if err != nil || len(stop.Failures) != 0 {
				t.Fatalf("the Stop after the sweeps = %+v, %v; want it to reach the worker", stop, err)
			}
			waitUntil(t, 10*time.Second, func() bool {
				rest, err := pair.dest.workerUnrest(child)
				return err == nil && rest.unrest == ""
			})

		})
	}
}

// remoteSpawnRow reads what the source's ledger holds of a remote spawn,
// lineage included, which the request reads leave out.
type remoteSpawnRow struct {
	found, lineage, answer, nested bool
	target                         string
}

func readRemoteSpawnRow(t *testing.T, pair *reachPair, token string) remoteSpawnRow {
	t.Helper()
	raw, err := sql.Open("sqlite", pair.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var row remoteSpawnRow
	err = raw.QueryRow(`SELECT lineage_only, answer IS NOT NULL, nested_open, target_thread_id
		FROM thread_requests WHERE token = ?`, token).Scan(&row.lineage, &row.answer, &row.nested, &row.target)
	if err == sql.ErrNoRows {
		return row
	}
	if err != nil {
		t.Fatal(err)
	}
	row.found = true
	return row
}

// remoteRestedWorker spawns a thread on the pair's destination that answers
// and rests, and waits until the source has collected the answer and
// recorded the thread at rest. Its next turn, once a message continues it,
// holds open.
func remoteRestedWorker(t *testing.T, pair *reachPair) (store.ThreadRequest, string) {
	t.Helper()
	installMockClaudeTurns(t, pair.dest, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"The first pass is done."}]}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}, {
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-2","role":"assistant","content":[{"type":"text","text":"Working on the follow-up."}]}}`,
	}})
	// The spawn's final message wakes its caller here, which takes a turn.
	installMockClaudeReplies(t, pair.source, "Noted.")
	ack := pair.spawnThere(t, "do the first pass", nil)
	child := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 10*time.Second, func() bool { return pair.receipt(t, ack.Token).SettledAt != 0 })
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	pair.poll(t)
	var spawn store.ThreadRequest
	waitUntil(t, 10*time.Second, func() bool {
		pair.source.threadRequestsWG.Wait()
		spawn = pair.request(t, ack.Token)
		return threadRequestSettled(spawn) && !spawn.NestedOpen
	})
	return spawn, child
}

// sweepPastTheFloor runs the retention sweep on both computers with every
// settled row past the floor. The spawn here settled an hour ago, so it is
// out of the Agents segment's settled window, as every row past the floor
// is.
func sweepPastTheFloor(t *testing.T, pair *reachPair, token string) {
	t.Helper()
	execOnStore(t, pair.dbPath, `UPDATE thread_requests SET settled_at = settled_at - 3600000 WHERE token = '`+token+`'`)
	floor := time.Now().Add(time.Minute).UnixMilli()
	for _, app := range []*App{pair.source, pair.dest} {
		if _, err := app.store.DeleteThreadRequestsBefore(floor); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}
}

// A remote worker at rest through both retention sweeps keeps its link at
// both ends: the source keeps the spawn as its lineage, payload cleared and
// token unknown, as the destination keeps the receipt. Continued there with
// its spawner's pane closed, the source's own sweep finds it: its row is
// open, counted and out of Past, and its Stop reaches it.
func TestARemoteWorkerAtRestPastTheFloorIsFoundWhenContinued(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	spawn, child := remoteRestedWorker(t, pair)
	sweepPastTheFloor(t, pair, spawn.Token)
	if _, found, err := pair.source.store.GetThreadRequest(spawn.Token); err != nil || found {
		t.Fatalf("the spawn's token here reads found %v, %v; want unknown past the floor", found, err)
	}
	if row := readRemoteSpawnRow(t, pair, spawn.Token); !row.found || !row.lineage || row.answer || row.target != child {
		t.Fatalf("the spawn here past the floor = %+v; want its lineage, answer cleared, naming the thread there", row)
	}

	// Its spawner's pane stays closed: only the source's own sweep asks.
	pair.source.threadPollOverride = 100 * time.Millisecond
	pair.source.startThreadRequestSweeps()
	if err := pair.dest.SendMessage(child, "now also update the changelog", nil); err != nil {
		t.Fatalf("SendMessage there: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool { return readRemoteSpawnRow(t, pair, spawn.Token).nested })
	var row ThreadAgentRow
	waitUntil(t, 10*time.Second, func() bool {
		listing, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID)
		if err != nil {
			t.Fatal(err)
		}
		row, _ = agentRowByToken(listing.Rows, spawn.Token)
		return row.Worker == workerRunning
	})
	(&agentPeerPair{reachPair: pair, spawn: spawn}).expectRemoteRowOpen(t, row, workerRunning)
	stop, err := pair.source.StopThreadAgentRow(t.Context(), pair.caller.ID, spawn.Token)
	if err != nil || len(stop.Failures) != 0 || !stop.Interrupted {
		t.Fatalf("the Stop of the lineage's worker = %+v, %v; want its turn interrupted there", stop, err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
}

// The source's lineage of a remote spawn lasts until that computer says
// the thread is gone: once it is deleted there, the source's own rest pass
// is refused and deletes the lineage, and stops asking.
func TestARemoteSpawnsLineageGoesOnceItsThreadThereIsGone(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	spawn, child := remoteRestedWorker(t, pair)
	sweepPastTheFloor(t, pair, spawn.Token)
	if row := readRemoteSpawnRow(t, pair, spawn.Token); !row.lineage {
		t.Fatalf("the spawn here past the floor = %+v; want its lineage", row)
	}
	if err := pair.dest.DeleteThread(child); err != nil {
		t.Fatalf("delete the thread there: %v", err)
	}
	// Nothing of the spawn's last answer is remembered, as after a restart:
	// only the rest pass's question asks about it.
	pair.source.threadAgentPeers.mu.Lock()
	clear(pair.source.threadAgentPeers.entries)
	pair.source.threadAgentPeers.mu.Unlock()
	pair.source.threadPollOverride = 100 * time.Millisecond
	pair.source.startThreadRequestSweeps()
	waitUntil(t, 20*time.Second, func() bool { return !readRemoteSpawnRow(t, pair, spawn.Token).found })
	if schedule, err := pair.source.store.NextThreadRequestWork(); err != nil || schedule.RestingRemote {
		t.Fatalf("schedule = %+v, %v; want nothing left to ask about", schedule, err)
	}
}
