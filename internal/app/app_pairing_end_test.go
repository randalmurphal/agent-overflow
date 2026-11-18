package app

import (
	"context"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The end of a pairing over both ledgers: a computer that revoked this one,
// and one the person forgot here.

// runningRemoteWatch records a command the destination accepted and still
// runs, as the watcher last saw it.
func runningRemoteWatch(t *testing.T, a *App, computerID string, thread store.Thread) store.RemoteWatch {
	t.Helper()
	w := store.RemoteWatch{ComputerID: computerID, RequestID: uuid.NewString(), ThreadID: thread.ID, Fingerprint: strings.Repeat("a", 64), Label: "nightly build"}
	if _, err := a.store.RegisterRemoteWatch(w); err != nil {
		t.Fatal(err)
	}
	w.Receipt = store.RemoteJob{ID: w.RequestID, SourceThreadID: thread.ID, State: "running", StartedAt: time.Now().UnixMilli(), ExitCode: -1}
	if err := a.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, "", 0); err != nil {
		t.Fatal(err)
	}
	return w
}

func savedRemoteWatch(t *testing.T, a *App, w store.RemoteWatch) store.RemoteWatch {
	t.Helper()
	saved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

// A computer that revoked this one answers nothing again. The poll's
// verdict ends everything this computer holds about it: a finished request
// still listening for a late reply leaves the poll with its answer kept,
// rather than being asked about every second, and a command it accepted is
// recorded with its outcome unknown rather than retried as an outage.
func TestAnEndedPairingRetiresItsFinishedRequestsAndItsCommands(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptFinished, "the build is green")
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestFinished || !row.Polling {
		t.Fatalf("collected %q polling=%v, want finished and still listening for a late reply", row.State, row.Polling)
	}
	command := runningRemoteWatch(t, pair.source, pair.computer, pair.caller)

	revokeReachDevice(t, pair.dest, "source")
	pair.poll(t)

	row := pair.request(t, token)
	if row.Polling || row.State != store.ThreadRequestFinished || string(row.Answer) != "the build is green" {
		t.Fatalf("after the pairing ended: %q %q polling=%v, want the answer kept and the poll ended", row.State, row.Answer, row.Polling)
	}
	saved := savedRemoteWatch(t, pair.source, command)
	if saved.Receipt.State != "unknown" || !strings.Contains(saved.Receipt.Error, "outcome is unknown") {
		t.Fatalf("the command's receipt after the pairing ended: %+v", saved.Receipt)
	}
	if saved.CancelOwedAt != 0 {
		t.Fatalf("a cancel is still owed to a computer that answers nothing: %+v", saved)
	}
}

// A command whose destination revoked this computer is told to its thread
// as finished with an outcome nobody can know, once; the requests to that
// computer settle in the same transition.
func TestARevokedCommandWatchRecordsItsOutcomeAsUnknown(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, destination, computer, project := pairedRemoteSource(t, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
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
	open := uuid.NewString()
	if err := source.store.InsertThreadRequest(store.ThreadRequest{Token: open, CallerThreadID: thread.ID, Kind: store.ThreadRequestSend,
		State: store.ThreadRequestRunning, TargetComputerID: computer, TargetThreadID: uuid.NewString(), Notify: true}); err != nil {
		t.Fatal(err)
	}

	revokeReachDevice(t, destination.app, "source")
	watch := func() {
		t.Helper()
		due, err := source.store.ListRemoteWatches("", time.Now().Add(time.Hour).UnixMilli(), 32)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range due {
			source.checkRemoteWatch(row)
		}
		source.waitAgentNoticeWork()
	}
	waitUntilE2E(t, 30*time.Second, "the watcher tells the thread the command's outcome", func() bool {
		watch()
		return savedRemoteWatch(t, source, store.RemoteWatch{ComputerID: computer, RequestID: id}).Notification != "pending"
	})

	w := savedRemoteWatch(t, source, store.RemoteWatch{ComputerID: computer, RequestID: id})
	if w.Receipt.State != "unknown" {
		t.Fatalf("a command on a computer that revoked this one reads %q (%s)", w.Receipt.State, w.Error)
	}
	if w.Notification != "queued" {
		t.Fatalf("the unknown outcome was not told to its thread: watch %q", w.Notification)
	}
	// Told once each: the command's outcome, and the request's error.
	durable, _ := queuedSendIDs(t, source, thread.ID)
	if !slices.Equal(durable, []string{threadWakeSendID(open), remoteCompletionSendID(w)}) {
		t.Fatalf("queue %q, want the request's wake and the command's completion once each", durable)
	}
	// The Background row says why, not that the job failed.
	items, err := source.remoteTrayItems(thread.ID, 0)
	if err != nil || len(items) != 2 || items[1].Status != "errored" || !strings.Contains(items[1].Meta, "outcome is unknown") {
		t.Fatalf("the Background rows of the command: %+v, %v", items, err)
	}
	row, found, err := source.store.GetThreadRequest(open)
	if err != nil || !found {
		t.Fatalf("read the request: found=%v err=%v", found, err)
	}
	if row.State != store.ThreadRequestErrored || row.Polling {
		t.Fatalf("a request to the computer that revoked this one is %q polling=%v", row.State, row.Polling)
	}
}

// Forgetting a computer settles every open request it holds, however many,
// and its warning names a bounded list with the total.
func TestForgettingAComputerSettlesEveryOpenRequest(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	const open = threadForgetRequestListing + 1
	tokens := make([]string, 0, open)
	for range open {
		token := uuid.NewString()
		if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSend,
			State: store.ThreadRequestRunning, TargetComputerID: pair.computer, TargetThreadID: uuid.NewString(), Notify: true}); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}

	_, message, _ := errorsx.PublicDetails(pair.source.RemoveBackend(pair.computer, false))
	listed := 0
	for _, token := range tokens {
		if strings.Contains(message, token) {
			listed++
		}
	}
	if !strings.Contains(message, "21 agent thread requests") || listed != threadForgetRequestListing {
		t.Fatalf("the warning does not name a bounded list (%d listed) and the total: %q", listed, message)
	}
	if err := pair.source.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	for _, token := range tokens {
		row := pair.request(t, token)
		if row.State != store.ThreadRequestErrored || row.Notify {
			t.Fatalf("request %s after the forget: %q notify=%v", token, row.State, row.Notify)
		}
	}
}

// A request being admitted to a computer while the person forgets it is
// either refused for want of the pairing or settled by the forget with the
// person's choice: admission checks the pairing and writes its row inside
// the fence the forget takes.
func TestARequestAdmittedWhileAComputerIsForgottenIsSettledByTheForget(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	entered, release := make(chan struct{}), make(chan struct{})
	pair.source.admitWorkForTest = func(threadID string) {
		if threadID == pair.caller.ID {
			close(entered)
			<-release
		}
	}
	spawned := make(chan error, 1)
	go func() {
		_, err := pair.adapter().Spawn(context.Background(), pair.callerIdentity(), threadtools.SpawnCall{
			Prompt: "admitted while the computer is forgotten", ComputerID: pair.computer, ProjectID: pair.project,
		})
		spawned <- err
	}()
	<-entered
	forgot, forgetDone := make(chan error, 1), make(chan struct{})
	go func() {
		forgot <- pair.source.RemoveBackend(pair.computer, true)
		close(forgetDone)
	}()
	// The forget is parked on the fence once the fence has two holders: the
	// admission inside it and the forget waiting for it.
	fence := "computer:" + pair.computer
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		for pair.source.remoteStartLocks().Refs(fence) < 2 {
			select {
			case <-forgetDone:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	select {
	case <-forgetDone:
		close(release)
		t.Fatalf("the forget finished (%v) while a request was being admitted to the computer it removed", <-forgot)
	case <-parked:
	}
	close(release)
	if err := <-forgot; err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	<-spawned
	rows, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the caller's requests: %d, %v", len(rows), err)
	}
	if row := rows[0]; row.State != store.ThreadRequestErrored || row.Notify || !strings.Contains(string(row.Answer), "forgotten") {
		t.Fatalf("the request admitted during the forget is %q notify=%v: %q", row.State, row.Notify, row.Answer)
	}
}
