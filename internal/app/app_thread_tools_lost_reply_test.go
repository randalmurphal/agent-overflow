package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// What the poll makes of a request whose answer was lost: a receipt the
// destination holds is collected at the poll's normal pace, and a token it
// does not hold is never started only once that answer is conclusive.

// newFaultyReachPair is a pair whose destination is this build behind the
// fault wrapper, so it advertises thread-tools.v2.
func newFaultyReachPair(t *testing.T) (*reachPair, *peerFaults) {
	t.Helper()
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app)
		faults = injected
		return wire
	})
	return pair, faults
}

// A spawn the destination admitted whose every answer was lost is accepted
// on the caller by the first poll at the normal pace, for a destination on
// thread-tools.v2 and for one on v0.0.19.
func TestALostSpawnAnswerIsCollectedWithinOneNormalPoll(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for name, older := range map[string]bool{"thread-tools.v2": false, "v0.0.19": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			newPair := newFaultyReachPair
			if older {
				newPair = newReleasedReceiverPair
			}
			pair, faults := newPair(t)
			installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
			faults.dropReplies.Store(threadPeerAdmissionAttempts)

			ack := pair.spawnThere(t, "admitted, its answer lost", nil)
			returned := time.Now()
			if ack.Token == "" || ack.State != store.ThreadRequestUnconfirmed {
				t.Fatalf("ack = %+v, want the token unconfirmed", ack)
			}
			receipt := pair.receipt(t, ack.Token)
			pair.pollAt(t, pair.source, returned.Add(pair.source.threadPollNormal()))
			row := pair.request(t, ack.Token)
			if (row.State != store.ThreadRequestAccepted && row.State != store.ThreadRequestRunning) || row.TargetThreadID != receipt.TargetThreadID {
				t.Fatalf("the caller's row after one normal poll = %q naming %q, want it accepted naming %s",
					row.State, row.TargetThreadID, receipt.TargetThreadID)
			}
		})
	}
}

// A destination on thread-tools.v2 whose handler for a lost spawn is still
// before admission answers that the token is still being admitted, and the
// caller's row stays unconfirmed with nothing reported. When the handler
// admits, the next poll collects the thread; when it exits without
// admitting, the next status is conclusive and the row settles as never
// started.
func TestAV2DestinationSaysItIsStillAdmittingALostSpawn(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for name, admits := range map[string]bool{"the handler admits": true, "the handler exits without admitting": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pair, faults := newFaultyReachPair(t)
			installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
			// The first attempt runs on after its connection drops; the
			// retries never reach a handler.
			faults.detachFirst.Store(true)
			faults.creating = make(chan struct{})
			faults.unreadRetries.Store(true)
			release := make(chan struct{})
			var held sync.Once
			pair.dest.peerAdmissionForTest = func(string) {
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
			project := pair.project
			if !admits {
				// Refused before admission: no receipt is ever written.
				project = entityid.New()
			}

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			ack, err := pair.adapter().Spawn(ctx, pair.callerIdentity(), threadtools.SpawnCall{
				Prompt: "held before admission", ComputerID: pair.computer, ProjectID: project,
			})
			returned := time.Now()
			if err != nil || ack.State != store.ThreadRequestUnconfirmed {
				t.Fatalf("spawn = %+v, %v; want the token unconfirmed", ack, err)
			}
			status, err := pair.source.callThreadRequestStatus(pair.computer, ThreadPeerPoll{Tokens: []string{ack.Token}})
			if err != nil || len(status.Requests) != 1 || status.Requests[0].Known || !status.Requests[0].Admitting {
				t.Fatalf("status = %+v, %v; want the token unknown and still being admitted", status.Requests, err)
			}
			pair.pollAt(t, pair.source, returned.Add(pair.source.threadPollNormal()))
			if row := pair.request(t, ack.Token); row.State != store.ThreadRequestUnconfirmed || row.Issue != "" {
				t.Fatalf("a poll while the handler was before admission left %q %q issue %q, want it unconfirmed with nothing reported",
					row.State, row.Answer, row.Issue)
			}

			released()
			faults.detached.Wait()
			pair.pollAt(t, pair.source, time.Now().Add(pair.source.threadPollNormal()))
			row := pair.request(t, ack.Token)
			if admits {
				receipt := pair.receipt(t, ack.Token)
				if (row.State != store.ThreadRequestAccepted && row.State != store.ThreadRequestRunning) || row.TargetThreadID != receipt.TargetThreadID {
					t.Fatalf("after the handler admitted, the row = %q naming %q, want it accepted naming %s",
						row.State, row.TargetThreadID, receipt.TargetThreadID)
				}
				return
			}
			if row.State != store.ThreadRequestRefused || !strings.Contains(string(row.Answer), "never accepted") {
				t.Fatalf("after the handler exited, the row = %q %q, want it never started", row.State, row.Answer)
			}
		})
	}
}

// A lost spawn v0.0.19 never got to stays unconfirmed until the request's
// admission deadline, one call's wait after its one attempt was sent,
// which a restart of the calling computer keeps, and then settles as never
// started.
func TestALostSpawnAReleasedReceiverNeverGotSettlesAtItsDeadline(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := newReleasedReceiverPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	// The attempt is held at the destination's door while the source still
	// waits on it, then dropped there without reaching the handler.
	faults.held, faults.hold = make(chan struct{}), make(chan struct{})
	faults.detachFirst.Store(true)
	faults.creating = make(chan struct{})
	close(faults.creating)
	faults.admit = make(chan struct{})
	released := sync.OnceFunc(func() { close(faults.hold) })
	t.Cleanup(func() {
		released()
		close(faults.admit)
		faults.detached.Wait()
	})

	sent := time.Now()
	type result struct {
		ack threadtools.RequestAck
		err error
	}
	spawned := make(chan result, 1)
	go func() {
		ack, err := pair.adapter().Spawn(t.Context(), pair.callerIdentity(), threadtools.SpawnCall{
			Prompt: "never got there", ComputerID: pair.computer, ProjectID: pair.project,
		})
		spawned <- result{ack, err}
	}()
	select {
	case <-faults.held:
	case got := <-spawned:
		t.Fatalf("the spawn returned before its attempt reached the destination: %+v, %v", got.ack, got.err)
	}
	// The attempt is out; whatever the source does from here, the deadline
	// is its attempt's.
	out := time.Now()
	released()
	got := <-spawned
	returned := time.Now()
	ack := got.ack
	if got.err != nil || ack.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("spawn = %+v, %v; want the token unconfirmed", ack, got.err)
	}
	window := threadPeerCallWait("ThreadToolCall")
	pair.pollAt(t, pair.source, returned.Add(pair.source.threadPollNormal()))
	if row := pair.request(t, ack.Token); row.State != store.ThreadRequestUnconfirmed || row.Issue != "" {
		t.Fatalf("a poll before the deadline left %q %q issue %q, want it unconfirmed with nothing reported", row.State, row.Answer, row.Issue)
	}

	// The calling computer restarts: a second App over the same database,
	// with nothing of the first one's call.
	restarted, _ := setupE2EAppOn(t, pair.dbPath, pair.settingsDir)
	restarted.configDir = pair.source.configDir
	restarted.backends = pair.manager
	publishReachIdentity(t, restarted, pair.sourceID)
	request := func() store.ThreadRequest {
		t.Helper()
		row, found, err := restarted.store.GetThreadRequest(ack.Token)
		if err != nil || !found {
			t.Fatalf("the restarted caller lost the request: found=%v err=%v", found, err)
		}
		return row
	}
	pair.pollAt(t, restarted, sent.Add(window-time.Millisecond))
	if row := request(); row.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("a poll just before the deadline settled the request %q %q", row.State, row.Answer)
	}
	pair.pollAt(t, restarted, out.Add(window))
	if row := request(); row.State != store.ThreadRequestRefused || !strings.Contains(string(row.Answer), "never accepted") {
		t.Fatalf("a poll at the deadline left %q %q, want it never started", row.State, row.Answer)
	}
}

// A poll while a request's first attempt is still out leaves the request
// alone: the destination has not read its frame, so its answer that the
// token is unknown, with nothing admitting it, says nothing yet.
func TestAPollLeavesARequestAloneWhileItsCallIsOut(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := newFaultyReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
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
			Prompt: "still on its way", ComputerID: pair.computer, ProjectID: pair.project,
		})
		spawned <- result{ack, err}
	}()
	select {
	case <-faults.held:
	case got := <-spawned:
		t.Fatalf("the spawn returned before its attempt reached the destination: %+v, %v", got.ack, got.err)
	}
	rows, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 4, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the spawn's request: %d rows, %v", len(rows), err)
	}
	pair.pollAt(t, pair.source, time.Now())
	if row := pair.request(t, rows[0].Token); row.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("a poll while the call was out settled the request %q %q", row.State, row.Answer)
	}

	released()
	if got := <-spawned; got.err != nil || got.ack.ThreadID == "" {
		t.Fatalf("spawn = %+v, %v; want its acceptance with its thread", got.ack, got.err)
	}
}

// A status asked while a handler admits the token and exits is never read
// as "unknown, nothing admitting it": whether a handler holds the token is
// read before the receipt, so a handler gone by the second read wrote its
// receipt first.
func TestAStatusAskedWhileTheHandlerAdmitsFindsItsReceipt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := newFaultyReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	faults.detachFirst.Store(true)
	faults.creating = make(chan struct{})
	faults.unreadRetries.Store(true)
	release := make(chan struct{})
	var held sync.Once
	pair.dest.peerAdmissionForTest = func(string) {
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

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ack, err := pair.adapter().Spawn(ctx, pair.callerIdentity(), threadtools.SpawnCall{
		Prompt: "admitted during a status", ComputerID: pair.computer, ProjectID: pair.project,
	})
	if err != nil || ack.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("spawn = %+v, %v; want the token unconfirmed", ack, err)
	}
	// Between the status's two reads, the handler admits and exits.
	var during sync.Once
	pair.dest.peerStatusReadForTest = func(string) {
		during.Do(func() {
			released()
			faults.detached.Wait()
		})
	}
	pair.pollAt(t, pair.source, time.Now().Add(pair.source.threadPollNormal()))
	receipt := pair.receipt(t, ack.Token)
	if row := pair.request(t, ack.Token); row.State == store.ThreadRequestRefused || row.TargetThreadID != receipt.TargetThreadID {
		t.Fatalf("the poll whose status crossed the admission left %q %q naming %q, want it accepted naming %s",
			row.State, row.Answer, row.TargetThreadID, receipt.TargetThreadID)
	}
}
