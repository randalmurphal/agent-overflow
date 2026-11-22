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
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/gitapp"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// holdPairing gives an app that holds no pairings a pairing with computerID,
// for a test that applies that computer's answers directly, as a poll does
// after its call: an answer applies only while a pairing is saved.
func holdPairing(t *testing.T, a *App, computerID string) {
	t.Helper()
	if a.backends != nil {
		t.Fatal("holdPairing: the app already holds pairings")
	}
	dir := t.TempDir()
	manager, err := newTestBackends(t, dir, "source", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := deviceclient.SaveSession(dir, deviceclient.Session{BackendID: computerID, SessionID: "held", Credential: "test", Endpoint: "https://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	a.backends = manager
}

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

// restoreReachDevice re-admits the revoked device labelled label to pairing,
// as the destination's owner does from its device list.
func restoreReachDevice(t *testing.T, app *App, label string) {
	t.Helper()
	overview, err := app.GetAccessOverview()
	if err != nil {
		t.Fatalf("read the access overview: %v", err)
	}
	for _, device := range overview.Devices {
		if device.Label == label && device.RevokedAtMs != 0 {
			if err := app.RestoreAccessDevice(device.ID); err != nil {
				t.Fatalf("restore %s: %v", label, err)
			}
			return
		}
	}
	t.Fatalf("no revoked device labelled %q", label)
}

// learnPairingEnded makes one ordinary call to the destination, outside the
// request and command sweeps, which is how this computer finds out the
// other one ended the pairing; and joins the work that end starts here.
func (p *reachPair) learnPairingEnded(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := p.manager.CallThreadPeer(ctx, p.computer, "ThreadToolResolve", nil); err == nil {
		t.Fatal("a call on a pairing the other computer ended succeeded")
	}
	if _, paired, err := p.manager.Lookup(p.computer); err != nil || paired {
		t.Fatalf("the pairing the other computer ended is still held here: paired=%v err=%v", paired, err)
	}
	p.source.threadRequestsWG.Wait()
}

// A pairing the other computer ended, learned by an ordinary call rather
// than by the ledger's own sweeps, ends what this computer holds for that
// computer at once, and what it ended stays ended: pairing the computer
// again takes none of it up.
func TestAPairingEndedOutsideTheLedgerStaysEndedWhenPairedAgain(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptFinished, "the first answer")
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestFinished || !row.Polling {
		t.Fatalf("collected %q polling=%v, want finished and listening for a late reply", row.State, row.Polling)
	}

	revokeReachDevice(t, pair.dest, "source")
	pair.learnPairingEnded(t)
	row := pair.request(t, token)
	if row.Polling || row.AckOwed != 0 || row.State != store.ThreadRequestFinished || string(row.Answer) != "the first answer" {
		t.Fatalf("after the pairing ended: %q %q polling=%v ack owed %d, want the answer kept and nothing more owed or asked",
			row.State, row.Answer, row.Polling, row.AckOwed)
	}

	// The other computer's owner lets this one pair again, and it does; a
	// late reply then lands there for the request the old pairing made.
	restoreReachDevice(t, pair.dest, "source")
	if id := attachDestination(t, pair, pair.destWire, pair.dest); id != pair.computer {
		t.Fatalf("the replacement pairing names the computer %s, want %s", id, pair.computer)
	}
	if stored, err := pair.dest.store.StoreThreadReceiptLateReply(token, []byte("a correction"), 0, nil); err != nil || !stored {
		t.Fatalf("store the late reply there: stored=%v err=%v", stored, err)
	}
	pair.poll(t)
	pair.call(t, "thread_status", `{"tokens":["`+token+`"]}`)
	if row := pair.request(t, token); len(row.LateReply) != 0 || row.Revision != 1 || row.Polling || row.AckOwed != 0 {
		t.Fatalf("the replacement pairing took up the request the old one ended: %+v", row)
	}
	if receipt := pair.receipt(t, token); receipt.CollectedRevision != 1 {
		t.Fatalf("the replacement pairing acknowledged revision %d of the old pairing's request", receipt.CollectedRevision)
	}
}

// agentRowOf finds a request's row in its caller's Agents segment, open or
// past.
func agentRowOf(t *testing.T, a *App, callerThreadID, token string) ThreadAgentRow {
	t.Helper()
	open, err := a.ListThreadAgentRows(t.Context(), callerThreadID)
	if err != nil {
		t.Fatal(err)
	}
	past, err := a.ListThreadPastAgentRows(t.Context(), callerThreadID, store.PastAgentRowsCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range append(open.Rows, past.Rows...) {
		if row.Token == token {
			return row
		}
	}
	t.Fatalf("no Agents row for %s", token)
	return ThreadAgentRow{}
}

// An answer from another computer applies only while this one holds a
// pairing with it: one a call brings back after the pairing went neither
// settles the request nor leaves it owing that computer anything, since the
// end of the pairing settles it instead.
func TestAnAnswerAppliesOnlyWhileAPairingIsSaved(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	computer := uuid.NewString()
	answer := func(token string) ThreadPeerRequest {
		return ThreadPeerRequest{Token: token, Known: true, State: store.ThreadReceiptFinished, Answer: "done there",
			AnswerKind: store.ThreadAnswerFinal, Revision: 1, SettledAt: time.Now().UnixMilli()}
	}
	seed := func() string {
		t.Helper()
		token := uuid.NewString()
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
			State: store.ThreadRequestRunning, TargetComputerID: computer, TargetThreadID: uuid.NewString()}); err != nil {
			t.Fatal(err)
		}
		return token
	}
	unchanged := func(token, why string) {
		t.Helper()
		if wakes, err := f.app.applyThreadPeerRequest(token, computer, answer(token), threadPeerHearing{at: time.Now()}); err != nil || len(wakes) != 0 {
			t.Fatalf("%s: wakes %+v, %v", why, wakes, err)
		}
		if row := f.request(t, token); row.State != store.ThreadRequestRunning || row.Revision != 0 || row.AckOwed != 0 {
			t.Fatalf("%s changed the request: %+v", why, row)
		}
	}
	unchanged(seed(), "an answer with no pairing held")
	holdPairing(t, f.app, computer)
	token := seed()
	if _, err := f.app.applyThreadPeerRequest(token, computer, answer(token), threadPeerHearing{at: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if row := f.request(t, token); row.State != store.ThreadRequestFinished || row.AckOwed != 1 {
		t.Fatalf("an answer while the pairing is held left the request %+v", row)
	}
}

// A request the end of a pairing settled names the computer as the person
// knew it, on its wake and on its Agents row, though this computer no
// longer holds a pairing that names it.
func TestARequestThePairingsEndSettledNamesItsComputer(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	if err := pair.source.RenameBackend(pair.computer, "GPU computer"); err != nil {
		t.Fatal(err)
	}
	token := pair.runningThere(t)
	// The wake stays on the queue, where its words can be read.
	pair.source.startSessionFn = func(string) error { return nil }

	revokeReachDevice(t, pair.dest, "source")
	pair.learnPairingEnded(t)
	pair.source.waitAgentNoticeWork()
	if row := pair.request(t, token); row.State != store.ThreadRequestErrored {
		t.Fatalf("the request to a computer that ended the pairing is %q", row.State)
	}
	if computer := agentRowOf(t, pair.source, pair.caller.ID, token).Computer; computer != "GPU computer" {
		t.Fatalf("the Agents row names the computer %q, want the name the person gave it", computer)
	}
	var wake string
	for _, row := range durableQueueRows(t, pair.source, pair.caller.ID) {
		if row.SendID == threadWakeSendID(token) {
			wake = row.Message
		}
	}
	if !strings.Contains(wake, "GPU computer") {
		t.Fatalf("the wake does not name the computer as the person knew it: %q", wake)
	}
}

// Forgetting a computer counts every obligation it would drop: a cancel a
// stopped command is still owed there, whose notification the stop
// dismissed, is named, and holds the forget as an unconfirmed cancellation
// does, so the command is not left running there with nobody to stop it.
func TestForgettingAComputerWaitsForACancelItStillOwes(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	command := runningRemoteWatch(t, pair.source, pair.computer, pair.caller)
	if err := pair.source.store.StopRemoteWatch(command.ComputerID, command.RequestID, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := pair.source.store.OweRemoteCancel(command.ComputerID, command.RequestID, now, now+30_000); err != nil {
		t.Fatal(err)
	}
	for _, abandon := range []bool{false, true} {
		err := pair.source.RemoveBackend(pair.computer, abandon)
		if err == nil || !strings.Contains(err.Error(), "1 stopped command whose cancellation") {
			t.Fatalf("forget (abandon=%v) with a cancel still owed there answered %v", abandon, err)
		}
	}
	if saved := savedRemoteWatch(t, pair.source, command); saved.CancelOwedAt == 0 {
		t.Fatalf("the forget dropped the cancel the command is owed: %+v", saved)
	}
	if _, paired, err := pair.manager.Lookup(pair.computer); err != nil || !paired {
		t.Fatalf("the computer was forgotten with a cancel still owed there: paired=%v err=%v", paired, err)
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
	if err := source.RenameBackend(computer, "GPU computer"); err != nil {
		t.Fatal(err)
	}
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
	// Each names the computer as the person knew it, though no pairing here
	// names it any more: the completion, the call row's listing and the
	// Background row.
	for _, queued := range durableQueueRows(t, source, thread.ID) {
		if queued.SendID == remoteCompletionSendID(w) && !strings.Contains(queued.Message, "Remote job finished on GPU computer") {
			t.Fatalf("the completion does not name the computer as the person knew it: %q", queued.Message)
		}
	}
	records, err := source.ListThreadRemoteCommands(thread.ID)
	if err != nil || len(records) != 1 || records[0].ComputerName != "GPU computer" {
		t.Fatalf("the command's listing names the computer %+v, %v", records, err)
	}
	if !strings.Contains(items[1].Meta, `"computer_name":"GPU computer"`) {
		t.Fatalf("the Background row does not name the computer: %s", items[1].Meta)
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
