package app

import (
	"context"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The sweeps' share of the end of a pairing: the work of a computer with no
// pairing saved is ended by the first sweep that finds it, when nothing
// announced the end (pairedRemoteSource wires no observer), and nothing is
// asked about it.

// A pairing whose profile went where this process was not told (another
// process removed it) is found by the poll: the computer has no pairing
// saved, so its work is ended then and the poll asks it nothing, though a
// connection to it is still open here.
func TestAPollEndsTheWorkOfAComputerWithNoPairingAndAsksNothingOfIt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := faultyReachPair(t)
	token := pair.runningThere(t)
	if err := deviceclient.ForgetSession(pair.profile, pair.computer); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []string
	record := func(poll *ThreadPeerPoll) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, poll.Tokens...)
	}
	faults.asked.Store(&record)

	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestErrored || string(row.Answer) != pairingEndedHere || row.Polling {
		t.Fatalf("the poll left the request %q %q polling=%v, want it ended", row.State, row.Answer, row.Polling)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(asked, token) {
		t.Fatalf("the poll asked a computer with no pairing about its request: %v", asked)
	}
}

// heldRemoteCommand starts a command on a paired computer that runs until
// it is stopped, watched here, and returns the watch's check, run once.
func heldRemoteCommand(t *testing.T) (source, destination *App, computer string, check func() store.RemoteWatch) {
	t.Helper()
	source, paired, computer, project := pairedRemoteSource(t, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
		<-ctx.Done()
		return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
	})
	source.startSessionFn = func(string) error { return nil }
	thread := remoteWatchThread(t, source, string(provider.Claude))
	caller := transport.WithCallerScope(context.Background(), transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID})
	id := uuid.NewString()
	if _, err := source.AgentRemoteStart(caller, AgentRemoteRequest{ComputerID: computer, Workspace: gitapp.WorkspaceRef{ProjectID: project},
		Request: remotejobs.Request{ID: id, Argv: []string{"hold", id}, TimeoutSeconds: 60}}); err != nil {
		t.Fatal(err)
	}
	check = func() store.RemoteWatch {
		t.Helper()
		source.checkRemoteWatch(savedRemoteWatch(t, source, store.RemoteWatch{ComputerID: computer, RequestID: id}))
		return savedRemoteWatch(t, source, store.RemoteWatch{ComputerID: computer, RequestID: id})
	}
	return source, paired.app, computer, check
}

// A command's status check refused because the pairing ended ends that
// pairing then, so its one check records the outcome as unknown.
func TestACommandCheckRefusedForAnEndedPairingEndsItAtOnce(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	_, destination, _, check := heldRemoteCommand(t)
	revokeReachDevice(t, destination, "source")
	if w := check(); w.Receipt.State != store.RemoteOutcomeUnknown {
		t.Fatalf("the refused check left the command %q (%s)", w.Receipt.State, w.Error)
	}
}

// A command whose pairing ended where its watcher was not told is ended by
// its next check, which asks that computer nothing.
func TestACommandCheckEndsAPairingNoLongerHeldWithoutAskingIt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, destination, computer, check := heldRemoteCommand(t)
	revokeReachDevice(t, destination, "source")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := source.backends.CallThreadPeer(ctx, computer, "ThreadToolResolve", nil); err == nil {
		t.Fatal("a call on a pairing the other computer ended succeeded")
	}
	if w := check(); w.Receipt.State != store.RemoteOutcomeUnknown {
		t.Fatalf("the check left the command of an ended pairing %q (%s)", w.Receipt.State, w.Error)
	}
}

// A poll refused because the pairing ended ends that pairing then, so its
// open requests settle on that one poll, as the other computer's verdict
// ends them though nothing here acted on its announcement.
func TestAPollRefusedForAnEndedPairingEndsItAtOnce(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.runningThere(t)
	pair.manager.SetChanged(func(attachedbackends.SetChange) {})
	revokeReachDevice(t, pair.dest, "source")
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestErrored || row.Polling || string(row.Answer) != pairingEndedByComputer {
		t.Fatalf("the refused poll left the request %q %q polling=%v (%s)", row.State, row.Answer, row.Polling, row.Issue)
	}
}

// Acknowledgements refused because the pairing ended end that pairing then,
// which owes that computer nothing more.
func TestAcknowledgementsRefusedForAnEndedPairingEndItAtOnce(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptFinished, "the first answer")
	pair.poll(t)
	// Its acknowledgement lost on the way, so it is owed again.
	if owed, err := pair.source.store.OweThreadRequestAck(token, 1); err != nil || !owed {
		t.Fatalf("owe the acknowledgement again: %v, %v", owed, err)
	}
	pair.manager.SetChanged(func(attachedbackends.SetChange) {})
	revokeReachDevice(t, pair.dest, "source")
	pair.source.sendOwedThreadRequestAcks(pair.computer)
	pair.source.threadRequestsWG.Wait()
	if row := pair.request(t, token); row.AckOwed != 0 || row.Polling {
		t.Fatalf("the refused acknowledgement left the request owing %d, polling=%v", row.AckOwed, row.Polling)
	}
}

// An owed cancel refused because the pairing ended ends that pairing then:
// the cancel is settled and the command's outcome recorded as unknown.
func TestAnOwedCancelRefusedForAnEndedPairingEndsItAtOnce(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, destination, computer, check := heldRemoteCommand(t)
	w := check()
	now := time.Now()
	if err := source.store.OweRemoteCancel(computer, w.RequestID, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	revokeReachDevice(t, destination, "source")
	source.retryOwedRemoteCancels(now)
	if got := savedRemoteWatch(t, source, w); got.CancelOwedAt != 0 || got.Receipt.State != store.RemoteOutcomeUnknown {
		t.Fatalf("the refused cancel left the command %q with its cancel owed since %d", got.Receipt.State, got.CancelOwedAt)
	}
}
