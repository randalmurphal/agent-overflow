package app

// An owned worker on a paired computer is its spawner's sub-agent as a
// local one is (docs/specs/standing-agents.md, "Owned workers"): a message
// the person or another thread sends it there while no run of the
// spawner's is open on it is a nudge, whose run the worker's computer keeps
// on the spawn's link and reports in its agent_rows answer, and which the
// spawner's computer records as the spawner's one row for the worker.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// remoteNudgedWorker is a worker the pair's caller spawned on the
// destination that answered and rested, the spawn's run ended here; each
// later turn makes one tool call and holds until its gate exists.
func remoteNudgedWorker(t *testing.T, pair *reachPair, gates ...string) (*agentPeerPair, string) {
	t.Helper()
	// The destination's events reach its taps as a running app's emit does.
	pair.destBus.mu.Lock()
	pair.destBus.tap = pair.dest.emit
	pair.destBus.mu.Unlock()
	turns := [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"The first pass is done."}]}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}}
	for i, gate := range gates {
		turns = append(turns, runTurnLines(fmt.Sprintf("nudged-%d", i), "Checked the docs.", gate))
	}
	installMockClaudeTurns(t, pair.dest, turns)
	installMockClaudeReplies(t, pair.source, "Noted.")
	ack := pair.spawnThere(t, "do the first pass", nil)
	child := pair.receipt(t, ack.Token).TargetThreadID
	waitUntil(t, 10*time.Second, func() bool { return pair.receipt(t, ack.Token).SettledAt != 0 })
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	pair.poll(t)
	waitUntil(t, 10*time.Second, func() bool {
		pair.source.threadRequestsWG.Wait()
		spawn := pair.request(t, ack.Token)
		return threadRequestSettled(spawn) && !spawn.NestedOpen
	})
	agentPair := &agentPeerPair{reachPair: pair, spawn: pair.request(t, ack.Token), child: child}
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return row.RunEnded() })
	// The spawner's wake for the spawn's answer is over.
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.source.triage.LiveStateSnapshotForThread(pair.caller.ID)).ActiveTurn
	})
	agentPair.spawn = pair.request(t, ack.Token)
	return agentPair, child
}

// nudgeThere has by message the worker on the destination.
func nudgeThere(t *testing.T, pair *reachPair, by, child string) {
	t.Helper()
	switch by {
	case "the person there":
		if err := pair.dest.SendMessage(context.Background(), child, "also check the docs", nil); err != nil {
			t.Fatalf("SendMessage there: %v", err)
		}
	case "a thread there":
		other, err := createTestThread(t, pair.dest, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (threadToolsApp{app: pair.dest}).Send(t.Context(), threadtools.Caller{ThreadID: other.ID, Title: other.Title},
			threadtools.SendCall{ThreadID: child, Message: "also check the docs"}); err != nil {
			t.Fatalf("Send there: %v", err)
		}
	case "another thread here":
		other, err := createTestThread(t, pair.source, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := pair.source.backendIdentity()
		if _, err := pair.adapter().Send(t.Context(), threadtools.Caller{ThreadID: other.ID, Title: other.Title, ComputerID: id, ComputerName: "source"},
			threadtools.SendCall{ThreadID: child, ComputerID: pair.computer, Message: "also check the docs"}); err != nil {
			t.Fatalf("Send from another thread here: %v", err)
		}
	default:
		t.Fatalf("no nudge by %q", by)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
}

// openNudgeRow reads the spawner's listing until it is one open nudge row
// for the worker, as the destination's answers report it.
func (p *agentPeerPair) openNudgeRow(t *testing.T) ThreadAgentRow {
	t.Helper()
	var listing ThreadAgentRows
	waitUntil(t, 10*time.Second, func() bool {
		cache := &p.source.threadAgentPeers
		cache.mu.Lock()
		if entry, ok := cache.entries[p.spawn.Token]; ok {
			entry.at = time.Time{}
		}
		cache.mu.Unlock()
		p.listRows(t)
		p.source.threadRequestsWG.Wait()
		listing = p.listRows(t)
		return len(listing.Rows) == 1 && listing.Rows[0].Kind == threadAgentKindNudge && listing.Rows[0].open()
	})
	return listing.Rows[0]
}

// nudgeEnded asks the destination again until the nudge's row here has its
// run ended.
func (p *agentPeerPair) nudgeEnded(t *testing.T, token string) store.ThreadRequest {
	t.Helper()
	var row store.ThreadRequest
	waitUntil(t, 10*time.Second, func() bool {
		p.askAgainUntil(t, func(store.ThreadRequest) bool { return true })
		row = p.request(t, token)
		return row.RunEnded()
	})
	return row
}

// The person's message there, a thread's there, or another thread's here,
// to the rested owned worker is a nudge: the spawner's one open row for the
// worker, a nudge row counting only the work since, holding the spawner,
// with the worker's prompts routed to it. When the worker rests the row
// ends with the run's figures and goes to Past beside the spawn's; nothing
// is delivered to the spawner and no session is started for it.
func TestARemoteNudgeIsOneRunOfItsSpawnerHere(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"the person there", "a thread there", "another thread here"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			pair := newReachPair(t)
			gate := filepath.Join(t.TempDir(), "nudged-turn")
			agentPair, child := remoteNudgedWorker(t, pair, gate)
			queuedBefore := len(durableQueueRows(t, pair.source, pair.caller.ID))

			nudgeThere(t, pair, by, child)
			row := agentPair.openNudgeRow(t)
			if row.ThreadID != child || row.ComputerID != pair.computer || row.Placement != store.PlacementOwned || row.Tokens != 0 || row.ToolCalls > 1 {
				t.Fatalf("nudge row %+v; want the owned worker there, counting only the work since", row)
			}
			if got, err := pair.source.GetThreadWorkerSummary(t.Context(), pair.caller.ID); err != nil || got.Open != 1 {
				t.Fatalf("summary %+v, %v; want the nudged worker counted", got, err)
			}
			if rest, err := pair.source.threadRest(pair.caller.ID); err != nil || rest.unrest != workerWaiting {
				t.Fatalf("the spawner reads %+v, %v; want it waiting on its worker", rest, err)
			}
			if route := pair.dest.routeWorkerPrompt(child); route == nil || route.ThreadID != pair.caller.ID || route.ComputerID == "" {
				t.Fatalf("a prompt of the nudged run there routes to %+v; want the spawner here", route)
			}
			if requests, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 20, 0); err != nil || len(requests) != 1 {
				t.Fatalf("thread_status lists %+v, %v; want the spawn alone", requests, err)
			}

			release(t, gate)
			// The worker's computer ends the run at its rest.
			receipt := pair.receipt(t, agentPair.spawn.Token)
			waitUntil(t, 10*time.Second, func() bool {
				run, err := pair.dest.store.ReceiptRunOf(receipt.OwnerDeviceID, pair.caller.ID, child)
				return err == nil && len(run.Nudges) == 1 && run.Nudges[0].End != nil
			})
			ended := agentPair.nudgeEnded(t, row.Token)
			if ended.RunTotal.ToolCalls != 1 || ended.RunTotal.Tokens == 0 {
				t.Fatalf("nudge's run %+v; want the nudged turn's call and tokens", ended.RunTotal)
			}
			page, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8)
			if err != nil || page.Total != 2 {
				t.Fatalf("past = %+v, %v; want the spawn's run and the nudge's", page, err)
			}
			if spawn := pair.request(t, agentPair.spawn.Token); *spawn.RunTotal != *agentPair.spawn.RunTotal {
				t.Fatalf("the spawn's run %+v; want it as it ended, %+v", spawn.RunTotal, agentPair.spawn.RunTotal)
			}
			if open := agentPair.listRows(t).Rows; len(open) != 0 && open[0].open() {
				t.Fatalf("open rows %+v after the worker rested", open)
			}
			pair.source.threadRequestsWG.Wait()
			if queued := durableQueueRows(t, pair.source, pair.caller.ID); len(queued) != queuedBefore {
				t.Fatalf("the spawner holds %+v; want nothing delivered for a nudge", queued)
			}
			if _, live := pair.source.sessionManager().get(pair.caller.ID); live {
				t.Fatal("the spawner was started for a nudge's end")
			}
		})
	}
}

// The spawner's own send to its rested worker there is no nudge, and a
// thread's message there during the run it starts joins that run: the
// worker's computer records no nudge and the spawner lists the send's row
// alone.
func TestARemoteNudgeDuringItsSpawnersRunJoinsIt(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "nudged-turn")
	agentPair, child := remoteNudgedWorker(t, pair, gate)
	send := pair.sendThere(t, child, "now also update the changelog")
	waitUntil(t, 10*time.Second, func() bool {
		return threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	receipt := pair.receipt(t, agentPair.spawn.Token)
	if run, err := pair.dest.store.ReceiptRunOf(receipt.OwnerDeviceID, pair.caller.ID, child); err != nil || len(run.Nudges) != 0 {
		t.Fatalf("the spawner's send recorded nudges %+v, %v", run.Nudges, err)
	}
	other, err := createTestThread(t, pair.dest, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (threadToolsApp{app: pair.dest}).Send(t.Context(), threadtools.Caller{ThreadID: other.ID, Title: other.Title},
		threadtools.SendCall{ThreadID: child, Message: "and the release notes"}); err != nil {
		t.Fatalf("Send there: %v", err)
	}
	if run, err := pair.dest.store.ReceiptRunOf(receipt.OwnerDeviceID, pair.caller.ID, child); err != nil || len(run.Nudges) != 0 {
		t.Fatalf("a message during the spawner's run recorded nudges %+v, %v", run.Nudges, err)
	}
	listing := agentPair.listRowsUntil(t, func(rows ThreadAgentRows) bool {
		row, ok := agentRowByToken(rows.Rows, send.Token)
		return ok && row.open()
	})
	if len(listing.Rows) != 1 {
		t.Fatalf("rows %+v; want the send's row alone", listing.Rows)
	}
}

// Show in Sidebar of the nudged worker there ends the nudge's run then, its
// worker still running: the spawner's row for it ends, and nothing reaches
// the spawner.
func TestARemoteNudgeEndsWhenItsWorkerGetsASidebarRowThere(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "nudged-turn")
	agentPair, child := remoteNudgedWorker(t, pair, gate)
	nudgeThere(t, pair, "the person there", child)
	row := agentPair.openNudgeRow(t)
	if _, err := pair.dest.SetThreadPlacement([]string{child}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	agentPair.nudgeEnded(t, row.Token)
	if !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn {
		t.Fatal("the worker's turn there ended; the test needs it running past the sidebar row")
	}
	if summary, err := pair.source.GetThreadWorkerSummary(t.Context(), pair.caller.ID); err != nil || summary.Open != 0 {
		t.Fatalf("summary %+v, %v; want nothing counted once the worker has a sidebar row", summary, err)
	}
	release(t, gate)
}

// The worker's computer restarts while the nudge's run is open: its boot
// reads no rest until it recovered what its threads wait on, and then ends
// the run at the worker's rest from what it keeps on the link, which the
// spawner's row follows.
func TestARemoteNudgeOutlivesARestartOfItsWorkersComputer(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "nudged-turn")
	agentPair, child := remoteNudgedWorker(t, pair, gate)
	nudgeThere(t, pair, "the person there", child)
	row := agentPair.openNudgeRow(t)

	pair.dest.barRestAtBoot()
	release(t, gate)
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	pair.dest.endRestedRuns()
	receipt := pair.receipt(t, agentPair.spawn.Token)
	if run, err := pair.dest.store.ReceiptRunOf(receipt.OwnerDeviceID, pair.caller.ID, child); err != nil || len(run.Nudges) != 1 || run.Nudges[0].End != nil {
		t.Fatalf("the nudges there %+v, %v; want one open while the boot recovers", run.Nudges, err)
	}
	pair.dest.recoverRestAtBoot()
	if ended := agentPair.nudgeEnded(t, row.Token); ended.RunTotal.ToolCalls != 1 {
		t.Fatalf("nudge's run %+v; want the nudged turn's call", ended.RunTotal)
	}
}

// The spawner's computer restarts while the nudge's run is open: with none
// of its memory left, it lists the nudge's row from what it recorded, and
// ends it when the worker there rests.
func TestARemoteNudgeOutlivesARestartOfItsSpawnersComputer(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "nudged-turn")
	agentPair, child := remoteNudgedWorker(t, pair, gate)
	nudgeThere(t, pair, "the person there", child)
	row := agentPair.openNudgeRow(t)

	restarted, _ := setupE2EAppOn(t, pair.dbPath, pair.settingsDir)
	restarted.configDir = pair.source.configDir
	restarted.backends = pair.manager
	publishReachIdentity(t, restarted, pair.sourceID)
	after := &agentPeerPair{reachPair: &reachPair{}, spawn: agentPair.spawn, child: child}
	*after.reachPair = *pair
	after.source = restarted
	if got := after.openNudgeRow(t); got.Token != row.Token {
		t.Fatalf("after the restart the open row is %+v; want the nudge's %s", got, row.Token)
	}
	release(t, gate)
	if ended := after.nudgeEnded(t, row.Token); ended.RunTotal.ToolCalls != 1 {
		t.Fatalf("nudge's run %+v; want the nudged turn's call", ended.RunTotal)
	}
}

// The pairing ends while the nudge's run is open: the worker's computer
// ends the run with the link and forgets it, since no device left can take
// it, and the spawner's computer ends its row with nothing more counted,
// lists no open row for a computer it no longer reaches and delivers
// nothing.
func TestARemoteNudgeEndsWithThePairing(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "nudged-turn")
	agentPair, child := remoteNudgedWorker(t, pair, gate)
	nudgeThere(t, pair, "the person there", child)
	row := agentPair.openNudgeRow(t)
	receipt := pair.receipt(t, agentPair.spawn.Token)
	queuedBefore := len(durableQueueRows(t, pair.source, pair.caller.ID))

	revokeReachDevice(t, pair.dest, "source")
	pair.learnPairingEnded(t)
	if run, err := pair.dest.store.ReceiptRunOf(receipt.OwnerDeviceID, pair.caller.ID, child); err != nil || len(run.Nudges) != 0 {
		t.Fatalf("the nudges there %+v, %v; want none kept for a device with no session", run.Nudges, err)
	}
	if open, err := pair.dest.store.ThreadRunsEnding(child); err != nil || open {
		t.Fatalf("a run is open there: %v, %v; want the nudge's ended with the link", open, err)
	}
	waitUntil(t, 10*time.Second, func() bool { return pair.request(t, row.Token).RunEnded() })
	waitUntil(t, 10*time.Second, func() bool {
		summary, err := pair.source.GetThreadWorkerSummary(t.Context(), pair.caller.ID)
		return err == nil && summary.Open == 0
	})
	for _, row := range agentPair.listRows(t).Rows {
		if row.open() {
			t.Fatalf("row %+v is open for a computer no longer paired", row)
		}
	}
	if queued := durableQueueRows(t, pair.source, pair.caller.ID); len(queued) != queuedBefore {
		t.Fatalf("the spawner holds %+v; want nothing delivered", queued)
	}
	release(t, gate)
}

// An answer that says the thread there is at rest while a run of the
// spawner's is open there, a nudge whose message has not started its turn
// yet, ends no run here; the same answer without the open run ends the
// spawner's runs, as a computer that predates it is read. The answer's
// nudge is recorded as the spawner's nudge row and ended by its own word.
func TestAnAnswerThatSaysARunIsOpenThereEndsNoRunHere(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: "spawn", CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSpawn, TargetComputerID: "pc", TargetThreadID: "w",
		State: store.ThreadRequestRunning, RunBase: &store.RunFigures{}, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.app.store.SettleThreadRequest("spawn", store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: time.Now().UnixMilli(),
	}); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	spawn := f.request(t, "spawn")
	atRest := false
	nudge := ThreadAgentNudge{Token: "nudge", StartedAt: time.Now().UnixMilli(), Base: ThreadAgentRunEnd{ToolCalls: 2}}
	answer := ThreadAgentRows{Open: &atRest, RunOpen: true, ToolCalls: 2, Nudges: []ThreadAgentNudge{nudge}}
	if err := f.app.endRemoteRunsAtRest(spawn, answer); err != nil {
		t.Fatal(err)
	}
	if f.request(t, "spawn").RunEnded() {
		t.Fatal("an answer saying a run is open there ended the spawn's run here")
	}
	if row := f.request(t, "nudge"); !row.Nudge || !row.RunOpen() || row.RunBase.ToolCalls != 2 {
		t.Fatalf("nudge row %+v; want it recorded open from the reported base", row)
	}
	answer.RunOpen = false
	answer.Nudges[0].End = &ThreadAgentRunEnd{ToolCalls: 5}
	if err := f.app.endRemoteRunsAtRest(spawn, answer); err != nil {
		t.Fatal(err)
	}
	if !f.request(t, "spawn").RunEnded() {
		t.Fatal("an answer at rest with no run open there left the spawn's run open")
	}
	if total := f.request(t, "nudge").RunTotal; total == nil || total.ToolCalls != 3 {
		t.Fatalf("nudge run %+v; want its three calls since its base", total)
	}
	if queued := durableQueueRows(t, f.app, f.caller.ID); len(queued) != 0 {
		t.Fatalf("the spawner holds %+v; want nothing delivered for a nudge", queued)
	}
}

// previousReleasePeer answers agent_rows as the release before nudge runs
// on a paired computer's worker did: with neither the nudge nor whether a
// run is open there.
type previousReleasePeer struct{ *App }

func (p *previousReleasePeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	reply, err := p.App.ThreadToolQuery(ctx, call)
	if err != nil || call.Tool != threadAgentPeerRows {
		return reply, err
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(reply.Result, &answer); err != nil {
		return ThreadPeerReply{}, err
	}
	delete(answer, "nudges")
	delete(answer, "runOpen")
	reply.Result, err = json.Marshal(answer)
	return reply, err
}

func servePreviousReleasePeer(t *testing.T, app *App) *pairedBackend {
	t.Helper()
	return servePairedApp(t, app, func(cfg *transport.Config) {
		dispatcher := transport.NewDispatcher()
		if _, err := dispatcher.Register(&previousReleasePeer{App: app}, transport.RegisterOptions{
			Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
		}); err != nil {
			t.Fatalf("register the previous release's destination: %v", err)
		}
		cfg.Dispatcher = dispatcher
	})
}

// A worker's computer on the previous release reports no nudge: the
// spawner lists no row for the person's run there, says nothing failed,
// and keeps the spawn in Past, as that release did.
func TestAPreviousReleasesWorkerReportsNoNudge(t *testing.T) {
	t.Parallel()
	pair := newReachPairServedBy(t, servePreviousReleasePeer)
	gate := filepath.Join(t.TempDir(), "nudged-turn")
	agentPair, child := remoteNudgedWorker(t, pair, gate)
	nudgeThere(t, pair, "the person there", child)
	agentPair.askAgainUntil(t, func(row store.ThreadRequest) bool { return row.NestedOpen })
	view := newThreadAgentView(pair.source)
	view.since = time.Now().Add(time.Second).UnixMilli()
	listing, _, err := pair.source.threadAgentTreeDepth(t.Context(), view, pair.caller.ID, threadAgentRowDepth, threadAgentPeersCached)
	if err != nil || len(listing.Rows) != 0 {
		t.Fatalf("listing %+v, %v; want no row, the nudge unreported", listing.Rows, err)
	}
	if past, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8); err != nil ||
		past.Total != 1 || past.Rows[0].NestedUnavailable != "" {
		t.Fatalf("past = %+v, %v; want the spawn alone, with nothing failed", past, err)
	}
	release(t, gate)
}

// A spawner's computer on the previous release reads the new answer as it
// read the old: the fields it knows say what they said, the spawner's own
// run ended with its figures and the thread open while the nudge runs, and
// the new fields are ones its decoder skips.
func TestAPreviousReleasesSpawnerReadsTheNewAnswerAsBefore(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "nudged-turn")
	agentPair, child := remoteNudgedWorker(t, pair, gate)
	nudgeThere(t, pair, "the person there", child)
	receipt := pair.receipt(t, agentPair.spawn.Token)
	listing, err := pair.dest.answerThreadAgentRows(t.Context(), receipt.OwnerDeviceID, pair.caller.ID,
		threadAgentPeerArgs{ThreadID: child, Depth: threadAgentRowDepth, Spawn: agentPair.spawn.Token})
	if err != nil {
		t.Fatal(err)
	}
	spawnEnd := agentPair.spawn.RunTotal
	if listing.RunEnd == nil || listing.RunEnd.ToolCalls != spawnEnd.ToolCalls || listing.RunEnd.Tokens != spawnEnd.Tokens {
		t.Fatalf("RunEnd %+v; want the spawner's own run's end %+v, the nudge apart from it", listing.RunEnd, spawnEnd)
	}
	if listing.Open == nil || !*listing.Open || len(listing.Nudges) != 1 || !listing.RunOpen {
		t.Fatalf("answer open %v nudges %+v runOpen %v; want the thread open with its nudge", listing.Open, listing.Nudges, listing.RunOpen)
	}
	encoded, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	// The previous release's answer type, as far as this reads it: the
	// fields it knows, the new ones absent.
	var previous struct {
		Open   *bool              `json:"open"`
		RunEnd *ThreadAgentRunEnd `json:"runEnd"`
	}
	if err := json.Unmarshal(encoded, &previous); err != nil || previous.RunEnd == nil || *previous.RunEnd != *listing.RunEnd || !*previous.Open {
		t.Fatalf("the previous release reads %+v, %v", previous, err)
	}
	release(t, gate)
}
