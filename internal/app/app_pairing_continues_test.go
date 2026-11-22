package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
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
	"agent-overflow/internal/testutil"
	"agent-overflow/internal/transport"
)

// Work belongs to the computer, not to one pairing with it: pairing that
// computer again continues the relationship, and what was started before
// is collected, acknowledged, cancelled and archived as if the pairing had
// never been replaced. Work ends only when the pairing ends and nothing
// replaces it.

// rePair is the person pairing this computer with the destination again
// from a fresh link, confirmed there, over the profile it already holds.
func (p *reachPair) rePair(t *testing.T) {
	t.Helper()
	if id := attachDestination(t, p, p.destWire, p.dest); id != p.computer {
		t.Fatalf("the new pairing names the computer %s, want %s", id, p.computer)
	}
}

// runCommandThere starts a command on the destination from the caller
// thread that runs until it is stopped, and returns its id and whether it
// has been stopped there.
func (p *reachPair) runCommandThere(t *testing.T) (id string, stopped func() bool) {
	t.Helper()
	running := make(chan context.Context, 1)
	jobs, err := remotejobs.New(context.Background(), p.dest.store, func(ctx context.Context, _ string, _ []string, _ io.Writer) (remotejobs.Outcome, error) {
		running <- ctx
		<-ctx.Done()
		return remotejobs.Outcome{ExitCode: -1}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	p.dest.remoteJobs = jobs
	t.Cleanup(jobs.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := p.source.SetAgentComputerEnabled(ctx, p.computer, true); err != nil {
		t.Fatal(err)
	}
	caller := transport.WithCallerScope(ctx, transport.CallerScope{Kind: transport.ScopeKindInteractive, ThreadID: p.caller.ID, ProjectID: p.caller.ProjectID})
	id = uuid.NewString()
	if _, err := p.source.AgentRemoteStart(caller, AgentRemoteRequest{ComputerID: p.computer, Workspace: gitapp.WorkspaceRef{ProjectID: p.project},
		Request: remotejobs.Request{ID: id, Argv: []string{"hold", id}, TimeoutSeconds: 60}}); err != nil {
		t.Fatal(err)
	}
	var run context.Context
	select {
	case run = <-running:
	case <-ctx.Done():
		t.Fatal("the command never started there")
	}
	return id, func() bool { return run.Err() != nil }
}

// A request, a running command and an owned worker on another computer,
// then a new explicit pairing confirmed over the saved profile: the request
// stays open and its answer is collected and acknowledged under the new
// pairing, the command's Stop reaches that computer, and archiving the
// spawner archives its worker there. Until the pairing ends, the work names
// the computer as the person calls it now.
func TestANewPairingContinuesTheWorkOfTheOneItReplaced(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	p := newOwnerOpPair(t, false)
	installMockClaudeReplies(t, p.source, "noted")
	p.source.startSessionFn = func(string) error { return nil }
	command, stopped := p.runCommandThere(t)

	p.rePair(t)
	if err := p.source.RenameBackend(p.computer, "Render box"); err != nil {
		t.Fatal(err)
	}
	p.poll(t)
	if row := p.request(t, p.token); threadRequestSettled(row) || !row.Polling {
		t.Fatalf("after the new pairing the spawn is %q polling=%v, want still open", row.State, row.Polling)
	}
	if name := agentRowOf(t, p.source, p.caller.ID, p.token).Computer; name != "Render box" {
		t.Fatalf("the open spawn names the computer %q, want its name now", name)
	}
	if w := savedRemoteWatch(t, p.source, store.RemoteWatch{ComputerID: p.computer, RequestID: command}); w.Receipt.State != "running" {
		t.Fatalf("after the new pairing the command reads %q, want still running", w.Receipt.State)
	}

	if result, err := p.dest.store.SettleLocalThreadRequest(p.token, store.ThreadReceiptOpenStates(),
		store.ThreadRequestSettlement{State: store.ThreadReceiptFinished, Answer: []byte("done there"), AnswerKind: store.ThreadAnswerFinal},
		store.ThreadRequestStateForReceipt(store.ThreadReceiptFinished)); err != nil || !result.Settled {
		t.Fatalf("settle the spawn there: %+v, %v", result, err)
	}
	p.poll(t)
	if row := p.request(t, p.token); row.State != store.ThreadRequestFinished || string(row.Answer) != "done there" || row.AckOwed != 0 {
		t.Fatalf("the spawn's answer under the new pairing: %q %q ack owed %d, want it collected and acknowledged", row.State, row.Answer, row.AckOwed)
	}
	if receipt := p.receipt(t, p.token); receipt.CollectedRevision != 1 {
		t.Fatalf("the destination holds acknowledgement %d, want 1", receipt.CollectedRevision)
	}

	if _, err := p.source.CancelThreadRemoteCommand(t.Context(), p.caller.ID, p.computer, command); err != nil {
		t.Fatalf("stop the command: %v", err)
	}
	if !stopped() {
		t.Fatal("the Stop under the new pairing did not reach the computer that runs the command")
	}

	result, err := p.source.ArchiveThread(p.caller.ID)
	if err != nil || len(result.WorkersNotArchived) != 0 {
		t.Fatalf("ArchiveThread = %+v, %v; want a complete archive", result, err)
	}
	if !archivedThere(t, p, p.worker) {
		t.Fatal("archiving the spawner under the new pairing left its worker there open")
	}
}

// revocationOf revokes this computer on the destination and returns the
// verdict the next call there brings back, without acting on it: the
// change the attached set announces when the destination ends the pairing.
func (p *reachPair) revocationOf(t *testing.T) attachedbackends.SetChange {
	t.Helper()
	verdicts := make(chan attachedbackends.SetChange, 1)
	p.manager.SetChanged(func(change attachedbackends.SetChange) {
		if change.Action == attachedbackends.SetRemoved {
			verdicts <- change
			return
		}
		p.source.backendSetChanged(change)
	})
	t.Cleanup(func() { p.manager.SetChanged(p.source.backendSetChanged) })
	revokeReachDevice(t, p.dest, "source")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := p.manager.CallThreadPeer(ctx, p.computer, "ThreadToolResolve", nil); err == nil {
		t.Fatal("a call on a pairing the other computer ended succeeded")
	}
	select {
	case change := <-verdicts:
		p.manager.SetChanged(p.source.backendSetChanged)
		return change
	case <-ctx.Done():
		t.Fatal("the destination's revocation was never announced")
	}
	return attachedbackends.SetChange{}
}

// A revocation the other computer answered ends that computer's work only
// while the pairing that received it is the one held when it is acted on:
// one that arrives after the person paired again ends nothing, and the
// same answer with the pairing unchanged ends that computer's work once.
func TestARevocationEndsWorkOnlyWhileItsPairingIsHeld(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	t.Run("after a new pairing", func(t *testing.T) {
		t.Parallel()
		pair := newReachPair(t)
		token := pair.runningThere(t)
		verdict := pair.revocationOf(t)
		restoreReachDevice(t, pair.dest, "source")
		pair.rePair(t)

		pair.source.backendSetChanged(verdict)
		pair.source.threadRequestsWG.Wait()
		if row := pair.request(t, token); threadRequestSettled(row) || !row.Polling {
			t.Fatalf("a revocation from before the new pairing ended %q polling=%v, want it still open", row.State, row.Polling)
		}
		if _, ended := pair.manager.TakeVerdict(pair.computer); ended {
			t.Fatal("the new pairing left the old one's verdict standing")
		}
	})
	t.Run("with a new pairing saved but not yet in use", func(t *testing.T) {
		t.Parallel()
		pair := newReachPair(t)
		token := pair.runningThere(t)
		session, err := deviceclient.LoadSession(pair.profile, pair.computer)
		if err != nil {
			t.Fatal(err)
		}
		verdict := pair.revocationOf(t)
		// A pairing saved as one being made is saved, before this process
		// installs it; admission reads the saved profile.
		if err := deviceclient.SaveSession(pair.profile, session); err != nil {
			t.Fatal(err)
		}

		pair.source.backendSetChanged(verdict)
		pair.source.threadRequestsWG.Wait()
		if row := pair.request(t, token); threadRequestSettled(row) || !row.Polling {
			t.Fatalf("a revocation acted on with a new pairing saved ended %q polling=%v, want it still open", row.State, row.Polling)
		}
	})
	t.Run("with the pairing unchanged", func(t *testing.T) {
		t.Parallel()
		pair := newReachPair(t)
		token := pair.runningThere(t)
		verdict := pair.revocationOf(t)

		pair.source.backendSetChanged(verdict)
		pair.source.threadRequestsWG.Wait()
		ended := pair.request(t, token)
		if ended.State != store.ThreadRequestErrored || string(ended.Answer) != pairingEndedByComputer {
			t.Fatalf("after the revocation the request is %q %q, want ended by it", ended.State, ended.Answer)
		}
		pair.source.backendSetChanged(verdict)
		pair.source.threadRequestsWG.Wait()
		if again := pair.request(t, token); again.UpdatedAt != ended.UpdatedAt || again.DeliveredAt != ended.DeliveredAt {
			t.Fatalf("the same revocation acted on twice changed the request again: %+v", again)
		}
	})
}

// A Forget whose removal of the pairing fails returns the error and leaves
// the pairing and every record for it as they were: the open request and
// its wake, a finished one's late-reply poll and its owed acknowledgement,
// all still collectable once the pairing can be read again.
func TestAForgetThatFailsLeavesEveryRecordAsItWas(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	finished := pair.settledThere(t, store.ThreadReceiptFinished, "the first answer")
	pair.poll(t)
	if owed, err := pair.source.store.OweThreadRequestAck(finished, 1); err != nil || !owed {
		t.Fatalf("owe the acknowledgement again: %v, %v", owed, err)
	}
	open := pair.runningThere(t)
	before := map[string]store.ThreadRequest{open: pair.request(t, open), finished: pair.request(t, finished)}

	access := filepath.Join(pair.profile, "agent-access.json")
	if err := os.WriteFile(access, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pair.source.RemoveBackend(pair.computer, true); err == nil {
		t.Fatal("a forget whose removal failed reported success")
	}
	if _, paired, err := pair.manager.Lookup(pair.computer); err != nil || !paired {
		t.Fatalf("after the failed forget the pairing is paired=%v (%v), want it kept", paired, err)
	}
	for token, was := range before {
		row := pair.request(t, token)
		if row.State != was.State || row.Notify != was.Notify || row.Polling != was.Polling || row.AckOwed != was.AckOwed || string(row.Answer) != string(was.Answer) {
			t.Fatalf("after the failed forget %s is %q notify=%v polling=%v ack owed %d, want %q notify=%v polling=%v ack owed %d",
				token, row.State, row.Notify, row.Polling, row.AckOwed, was.State, was.Notify, was.Polling, was.AckOwed)
		}
	}

	if err := os.Remove(access); err != nil {
		t.Fatal(err)
	}
	if result, err := pair.dest.store.SettleLocalThreadRequest(open, store.ThreadReceiptOpenStates(),
		store.ThreadRequestSettlement{State: store.ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal},
		store.ThreadRequestStateForReceipt(store.ThreadReceiptFinished)); err != nil || !result.Settled {
		t.Fatalf("settle the open request there: %+v, %v", result, err)
	}
	pair.poll(t)
	if row := pair.request(t, open); row.State != store.ThreadRequestFinished || string(row.Answer) != "done" {
		t.Fatalf("after the failed forget the open request collected %q %q", row.State, row.Answer)
	}
	if row := pair.request(t, finished); row.AckOwed != 0 {
		t.Fatalf("after the failed forget the owed acknowledgement is still owed: %d", row.AckOwed)
	}
}

// A request from v0.0.19 kept no name for its computer. Forgetting the
// computer writes the name it has then onto it, which names it afterwards.
func TestAForgottenComputerNamesWorkFromBeforeNamesWereKept(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	if err := pair.source.RenameBackend(pair.computer, "GPU computer"); err != nil {
		t.Fatal(err)
	}
	token := pair.runningThere(t)
	execOnStore(t, pair.dbPath, `UPDATE thread_requests SET target_computer_name = '' WHERE token = '`+token+`'`)

	if err := pair.source.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if name := agentRowOf(t, pair.source, pair.caller.ID, token).Computer; name != "GPU computer" {
		t.Fatalf("the forgotten computer's request names it %q, want the name it had when it was forgotten", name)
	}
}

// A saved pairing that can no longer be read is still forgotten, and its
// work ends: forgetting is the way out of a damaged profile. The profile is
// found damaged with no live connection built from it, as at a start.
func TestAPairingThatCannotBeReadIsStillForgotten(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.runningThere(t)
	pair.manager.Close()
	session := filepath.Join(pair.profile, deviceclient.SessionsDirName, pair.computer+".json")
	if err := os.WriteFile(session, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := pair.source.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget a pairing that cannot be read: %v", err)
	}
	if _, paired, err := pair.manager.Lookup(pair.computer); err != nil || paired {
		t.Fatalf("after the forget the pairing is paired=%v (%v), want it gone", paired, err)
	}
	if row := pair.request(t, token); row.State != store.ThreadRequestErrored {
		t.Fatalf("after the forget the open request is %q, want it ended", row.State)
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
	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitBudget(t))
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
	if err := a.app.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: thread.ID, Kind: store.ThreadRequestSpawn,
		State: store.ThreadRequestRunning, TargetComputerID: c.id}); err != nil {
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
