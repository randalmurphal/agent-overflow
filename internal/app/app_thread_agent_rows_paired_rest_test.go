package app

// A request a paired computer holds for its thread's rest reads as that
// thread waits, here as there; a paired spawn under Past keeps its counts;
// and a computer whose version cannot report rest says so.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A send to the caller's own worker on a paired computer, whose turn there
// ended with a loop armed, is held there: the spawner's row reads it
// waiting on the loop, not running, and thread_status says what it waits
// on. A send's row has no listing of its thread there to say so; the poll
// does.
func TestAPairedSendHeldForItsWorkersRestReadsWaiting(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	gate := filepath.Join(t.TempDir(), "turn")
	installMockClaudeTurns(t, pair.dest, [][]string{
		{mockClaudeInitLine, `{"type":"assistant","message":{"id":"msg-ready","role":"assistant","content":[{"type":"text","text":"Ready."}]}}`, mockClaudeResultLine},
		gatedTurn(gate, "Watching it there."),
	})
	spawned := pair.spawnThere(t, "get ready", nil)
	child := ""
	waitUntil(t, 20*time.Second, func() bool {
		receipt := pair.receipt(t, spawned.Token)
		child = receipt.TargetThreadID
		return receipt.State == store.ThreadReceiptFinished
	})
	pair.poll(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	sent, err := pair.adapter().Send(ctx, pair.callerIdentity(), threadtools.SendCall{ThreadID: child, ComputerID: pair.computer, Message: "watch the board", Notify: true})
	if err != nil {
		t.Fatalf("Send there: %v", err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		receipt := pair.receipt(t, sent.Token)
		return receipt.State == store.ThreadReceiptRunning && receipt.TurnID != ""
	})
	destID, _ := pair.dest.backendIdentity()
	if _, err := (threadToolsApp{app: pair.dest}).Loop(t.Context(), threadtools.Caller{ThreadID: child, ComputerID: destID, ComputerName: "destination"},
		threadtools.LoopCall{Label: "board", Note: "check", EverySeconds: 3600}); err != nil {
		t.Fatalf("Loop there: %v", err)
	}
	release(t, gate)
	waitUntil(t, 20*time.Second, func() bool { return pair.dest.restHeld(child, sent.Token) })
	pair.poll(t)

	rows, err := pair.source.ListThreadAgentRows(t.Context(), pair.caller.ID)
	if err != nil {
		t.Fatal(err)
	}
	row, listed := agentRowByToken(rows.Rows, sent.Token)
	if !listed || row.State != threadtools.RequestRunning || row.Worker != workerWaiting || row.Activity != "Loop · board" || !row.open() {
		t.Fatalf("row %+v; want it open, waiting on the loop there", row)
	}
	report, err := pair.adapter().RequestStates(t.Context(), pair.callerIdentity(), threadtools.StatusCall{Tokens: []string{sent.Token}})
	if err != nil || len(report.Requests) != 1 || report.Requests[0].WaitingOn != "Loop · board" || report.Requests[0].RestUnreported {
		t.Fatalf("status %+v, %v; want it waiting on the loop there", report, err)
	}
}

// A paired spawn under Past keeps what its thread did, read from its
// computer's answer, asked for on the page's read; a computer no longer
// paired says so instead.
func TestAPairedSpawnUnderPastKeepsItsCounts(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-call","role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":"make check"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"call-1","type":"tool_result","content":"ok","is_error":false}]}}`,
		`{"type":"assistant","message":{"id":"msg-done","role":"assistant","content":[{"type":"text","text":"Checked."}]}}`,
		mockClaudeResultLine}})
	// The caller here takes the answer as a wake.
	installMockClaudeTurns(t, pair.source, [][]string{echoedTurn("noted", "Noted.")})
	ack := pair.spawnThere(t, "run the check", map[string]any{"notify": true})
	waitUntil(t, 20*time.Second, func() bool { return pair.receipt(t, ack.Token).State == store.ThreadReceiptFinished })
	pair.poll(t)
	past := func() ThreadAgentRow {
		t.Helper()
		page, err := pair.source.ListThreadPastAgentRows(pair.caller.ID, store.PastAgentRowsCursor{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		row, _ := agentRowByToken(page.Rows, ack.Token)
		return row
	}
	var row ThreadAgentRow
	waitUntil(t, 20*time.Second, func() bool {
		row = past()
		return row.ToolCalls == 1
	})
	there, err := pair.dest.store.ThreadTallies([]string{pair.receipt(t, ack.Token).TargetThreadID}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tally := there[pair.receipt(t, ack.Token).TargetThreadID]; row.RuntimeMs != tally.RuntimeMs || row.TurnOpen || row.NestedUnavailable != "" || row.RestUnreported {
		t.Fatalf("past row %+v; want its thread's counts and runtime there, %+v", row, tally)
	}
	if err := pair.source.RemoveBackend(pair.computer, true); err != nil {
		t.Fatalf("forget the computer: %v", err)
	}
	pair.source.threadRequestsWG.Wait()
	if row := past(); row.NestedUnavailable == "" || row.ToolCalls != 0 {
		t.Fatalf("past row of a computer no longer paired %+v; want it saying so, with no counts", row)
	}
}

// A computer whose version settles a spawn when its turn ends, and does not
// say it settles at rest, has its `finished` answer collected marked: the
// row and thread_status say its rest is unreported. One that reports rest,
// a reply, and a send to a thread the caller did not spawn there are not
// marked.
func TestAnOlderPairedComputersFinishedSpawnSaysItsRestIsUnreported(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	computer := entityid.New()
	collect := func(kind string, answer ThreadPeerRequest) store.ThreadRequest {
		t.Helper()
		token := uuid.NewString()
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: f.caller.ID, Kind: kind,
			State: store.ThreadRequestRunning, TargetComputerID: computer, TargetThreadID: uuid.NewString()}); err != nil {
			t.Fatal(err)
		}
		answer.Token, answer.Known, answer.Revision = token, true, 1
		answer.State, answer.Answer, answer.SettledAt = store.ThreadReceiptFinished, "Done.", time.Now().UnixMilli()
		if answer.AnswerKind == "" {
			answer.AnswerKind = store.ThreadAnswerFinal
		}
		if _, _, err := f.app.collectRemoteThreadRequest(f.request(t, token), answer); err != nil {
			t.Fatalf("collectRemoteThreadRequest: %v", err)
		}
		return f.request(t, token)
	}
	older := collect(store.ThreadRequestSpawn, ThreadPeerRequest{})
	if older.State != store.ThreadRequestFinished || !older.RestUnreported {
		t.Fatalf("an older computer's spawn %+v; want it finished, its rest unreported", older)
	}
	report := f.statusOf(t, older.Token, 0)
	if len(report.Requests) != 1 || !report.Requests[0].RestUnreported {
		t.Fatalf("status %+v; want its rest unreported", report)
	}
	if row, _ := agentRowByToken(f.agentRows(t, f.caller.ID).Rows, older.Token); !row.RestUnreported {
		t.Fatalf("row %+v; want its rest unreported", row)
	}
	for what, row := range map[string]store.ThreadRequest{
		"a computer that reports rest":       collect(store.ThreadRequestSpawn, ThreadPeerRequest{RestRule: true}),
		"a reply":                            collect(store.ThreadRequestSpawn, ThreadPeerRequest{AnswerKind: store.ThreadAnswerReply}),
		"a send to a thread it did not make": collect(store.ThreadRequestSend, ThreadPeerRequest{}),
	} {
		if row.RestUnreported {
			t.Fatalf("%s: %+v; want its rest not marked unreported", what, row)
		}
	}
}
