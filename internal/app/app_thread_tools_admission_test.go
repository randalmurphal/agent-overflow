package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/entityid"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// Admission of a request on the computer that runs it: a retry of one token
// is answered from what the attempt that accepted it made, and never
// settles it.

// A retry arriving while the first attempt is still making the thread (a
// worktree add whose hook runs for minutes) is answered with the pending
// acceptance. The first attempt names its thread, which stays, and the
// source collects it under the one token.
func TestARetryWhileTheFirstAttemptMakesItsThreadGetsThePendingAcceptance(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app)
		faults = injected
		return wire
	})
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	faults.creating = make(chan struct{})
	faults.detachFirst.Store(true)
	release := make(chan struct{})
	var held sync.Once
	pair.dest.spawnNamingForTest = func(string) {
		held.Do(func() {
			close(faults.creating)
			<-release
		})
	}
	released := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		released()
		faults.detached.Wait()
	})

	ack := pair.spawnThere(t, "made while the source retries", nil)
	if ack.Token == "" || ack.State != store.ThreadRequestAccepted {
		t.Fatalf("ack = %+v, want the acceptance the destination holds", ack)
	}
	// No thread is named yet, so nothing says how it was placed: that is
	// not a computer too old to own it.
	if ack.Notice != "" {
		t.Fatalf("the pending acceptance carried a notice: %q", ack.Notice)
	}
	if receipt := pair.receipt(t, ack.Token); receipt.State != store.ThreadReceiptAccepted || receipt.TargetThreadID != "" {
		t.Fatalf("receipt while its thread is made = %+v, want it accepted and open", receipt)
	}
	// This build advertises thread-tools.v2, so the spawn was sent again.
	if sent := faults.attempts.Load(); sent != 2 {
		t.Fatalf("the destination was sent the spawn %d times, want it sent again once", sent)
	}

	released()
	faults.detached.Wait()
	receipt := pair.receipt(t, ack.Token)
	if receipt.TargetThreadID == "" || store.ThreadReceiptSettled(receipt.State) {
		t.Fatalf("receipt after its thread was made = %+v, want it naming a running thread", receipt)
	}
	if _, err := pair.dest.store.GetThread(receipt.TargetThreadID); err != nil {
		t.Fatalf("the thread the first attempt made: %v", err)
	}
	pair.poll(t)
	if row := pair.request(t, ack.Token); row.TargetThreadID != receipt.TargetThreadID || threadRequestSettled(row) {
		t.Fatalf("source row = %+v, want it naming %s and open", row, receipt.TargetThreadID)
	}
}

// newReleasedReceiverPair stands the destination up as v0.0.19: its hello
// advertises thread-tools.v1 alone, and a spawn or ask sent again while its
// thread is being made settles the receipt interrupted as that release does.
func newReleasedReceiverPair(t *testing.T) (*reachPair, *peerFaults) {
	t.Helper()
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app, func(cfg *transport.Config) {
			cfg.ThreadToolsOlderThan = transport.CapabilityThreadToolsV2
		})
		faults = injected
		return wire
	})
	faults.released.Store(true)
	return pair, faults
}

// A spawn whose answer is lost on its way back from v0.0.19 is never sent
// there again, which would settle a receipt its first attempt may still be
// writing: the caller is given the token unconfirmed, and the poll follows
// it. Here the first attempt's handler has not admitted the token yet, so
// the destination's status answers that it is unknown, which from v0.0.19
// says nothing before the request's admission deadline: the row stays
// unconfirmed, no second send follows, and the receipt the handler writes
// later is collected.
func TestALostSpawnIsNeverSentAgainToAReleasedReceiver(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := newReleasedReceiverPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	faults.creating = make(chan struct{})
	close(faults.creating)
	faults.admit = make(chan struct{})
	faults.detachFirst.Store(true)
	admitted := sync.OnceFunc(func() { close(faults.admit) })
	t.Cleanup(func() {
		admitted()
		faults.detached.Wait()
	})

	ack := pair.spawnThere(t, "admitted after its answer was lost", nil)
	returned := time.Now()
	if ack.Token == "" || ack.State != store.ThreadRequestUnconfirmed || ack.Outcome != threadtools.OutcomeUnconfirmed {
		t.Fatalf("ack = %+v, want the token unconfirmed", ack)
	}
	if attempts := faults.attempts.Load(); attempts != 1 {
		t.Fatalf("the released receiver was sent the spawn %d times, want once", attempts)
	}
	if _, found, err := pair.dest.store.GetThreadRequestReceipt(ack.Token); err != nil || found {
		t.Fatalf("the destination holds the token before its handler admitted it: found=%v err=%v", found, err)
	}
	pair.pollAt(t, pair.source, returned.Add(pair.source.threadPollNormal()))
	if row := pair.request(t, ack.Token); row.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("a poll before the admission deadline settled the request %q %q", row.State, row.Answer)
	}

	admitted()
	faults.detached.Wait()
	if attempts := faults.attempts.Load(); attempts != 1 {
		t.Fatalf("the released receiver was sent the spawn %d times, want once", attempts)
	}
	receipt := pair.receipt(t, ack.Token)
	if receipt.TargetThreadID == "" || store.ThreadReceiptSettled(receipt.State) {
		t.Fatalf("receipt the first attempt wrote = %+v, want it naming a running thread", receipt)
	}
	pair.poll(t)
	if row := pair.request(t, ack.Token); row.TargetThreadID != receipt.TargetThreadID || threadRequestSettled(row) {
		t.Fatalf("source row = %+v, want it naming %s and open", row, receipt.TargetThreadID)
	}
}

// Each retry is a dispatch of its own and records it on the row before it
// is sent: a poll while the retry is still out leaves the request alone,
// even once the first attempt's deadline has passed, instead of reading the
// destination's "unknown" as a refusal of a call it is about to admit.
func TestARetryRecordsItsOwnDispatch(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app)
		faults = injected
		return wire
	})
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	// As if the first attempt had used its whole wait: its deadline is past.
	execOnStore(t, pair.dbPath, `CREATE TRIGGER deadline_passed AFTER INSERT ON thread_requests
		BEGIN UPDATE thread_requests SET next_check = 0, admit_until = 0 WHERE token = NEW.token; END`)
	faults.unread.Store(1)
	faults.held, faults.hold = make(chan struct{}), make(chan struct{})
	released := sync.OnceFunc(func() { close(faults.hold) })
	t.Cleanup(released)

	type result struct {
		ack threadtools.RequestAck
		err error
	}
	spawned := make(chan result, 1)
	go func() {
		ack, err := pair.adapter().Spawn(t.Context(), pair.callerIdentity(), threadtools.SpawnCall{
			Prompt: "retried", ComputerID: pair.computer, ProjectID: pair.project,
		})
		spawned <- result{ack, err}
	}()
	select {
	case <-faults.held:
	case got := <-spawned:
		t.Fatalf("the spawn returned before its retry was sent: %+v, %v", got.ack, got.err)
	}
	rows, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 4, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the spawn's request: %d rows, %v", len(rows), err)
	}
	pair.source.pollRemoteThreadRequests(time.Now())
	pair.source.threadRequestsWG.Wait()
	if row := pair.request(t, rows[0].Token); row.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("a poll while the retry was out settled the request %q %q", row.State, row.Answer)
	}

	released()
	if got := <-spawned; got.err != nil || got.ack.ThreadID == "" || got.ack.State == store.ThreadRequestUnconfirmed {
		t.Fatalf("spawn = %+v, %v; want the retry's acceptance with its thread", got.ack, got.err)
	}
}

// A lost spawn whose v0.0.19 destination is still making its thread is
// followed, not sent again: the poll reads the receipt that names no thread
// yet as accepted and never settles it, and collects the thread once the
// first attempt names it.
func TestALostSpawnFollowsAReleasedReceiversUnnamedReceipt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := newReleasedReceiverPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	faults.creating = make(chan struct{})
	faults.detachFirst.Store(true)
	release := make(chan struct{})
	var held sync.Once
	pair.dest.spawnNamingForTest = func(string) {
		held.Do(func() {
			close(faults.creating)
			<-release
		})
	}
	released := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		released()
		faults.detached.Wait()
	})

	ack := pair.spawnThere(t, "made while the source recovers", nil)
	if ack.Token == "" || ack.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("ack = %+v, want the token unconfirmed", ack)
	}
	if attempts := faults.attempts.Load(); attempts != 1 {
		t.Fatalf("the released receiver was sent the spawn %d times, want once", attempts)
	}
	pair.poll(t)
	if row := pair.request(t, ack.Token); row.State != store.ThreadRequestAccepted || row.TargetThreadID != "" {
		t.Fatalf("source row while the thread is made = %+v, want it accepted with no thread yet", row)
	}
	if receipt := pair.receipt(t, ack.Token); store.ThreadReceiptSettled(receipt.State) {
		t.Fatalf("the receipt was settled while its thread was made: %q %q", receipt.State, receipt.Answer)
	}

	released()
	faults.detached.Wait()
	receipt := pair.receipt(t, ack.Token)
	if receipt.TargetThreadID == "" || store.ThreadReceiptSettled(receipt.State) {
		t.Fatalf("receipt after its thread was made = %+v, want it naming a running thread", receipt)
	}
	pair.poll(t)
	if row := pair.request(t, ack.Token); row.TargetThreadID != receipt.TargetThreadID || threadRequestSettled(row) {
		t.Fatalf("source row = %+v, want it naming %s and open", row, receipt.TargetThreadID)
	}
	if attempts := faults.attempts.Load(); attempts != 1 {
		t.Fatalf("the released receiver was sent the spawn %d times, want once", attempts)
	}
}

// Against v0.0.19 only a call that makes no thread is sent again after an
// attempt that may have reached it. A spawn that failed there unread is
// left to the poll, which settles it as never started once its admission
// deadline has passed; a send whose answer was lost is sent again and
// answered from the receipt it holds.
func TestAReleasedReceiverIsSentAgainOnlyWhatMakesNoThread(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := newReleasedReceiverPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})

	faults.unread.Store(1)
	lost := pair.spawnThere(t, "unread there", nil)
	if lost.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("spawn = %+v, want the token unconfirmed", lost)
	}
	if attempts := faults.attempts.Load(); attempts != 0 {
		t.Fatalf("the unread spawn reached the handler %d times, want it sent once and never read", attempts)
	}
	pair.poll(t)
	if row := pair.request(t, lost.Token); row.State != store.ThreadRequestRefused || !strings.Contains(string(row.Answer), "never accepted") {
		t.Fatalf("the poll past the deadline settled the spawn %q %q, want it never started", row.State, row.Answer)
	}

	spawn := pair.spawnThere(t, "answered", nil)
	if spawn.ThreadID == "" {
		t.Fatalf("spawn = %+v, want it accepted with its thread", spawn)
	}
	faults.dropReplies.Store(faults.calls.Load() + 1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	send, err := pair.adapter().Send(ctx, pair.callerIdentity(), threadtools.SendCall{
		ThreadID: spawn.ThreadID, ComputerID: pair.computer, Message: "and this",
	})
	if err != nil || send.ThreadID != spawn.ThreadID {
		t.Fatalf("send = %+v, %v; want it accepted into %s", send, err, spawn.ThreadID)
	}
	if answered := faults.calls.Load(); answered != 3 {
		t.Fatalf("the destination answered %d calls, want the spawn and the send twice", answered)
	}
}

// worktreeOfBranch is the path git lists for the worktree that has branch
// checked out, or "".
func worktreeOfBranch(t *testing.T, repo, branch string) string {
	t.Helper()
	listed, _, err := gitops.NewCore().Execute(repo, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("list worktrees: %v", err)
	}
	path := ""
	for line := range strings.Lines(listed) {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "worktree "); ok {
			path = rest
		}
		if line == "branch refs/heads/"+branch {
			return path
		}
	}
	return ""
}

// A token this device already holds a receipt for is answered from that
// receipt and never checked or started again, so a retry whose arguments
// would now be refused cannot settle the request the first attempt runs.
func TestARetryOfAHeldTokenIsAnsweredFromItsReceipt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working on it")
	target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	source := threadtools.Caller{ThreadID: entityid.New(), ComputerID: entityid.New(), ComputerName: "Their Mac", Title: "Remote lead"}
	token := newThreadRequestToken()
	send := func(threadID string) (ThreadPeerReply, error) {
		args, err := json.Marshal(threadtools.SendCall{ThreadID: threadID, Message: "look"})
		if err != nil {
			t.Fatal(err)
		}
		return f.app.runThreadPeerRequest(t.Context(), "peer-device", ThreadPeerCall{
			Tool: "thread_send", Args: args, Token: token, Source: source,
		})
	}
	if _, err := send(target.ID); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	reply, err := send(entityid.New())
	if err != nil {
		t.Fatalf("a retry of a held token was refused: %v", err)
	}
	if reply.Request == nil || !reply.Request.Known || reply.Request.TargetThreadID != target.ID {
		t.Fatalf("retry answer = %+v, want the receipt naming %s", reply.Request, target.ID)
	}
	if receipt := f.receipt(t, token); store.ThreadReceiptSettled(receipt.State) {
		t.Fatalf("the retry settled the request: %q %q", receipt.State, receipt.Answer)
	}
}

// Two attempts of one token can both reach admission before either wrote
// the receipt. The one whose receipt write finds the other's never creates
// or settles: it reports the acceptance as it stands, and a refusal of its
// arguments before admission settles nothing either. The thread the first
// attempt makes is named and kept.
func TestAConcurrentAttemptNeverSettlesTheReceiptItDidNotWrite(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	adapter := f.adapter()
	origin := threadRequestOrigin{
		caller:      threadtools.Caller{ThreadID: entityid.New(), ComputerID: entityid.New(), ComputerName: "Their Mac", Title: "Remote lead"},
		foreign:     true,
		ownerDevice: "peer-device",
	}
	made, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	token := newThreadRequestToken()
	creating, release := make(chan struct{}), make(chan struct{})
	released := sync.OnceFunc(func() { close(release) })
	t.Cleanup(released)
	type accepted struct {
		target string
		err    error
	}
	first := make(chan accepted, 1)
	go func() {
		target, _, err := adapter.acceptRequest(origin, store.ThreadRequest{Token: token, Kind: store.ThreadRequestSpawn},
			func() (store.Thread, madeWithThread, error) {
				close(creating)
				<-release
				return made, madeWithThread{}, nil
			}, false)
		first <- accepted{target, err}
	}()
	<-creating

	target, fresh, err := adapter.acceptRequest(origin, store.ThreadRequest{Token: token, Kind: store.ThreadRequestSpawn},
		func() (store.Thread, madeWithThread, error) {
			t.Error("the second attempt made a thread")
			return store.Thread{}, madeWithThread{}, nil
		}, false)
	if err != nil || fresh || target != "" {
		t.Fatalf("second attempt = %q fresh %v, %v; want the pending acceptance", target, fresh, err)
	}
	for name, refused := range map[string]func() error{
		"an ask of a thread that does not exist": func() error {
			_, err := adapter.acceptAsk(t.Context(), origin, token, threadtools.AskCall{ThreadID: entityid.New(), Question: "?"})
			return err
		},
		"a send to a thread that does not exist": func() error {
			_, err := adapter.acceptSend(t.Context(), origin, token, threadtools.SendCall{ThreadID: entityid.New(), Message: "?"})
			return err
		},
		"a spawn into a group it cannot be placed in": func() error {
			_, err := adapter.acceptSpawn(t.Context(), origin, token, threadtools.SpawnCall{Prompt: "?", Placement: threadtools.PlacementOwned, Group: "Release"})
			return err
		},
	} {
		if err := refused(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
		if receipt := f.receipt(t, token); store.ThreadReceiptSettled(receipt.State) {
			t.Fatalf("%s settled the receipt another attempt wrote: %q %q", name, receipt.State, receipt.Answer)
		}
	}

	released()
	got := <-first
	if got.err != nil || got.target != made.ID {
		t.Fatalf("first attempt = %q, %v; want it to name %s", got.target, got.err, made.ID)
	}
	if receipt := f.receipt(t, token); receipt.TargetThreadID != made.ID || store.ThreadReceiptSettled(receipt.State) {
		t.Fatalf("receipt = %+v, want it open and naming %s", receipt, made.ID)
	}
	if _, err := f.app.store.GetThread(made.ID); err != nil {
		t.Fatalf("the first attempt's thread: %v", err)
	}
}

// The boot settles a receipt the last run left naming no thread as never
// started: the attempt that was making its thread is gone with that run.
func TestABootSettlesAnUnnamedReceiptAsNeverStarted(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	token := newThreadRequestToken()
	if _, created, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "peer-device", SourceComputerID: entityid.New(), SourceThreadID: entityid.New(),
		Kind: store.ThreadRequestSpawn,
	}); err != nil || !created {
		t.Fatalf("accept the receipt: created=%v err=%v", created, err)
	}
	f.app.sweepThreadRequestsAtBoot()
	receipt := f.receipt(t, token)
	if receipt.State != store.ThreadReceiptInterrupted || string(receipt.Answer) != threadtools.InterruptNeverStarted {
		t.Fatalf("receipt = %q %q, want it interrupted as never started", receipt.State, receipt.Answer)
	}
}
