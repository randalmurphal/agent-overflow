package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Dispatch of a request to another computer: the source refuses a request
// no attempt could have started, and gives the caller the token whenever
// one may have.

// A computer that cannot be reached is told apart from one that may have
// the request: nothing was sent, so the request is refused by its own call
// with that said, and nothing is left for the poller.
func TestThreadToolsAnUnreachableComputerIsRefusedAtOnce(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	seen := map[string]bool{}
	spawn := func(ctx context.Context) error {
		_, err := pair.adapter().Spawn(ctx, pair.callerIdentity(), threadtools.SpawnCall{
			Prompt: "work for a computer that is not there", ComputerID: pair.computer, ProjectID: pair.project,
		})
		return err
	}
	refused := func(t *testing.T, err error, said string) {
		t.Helper()
		_, message, ok := errorsx.PublicDetails(err)
		if !ok || !strings.Contains(message, said) {
			t.Fatalf("spawn = %v, want a refusal saying %q", err, said)
		}
		rows, err := pair.source.store.ListThreadRequestsByCaller(pair.caller.ID, 64, 0)
		if err != nil {
			t.Fatal(err)
		}
		var row store.ThreadRequest
		for _, listed := range rows {
			if !seen[listed.Token] {
				row = listed
			}
			seen[listed.Token] = true
		}
		if row.State != store.ThreadRequestRefused || row.DeliveredHow != store.ThreadWakeInline || string(row.Answer) != message {
			t.Fatalf("request = %q delivered %q answer %q, want refused with the call's text", row.State, row.DeliveredHow, row.Answer)
		}
		due, err := pair.source.store.DueThreadRequestPolls(time.Now().Add(24*time.Hour).UnixMilli(), 64, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, polled := range due {
			if polled.Token == row.Token {
				t.Fatalf("the refused request is still polled: %+v", polled)
			}
		}
	}

	// A call cancelled before it was sent: the caller's turn ended first.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	refused(t, spawn(cancelled), "before it was sent")

	shutdownReachDestination(t, pair)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	refused(t, spawn(ctx), "nothing was sent")
}

// A retry the destination refuses says nothing about the attempt before it,
// whose answer was lost and which started the work there. The caller is
// given the token to check, never an error it would answer by starting the
// same work again, and the poll collects what the first attempt started.
func TestThreadToolsARetryRefusedAfterALostAnswerGivesTheToken(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, injected := serveFaultyApp(t, app)
		faults = injected
		return wire
	})
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	faults.dropReplies.Store(1)
	faults.refuseRetries.Store(true)

	ack := pair.spawnThere(t, "accepted, then refused on the retry", nil)
	if ack.Token == "" || ack.State != store.ThreadRequestUnconfirmed || ack.Outcome != threadtools.OutcomeUnconfirmed {
		t.Fatalf("ack = %+v, want the token as unconfirmed", ack)
	}
	if row := pair.request(t, ack.Token); row.State != store.ThreadRequestUnconfirmed {
		t.Fatalf("the source row settled %q on a refused retry", row.State)
	}
	receipt := pair.receipt(t, ack.Token)
	pair.poll(t)
	row := pair.request(t, ack.Token)
	if row.TargetThreadID == "" || row.TargetThreadID != receipt.TargetThreadID || threadRequestSettled(row) {
		t.Fatalf("source row after the poll = %+v, want the thread the first attempt started", row)
	}
}

// The destination accepted the request and its answer arrived, but this
// computer could not record it. The caller still gets the token, the state
// the destination reported and what failed here, never only an error, and
// the poll records the answer once it can.
func TestThreadToolsAnAnswerThatCannotBeRecordedStillGivesTheToken(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	execOnStore(t, pair.dbPath, `CREATE TRIGGER refuse_target BEFORE UPDATE OF target_thread_id ON thread_requests
		BEGIN SELECT RAISE(ABORT, 'injected record failure'); END`)

	ack := pair.spawnThere(t, "accepted where this computer cannot record it", nil)
	if ack.Token == "" || ack.Outcome != threadtools.OutcomeUnconfirmed {
		t.Fatalf("ack = %+v, want the token as unconfirmed", ack)
	}
	receipt := pair.receipt(t, ack.Token)
	if ack.ThreadID != receipt.TargetThreadID || ack.State != receipt.State {
		t.Fatalf("ack names %q %q, want what the destination reported: %q %q", ack.ThreadID, ack.State, receipt.TargetThreadID, receipt.State)
	}
	if !strings.Contains(ack.Notice, "could not record") {
		t.Fatalf("ack notice = %q, want it to say what failed here", ack.Notice)
	}

	execOnStore(t, pair.dbPath, `DROP TRIGGER refuse_target`)
	pair.poll(t)
	if row := pair.request(t, ack.Token); row.TargetThreadID != receipt.TargetThreadID || threadRequestSettled(row) {
		t.Fatalf("source row after the poll = %+v, want it naming %s and open", row, receipt.TargetThreadID)
	}

	// The request cannot be read back to render its acknowledgement: the
	// caller is still given what the destination said under the token.
	execOnStore(t, pair.dbPath, `CREATE TRIGGER lose_request AFTER UPDATE OF next_check ON thread_requests
		WHEN NEW.token <> '`+ack.Token+`' BEGIN DELETE FROM thread_requests WHERE token = NEW.token; END`)
	lost := pair.spawnThere(t, "accepted where this computer loses the request", nil)
	if lost.Token == "" || lost.Outcome != threadtools.OutcomeUnconfirmed || !strings.Contains(lost.Notice, "could not record") {
		t.Fatalf("ack = %+v, want the token as unconfirmed with what failed", lost)
	}
	if there := pair.receipt(t, lost.Token); lost.ThreadID != there.TargetThreadID || lost.State != there.State {
		t.Fatalf("ack names %q %q, want what the destination reported: %q %q", lost.ThreadID, lost.State, there.TargetThreadID, there.State)
	}
}
