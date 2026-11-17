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
		due, err := pair.source.store.DueThreadRequestPolls(time.Now().Add(24*time.Hour).UnixMilli(), 64)
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
