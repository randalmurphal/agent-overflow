package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/transport"
)

// A call that starts a request holds its token in the wait registry from
// before the message can be answered until its response is written
// (holdNewRequest), at any wait. These tests answer the request at each
// point of that span and check the answer reaches the caller exactly once.

// mockClaudeHoldingTheTurnOn is mockClaudeHoldingTheTurn for any app.
func mockClaudeHoldingTheTurnOn(t *testing.T, app *App, text string) {
	t.Helper()
	installMockClaudeTurns(t, app, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-hold","role":"assistant","content":[{"type":"text","text":` + quoteJSON(text) + `}]}}`,
	}})
}

// replyOnceRunning answers token from its target once the receipt is
// running, through the same thread_reply a responder makes.
func replyOnceRunning(t *testing.T, app *App, token, text string) {
	t.Helper()
	var receipt store.ThreadRequestReceipt
	waitUntilE2E(t, 30*time.Second, "the receipt of "+token+" runs", func() bool {
		row, found, err := app.store.GetThreadRequestReceipt(token)
		receipt = row
		return err == nil && found && row.State == store.ThreadReceiptRunning
	})
	thread, err := app.store.GetThread(receipt.TargetThreadID)
	if err != nil {
		t.Fatalf("GetThread(%s): %v", receipt.TargetThreadID, err)
	}
	responder := threadtools.Caller{ThreadID: thread.ID, Title: thread.Title, ComputerID: "local-computer", ComputerName: "This Mac"}
	if _, err := (threadToolsApp{app: app}).Reply(context.Background(), responder, threadtools.ReplyCall{Token: token, Text: text}); err != nil {
		t.Fatalf("Reply(%s): %v", token, err)
	}
}

// wakesFor is the caller's queued messages that carry token's first answer.
func wakesFor(t *testing.T, app *App, callerID, token string) []store.FlushQueueItem {
	t.Helper()
	return slices.DeleteFunc(durableQueueRows(t, app, callerID), func(row store.FlushQueueItem) bool {
		return row.SendID != threadWakeSendID(token)
	})
}

// An answer that lands after a spawn, send or ask dispatched its message and
// before its ack reads the request is the ack's to deliver: no wake is
// queued and the ack carries the reply inline. The hook replies in exactly
// that moment.
func TestAnAnswerBeforeTheAckReadsTheRequestIsTheAcksToDeliver(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{store.ThreadRequestSpawn, store.ThreadRequestSend, store.ThreadRequestAsk} {
		for _, waitSeconds := range []int{30, 0} {
			t.Run(fmt.Sprintf("%s/wait %d", kind, waitSeconds), func(t *testing.T) {
				t.Parallel()
				f := newRequestFixture(t)
				f.mockClaudeHoldingTheTurn(t, "looking")
				var target store.Thread
				switch kind {
				case store.ThreadRequestSend:
					made, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
					if err != nil {
						t.Fatal(err)
					}
					target = made
				case store.ThreadRequestAsk:
					target = f.forkableThread(t, "ask-gap")
				}
				const answer = "the stall is in the watchdog"
				replied := false
				f.app.requestAckForTest = func(token string) {
					replyOnceRunning(t, f.app, token, answer)
					replied = true
				}

				ctx, pending := withThreadToolsPending(t.Context())
				var ack threadtools.RequestAck
				var err error
				switch kind {
				case store.ThreadRequestSpawn:
					ack, err = f.adapter().Spawn(ctx, f.callerIdentity(), threadtools.SpawnCall{Prompt: "where is the stall?", WaitSeconds: waitSeconds})
				case store.ThreadRequestSend:
					ack, err = f.adapter().Send(ctx, f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "where is the stall?", WaitSeconds: waitSeconds})
				case store.ThreadRequestAsk:
					ack, err = f.adapter().Ask(ctx, f.callerIdentity(), threadtools.AskCall{ThreadID: target.ID, Question: "where is the stall?", WaitSeconds: waitSeconds})
				}
				if err != nil {
					t.Fatalf("%s: %v", kind, err)
				}
				if !replied {
					t.Fatal("the reply never ran between the dispatch and the ack")
				}
				if ack.Outcome != threadtools.OutcomeSettled || ack.AnswerKind != threadtools.AnswerReply || ack.Answer != answer {
					t.Fatalf("ack = %+v, want the reply settled in it", ack)
				}
				if ack.Delivered != store.ThreadWakeInline || ack.WakeQueued {
					t.Fatalf("ack delivered = %q, wake queued = %v: the reply in the ack was also queued as a message", ack.Delivered, ack.WakeQueued)
				}
				pending.run(true)
				f.app.threadRequestsWG.Wait()
				f.app.retryUndeliveredThreadWakes(time.Now())
				f.app.threadRequestsWG.Wait()
				if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
					t.Fatalf("a wake was queued for the reply the ack carried: %+v", rows)
				}
				if row := f.request(t, ack.Token); row.DeliveredHow != store.ThreadWakeInline || row.DeliveredAt == 0 {
					t.Fatalf("delivery = %q at %d, want the ack's inline delivery recorded", row.DeliveredHow, row.DeliveredAt)
				}
			})
		}
	}
}

// The same moment for a send to a paired computer: the destination's reply
// is collected here by a poll while the call holds the token, before its ack
// reads the request, so the ack carries it and no wake is queued.
func TestAPairedAnswerBeforeTheAckReadsTheRequestIsTheAcksToDeliver(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	for _, waitSeconds := range []int{30, 0} {
		t.Run(fmt.Sprintf("wait %d", waitSeconds), func(t *testing.T) {
			t.Parallel()
			pair := newReachPair(t)
			target, err := createTestThread(t, pair.dest, pair.caller.Provider, t.TempDir(), pair.caller.Model, pair.caller.Mode)
			if err != nil {
				t.Fatal(err)
			}
			mockClaudeHoldingTheTurnOn(t, pair.dest, "looking")
			installMockClaudeReplies(t, pair.source, "noted")
			const answer = "the stall is in the watchdog"
			replied := false
			pair.source.requestAckForTest = func(token string) {
				replyOnceRunning(t, pair.dest, token, answer)
				pair.poll(t)
				if row := pair.request(t, token); row.State != store.ThreadRequestReplied {
					t.Fatalf("the poll did not collect the reply: %+v", row)
				}
				replied = true
			}

			ctx, pending := withThreadToolsPending(t.Context())
			ack, err := pair.adapter().Send(ctx, pair.callerIdentity(), threadtools.SendCall{
				ThreadID: target.ID, ComputerID: pair.computer, Message: "where is the stall?", WaitSeconds: waitSeconds,
			})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if !replied {
				t.Fatal("the reply never ran between the dispatch and the ack")
			}
			if ack.Outcome != threadtools.OutcomeSettled || ack.AnswerKind != threadtools.AnswerReply || ack.Answer != answer {
				t.Fatalf("ack = %+v, want the reply settled in it", ack)
			}
			if ack.Delivered != store.ThreadWakeInline || ack.WakeQueued {
				t.Fatalf("ack delivered = %q, wake queued = %v: the reply in the ack was also queued as a message", ack.Delivered, ack.WakeQueued)
			}
			pending.run(true)
			pair.source.threadRequestsWG.Wait()
			pair.source.retryUndeliveredThreadWakes(time.Now())
			pair.source.threadRequestsWG.Wait()
			if rows := durableQueueRows(t, pair.source, pair.caller.ID); len(rows) != 0 {
				t.Fatalf("a wake was queued for the reply the ack carried: %+v", rows)
			}
			if row := pair.request(t, ack.Token); row.DeliveredHow != store.ThreadWakeInline || row.DeliveredAt == 0 {
				t.Fatalf("delivery = %q at %d, want the ack's inline delivery recorded", row.DeliveredHow, row.DeliveredAt)
			}
		})
	}
}

// A reply that lands after the ack read the request but before its response
// was written is not in the ack, so the release leaves it owed and the retry
// pass delivers it as one wake: at wait 0, and after a positive wait that
// timed out.
func TestAnAnswerAfterTheAckReadArrivesOnceAsAWake(t *testing.T) {
	t.Parallel()
	for _, waitSeconds := range []int{0, 1} {
		t.Run(fmt.Sprintf("wait %d", waitSeconds), func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.mockClaudeHoldingTheTurn(t, "looking")
			ctx, pending := withThreadToolsPending(t.Context())
			ack, err := f.adapter().Spawn(ctx, f.callerIdentity(), threadtools.SpawnCall{Prompt: "where is the stall?", WaitSeconds: waitSeconds})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if ack.Outcome != threadtools.OutcomeBackgrounded || ack.Answer != "" || ack.Delivered != "" {
				t.Fatalf("ack = %+v, want the request still running with no answer in it", ack)
			}
			if waitSeconds > 0 && !ack.Notify {
				t.Fatal("the timed-out wait did not arm the wake")
			}

			replyOnceRunning(t, f.app, ack.Token, "the stall is in the watchdog")
			f.app.threadRequestsWG.Wait()
			if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
				t.Fatalf("a wake was queued while the call still held the token: %+v", rows)
			}
			if row := f.request(t, ack.Token); row.State != store.ThreadRequestReplied || row.DeliveredAt != 0 {
				t.Fatalf("request = %+v, want replied and not delivered", row)
			}

			pending.run(true)
			if f.app.requestWaitActive(ack.Token) {
				t.Fatal("the response was written and the token is still held")
			}
			for range 2 {
				f.app.retryUndeliveredThreadWakes(time.Now())
				f.app.threadRequestsWG.Wait()
			}
			wakes := wakesFor(t, f.app, f.caller.ID, ack.Token)
			if len(wakes) != 1 || !strings.Contains(wakes[0].Message, "watchdog") {
				t.Fatalf("wakes = %+v, want the reply queued once", wakes)
			}
			if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 1 {
				t.Fatalf("queued = %+v, want the one wake", rows)
			}
			if row := f.request(t, ack.Token); row.DeliveredHow != store.ThreadWakeQueued {
				t.Fatalf("delivery = %q, want the wake's", row.DeliveredHow)
			}
		})
	}
}

// A start that fails after it took its hold releases it with its response
// and owes nothing: no wake, no Agents row, and no request row when it was
// refused before one was written.
func TestAFailedStartReleasesItsHoldAndOwesNothing(t *testing.T) {
	t.Parallel()

	t.Run("send refused at its dispatch", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		f.mockClaude(t, "on it")
		if err := f.app.initIdentity("backend-under-test"); err != nil {
			t.Fatal(err)
		}
		limited := pairSessionWithScopes(t, f.app, "thumb-hold", []identity.Scope{
			identity.ScopeThreadsRead, identity.ScopeThreadsOperate, identity.ScopeTerminalOperate,
		})
		target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
		if err != nil {
			t.Fatal(err)
		}
		ctx, pending := withThreadToolsPending(callFrom(limited.ID, false))
		_, err = f.adapter().Send(ctx, f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "keep going", WaitSeconds: 30})
		wantScopeRefusal(t, err, transport.ScopeThreadsAutonomy)
		rows, err := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 10, 0)
		if err != nil || len(rows) != 1 {
			t.Fatalf("requests = %+v, %v; want the one refused send", rows, err)
		}
		token := rows[0].Token
		if row := rows[0]; row.State != store.ThreadRequestRefused || row.DeliveredAt == 0 {
			t.Fatalf("request = %+v, want refused and delivered by the call's error", row)
		}
		if !f.app.requestsHeld([]string{token})[token] {
			t.Fatal("the hold ended before the refusal was written")
		}
		pending.run(true)
		assertOwesNothing(t, f, token)
	})

	t.Run("spawn refused before its request was written", func(t *testing.T) {
		t.Parallel()
		f := newRequestFixture(t)
		ctx, pending := withThreadToolsPending(t.Context())
		var token string
		f.app.spawnNamingForTest = func(string) { t.Error("the refused spawn made a thread") }
		_, err := f.adapter().Spawn(ctx, f.callerIdentity(), threadtools.SpawnCall{
			Prompt: "start", Placement: threadtools.PlacementOwned, Group: "Sweep", WaitSeconds: 30,
		})
		if err == nil {
			t.Fatal("a spawn both owned and grouped was admitted")
		}
		f.app.threadRequests.mu.Lock()
		for key := range f.app.threadRequests.parked {
			token = key
		}
		f.app.threadRequests.mu.Unlock()
		if token == "" {
			t.Fatal("the spawn took no hold before refusing, so this no longer covers its error exit")
		}
		if _, found, err := f.app.store.GetThreadRequest(token); err != nil || found {
			t.Fatalf("the refused spawn left a request row: found=%v err=%v", found, err)
		}
		pending.run(true)
		assertOwesNothing(t, f, token)
	})
}

// assertOwesNothing checks a finished call left token unheld, queued no
// wake, and shows no Agents row for it, after the retry pass too.
func assertOwesNothing(t *testing.T, f *requestFixture, token string) {
	t.Helper()
	if f.app.requestsHeld([]string{token})[token] {
		t.Fatal("the response was written and the token is still held")
	}
	f.app.threadRequestsWG.Wait()
	f.app.retryUndeliveredThreadWakes(time.Now())
	f.app.threadRequestsWG.Wait()
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("a failed start queued a message: %+v", rows)
	}
	listing, err := f.app.ListThreadAgentRows(t.Context(), f.caller.ID)
	if err != nil {
		t.Fatalf("ListThreadAgentRows: %v", err)
	}
	for _, row := range listing.Rows {
		if row.Token == token {
			t.Fatalf("a failed start left an Agents row: %+v", row)
		}
	}
}

// HarnessRequestWaits answers from the wait registry: false before any call
// holds a token, true while a starting call or a status wait holds it, false
// once that call returns, and the same again for a second wait.
func TestHarnessRequestWaitsReadsTheWaitRegistry(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	harness := NewHarness(f.app, HarnessPaths{})
	target := f.busyThread(t, "harness-waits")
	const never = "never-waited-on"
	held := func(t *testing.T, token string) bool {
		t.Helper()
		got, err := harness.HarnessRequestWaits([]string{token, never})
		if err != nil {
			t.Fatalf("HarnessRequestWaits: %v", err)
		}
		if len(got) != 2 || got[never] {
			t.Fatalf("HarnessRequestWaits = %v, want both tokens named and %s free", got, never)
		}
		return got[token]
	}

	heldWhileStarting := false
	f.app.requestAckForTest = func(token string) { heldWhileStarting = held(t, token) }
	ack, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{ThreadID: target.ID, Message: "when you can"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.app.requestAckForTest = nil
	if !heldWhileStarting {
		t.Fatal("the send's own hold did not read as a call waiting on its token")
	}
	if held(t, ack.Token) {
		t.Fatal("the send returned and its token still reads as held")
	}

	status := func(waitSeconds int) chan error {
		done := make(chan error, 1)
		go func() {
			_, err := f.adapter().RequestStates(context.Background(), f.callerIdentity(), threadtools.StatusCall{
				Tokens: []string{ack.Token}, WaitSeconds: waitSeconds,
			})
			done <- err
		}()
		return done
	}
	returned := func(t *testing.T, done chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RequestStates: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the status wait did not return")
		}
	}

	// A wait the caller's interrupt cancels.
	first := status(30)
	waitUntil(t, 10*time.Second, func() bool { return held(t, ack.Token) })
	f.app.cancelThreadRequestWaits(f.caller.ID)
	returned(t, first)
	if held(t, ack.Token) {
		t.Fatal("the cancelled wait returned and the token still reads as held")
	}

	// A second wait on the same token, which runs out.
	second := status(1)
	waitUntil(t, 10*time.Second, func() bool { return held(t, ack.Token) })
	returned(t, second)
	if held(t, ack.Token) {
		t.Fatal("the second wait returned and the token still reads as held")
	}
}
