package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/entityid"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadapp"
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

// A spawn whose reply was lost while v0.0.19 is still making its thread is
// not sent there again, which would settle it interrupted while its prompt
// ran anyway: the source reads the receipt and follows it.
func TestALostSpawnIsFollowedNotSentAgainToAReleasedReceiver(t *testing.T) {
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
	if ack.Token == "" || ack.State != store.ThreadRequestAccepted {
		t.Fatalf("ack = %+v, want the acceptance the destination holds", ack)
	}
	if sent := faults.attempts.Load(); sent != 1 {
		t.Fatalf("the released receiver was sent the spawn %d times, want once", sent)
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
}

// A released receiver that holds no receipt for the token never ran the
// call, so the spawn is sent to it again; a send makes no thread and is
// sent again as it is, answered from the receipt it holds.
func TestAReleasedReceiverIsSentAgainWhatItCanAnswerOnce(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := newReleasedReceiverPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})

	faults.unread.Store(1)
	spawn := pair.spawnThere(t, "unread the first time", nil)
	if spawn.ThreadID == "" || threadtools.RequestUnconfirmed == spawn.State {
		t.Fatalf("spawn = %+v, want it accepted with its thread", spawn)
	}
	if answered := faults.calls.Load(); answered != 1 {
		t.Fatalf("the spawn ran %d times there, want once", answered)
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

// A spawn settled while its worktree is being cut never names its thread,
// and the creation that cut the worktree takes it back: the checkout and
// the branch it made are gone with the thread.
func TestASpawnSettledWhileItsWorktreeIsCutRemovesTheCheckoutAndBranch(t *testing.T) {
	t.Parallel()
	repo, _ := initGitRepoWithOrigin(t)
	f := newRequestFixtureIn(t, repo)
	f.mockClaude(t, "never asked")
	const branch = "settled-while-cut"
	creating, release := make(chan struct{}), make(chan struct{})
	released := sync.OnceFunc(func() { close(release) })
	t.Cleanup(released)
	f.app.spawnNamingForTest = func(string) {
		close(creating)
		<-release
	}
	spawned := make(chan error, 1)
	go func() {
		_, err := f.adapter().Spawn(context.WithoutCancel(t.Context()), f.callerIdentity(),
			threadtools.SpawnCall{Prompt: "work in a fresh checkout", WorktreeBranch: branch})
		spawned <- err
	}()
	select {
	case <-creating:
	case err := <-spawned:
		t.Fatalf("the spawn ended before naming its thread: %v", err)
	}
	checkout := worktreeOfBranch(t, repo, branch)
	if checkout == "" || !branchExists(repo, branch) {
		t.Fatalf("the cut made no checkout of %s", branch)
	}
	rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 4, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the spawn's request: %d rows, %v", len(rows), err)
	}
	if _, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: rows[0].Token}); err != nil {
		t.Fatalf("cancel the spawn while its worktree is cut: %v", err)
	}

	released()
	if err := <-spawned; err == nil {
		t.Fatal("a spawn settled while its thread was made was accepted")
	}
	if _, err := os.Stat(checkout); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the checkout %s is still there: %v", checkout, err)
	}
	if path := worktreeOfBranch(t, repo, branch); path != "" {
		t.Errorf("git still lists the worktree %s", path)
	}
	if branchExists(repo, branch) {
		t.Errorf("the branch %s the cut made is still there", branch)
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
			func() (store.Thread, threadapp.CutWorktree, error) {
				close(creating)
				<-release
				return made, threadapp.CutWorktree{}, nil
			}, false)
		first <- accepted{target, err}
	}()
	<-creating

	target, fresh, err := adapter.acceptRequest(origin, store.ThreadRequest{Token: token, Kind: store.ThreadRequestSpawn},
		func() (store.Thread, threadapp.CutWorktree, error) {
			t.Error("the second attempt made a thread")
			return store.Thread{}, threadapp.CutWorktree{}, nil
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
