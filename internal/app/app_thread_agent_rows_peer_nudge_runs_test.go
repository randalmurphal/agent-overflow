package app

// Every nudge run on a paired computer's worker reaches the spawner's
// computer: each is kept there on its own until that computer took its
// end, so a run that ends and a run that starts between two of its reads,
// or while it does not answer, are both recorded here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// nudgeFaultPeer is a destination whose agent_rows answers a test can
// lose: unreachable fails a call before it reaches the app, dropReply after
// the app answered it. Once gated it answers only the calls readOnce
// permits, so a read the spawner starts on its own changes nothing. It
// records the nudge runs each call said it took.
type nudgeFaultPeer struct {
	*App
	unreachable atomic.Bool
	dropReply   atomic.Bool
	gated       atomic.Bool
	permits     atomic.Int32
	mu          sync.Mutex
	taken       [][]string
}

var errNudgeReplyLost = errors.New("the reply was lost")

func (p *nudgeFaultPeer) ThreadToolQuery(ctx context.Context, call ThreadPeerCall) (ThreadPeerReply, error) {
	if call.Tool != threadAgentPeerRows {
		return p.App.ThreadToolQuery(ctx, call)
	}
	if p.gated.Load() && p.permits.Add(-1) < 0 {
		p.permits.Add(1)
		return ThreadPeerReply{}, errPeerUnreachable
	}
	if p.unreachable.Load() {
		return ThreadPeerReply{}, errPeerUnreachable
	}
	var args threadAgentPeerArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return ThreadPeerReply{}, err
	}
	p.mu.Lock()
	p.taken = append(p.taken, args.NudgesTaken)
	p.mu.Unlock()
	reply, err := p.App.ThreadToolQuery(ctx, call)
	if err == nil && p.dropReply.Load() {
		return ThreadPeerReply{}, errNudgeReplyLost
	}
	return reply, err
}

// ThreadToolRequestStatus refuses the spawner's polls once gated: they read
// its links' state too, which would record and take nudge runs outside the
// reads readOnce permits.
func (p *nudgeFaultPeer) ThreadToolRequestStatus(ctx context.Context, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	if p.gated.Load() {
		return ThreadPeerPollReply{}, errPeerUnreachable
	}
	return p.App.ThreadToolRequestStatus(ctx, poll)
}

// sawTaken reports whether a call since the last reset said it took token.
func (p *nudgeFaultPeer) sawTaken(token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, taken := range p.taken {
		if slices.Contains(taken, token) {
			return true
		}
	}
	return false
}

func (p *nudgeFaultPeer) resetTaken() {
	p.mu.Lock()
	p.taken = nil
	p.mu.Unlock()
}

// newNudgeFaultPair is newReachPair with its destination served through a
// nudgeFaultPeer.
func newNudgeFaultPair(t *testing.T) (*reachPair, *nudgeFaultPeer) {
	t.Helper()
	var peer *nudgeFaultPeer
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		peer = &nudgeFaultPeer{App: app}
		return servePairedApp(t, app, func(cfg *transport.Config) {
			dispatcher := transport.NewDispatcher()
			if _, err := dispatcher.Register(peer, transport.RegisterOptions{
				Package: "main", TypeName: "App", AllowList: transport.NewMethodAllowList(),
			}); err != nil {
				t.Fatalf("register the destination: %v", err)
			}
			cfg.Dispatcher = dispatcher
		})
	})
	return pair, peer
}

// destNudges is the nudge runs the destination keeps for the spawn's link.
func destNudges(t *testing.T, pair *agentPeerPair) []store.NudgeRun {
	t.Helper()
	receipt := pair.receipt(t, pair.spawn.Token)
	run, err := pair.dest.store.ReceiptRunOf(receipt.OwnerDeviceID, pair.caller.ID, pair.child)
	if err != nil {
		t.Fatal(err)
	}
	return run.Nudges
}

// readOnce reads the spawn's rows once, as its pane's background read
// does, with the spawner's own reads held off and none in flight, and
// gates peer to answer that read alone from then on.
func readOnce(t *testing.T, pair *agentPeerPair, peer *nudgeFaultPeer) error {
	t.Helper()
	c := &pair.source.threadAgentPeers
	holdOff := func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.backoff == nil {
			c.backoff = map[string]threadAgentPeerBackoff{}
		}
		c.backoff[pair.computer] = threadAgentPeerBackoff{until: time.Now().Add(time.Hour)}
	}
	holdOff()
	defer holdOff()
	waitUntil(t, 10*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.inFlight == 0
	})
	peer.gated.Store(true)
	peer.permits.Store(1)
	defer peer.permits.Store(0)
	return pair.source.readPeerAgentRows(t.Context(), pair.spawn, threadAgentRowDepth).err
}

// restThere lets the worker's turn there end at gate and waits until its
// computer ended the runs its rest ends.
func restThere(t *testing.T, pair *reachPair, child, gate string) {
	t.Helper()
	release(t, gate)
	waitUntil(t, 10*time.Second, func() bool {
		return !threadToolsLiveState(pair.dest.triage.LiveStateSnapshotForThread(child)).ActiveTurn
	})
	waitUntil(t, 10*time.Second, func() bool {
		pair.dest.endRestedRuns()
		open, err := pair.dest.store.ThreadRunsEnding(child)
		return err == nil && !open
	})
}

// nudgeGates is n gate files, one per nudged turn.
func nudgeGates(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	gates := make([]string, n)
	for i := range gates {
		gates[i] = filepath.Join(dir, fmt.Sprintf("nudge-%d", i))
	}
	return gates
}

// The spawner's computer recorded a nudge open there; it ends, and the next
// starts, before that computer reads again. The next read ends the first
// here with its run's figures and records the second open.
func TestARemoteNudgeThatEndsBeforeTheNextStartsIsEndedHere(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	gates := nudgeGates(t, 2)
	agentPair, child := remoteNudgedWorker(t, pair, gates...)
	nudgeThere(t, pair, "the person there", child)
	first := agentPair.openNudgeRow(t)

	restThere(t, pair, child, gates[0])
	nudgeThere(t, pair, "the person there", child)
	if ended := agentPair.nudgeEnded(t, first.Token); ended.RunTotal.ToolCalls != 1 {
		t.Fatalf("the first nudge's run %+v; want its turn's call", ended.RunTotal)
	}
	second := agentPair.openNudgeRow(t)
	if second.Token == first.Token {
		t.Fatalf("the open row is the first nudge's %s; want the second's", first.Token)
	}
	restThere(t, pair, child, gates[1])
	if ended := agentPair.nudgeEnded(t, second.Token); ended.RunTotal.ToolCalls != 1 {
		t.Fatalf("the second nudge's run %+v; want its turn's call", ended.RunTotal)
	}
}

// Three nudges there between two reads of the spawner's computer: the first
// it recorded open, the second started and ended, the third still open.
// The next read records each, ended or open, and Past keeps all three.
func TestThreeRemoteNudgesBetweenTwoReadsAreEachRecordedHere(t *testing.T) {
	t.Parallel()
	pair := newReachPair(t)
	gates := nudgeGates(t, 3)
	agentPair, child := remoteNudgedWorker(t, pair, gates...)
	nudgeThere(t, pair, "the person there", child)
	first := agentPair.openNudgeRow(t)
	restThere(t, pair, child, gates[0])
	nudgeThere(t, pair, "the person there", child)
	restThere(t, pair, child, gates[1])
	nudgeThere(t, pair, "the person there", child)

	agentPair.nudgeEnded(t, first.Token)
	third := agentPair.openNudgeRow(t)
	restThere(t, pair, child, gates[2])
	agentPair.nudgeEnded(t, third.Token)
	page, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	var nudges []ThreadAgentRow
	for _, row := range page.Rows {
		if row.Kind == threadAgentKindNudge {
			nudges = append(nudges, row)
		}
	}
	if len(nudges) != 3 {
		t.Fatalf("Past nudge rows %+v; want the three nudges' runs", nudges)
	}
	for _, row := range nudges {
		if row.ToolCalls != 1 || row.open() {
			t.Fatalf("Past nudge row %+v; want each ended with its turn's call", row)
		}
	}
}

// The spawner's computer recorded a nudge open there and then cannot read
// that computer while the nudge ends and the next starts. Its first read
// after ends the first here with its run's figures and records the second.
func TestARemoteNudgeThatEndsWhileItsSpawnerCannotReadIsEndedHereAfter(t *testing.T) {
	t.Parallel()
	pair, peer := newNudgeFaultPair(t)
	gates := nudgeGates(t, 2)
	agentPair, child := remoteNudgedWorker(t, pair, gates...)
	nudgeThere(t, pair, "the person there", child)
	first := agentPair.openNudgeRow(t)

	peer.unreachable.Store(true)
	restThere(t, pair, child, gates[0])
	nudgeThere(t, pair, "the person there", child)
	if err := pair.source.readPeerAgentRows(t.Context(), agentPair.spawn, threadAgentRowDepth).err; err == nil {
		t.Fatal("a read of a computer that cannot be reached succeeded")
	}
	if pair.request(t, first.Token).RunEnded() {
		t.Fatal("the first nudge ended here with no answer from its computer")
	}
	peer.unreachable.Store(false)
	if ended := agentPair.nudgeEnded(t, first.Token); ended.RunTotal.ToolCalls != 1 {
		t.Fatalf("the first nudge's run %+v; want its turn's call", ended.RunTotal)
	}
	second := agentPair.openNudgeRow(t)
	if second.Token == first.Token {
		t.Fatalf("the open row is the first nudge's %s; want the second's", first.Token)
	}
	restThere(t, pair, child, gates[1])
	agentPair.nudgeEnded(t, second.Token)
}

// The worker's computer keeps an ended nudge run until the spawner's
// computer says it took it. A take whose call is lost is sent again with
// the next read; one that lands but whose answer is lost is sent again
// too, and changes nothing there. The run is recorded here once, with its
// figures.
func TestALostTakeOfARemoteNudgeRunIsSentAgain(t *testing.T) {
	t.Parallel()
	pair, peer := newNudgeFaultPair(t)
	gates := nudgeGates(t, 1)
	agentPair, child := remoteNudgedWorker(t, pair, gates...)
	nudgeThere(t, pair, "the person there", child)
	row := agentPair.openNudgeRow(t)
	// Only the reads below reach the worker's computer from here on.
	peer.gated.Store(true)
	restThere(t, pair, child, gates[0])
	waitUntil(t, 10*time.Second, func() bool {
		if err := readOnce(t, agentPair, peer); err != nil {
			t.Fatal(err)
		}
		return pair.request(t, row.Token).RunEnded()
	})
	recorded := pair.request(t, row.Token).RunTotal
	if recorded == nil || recorded.ToolCalls != 1 {
		t.Fatalf("the nudge's run %+v; want its turn's call", recorded)
	}
	if taken := pair.source.threadAgentPeers.nudgesTakenOf(agentPair.spawn.Token); !slices.Equal(taken, []string{row.Token}) {
		t.Fatalf("the next read takes %v; want the ended nudge", taken)
	}
	if kept := destNudges(t, agentPair); len(kept) != 1 || kept[0].End == nil {
		t.Fatalf("the worker's computer keeps %+v; want the ended run until it is taken", kept)
	}

	peer.unreachable.Store(true)
	if err := readOnce(t, agentPair, peer); err == nil {
		t.Fatal("a read of a computer that cannot be reached succeeded")
	}
	peer.unreachable.Store(false)
	if kept := destNudges(t, agentPair); len(kept) != 1 {
		t.Fatalf("a take that never arrived forgot %+v there", kept)
	}
	if taken := pair.source.threadAgentPeers.nudgesTakenOf(agentPair.spawn.Token); !slices.Equal(taken, []string{row.Token}) {
		t.Fatalf("after a lost call the next read takes %v; want the ended nudge again", taken)
	}

	peer.dropReply.Store(true)
	if err := readOnce(t, agentPair, peer); err == nil {
		t.Fatal("a read whose reply was lost succeeded")
	}
	peer.dropReply.Store(false)
	if kept := destNudges(t, agentPair); len(kept) != 0 {
		t.Fatalf("the take landed and the worker's computer still keeps %+v", kept)
	}
	if taken := pair.source.threadAgentPeers.nudgesTakenOf(agentPair.spawn.Token); !slices.Equal(taken, []string{row.Token}) {
		t.Fatalf("after a lost answer the next read takes %v; want the ended nudge again", taken)
	}

	peer.resetTaken()
	if err := readOnce(t, agentPair, peer); err != nil {
		t.Fatal(err)
	}
	if !peer.sawTaken(row.Token) {
		t.Fatal("the read after a lost answer did not take the nudge again")
	}
	if taken := pair.source.threadAgentPeers.nudgesTakenOf(agentPair.spawn.Token); len(taken) != 0 {
		t.Fatalf("after an answer without the nudge the next read takes %v; want nothing", taken)
	}
	if total := pair.request(t, row.Token).RunTotal; total == nil || *total != *recorded {
		t.Fatalf("the nudge's run %+v; want it as first recorded, %+v", total, recorded)
	}
}

// The spawner's computer reads its spawns there through its owner wait as
// well: with no agent_rows read answered, that read records each nudge
// run, and its next read takes the ended ones, which the worker's computer
// then forgets.
func TestTheOwnerWaitTakesTheRemoteNudgeRunsItRecorded(t *testing.T) {
	t.Parallel()
	pair, peer := newNudgeFaultPair(t)
	gates := nudgeGates(t, 3)
	agentPair, child := remoteNudgedWorker(t, pair, gates...)
	peer.unreachable.Store(true)
	startReconciledOwner(t, pair)
	for _, gate := range gates[:2] {
		nudgeThere(t, pair, "the person there", child)
		restThere(t, pair, child, gate)
	}
	ended := map[string]bool{}
	waitUntil(t, 10*time.Second, func() bool {
		page, err := pair.source.ListThreadPastAgentRows(t.Context(), pair.caller.ID, store.PastAgentRowsCursor{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Rows {
			if row.Kind == threadAgentKindNudge && !row.open() {
				ended[row.Token] = true
			}
		}
		return len(ended) == 2
	})
	nudgeThere(t, pair, "the person there", child)
	waitUntil(t, 10*time.Second, func() bool {
		kept := destNudges(t, agentPair)
		return len(kept) == 1 && kept[0].End == nil && !ended[kept[0].Token]
	})
	restThere(t, pair, child, gates[2])
}

// A nudge run's end that the spawner's computer fails to record is not
// taken: the worker's computer keeps it and reports it again, and the next
// read records it and takes it.
func TestARemoteNudgeEndThatFailsToRecordIsNotTaken(t *testing.T) {
	t.Parallel()
	pair, peer := newNudgeFaultPair(t)
	gates := nudgeGates(t, 1)
	agentPair, child := remoteNudgedWorker(t, pair, gates...)
	nudgeThere(t, pair, "the person there", child)
	row := agentPair.openNudgeRow(t)
	peer.gated.Store(true)
	execOnStore(t, pair.dbPath, `CREATE TRIGGER refuse_nudge_end BEFORE UPDATE OF run_tool_calls ON thread_requests
		WHEN OLD.nudge = 1 AND NEW.run_tool_calls IS NOT NULL BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	restThere(t, pair, child, gates[0])
	if err := readOnce(t, agentPair, peer); err == nil {
		t.Fatal("a read whose nudge end could not be recorded reported no failure")
	}
	if pair.request(t, row.Token).RunEnded() {
		t.Fatal("the nudge's end was recorded through the refusing trigger")
	}
	if taken := pair.source.threadAgentPeers.nudgesTakenOf(agentPair.spawn.Token); len(taken) != 0 {
		t.Fatalf("the next read takes %v; want nothing, the end not recorded", taken)
	}
	execOnStore(t, pair.dbPath, `DROP TRIGGER refuse_nudge_end`)
	if err := readOnce(t, agentPair, peer); err != nil {
		t.Fatal(err)
	}
	if kept := destNudges(t, agentPair); len(kept) != 1 || kept[0].Token != row.Token || kept[0].End == nil {
		t.Fatalf("the worker's computer keeps %+v; want the ended run, never taken", kept)
	}
	if ended := pair.request(t, row.Token); !ended.RunEnded() || ended.RunTotal.ToolCalls != 1 {
		t.Fatalf("the nudge's run %+v; want it ended with its turn's call", ended.RunTotal)
	}
	if taken := pair.source.threadAgentPeers.nudgesTakenOf(agentPair.spawn.Token); !slices.Equal(taken, []string{row.Token}) {
		t.Fatalf("the next read takes %v; want the nudge now recorded", taken)
	}
}
