package app

import (
	"encoding/json"
	"sync"
	"testing"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
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
			func() (store.Thread, error) {
				close(creating)
				<-release
				return made, nil
			}, false)
		first <- accepted{target, err}
	}()
	<-creating

	target, fresh, err := adapter.acceptRequest(origin, store.ThreadRequest{Token: token, Kind: store.ThreadRequestSpawn},
		func() (store.Thread, error) {
			t.Error("the second attempt made a thread")
			return store.Thread{}, nil
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
