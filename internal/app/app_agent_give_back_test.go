package app

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"

	"github.com/google/uuid"
)

// The answers, reminders and remote completions a session death, a failed
// Codex resend or a restart finds undelivered go back to their requests and
// watches, as a Stop's do (giveBackToOwners). None of them lands in the
// person's composer, and the person's own messages beside them still do.

// requireGivenBack fails unless the request is owed its answer again,
// disarmed, and the watch is pending, with nothing left queued.
func requireGivenBack(t *testing.T, f *requestFixture, token string, late bool, w store.RemoteWatch) {
	t.Helper()
	requireOwedAgain(t, f, token, late)
	requirePendingAgain(t, f.app, w)
	if durable, memory := queuedSendIDs(t, f.app, f.caller.ID); len(durable) != 0 || len(memory) != 0 {
		t.Fatalf("queue %q (in memory %q), want it empty", durable, memory)
	}
}

// requireOwedAgain fails unless the request is owed its answer, disarmed.
func requireOwedAgain(t *testing.T, f *requestFixture, token string, late bool) {
	t.Helper()
	row := f.request(t, token)
	if threadRequestDelivered(row, late) || row.Notify {
		t.Fatalf("the request reads delivered %v as %q, notify %v; want it owed and disarmed", threadRequestDelivered(row, late), row.DeliveredHow, row.Notify)
	}
	if late && row.DeliveredAt == 0 {
		t.Fatal("returning the late reply undid the first answer's delivery")
	}
}

// requirePendingAgain fails unless the watch is pending.
func requirePendingAgain(t *testing.T, a *App, w store.RemoteWatch) {
	t.Helper()
	saved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Notification != "pending" {
		t.Fatalf("the watch reads %q, want pending", saved.Notification)
	}
}

// requireDraft fails unless the caller's composer holds exactly want.
func requireDraft(t *testing.T, a *App, threadID, want string) {
	t.Helper()
	draft, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Content != want {
		t.Fatalf("composer = %q, want %q", draft.Content, want)
	}
}

// queueOwnedAndPersonMessages queues a person's message, the request's
// wake and the watch's completion on the caller, in that order.
func queueOwnedAndPersonMessages(t *testing.T, f *requestFixture, token string, late bool, w store.RemoteWatch, output string) {
	t.Helper()
	if _, err := f.app.RegisterQueueItem(context.Background(), f.caller.ID, "my own words", SendMessageOptions{SendID: "person-send"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	f.app.deliverThreadWake(token, late)
	if err := f.app.queueAgentNotice(f.app.remoteCompletionNotice(w, remoteCompletionOutput{Tail: output})); err != nil {
		t.Fatalf("queueAgentNotice: %v", err)
	}
	f.app.waitAgentNoticeWork()
	if row := f.request(t, token); row.DeliveredHow != store.ThreadWakeQueued {
		t.Fatalf("the wake was not queued: delivered as %q", row.DeliveredHow)
	}
}

// A session that ends with an answer, a late reply or a reminder and a
// completion still in its queue gives them back; the person's message beside
// them goes to the composer.
func TestASessionDeathGivesBackWhatWasStillQueued(t *testing.T) {
	t.Parallel()
	for _, tc := range owedWakeSeeds() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.app.startSessionFn = func(string) error { return nil }
			token := tc.seed(t, f)
			w := completedRemoteWatch(t, f.app, f.caller)
			// No dispatcher is configured, so everything stays queued.
			queueOwnedAndPersonMessages(t, f, token, tc.late, w, "build passed")

			f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)

			requireGivenBack(t, f, token, tc.late, w)
			requireDraft(t, f.app, f.caller.ID, "my own words")
		})
	}
}

// claudeRecordingSession gives the caller a live Claude session whose stdin
// is recorded and which never echoes, so every message written to it stays
// unconfirmed, and configures the dispatcher.
func claudeRecordingSession(t *testing.T, f *requestFixture) string {
	t.Helper()
	stdinLog := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	sess, err := claude.NewSession(context.Background(), f.caller.ID,
		claude.Config{Binary: writeClaudeStdinRecorderBinary(t, stdinLog), WorkDir: f.caller.WorkspacePath},
		func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("claude.NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	f.app.sessionManager().put(f.caller.ID, session{
		Provider: string(provider.Claude),
		Token:    "tok",
		Claude:   sess,
		Liveness: newSessionLiveness(time.Now()),
	})
	f.app.configureTriageQueueCallbacks()
	return stdinLog
}

// settledAnswerHolding settles a request of the caller's with an answer that
// holds text, and returns its token.
func settledAnswerHolding(t *testing.T, f *requestFixture, text string) string {
	t.Helper()
	token := uuid.NewString()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
		State: store.ThreadRequestUnconfirmed, TargetComputerID: uuid.NewString(),
		TargetThreadID: uuid.NewString(), Notify: true,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	now := time.Now()
	settled, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte(text), AnswerKind: store.ThreadAnswerFinal,
		SettledAt: now.UnixMilli(), ExpiresAt: now.Add(threadLateReplyWindow).UnixMilli(),
	})
	if err != nil || !settled {
		t.Fatalf("settle %s: settled=%v err=%v", token, settled, err)
	}
	return token
}

// Claude joins the messages queued during a turn into one. One that holds a
// person's message, an answer and a completion, both of whose texts hold the
// separator the join puts between messages, splits back by the lengths its
// dispatch recorded: the person's text alone goes to the composer, and the
// answer and completion, whose rows went at the write, go back unconfirmed.
func TestASessionDeathSplitsAJoinedAnswerAndCompletionExactly(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := settledAnswerHolding(t, f, "first half"+usermessage.JoinSeparator+"second half")
	w := completedRemoteWatch(t, f.app, f.caller)
	queueOwnedAndPersonMessages(t, f, token, false, w, "line one"+usermessage.JoinSeparator+"line two")
	if _, err := f.app.RegisterQueueItem(context.Background(), f.caller.ID, "and after", SendMessageOptions{SendID: "person-after"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}

	stdinLog := claudeRecordingSession(t, f)
	f.app.triage.FlushQueuedItems(f.caller.ID)
	waitForRecordedUserEnvelopes(t, stdinLog, 1)
	waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, f.app, f.caller.ID)) == 0 })
	f.app.flushDispatch.wg.Wait()

	f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)

	requireGivenBack(t, f, token, false, w)
	requireDraft(t, f.app, f.caller.ID, "my own words"+usermessage.JoinSeparator+"and after")
}

// An answer or a completion sent alone and never confirmed has no queue row
// left when its session ends, and goes back all the same.
func TestASessionDeathGivesBackAnUnconfirmedMessageWithNoRow(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"answer", "completion"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.app.startSessionFn = func(string) error { return nil }
			var token string
			var w store.RemoteWatch
			if kind == "answer" {
				token = owedWakeSeeds()[0].seed(t, f)
				f.app.deliverThreadWake(token, false)
			} else {
				w = completedRemoteWatch(t, f.app, f.caller)
				if err := f.app.queueAgentNotice(f.app.remoteCompletionNotice(w, remoteCompletionOutput{Tail: "done"})); err != nil {
					t.Fatalf("queueAgentNotice: %v", err)
				}
			}
			f.app.waitAgentNoticeWork()
			stdinLog := claudeRecordingSession(t, f)
			f.app.triage.FlushQueuedItems(f.caller.ID)
			waitForRecordedUserEnvelopes(t, stdinLog, 1)
			waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, f.app, f.caller.ID)) == 0 })
			f.app.flushDispatch.wg.Wait()

			f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)

			if kind == "answer" {
				requireOwedAgain(t, f, token, false)
			} else {
				requirePendingAgain(t, f.app, w)
			}
			if durable, memory := queuedSendIDs(t, f.app, f.caller.ID); len(durable) != 0 || len(memory) != 0 {
				t.Fatalf("queue %q (in memory %q), want it empty", durable, memory)
			}
			requireDraft(t, f.app, f.caller.ID, "")
		})
	}
}

// A Codex resend that fails after an interrupt returns what it carried to
// the composer. An answer and a completion among it go back to their owners
// instead.
func TestAFailedResendGivesBackItsAnswerAndCompletion(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := owedWakeSeeds()[0].seed(t, f)
	w := completedRemoteWatch(t, f.app, f.caller)
	queueOwnedAndPersonMessages(t, f, token, false, w, "build passed")
	// The dispatch took them: their rows went at the write.
	rows := durableQueueRows(t, f.app, f.caller.ID)
	f.app.triage.CleanupThread(f.caller.ID)
	var persisted []triage.EagerPersistedFlush
	for i, row := range rows {
		if err := f.app.store.DeleteDispatchedFlushQueueItem(row.ID); err != nil {
			t.Fatal(err)
		}
		meta, err := usermessage.Marshal(usermessage.Input{SendID: row.SendID})
		if err != nil {
			t.Fatal(err)
		}
		persisted = append(persisted, triage.EagerPersistedFlush{
			UserItemID: "user:eager:" + row.ID, Content: row.Message, Meta: meta,
			QueueItemID: row.ID, EnqueuedAt: row.EnqueuedAt + int64(i),
		})
	}

	f.app.restoreEagerPersistedFlushesToDraft(f.caller.ID, persisted)

	requireGivenBack(t, f, token, false, w)
	requireDraft(t, f.app, f.caller.ID, "my own words")
}

// A restart finds an answer, a late reply or a reminder and a completion
// still queued from the last run: they go back to their owners, and the
// person's message beside them goes to the composer.
func TestTheBootSweepGivesBackWhatWasQueuedAtQuit(t *testing.T) {
	t.Parallel()
	for _, tc := range owedWakeSeeds() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.app.startSessionFn = func(string) error { return nil }
			token := tc.seed(t, f)
			w := completedRemoteWatch(t, f.app, f.caller)
			queueOwnedAndPersonMessages(t, f, token, tc.late, w, "build passed")

			after := f.restart(t)
			after.app.restoreDurableFlushQueueAtBoot()

			requireGivenBack(t, after, token, tc.late, w)
			requireDraft(t, after.app, f.caller.ID, "my own words")
		})
	}
}

// A restore whose composer write fails requeues every item whole, the app's
// messages included, so none of them is settled: no notice says an outside
// send was not delivered, a loop wake's row records nothing, and an answer
// still reads delivered; all four are queued again for delivery. With a
// readable composer, each is settled once. Both restores run: a session
// death's and a failed Codex resend's. The write fails on an existing draft
// whose attachments do not decode, the seam
// TestCodexResendAfterInterrupt_DraftMergeFailureRequeues uses.
func TestARestoreWhoseDraftWriteFailsSettlesNothingItRequeues(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"session death", "failed resend"} {
		for _, fails := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, draft write fails %v", path, fails), func(t *testing.T) {
				t.Parallel()
				f := newRequestFixture(t)
				f.app.startSessionFn = func(string) error { return nil }
				attachments := "[]"
				if fails {
					attachments = "corrupt"
				}
				if _, err := f.app.store.UpsertThreadDraft(store.ThreadDraft{
					ThreadID: f.caller.ID, Attachments: attachments, UpdatedAt: time.Now().UnixMilli(),
				}); err != nil {
					t.Fatal(err)
				}
				wake, err := f.app.store.RegisterAgentWake(store.AgentWake{
					ID: "wake-board", ThreadID: f.caller.ID, Kind: store.AgentWakeLoop, Label: "board",
					IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
				})
				if err != nil {
					t.Fatal(err)
				}
				token := settledAnswerHolding(t, f, "the notes are ready")
				if marked, err := f.app.store.MarkThreadRequestDelivered(token, store.ThreadWakeQueued, time.Now().UnixMilli(), false); err != nil || !marked {
					t.Fatalf("MarkThreadRequestDelivered: marked=%v err=%v", marked, err)
				}
				sends := []struct{ sendID, text string }{
					{"person-send", "my own words"},
					{agentSendIDRoot + "deploy", "deploy 11 is done"},
					{agentWakeSendID(wake.ID, 1000), "Loop board fired"},
					{threadWakeSendID(token), "the notes are ready"},
				}
				var want []string
				for _, s := range sends {
					want = append(want, s.sendID)
				}

				if path == "session death" {
					var items []triage.UnconfirmedFlushItem
					for i, s := range sends {
						meta, err := usermessage.Marshal(usermessage.Input{SendID: s.sendID})
						if err != nil {
							t.Fatal(err)
						}
						row := store.Item{ThreadID: f.caller.ID, ID: "user:death:" + s.sendID, Summary: s.text, Meta: meta}
						items = append(items, triage.UnconfirmedFlushItem{UserItemID: row.ID, Message: s.text, EnqueuedAt: int64(1000 + i), QuietItem: &row})
					}
					f.app.restoreFlushItemsToDraftLocked(f.caller.ID, items, queueRestoredReasonSessionDied)
				} else {
					var persisted []triage.EagerPersistedFlush
					for i, s := range sends {
						meta, err := usermessage.Marshal(usermessage.Input{SendID: s.sendID})
						if err != nil {
							t.Fatal(err)
						}
						persisted = append(persisted, triage.EagerPersistedFlush{
							UserItemID: "user:eager:" + s.sendID, Content: s.text, Meta: meta, EnqueuedAt: int64(1000 + i),
						})
					}
					f.app.restoreEagerPersistedFlushesToDraft(f.caller.ID, persisted)
				}

				notices := noticeRows(t, f.app, f.caller.ID, "A message sent with agent-overflow thread send was not delivered")
				saved, found, err := f.app.store.GetAgentWake(wake.ID)
				if err != nil || !found {
					t.Fatalf("GetAgentWake: found=%v err=%v", found, err)
				}
				_, memory := queuedSendIDs(t, f.app, f.caller.ID)
				request := f.request(t, token)
				if fails {
					if len(notices) != 0 {
						t.Errorf("notices %q for a send queued again", notices)
					}
					if saved.Undelivered != "" {
						t.Errorf("the wake row records %q for a wake queued again", saved.Undelivered)
					}
					if request.DeliveredHow != store.ThreadWakeQueued || !request.Notify {
						t.Errorf("the request reads delivered as %q, notify %v; want it still queued", request.DeliveredHow, request.Notify)
					}
					if !slices.Equal(memory, want) {
						t.Errorf("queued again %q, want %q", memory, want)
					}
					return
				}
				outcome := agentWakeOutcomeSessionEnded
				if path == "failed resend" {
					outcome = agentWakeOutcomeNotDelivered
				}
				if len(notices) != 1 {
					t.Errorf("notices %q, want one", notices)
				}
				if saved.Undelivered != outcome {
					t.Errorf("the wake row records %q, want %q", saved.Undelivered, outcome)
				}
				requireOwedAgain(t, f, token, false)
				if len(memory) != 0 {
					t.Errorf("queued again %q, want nothing", memory)
				}
				requireDraft(t, f.app, f.caller.ID, "my own words")
			})
		}
	}
}
