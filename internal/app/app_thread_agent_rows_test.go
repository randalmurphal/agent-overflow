package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

func agentRowTokens(rows []ThreadAgentRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Token)
	}
	return out
}

// watchAgentRows routes the fixture's emissions through the Agents rows'
// tap and records the app's own, as the production emit funnel does.
func (f *requestFixture) watchAgentRows() {
	f.bus.mu.Lock()
	f.bus.tap = f.app.tapThreadAgentRows
	f.bus.mu.Unlock()
	f.app.testEmitHook = f.bus.emit
}

func (f *requestFixture) agentRows(t *testing.T, threadID string) ThreadAgentRows {
	t.Helper()
	rows, err := f.app.ListThreadAgentRows(t.Context(), threadID)
	if err != nil {
		t.Fatalf("ListThreadAgentRows(%s): %v", threadID, err)
	}
	return rows
}

func (f *requestFixture) plainThread(t *testing.T) store.Thread {
	t.Helper()
	thread, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	return thread
}

// seedAgentRow writes a request row by hand, for a shape the tools would
// take several live threads to reach.
func (f *requestFixture) seedAgentRow(t *testing.T, token, caller, kind, target string, notify bool) {
	t.Helper()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: kind, TargetThreadID: target, Notify: notify,
		State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("insert %s: %v", token, err)
	}
}

// A spawn lists with its thread's title, provider and running state, and the
// spawned thread's own spawn nests under it. A send the thread is owed no
// answer for is not an agent; one it is owed an answer for is.
func TestThreadAgentRowsListAndNestBySpawn(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "reading the logs")
	child := f.runningRequest(t, "triage the crash")
	grand, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, child.ThreadID), threadtools.SpawnCall{
		Prompt: "bisect it", Title: "Bisect", Notify: true,
	})
	if err != nil {
		t.Fatalf("nested Spawn: %v", err)
	}
	peer := f.busyThread(t, "peer-busy")
	quiet, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: peer.ID, Message: "fyi"})
	if err != nil {
		t.Fatalf("quiet Send: %v", err)
	}
	owed, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: peer.ID, Message: "and answer this", Notify: true})
	if err != nil {
		t.Fatalf("owed Send: %v", err)
	}

	listing := f.agentRows(t, f.caller.ID)
	want := []string{child.Token, grand.Token, owed.Token}
	if got := agentRowTokens(listing.Rows); !slices.Equal(got, want) {
		t.Fatalf("rows %v; want %v (a send owed nothing, %s, is not an agent)", got, want, quiet.Token)
	}
	top, nested := listing.Rows[0], listing.Rows[1]
	if top.Depth != 0 || top.Kind != store.ThreadRequestSpawn || top.Provider != string(provider.Claude) ||
		top.State != threadtools.RequestRunning || top.ThreadID != child.ThreadID || top.OpenThreadID != child.ThreadID {
		t.Fatalf("top row = %+v", top)
	}
	if nested.Depth != 1 || nested.CallerThreadID != child.ThreadID || nested.Title != "Bisect" {
		t.Fatalf("nested row = %+v", nested)
	}
	if listing.Rows[2].Depth != 0 || listing.Rows[2].Kind != store.ThreadRequestSend {
		t.Fatalf("owed send row = %+v", listing.Rows[2])
	}
	// The child lists its own spawn on its own, unnested.
	if got := agentRowTokens(f.agentRows(t, child.ThreadID).Rows); !slices.Equal(got, []string{grand.Token}) {
		t.Fatalf("the child's own rows = %v", got)
	}
}

// A target whose last message is the request it was sent shows that
// request's text, not the footer the app appended to it, however long the
// footer is.
func TestThreadAgentRowsShowTheLastAskWithoutItsFooter(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	target := f.plainThread(t)
	if err := f.app.store.InsertTurn(store.Turn{TurnID: "ask", ThreadID: target.ID, TurnIndex: 0, StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	footer := threadtools.Footer{Title: "Lead", ThreadID: f.caller.ID, Token: "row", AnswerRequested: true,
		SenderReachable: true, UserMessage: strings.Repeat("why ", 100)}
	if err := f.app.store.InsertItem(store.Item{
		ID: "ask-row", ThreadID: target.ID, Kind: "user_text", Role: "user", Status: "completed",
		Summary: "Rebuild the index.\n\n" + footer.String(), CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	f.seedAgentRow(t, "row", f.caller.ID, store.ThreadRequestSpawn, target.ID, true)

	rows := f.agentRows(t, f.caller.ID).Rows
	if len(rows) != 1 || rows[0].LastMessage != "Rebuild the index." || rows[0].LastMessageRole != "user" {
		t.Fatalf("rows = %+v; want the ask without its footer", rows)
	}
}

// The listing reads a settled answer without delivering it: the caller's
// agent is still owed its wake.
func TestThreadAgentRowsReadNoDelivery(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "the rebuild took nine minutes")
	ctx, pending := withThreadToolsPending(t.Context())
	ack, err := f.adapter().Spawn(ctx, f.callerIdentity(), threadtools.SpawnCall{Prompt: "how long?"})
	pending.run(false)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)

	rows := f.agentRows(t, f.caller.ID).Rows
	if len(rows) != 1 || rows[0].State != threadtools.RequestFinished || !strings.Contains(rows[0].Answer, "nine minutes") ||
		rows[0].Provider != string(provider.Claude) {
		t.Fatalf("rows = %+v; want the settled spawn with its answer and provider", rows)
	}
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, ack.Token); row.DeliveredAt != 0 || row.DeliveredHow != "" {
		t.Fatalf("listing the row delivered its answer: %q at %d", row.DeliveredHow, row.DeliveredAt)
	}
}

// Nesting stops at the depth bound, which the last row says it hides, and a
// spawn cycle lists each thread once.
func TestThreadAgentRowsBoundTheirDepthAndCycles(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	chain := []string{f.caller.ID}
	for range threadAgentRowDepth + 1 {
		chain = append(chain, f.plainThread(t).ID)
	}
	for index := 0; index+1 < len(chain); index++ {
		f.seedAgentRow(t, "chain-"+string(rune('a'+index)), chain[index], store.ThreadRequestSpawn, chain[index+1], false)
	}
	// The last thread spawns the caller back.
	f.seedAgentRow(t, "cycle", chain[len(chain)-1], store.ThreadRequestSpawn, f.caller.ID, false)

	rows := f.agentRows(t, f.caller.ID).Rows
	if len(rows) != threadAgentRowDepth {
		t.Fatalf("listed %d rows; want %d, one per level", len(rows), threadAgentRowDepth)
	}
	for depth, row := range rows {
		if row.Depth != depth {
			t.Fatalf("row %d at depth %d", depth, row.Depth)
		}
	}
	if last := rows[len(rows)-1]; !last.NestedHidden {
		t.Fatalf("the row at the bound does not say it hides nested rows: %+v", last)
	}
	// From the last thread the cycle comes back to the caller, which the
	// walk lists once and does not enter again.
	tail := f.agentRows(t, chain[len(chain)-1]).Rows
	if got := agentRowTokens(tail); len(got) != threadAgentRowDepth || got[0] != "cycle" {
		t.Fatalf("rows from the cycle's end = %v", got)
	}
}

// A row reads as the listing shows it, at its depth under the thread that
// reads it: within the listing's depth, made here by that thread or a
// thread its spawns made, and still listed. Any other reads as not
// listed.
func TestThreadAgentRowReadsOneListedRow(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	chain := []string{f.caller.ID}
	for range threadAgentRowDepth + 1 {
		chain = append(chain, f.plainThread(t).ID)
	}
	for index := 0; index+1 < len(chain); index++ {
		f.seedAgentRow(t, "chain-"+string(rune('a'+index)), chain[index], store.ThreadRequestSpawn, chain[index+1], false)
	}
	peer := f.plainThread(t)
	f.seedAgentRow(t, "quiet", f.caller.ID, store.ThreadRequestSend, peer.ID, false)
	f.seedAgentRow(t, "owed", chain[1], store.ThreadRequestSend, peer.ID, true)
	listing := f.agentRows(t, f.caller.ID).Rows
	depths := map[string]int{}
	for _, row := range listing {
		depths[row.Token] = row.Depth
	}
	if depths["chain-d"] != 3 || depths["owed"] != 1 {
		t.Fatalf("the listing places chain-d at %d and owed at %d; want 3 and 1", depths["chain-d"], depths["owed"])
	}
	read := func(threadID, token string) ThreadAgentRowRead {
		t.Helper()
		got, err := f.app.GetThreadAgentRows(t.Context(), threadID, []string{token})
		if err != nil || len(got) != 1 || got[0].Token != token {
			t.Fatalf("GetThreadAgentRows(%s) = %+v, %v", token, got, err)
		}
		return got[0]
	}
	for _, want := range listing {
		got := read(f.caller.ID, want.Token)
		if !got.Listed || got.Row == nil {
			t.Fatalf("%s reads as not listed", want.Token)
		}
		if *got.Row != want {
			t.Fatalf("%s reads\n %+v\nwant the listing's\n %+v", want.Token, *got.Row, want)
		}
	}
	// Read together, in the order asked, with what is not listed named.
	tokens := append(agentRowTokens(listing), "quiet")
	slices.Reverse(tokens)
	batch, err := f.app.GetThreadAgentRows(t.Context(), f.caller.ID, tokens)
	if err != nil || len(batch) != len(tokens) {
		t.Fatalf("GetThreadAgentRows = %d reads, %v", len(batch), err)
	}
	for i, got := range batch {
		if got.Token != tokens[i] || got.Listed != (got.Token != "quiet") {
			t.Fatalf("read %d = %+v; want %s", i, got, tokens[i])
		}
	}
	if _, err := f.app.GetThreadAgentRows(t.Context(), f.caller.ID, make([]string, maxThreadAgentRowReads+1)); err == nil {
		t.Fatal("an oversized read was answered")
	}
	for _, token := range []string{"quiet", "chain-e", "missing"} {
		if got := read(f.caller.ID, token); got.Listed || got.Row != nil {
			t.Fatalf("%s reads as listed: %+v", token, got)
		}
	}
	if got := read(peer.ID, "owed"); got.Listed {
		t.Fatalf("a thread outside the lineage read %+v", got)
	}
	if got := read(chain[1], "owed"); !got.Listed || got.Row.Depth != 0 {
		t.Fatalf("the caller's own read = %+v", got)
	}
}

// A worker reused after the retention floor cleared its spawn still belongs
// to its owner: the new work it opens lists under the expired spawn, which
// waits on it, both count in the owner's summary, and Stop of that row, or
// Stop All, reaches it and reports the row stopped. The expired spawn shows
// no answer.
func TestAnExpiredSpawnsNewWorkListsAndStopsUnderItsOwner(t *testing.T) {
	t.Parallel()
	for _, all := range []bool{false, true} {
		f := newRequestFixture(t)
		f.mockClaudeHoldingTheTurn(t, "working")
		worker := f.plainThread(t)
		f.seedAgentRow(t, "lead-worker", f.caller.ID, store.ThreadRequestSpawn, worker.ID, false)
		long := time.Now().Add(-time.Hour).UnixMilli()
		if settled, err := f.app.store.SettleThreadRequest("lead-worker", store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: long,
		}); err != nil || !settled {
			t.Fatalf("settle: %v %v", settled, err)
		}
		if _, _, err := f.app.sweepThreadRequestsBefore(time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if _, found, err := f.app.store.GetThreadRequest("lead-worker"); err != nil || found {
			t.Fatalf("the spawn's token past the floor reads found=%v, %v; want unknown", found, err)
		}
		open, err := f.adapter().Spawn(t.Context(), f.targetIdentity(t, worker.ID), threadtools.SpawnCall{Prompt: "again", Notify: true})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		f.awaitRequestState(t, open.Token, store.ThreadRequestRunning)
		rows := f.agentRows(t, f.caller.ID).Rows
		if got, want := agentRowTokens(rows), []string{"lead-worker", open.Token}; !slices.Equal(got, want) {
			t.Fatalf("rows %v; want the worker's new spawn under the expired one %v", got, want)
		}
		if rows[0].Answer != "" || rows[0].Worker != workerWaiting {
			t.Fatalf("the expired spawn %+v; want no answer, waiting on its worker's spawn", rows[0])
		}
		if summary, err := f.app.GetThreadWorkerSummary(t.Context(), f.caller.ID); err != nil || summary.Open != 2 {
			t.Fatalf("summary %+v, %v; want the worker and its open spawn counted", summary, err)
		}
		var stop ThreadAgentStop
		if all {
			stops, err := f.app.StopThreadAgentRows(t.Context(), f.caller.ID)
			if err != nil || len(stops) != 1 {
				t.Fatalf("StopThreadAgentRows = %+v, %v", stops, err)
			}
			stop = stops[0]
		} else if stop, err = f.app.StopThreadAgentRow(t.Context(), f.caller.ID, "lead-worker"); err != nil {
			t.Fatalf("StopThreadAgentRow: %v", err)
		}
		if stop.Token != "lead-worker" || stop.Outcome != threadAgentStopped || stop.Nested != 1 || len(stop.Failures) != 0 {
			t.Fatalf("stop (all %v) = %+v; want the expired spawn stopped with its worker's one agent", all, stop)
		}
		f.awaitRequestState(t, open.Token, store.ThreadRequestCancelled)
	}
}

// A worker's quiet send it waits on is open work under its owner, as the
// worker's own listing shows it: the owner's listing nests it under the
// worker's settled spawn, which waits on it, and its summary counts both
// while the call waits, and once the wait ends with the send open its wake
// is armed and it stays.
func TestAWorkersParkedQuietSendListsUnderItsOwner(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	worker, peer := f.plainThread(t), f.plainThread(t)
	f.seedAgentRow(t, "lead-worker", f.caller.ID, store.ThreadRequestSpawn, worker.ID, false)
	long := time.Now().Add(-time.Hour).UnixMilli()
	if settled, err := f.app.store.SettleThreadRequest("lead-worker", store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: long,
	}); err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	type sent struct {
		ack threadtools.RequestAck
		err error
	}
	done := make(chan sent, 1)
	go func() {
		ack, err := f.adapter().Send(t.Context(), f.targetIdentity(t, worker.ID), threadtools.SendCall{
			ThreadID: peer.ID, Message: "look", WaitSeconds: 2,
		})
		done <- sent{ack, err}
	}()
	var token string
	waitUntil(t, 10*time.Second, func() bool {
		parked := f.app.parkedRequestTokens()
		if len(parked) == 1 {
			token = parked[0]
		}
		return token != ""
	})
	if row := f.request(t, token); row.Notify || row.CallerThreadID != worker.ID {
		t.Fatalf("the parked send %+v; want the worker's, with no wake armed", row)
	}
	if got := agentRowTokens(f.agentRows(t, worker.ID).Rows); !slices.Equal(got, []string{token}) {
		t.Fatalf("the worker's own rows %v; want its parked send", got)
	}
	if got, want := agentRowTokens(f.agentRows(t, f.caller.ID).Rows), []string{"lead-worker", token}; !slices.Equal(got, want) {
		t.Fatalf("the owner's rows while the worker waits %v; want %v", got, want)
	}
	if summary, err := f.app.GetThreadWorkerSummary(t.Context(), f.caller.ID); err != nil || summary.Open != 2 {
		t.Fatalf("the owner's summary while the worker waits %+v, %v; want the worker and the send counted", summary, err)
	}
	result := <-done
	if result.err != nil || result.ack.Token != token {
		t.Fatalf("Send = %+v, %v", result.ack, result.err)
	}
	if row := f.request(t, token); !row.Notify || threadRequestSettled(row) {
		t.Fatalf("the send after its wait %+v; want it open with its wake armed", row)
	}
	if got, want := agentRowTokens(f.agentRows(t, f.caller.ID).Rows), []string{"lead-worker", token}; !slices.Equal(got, want) {
		t.Fatalf("the owner's rows after the wait %v; want %v", got, want)
	}
}

// A settled row whose answer the caller's provider took in names the
// timeline row that carried it, which the pane opens.
func TestThreadAgentRowsNameTheWakeThatDeliveredTheAnswer(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := owedWakeSeeds()[0].seed(t, f)
	f.app.deliverThreadWake(token, false)
	f.app.waitAgentNoticeWork()
	envelope := dispatchUnconfirmed(t, f)
	listed := func() ThreadAgentRow {
		row, ok := agentRowByToken(f.agentRows(t, f.caller.ID).Rows, token)
		if !ok {
			t.Fatalf("the settled send is not listed")
		}
		return row
	}
	if row := listed(); row.WakeItemID != "" {
		t.Fatalf("an unconfirmed wake is named: %q", row.WakeItemID)
	}
	wireID, _ := envelope["uuid"].(string)
	text, _ := envelopeText(t, envelope)
	meta, _ := json.Marshal(map[string]any{"provider_item_id": wireID})
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: f.caller.ID, Content: text, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if row, wake := listed(), f.wakeRow(t, token); row.WakeItemID != wake.ID {
		t.Fatalf("WakeItemID = %q; want the wake row %q", row.WakeItemID, wake.ID)
	}
}

// A tool row reads its request's answer from the ledger: the wake row the
// provider confirmed, one joined with another answer's included, whether or
// not the Agents listing still shows the request. A token the thread did
// not make reads nothing, and an oversized read is refused.
func TestThreadRequestStatesNameTheConfirmedWake(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	first := owedWakeSeeds()[0].seed(t, f)
	second := owedWakeSeeds()[0].seed(t, f)
	f.app.deliverThreadWake(first, false)
	f.app.deliverThreadWake(second, false)
	f.app.waitAgentNoticeWork()
	envelope := dispatchUnconfirmed(t, f)
	tokens := []string{first, second, uuid.NewString()}
	states := func() []ThreadRequestState {
		got, err := f.app.GetThreadRequestStates(f.caller.ID, tokens)
		if err != nil {
			t.Fatalf("GetThreadRequestStates: %v", err)
		}
		return got
	}
	for _, state := range states() {
		if state.WakeItemID != "" || state.Answered {
			t.Fatalf("an unconfirmed wake is named or read as the answer: %+v", state)
		}
	}
	wireID, _ := envelope["uuid"].(string)
	text, _ := envelopeText(t, envelope)
	meta, _ := json.Marshal(map[string]any{"provider_item_id": wireID})
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: f.caller.ID, Content: text, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wake := f.wakeRow(t, first)
	got := states()
	if len(got) != 2 || got[0].Token != first || got[1].Token != second {
		t.Fatalf("states = %+v; want the two requests the thread made", got)
	}
	for _, state := range got {
		if state.WakeItemID != wake.ID || !state.Answered || state.State != store.ThreadRequestFinished {
			t.Fatalf("state = %+v; want finished, answered in the joined wake %q", state, wake.ID)
		}
	}
	if other, err := f.app.GetThreadRequestStates(uuid.NewString(), tokens); err != nil || len(other) != 0 {
		t.Fatalf("another thread read %+v, %v; want nothing", other, err)
	}
	if _, err := f.app.GetThreadRequestStates(f.caller.ID, make([]string, maxThreadRequestStates+1)); err == nil {
		t.Fatal("an oversized read was answered")
	}
}

// An answer a status reply carried is the delivery and leaves no wake row:
// the ledger reads it answered once that reply is written, and not while it
// is on its way or when it never reached the model.
func TestThreadRequestStatesReadAnAnswerAStatusReplyCarried(t *testing.T) {
	t.Parallel()
	for _, written := range []bool{true, false} {
		t.Run(map[bool]string{true: "written", false: "dropped"}[written], func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			token := f.settledAnswerOwingAWake(t)
			state := func() ThreadRequestState {
				t.Helper()
				got, err := f.app.GetThreadRequestStates(f.caller.ID, []string{token})
				if err != nil || len(got) != 1 {
					t.Fatalf("GetThreadRequestStates = %+v, %v; want the one request", got, err)
				}
				return got[0]
			}
			if got := state(); got.Answered {
				t.Fatalf("state = %+v; an answer nothing delivered reads answered", got)
			}

			ctx, pending := withThreadToolsPending(t.Context())
			report, err := f.adapter().RequestStates(ctx, f.callerIdentity(), threadtools.StatusCall{Tokens: []string{token}})
			if err != nil || report.Requests[0].Delivered != store.ThreadWakeInline {
				t.Fatalf("RequestStates = %+v, %v; want the answer carried inline", report, err)
			}
			if got := state(); got.Answered {
				t.Fatalf("state = %+v; the reply carrying the answer is still on its way", got)
			}
			pending.run(written)

			got := state()
			if got.Answered != written || got.WakeItemID != "" || got.State != store.ThreadRequestFinished {
				t.Fatalf("state = %+v; want finished, answered %v, no wake row", got, written)
			}
		})
	}
}

// A late reply a status call collected inline after the first answer came by
// a wake is its own delivery: the first answer's route and wake row stand, so
// the Agents row and the ledger still name that wake.
func TestALateReplyCollectedInlineKeepsTheFirstAnswersWake(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := owedWakeSeeds()[0].seed(t, f)
	f.app.deliverThreadWake(token, false)
	f.app.waitAgentNoticeWork()
	envelope := dispatchUnconfirmed(t, f)
	wireID, _ := envelope["uuid"].(string)
	text, _ := envelopeText(t, envelope)
	meta, _ := json.Marshal(map[string]any{"provider_item_id": wireID})
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: f.caller.ID, Content: text, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	wake := f.wakeRow(t, token)
	if stored, err := f.app.store.StoreThreadRequestLateReply(token, []byte("one more thing"), time.Now().UnixMilli(), nil, false); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}

	ctx, pending := withThreadToolsPending(t.Context())
	report, err := f.adapter().RequestStates(ctx, f.callerIdentity(), threadtools.StatusCall{Tokens: []string{token}})
	if err != nil || report.Requests[0].Delivered != store.ThreadWakeInline || !strings.Contains(report.Requests[0].Answer, "one more thing") {
		t.Fatalf("RequestStates = %+v, %v; want the late reply carried inline", report, err)
	}
	pending.run(true)

	if row := f.request(t, token); row.DeliveredHow != store.ThreadWakeQueued || row.LateDeliveredHow != store.ThreadWakeInline || row.LateDeliveredAt == 0 {
		t.Fatalf("routes: first %q, late %q at %d; want queued, then inline", row.DeliveredHow, row.LateDeliveredHow, row.LateDeliveredAt)
	}
	states, err := f.app.GetThreadRequestStates(f.caller.ID, []string{token})
	if err != nil || len(states) != 1 || states[0].WakeItemID != wake.ID || !states[0].Answered {
		t.Fatalf("GetThreadRequestStates = %+v, %v; want answered in the wake row %q", states, err, wake.ID)
	}
	row, ok := agentRowByToken(f.agentRows(t, f.caller.ID).Rows, token)
	if !ok || row.WakeItemID != wake.ID {
		t.Fatalf("Agents row = %+v (listed %v); want the wake row %q", row, ok, wake.ID)
	}
}

// The other way round: a first answer a status call collected leaves no
// row, and a late reply that came by a wake names its own.
func TestALateReplyByAWakeAfterAnInlineAnswerNamesItsWake(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := owedWakeSeeds()[0].seed(t, f)
	ctx, pending := withThreadToolsPending(t.Context())
	if report, err := f.adapter().RequestStates(ctx, f.callerIdentity(), threadtools.StatusCall{Tokens: []string{token}}); err != nil || report.Requests[0].Delivered != store.ThreadWakeInline {
		t.Fatalf("RequestStates = %+v, %v; want the answer carried inline", report, err)
	}
	pending.run(true)
	if stored, err := f.app.store.StoreThreadRequestLateReply(token, []byte("one more thing"), time.Now().UnixMilli(), nil, false); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	f.app.deliverThreadWake(token, true)
	f.app.waitAgentNoticeWork()
	envelope := dispatchUnconfirmed(t, f)
	wireID, _ := envelope["uuid"].(string)
	text, _ := envelopeText(t, envelope)
	meta, _ := json.Marshal(map[string]any{"provider_item_id": wireID})
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventUserText, ThreadID: f.caller.ID, Content: text, Meta: meta, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("echo: %v", err)
	}

	row := f.request(t, token)
	if row.DeliveredHow != store.ThreadWakeInline || row.LateDeliveredHow != store.ThreadWakeQueued || row.LateConfirmedAt == 0 {
		t.Fatalf("routes: first %q, late %q confirmed at %d; want inline, then a confirmed wake", row.DeliveredHow, row.LateDeliveredHow, row.LateConfirmedAt)
	}
	record, found, err := f.app.findRecordedSend(f.caller.ID, threadWakeSendIDFor(token, true))
	if err != nil || !found || !record.dispatched {
		t.Fatalf("late wake row: %+v found %v, %v", record, found, err)
	}
	states, err := f.app.GetThreadRequestStates(f.caller.ID, []string{token})
	if err != nil || len(states) != 1 || states[0].WakeItemID != record.item.ID || !states[0].Answered {
		t.Fatalf("GetThreadRequestStates = %+v, %v; want answered in the late wake row %q", states, err, record.item.ID)
	}
}

// The thread's own Stop ends its turn and leaves the threads it is waiting
// on running: a Stop does not quiet later wakes.
func TestThreadStopLeavesItsAgentRows(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "working")
	child := f.runningRequest(t, "lead the fix")
	if err := f.app.interruptTurnCtx(t.Context(), f.caller.ID, stopByUser(nil)); err != nil {
		t.Fatalf("interrupt the caller: %v", err)
	}
	if row := f.request(t, child.Token); row.State != store.ThreadRequestRunning || !row.Notify {
		t.Fatalf("after the caller's Stop the child's request is %q, notify %v", row.State, row.Notify)
	}
	if rows := f.agentRows(t, f.caller.ID).Rows; len(rows) != 1 || rows[0].State != threadtools.RequestRunning {
		t.Fatalf("rows after the caller's Stop = %+v", rows)
	}
}

// A blocked row names the tool once, with what the prompt says about it.
func TestThreadAgentBlockedOnNamesTheToolOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ title, description, want string }{
		{"Bash", "make package", "Bash: make package"},
		{"Bash", "", "Bash"},
		{"Run the deploy", "make deploy", "Bash: Run the deploy"},
	} {
		var snapshot triage.LiveStateSnapshot
		snapshot.Interactive.Approvals = []provider.ApprovalRequest{{ToolName: "Bash", Title: tc.title, Description: tc.description}}
		if on, detail, _ := threadAgentBlockedOn(snapshot); on != "approval" || detail != tc.want {
			t.Errorf("title %q, description %q: %s %q, want approval %q", tc.title, tc.description, on, detail, tc.want)
		}
	}
}

// A row's line is bounded before its whitespace is folded: a streamed
// summary of any length costs what a short one does.
//
// Serial: it measures the process's allocations.
func TestClipLineBoundsItsWork(t *testing.T) {
	huge := strings.Repeat("word  ", 1<<18)
	line := clipLine(huge)
	if utf8.RuneCountInString(line) != threadAgentLineMax+1 || !strings.HasSuffix(line, "…") {
		t.Fatalf("clipped to %d runes: %q", utf8.RuneCountInString(line), line[:40])
	}
	result := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			clipLine(huge)
		}
	})
	if bytes := result.AllocedBytesPerOp(); bytes > 32<<10 {
		t.Fatalf("clipping a %d-byte summary allocated %d bytes", len(huge), bytes)
	}
	if got := clipLine(strings.Repeat("é", threadAgentLineMax*utf8.UTFMax*2)); !utf8.ValidString(got) {
		t.Fatal("the byte cut split a rune")
	}
}
