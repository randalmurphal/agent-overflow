package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// The pairing a record names is the relationship with that computer, not
// the session that carries it today: an automatic upgrade of the session
// keeps the relationship and its work, an explicit new pairing starts a new
// one, and nothing made on behalf of a record crosses from one to the other.

// rePair is the person pairing this computer with the destination again
// from a fresh link, confirmed there, over the profile it already holds.
func (p *reachPair) rePair(t *testing.T) {
	t.Helper()
	if id := attachDestination(t, p, p.destWire, p.dest); id != p.computer {
		t.Fatalf("the new pairing names the computer %s, want %s", id, p.computer)
	}
}

// ownDeviceOf is the device id the destination knows source's profile for
// it by: the one whose live sessions include that profile's session.
func ownDeviceOf(t *testing.T, destination *App, sourceProfile, destinationID string) string {
	t.Helper()
	session, err := deviceclient.LoadSession(sourceProfile, destinationID)
	if err != nil {
		t.Fatal(err)
	}
	overview, err := destination.GetAccessOverview()
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range overview.Devices {
		for _, held := range device.Sessions {
			if held.ID == session.SessionID {
				return device.ID
			}
		}
	}
	t.Fatalf("the destination holds no device for session %s", session.SessionID)
	return ""
}

// Two computers with an ordinary pairing, a request open from one to the
// other and a command running there, both join a third computer's group,
// which upgrades the profile to a group session. Nobody ended the pairing,
// so the request and the command stay open and are collected under the
// upgraded session.
func TestAnAutomaticUpgradeOfAPairingKeepsItsWork(t *testing.T) {
	remotetest.Require(t)
	ownConnectionNetwork(t)
	a, b, c := ownConnectionBackend(t), ownConnectionBackend(t), ownConnectionBackend(t)
	pairOwnConnection(t, c, a.app.backends, false)
	a.app.startSessionFn = func(string) error { return nil }
	a.app.installThreadRequestObserver()

	// A command running on C, started from a thread on A.
	release := make(chan struct{})
	jobs, err := remotejobs.New(context.Background(), c.app.store, func(ctx context.Context, _ string, _ []string, out io.Writer) (remotejobs.Outcome, error) {
		select {
		case <-release:
			_, _ = io.WriteString(out, "built")
			return remotejobs.Outcome{ExitCode: 0}, nil
		case <-ctx.Done():
			return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	c.app.remoteJobs = jobs
	t.Cleanup(jobs.Close)
	project, err := c.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "destination", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.app.SetAgentComputerEnabled(ctx, c.id, true); err != nil {
		t.Fatal(err)
	}
	thread := remoteWatchThread(t, a.app, string(provider.Claude))
	caller := transport.WithCallerScope(context.Background(), transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID})
	commandID := uuid.NewString()
	if _, err := a.app.AgentRemoteStart(caller, AgentRemoteRequest{ComputerID: c.id, Workspace: gitapp.WorkspaceRef{ProjectID: project.ID},
		Request: remotejobs.Request{ID: commandID, Argv: []string{"build", commandID}, TimeoutSeconds: 60}}); err != nil {
		t.Fatal(err)
	}

	// A request open on C, its row here written as admission writes it.
	token := uuid.NewString()
	pairing, err := a.app.currentPairing(c.id)
	if err != nil || pairing == "" {
		t.Fatalf("the pairing held with C: %q, %v", pairing, err)
	}
	if err := a.app.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: thread.ID, Kind: store.ThreadRequestSpawn,
		State: store.ThreadRequestRunning, TargetComputerID: c.id, Pairing: pairing}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{Token: token, OwnerDeviceID: ownDeviceOf(t, c.app, a.profile, c.id),
		Kind: store.ThreadRequestSpawn, State: store.ThreadReceiptRunning}); err != nil {
		t.Fatal(err)
	}

	before, err := deviceclient.LoadSession(a.profile, c.id)
	if err != nil {
		t.Fatal(err)
	}
	pairOwnConnection(t, b, a.app.backends, true)
	pairOwnConnection(t, b, c.app.backends, true)
	for range 3 {
		for _, host := range []ownConnectionHost{a, c, b} {
			reconcileOwnConnection(t, host)
		}
	}
	after, err := deviceclient.LoadSession(a.profile, c.id)
	if err != nil || !after.OwnDevice || after.SessionID == before.SessionID {
		t.Fatalf("the group did not upgrade A's profile for C, so this test guards nothing: %+v %v", after, err)
	}

	if result, err := c.app.store.SettleLocalThreadRequest(token, store.ThreadReceiptOpenStates(),
		store.ThreadRequestSettlement{State: store.ThreadReceiptFinished, Answer: []byte("done on C"), AnswerKind: store.ThreadAnswerFinal},
		store.ThreadRequestStateForReceipt(store.ThreadReceiptFinished)); err != nil || !result.Settled {
		t.Fatalf("settle the receipt on C: %+v, %v", result, err)
	}
	a.app.pollRemoteThreadRequests(time.Now().Add(time.Hour))
	a.app.threadRequestsWG.Wait()
	row, _, err := a.app.store.GetThreadRequest(token)
	if err != nil || row.State != store.ThreadRequestFinished || string(row.Answer) != "done on C" {
		t.Fatalf("after the upgrade the request is %q %q (%v), want collected from C", row.State, row.Answer, err)
	}

	check := func() store.RemoteWatch {
		t.Helper()
		a.app.checkRemoteWatch(savedRemoteWatch(t, a.app, store.RemoteWatch{ComputerID: c.id, RequestID: commandID}))
		return savedRemoteWatch(t, a.app, store.RemoteWatch{ComputerID: c.id, RequestID: commandID})
	}
	if w := check(); w.Receipt.State != "running" || w.Error != "" {
		t.Fatalf("after the upgrade the command reads %q (%s), want still running", w.Receipt.State, w.Error)
	}
	close(release)
	waitUntilE2E(t, 30*time.Second, "the command's finish is collected under the upgraded session", func() bool {
		return check().Receipt.State == "succeeded"
	})
}

// migratedRecords reshapes the source's records on the destination as the
// migration from v0.0.19 leaves them, naming no pairing, and boots over
// them as Start does, which binds them before any pairing can change.
func (p *reachPair) migratedRecords(t *testing.T) {
	t.Helper()
	execOnStore(t, p.dbPath, `UPDATE thread_requests SET pairing = '' WHERE target_computer_id <> ''`)
	execOnStore(t, p.dbPath, `UPDATE remote_watches SET pairing = ''`)
	p.source.bindRecordPairingsAtBoot()
}

// A record from v0.0.19 belongs to the pairing held when this build first
// boots over it: it is collected under that pairing, and acknowledged.
func TestARecordFromBeforePairingsWereNamedIsBoundToThePairingHeldAtBoot(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptFinished, "the answer")
	pair.migratedRecords(t)
	if row := pair.request(t, token); row.Pairing != pair.held(t) {
		t.Fatalf("the boot bound the record to %q, want the pairing held", row.Pairing)
	}
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestFinished || string(row.Answer) != "the answer" {
		t.Fatalf("the record bound at boot was not collected: %q %q", row.State, row.Answer)
	}
	if receipt := pair.receipt(t, token); receipt.CollectedRevision != 1 {
		t.Fatalf("the destination holds acknowledgement %d, want 1", receipt.CollectedRevision)
	}
}

// A record from v0.0.19 whose computer has no pairing left at boot belongs
// to none: the boot ends it, and a pairing made afterwards does not take it.
func TestARecordFromBeforePairingsWereNamedWithNoPairingLeftIsEndedAtBoot(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := uuid.NewString()
	if err := pair.source.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: pair.caller.ID, Kind: store.ThreadRequestSend,
		State: store.ThreadRequestRunning, TargetComputerID: uuid.NewString(), TargetThreadID: uuid.NewString(), Notify: true}); err != nil {
		t.Fatal(err)
	}
	pair.source.bindRecordPairingsAtBoot()
	if row := pair.request(t, token); row.State != store.ThreadRequestErrored || row.Pairing != store.PairingNoneHeld || row.Polling {
		t.Fatalf("with no pairing left the boot left the record %q %q polling=%v", row.State, row.Pairing, row.Polling)
	}
	if computers, err := pair.source.store.UnboundPairingComputers(); err != nil || len(computers) != 0 {
		t.Fatalf("records naming no pairing after the boot: %v, %v", computers, err)
	}
}

// A record shaped as the migration from v0.0.19 leaves it, naming no
// pairing, then a new explicit pairing confirmed over the saved profile:
// the record belongs to the pairing it was made under, which the new one
// is not, so it is ended rather than collected, and nothing is
// acknowledged for it.
func TestARecordFromBeforePairingsWereNamedIsNotTakenByANewPairing(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptFinished, "the old pairing's answer")
	pair.migratedRecords(t)

	pair.rePair(t)
	pair.poll(t)
	row := pair.request(t, token)
	if row.State != store.ThreadRequestErrored || row.AckOwed != 0 {
		t.Fatalf("the new pairing took up the old pairing's request: %q %q ack owed %d", row.State, row.Answer, row.AckOwed)
	}
	if receipt := pair.receipt(t, token); receipt.CollectedRevision != 0 {
		t.Fatalf("the new pairing acknowledged revision %d of the old pairing's request", receipt.CollectedRevision)
	}
}

// A request, then a new explicit pairing, then the agent's thread_cancel of
// the old token: the cancel is for the old pairing, so nothing is sent to
// the destination and the request is ended here.
func TestACancelForAReplacedPairingSendsNothing(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := faultyReachPair(t)
	token := pair.runningThere(t)
	var mu sync.Mutex
	var sent []string
	record := func(call ThreadPeerCall) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, call.Tool)
	}
	faults.answered.Store(&record)

	pair.rePair(t)
	if _, err := pair.adapter().Cancel(context.Background(), pair.callerIdentity(), threadtools.CancelCall{Token: token}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	mu.Lock()
	reached := slices.Contains(sent, "thread_cancel")
	mu.Unlock()
	if reached {
		t.Fatal("the cancel of the old pairing's request was sent under the new pairing")
	}
	if receipt := pair.receipt(t, token); receipt.State != store.ThreadReceiptRunning {
		t.Fatalf("the destination's receipt is %q, want the work there untouched", receipt.State)
	}
	if row := pair.request(t, token); row.State != store.ThreadRequestErrored || row.Polling {
		t.Fatalf("the cancelled request of the old pairing is %q polling=%v, want ended here", row.State, row.Polling)
	}
}

// The same for an ask stopped with its caller, as an archive, delete or
// move stops it: nothing is sent, and the ask is ended here.
func TestAnAskStoppedWithItsCallerAfterANewPairingSendsNothing(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := faultyReachPair(t)
	token := pair.runningThere(t)
	var mu sync.Mutex
	var sent []string
	record := func(call ThreadPeerCall) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, call.Tool)
	}
	faults.answered.Store(&record)

	pair.rePair(t)
	if err := pair.source.cancelOwnedAsks(context.Background(), pair.caller.ID, []store.ThreadRequest{pair.request(t, token)}); err != nil {
		t.Fatalf("stop the ask: %v", err)
	}
	pair.source.threadRequestsWG.Wait()
	mu.Lock()
	reached := slices.Contains(sent, "thread_cancel")
	mu.Unlock()
	if reached {
		t.Fatal("the stop of the old pairing's ask was sent under the new pairing")
	}
	if receipt := pair.receipt(t, token); receipt.State != store.ThreadReceiptRunning {
		t.Fatalf("the destination's receipt is %q, want the work there untouched", receipt.State)
	}
	if row := pair.request(t, token); row.State != store.ThreadRequestErrored || row.Polling {
		t.Fatalf("the stopped ask of the old pairing is %q polling=%v, want ended here", row.State, row.Polling)
	}
}

// replacedCommand starts a command on a paired computer that runs until it
// is stopped, then pairs with that computer again from a new link. stopped
// reports whether the command has been stopped there: the destination
// cancels its run before it answers the cancel, so it reads true as soon as
// a cancel call returns.
func replacedCommand(t *testing.T) (source *App, thread store.Thread, computer, id string, stopped func() bool) {
	t.Helper()
	running := make(chan context.Context, 1)
	source, destination, computer, project := pairedRemoteSource(t, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
		running <- ctx
		<-ctx.Done()
		return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
	})
	source.startSessionFn = func(string) error { return nil }
	thread = remoteWatchThread(t, source, string(provider.Claude))
	caller := transport.WithCallerScope(context.Background(), transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: thread.ID, ProjectID: thread.ProjectID})
	id = uuid.NewString()
	if _, err := source.AgentRemoteStart(caller, AgentRemoteRequest{ComputerID: computer, Workspace: gitapp.WorkspaceRef{ProjectID: project},
		Request: remotejobs.Request{ID: id, Argv: []string{"hold", id}, TimeoutSeconds: 60}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var run context.Context
	select {
	case run = <-running:
	case <-ctx.Done():
		t.Fatal("the command never started there")
	}

	invite, _ := destination.mintLink(t, "full")
	if _, err := source.backends.Add(ctx, invite.URL); err != nil {
		t.Fatal(err)
	}
	if err := destination.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := source.backends.Await(ctx, computer); err != nil {
		t.Fatal(err)
	}
	return source, thread, computer, id, func() bool { return run.Err() != nil }
}

// The same for a command: the person stops a command the old pairing
// started, after a new pairing replaced it. Nothing reaches the computer
// that runs it, and its outcome is recorded here as unknown.
func TestAStopOfACommandForAReplacedPairingSendsNothing(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, thread, computer, id, stopped := replacedCommand(t)
	receipt, err := source.CancelThreadRemoteCommand(context.Background(), thread.ID, computer, id)
	if err != nil {
		t.Fatalf("stop the command: %v", err)
	}
	if stopped() {
		t.Fatal("the stop of the old pairing's command reached the computer that runs it")
	}
	w := savedRemoteWatch(t, source, store.RemoteWatch{ComputerID: computer, RequestID: id})
	if receipt.State == "canceled" || w.Receipt.State != store.RemoteOutcomeUnknown {
		t.Fatalf("the stop answered %q and left the command %q, want its outcome unknown here", receipt.State, w.Receipt.State)
	}
}

// The same for a command stopped with its thread, as an archive stops it:
// nothing is sent and no cancel is owed; the command is dismissed with the
// thread, as one whose cancel was taken is.
func TestACommandStoppedWithItsThreadAfterANewPairingSendsNothing(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	source, thread, computer, id, stopped := replacedCommand(t)
	undelivered, err := source.cancelThreadRemoteCommands(context.Background(), thread.ID, true)
	if undelivered != nil || err != nil {
		t.Fatalf("stop the thread's commands: undelivered %v, %v", undelivered, err)
	}
	source.threadRequestsWG.Wait()
	if stopped() {
		t.Fatal("the stop of the old pairing's command reached the computer that runs it")
	}
	if w := savedRemoteWatch(t, source, store.RemoteWatch{ComputerID: computer, RequestID: id}); w.Notification != "dismissed" || w.CancelOwedAt != 0 {
		t.Fatalf("the command's notification is %q with a cancel owed since %d, want it dismissed and nothing owed", w.Notification, w.CancelOwedAt)
	}
}

// An acknowledgement whose computer's profile cannot be read waits out the
// error delay like any other failure, instead of being due again at once.
func TestAnAcknowledgementWithAnUnreadableProfileIsPutOff(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptReplied, "the answer")
	pair.poll(t)
	if owed, err := pair.source.store.OweThreadRequestAck(token, 1); err != nil || !owed {
		t.Fatalf("owe the acknowledgement again: %v, %v", owed, err)
	}
	execOnStore(t, pair.dbPath, `UPDATE thread_requests SET next_check = 0, polling = 0 WHERE token = '`+token+`'`)
	if err := os.WriteFile(filepath.Join(pair.profile, deviceclient.SessionsDirName, pair.computer+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	pair.source.pollRemoteThreadRequests(now)
	pair.source.threadRequestsWG.Wait()
	due, err := pair.source.store.DueThreadRequestAcks(now.UnixMilli(), 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("the acknowledgement is due again at once: %+v", due)
	}
	if row := pair.request(t, token); row.AckOwed != 1 || row.NextCheck < now.Add(threadPollErrorDelay).UnixMilli()-1000 {
		t.Fatalf("the acknowledgement was put off to %d (owed %d), want the error delay", row.NextCheck-now.UnixMilli(), row.AckOwed)
	}
}

// Work a replaced pairing admitted names the computer as it was named when
// the work was admitted, while a new pairing with it is live under another
// name.
func TestEndedWorkKeepsTheNameItWasAdmittedUnder(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	if err := pair.source.RenameBackend(pair.computer, "GPU computer"); err != nil {
		t.Fatal(err)
	}
	pair.source.startSessionFn = func(string) error { return nil }
	token := pair.runningThere(t)
	pairing, err := pair.source.currentPairing(pair.computer)
	if err != nil {
		t.Fatal(err)
	}
	command := store.RemoteWatch{ComputerID: pair.computer, RequestID: uuid.NewString(), ThreadID: pair.caller.ID, Fingerprint: strings.Repeat("a", 64), Label: "nightly build",
		Pairing: pairing, ComputerName: "GPU computer"}
	if _, err := pair.source.store.RegisterRemoteWatch(command); err != nil {
		t.Fatal(err)
	}

	pair.rePair(t)
	if err := pair.source.RenameBackend(pair.computer, "Render box"); err != nil {
		t.Fatal(err)
	}
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestErrored {
		t.Fatalf("the old pairing's request is %q, want ended", row.State)
	}
	if name := agentRowOf(t, pair.source, pair.caller.ID, token).Computer; name != "GPU computer" {
		t.Fatalf("the ended request's Agents row names the computer %q, want the name it was admitted under", name)
	}
	pair.source.checkRemoteWatch(savedRemoteWatch(t, pair.source, command))
	records, err := pair.source.ListThreadRemoteCommands(pair.caller.ID)
	if err != nil || len(records) != 1 || records[0].ComputerName != "GPU computer" {
		t.Fatalf("the ended command names the computer %+v (%v), want the name it was admitted under", records, err)
	}
}
