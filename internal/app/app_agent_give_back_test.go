package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
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

// dispatchUnconfirmed hands everything queued on the caller to a Claude
// session that never echoes, as one joined message, and returns the
// envelope it wrote: the rows go at the write and the messages stay
// unconfirmed.
func dispatchUnconfirmed(t *testing.T, f *requestFixture) map[string]any {
	t.Helper()
	stdinLog := claudeRecordingSession(t, f)
	f.app.triage.FlushQueuedItems(f.caller.ID)
	envelopes := waitForRecordedUserEnvelopes(t, stdinLog, 1)
	if len(envelopes) != 1 {
		t.Fatalf("the session was written %d messages, want one", len(envelopes))
	}
	waitUntil(t, 10*time.Second, func() bool { return len(durableQueueRows(t, f.app, f.caller.ID)) == 0 })
	f.app.flushDispatch.wg.Wait()
	return envelopes[0]
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

	dispatchUnconfirmed(t, f)

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
			dispatchUnconfirmed(t, f)

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
// send was not delivered, a loop wake's row records nothing and its queue row
// stays, a worker's prompt stays routed to the agent, and an answer still
// reads delivered; all five are queued again for delivery. With a readable
// composer, each is settled once. Both restores run: a session death's and a
// failed Codex resend's. The write fails on an existing draft whose
// attachments do not decode, the seam
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
				prompt := f.app.workerPrompts.add(workerPrompt{
					workerID: "worker-thread", agentID: f.caller.ID, requestID: "approval-1",
				}, func(string, string) bool { return true })
				// The loop wake never left the queue: its durable row is the
				// only copy a quit would keep.
				wakeRow := store.FlushQueueItem{
					ID: "queue-wake-board", ThreadID: f.caller.ID, SendID: agentWakeSendID(wake.ID, 1000),
					Message: "Loop board fired", EnqueuedAt: 1002,
				}
				if err := f.app.store.InsertFlushQueueItem(wakeRow); err != nil {
					t.Fatal(err)
				}
				sends := []struct{ sendID, text, queueID string }{
					{"person-send", "my own words", ""},
					{agentSendIDRoot + "deploy", "deploy 11 is done", ""},
					{wakeRow.SendID, wakeRow.Message, wakeRow.ID},
					{threadWakeSendID(token), "the notes are ready", ""},
					{prompt.sendID(), `Thread "worker" is waiting on your answer`, ""},
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
						items = append(items, triage.UnconfirmedFlushItem{
							QueueItemID: s.queueID, UserItemID: row.ID, Message: s.text, EnqueuedAt: int64(1000 + i), QuietItem: &row,
						})
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
							QueueItemID: s.queueID, UserItemID: "user:eager:" + s.sendID, Content: s.text, Meta: meta, EnqueuedAt: int64(1000 + i),
						})
					}
					f.app.restoreEagerPersistedFlushesToDraft(f.caller.ID, persisted)
				}

				notices := noticeRows(t, f.app, f.caller.ID, "A message sent with agent-overflow thread send was not delivered")
				saved, found, err := f.app.store.GetAgentWake(wake.ID)
				if err != nil || !found {
					t.Fatalf("GetAgentWake: found=%v err=%v", found, err)
				}
				durable, memory := queuedSendIDs(t, f.app, f.caller.ID)
				request := f.request(t, token)
				routed := f.app.workerPrompts.isRouted(prompt.token)
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
					if !routed {
						t.Errorf("the worker's prompt went back to the user for a wake queued again")
					}
					if !slices.Equal(durable, []string{wakeRow.SendID}) {
						t.Errorf("queue rows %q for a wake queued again, want its row %q", durable, wakeRow.SendID)
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
				if routed {
					t.Errorf("the worker's prompt is still routed to an agent that did not take it")
				}
				if len(durable) != 0 {
					t.Errorf("queue rows %q, want none", durable)
				}
				if len(memory) != 0 {
					t.Errorf("queued again %q, want nothing", memory)
				}
				requireDraft(t, f.app, f.caller.ID, "my own words")
			})
		}
	}
}

// A joined message that holds an answer and whose dispatch recorded no
// parts cannot be split, so all of its text goes back to the composer, an
// outside send's and a wake's with it. The wake's row still records it, the
// worker's prompt goes back to the user and the answer to its request, but
// the send leaves no notice: its text is in front of the person, and a
// notice would invite sending it twice.
func TestAnUnsplittableJoinGoesToTheComposerWhole(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"session death", "failed resend"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.app.startSessionFn = func(string) error { return nil }
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
			prompt := f.app.workerPrompts.add(workerPrompt{
				workerID: "worker-thread", agentID: f.caller.ID, requestID: "approval-1",
				approval: &provider.ApprovalRequest{ToolName: "Bash", Description: "run it"},
			}, func(string, string) bool { return true })
			ids := []string{"person-send", agentSendIDRoot + "deploy", agentWakeSendID(wake.ID, 1000), prompt.sendID(), threadWakeSendID(token)}
			content := strings.Join([]string{
				"my own words",
				renderAgentSendWake("deploy 11 is done"),
				renderAgentLoopWake(wake, agentWakeFire{}),
				renderWorkerPromptWake("Worker", "worker-thread", prompt),
				"the notes are ready",
			}, usermessage.JoinSeparator)
			meta, err := usermessage.Marshal(usermessage.Input{SendID: ids[0], JoinedSendIDs: ids})
			if err != nil {
				t.Fatal(err)
			}

			if path == "session death" {
				row := store.Item{ThreadID: f.caller.ID, ID: "user:death:joined", Summary: content, Meta: meta}
				f.app.restoreFlushItemsToDraftLocked(f.caller.ID, []triage.UnconfirmedFlushItem{
					{UserItemID: row.ID, Message: content, EnqueuedAt: 1000, QuietItem: &row},
				}, queueRestoredReasonSessionDied)
			} else {
				f.app.restoreEagerPersistedFlushesToDraft(f.caller.ID, []triage.EagerPersistedFlush{
					{UserItemID: "user:eager:joined", Content: content, Meta: meta, EnqueuedAt: 1000},
				})
			}

			requireDraft(t, f.app, f.caller.ID, content)
			if notices := noticeRows(t, f.app, f.caller.ID, "A message sent with agent-overflow thread send was not delivered"); len(notices) != 0 {
				t.Errorf("notices %q for a send whose text is in the composer", notices)
			}
			saved, found, err := f.app.store.GetAgentWake(wake.ID)
			if err != nil || !found {
				t.Fatalf("GetAgentWake: found=%v err=%v", found, err)
			}
			outcome := agentWakeOutcomeSessionEnded
			if path == "failed resend" {
				outcome = agentWakeOutcomeNotDelivered
			}
			if saved.Undelivered != outcome {
				t.Errorf("the wake row records %q, want %q", saved.Undelivered, outcome)
			}
			if f.app.workerPrompts.isRouted(prompt.token) {
				t.Errorf("the worker's prompt is still routed to an agent that did not take it")
			}
			requireOwedAgain(t, f, token, false)
			if _, memory := queuedSendIDs(t, f.app, f.caller.ID); len(memory) != 0 {
				t.Errorf("queued again %q, want nothing", memory)
			}
		})
	}
}

// requireDueAtOnce fails unless the watch is pending and due now, with no
// session end counted against it.
func requireDueAtOnce(t *testing.T, a *App, w store.RemoteWatch) {
	t.Helper()
	requirePendingAgain(t, a, w)
	saved, err := a.store.GetRemoteWatch(w.ComputerID, w.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.NextCheck > time.Now().UnixMilli() || saved.WakeAttempts != 0 {
		t.Fatalf("the watch is due at %d after %d session ends; want due now after none", saved.NextCheck, saved.WakeAttempts)
	}
}

// A person's Stop or an archive while the provider holds an answer, a late
// reply or a reminder and a completion it has not confirmed gives them
// back: CleanupThread drops them from the session with the person's text
// joined to them, which stays dropped. A Stop's completion is due again at
// once; an archive's waits for the unarchive.
func TestAStopOrArchiveGivesBackWhatTheProviderNeverConfirmed(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"stop", "archive"} {
		for _, tc := range owedWakeSeeds() {
			t.Run(end+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				f := newRequestFixture(t)
				f.app.startSessionFn = func(string) error { return nil }
				token := tc.seed(t, f)
				w := completedRemoteWatch(t, f.app, f.caller)
				queueOwnedAndPersonMessages(t, f, token, tc.late, w, "build passed")
				dispatchUnconfirmed(t, f)

				if end == "stop" {
					if err := f.app.StopSession(f.caller.ID); err != nil {
						t.Fatalf("StopSession: %v", err)
					}
				} else if err := f.app.ArchiveThread(f.caller.ID); err != nil {
					t.Fatalf("ArchiveThread: %v", err)
				}

				requireGivenBack(t, f, token, tc.late, w)
				if end == "stop" {
					requireDueAtOnce(t, f.app, w)
				}
				requireDraft(t, f.app, f.caller.ID, "")
			})
		}
	}
}

// An answer or a completion whose echo the store recorded before the
// Stop's give-back ran stays delivered: the provider took it in, even
// though the session's pending entry was still there when the Stop read
// it.
func TestAStopKeepsAnAnswerOrCompletionTheProviderConfirmed(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := owedWakeSeeds()[0].seed(t, f)
	w := completedRemoteWatch(t, f.app, f.caller)
	queueOwnedAndPersonMessages(t, f, token, false, w, "build passed")
	dispatchUnconfirmed(t, f)
	f.app.confirmOwnedDeliveries(f.caller.ID, []string{threadWakeSendID(token), remoteCompletionSendID(w)})

	if err := f.app.StopSession(f.caller.ID); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	if row := f.request(t, token); row.DeliveredHow != store.ThreadWakeQueued || row.ConfirmedAt == 0 {
		t.Fatalf("a confirmed answer was given back: delivered as %q, confirmed at %d", row.DeliveredHow, row.ConfirmedAt)
	}
	if saved, err := f.app.store.GetRemoteWatch(w.ComputerID, w.RequestID); err != nil || saved.Notification != "queued" {
		t.Fatalf("a confirmed completion's watch = %+v %v, want queued", saved, err)
	}
}

// The provider's echo of a queued answer, late reply or reminder is the
// second phase of its delivery mark: before it, a restart gives the
// answer back; after it, nothing does.
func TestTheEchoConfirmsAnAnswersDelivery(t *testing.T) {
	t.Parallel()
	for _, tc := range owedWakeSeeds() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.app.startSessionFn = func(string) error { return nil }
			token := tc.seed(t, f)
			f.app.deliverThreadWake(token, tc.late)
			f.app.waitAgentNoticeWork()
			envelope := dispatchUnconfirmed(t, f)
			confirmedAt := func() int64 {
				row := f.request(t, token)
				if tc.late {
					return row.LateConfirmedAt
				}
				return row.ConfirmedAt
			}
			if confirmedAt() != 0 {
				t.Fatal("the answer was confirmed before its echo")
			}

			wireID, _ := envelope["uuid"].(string)
			text, _ := envelopeText(t, envelope)
			meta, _ := json.Marshal(map[string]any{"provider_item_id": wireID})
			if err := f.app.triage.Handle(provider.ProviderEvent{
				Kind: provider.EventUserText, ThreadID: f.caller.ID, Content: text,
				Meta: meta, Timestamp: time.Now(),
			}); err != nil {
				t.Fatalf("echo: %v", err)
			}
			if confirmedAt() == 0 {
				t.Fatal("the echo did not confirm the answer's delivery")
			}

			after := f.restart(t)
			after.app.restoreDurableFlushQueueAtBoot()
			if row := after.request(t, token); !threadRequestDelivered(row, tc.late) {
				t.Fatal("the boot sweep gave back an answer the provider confirmed")
			}
		})
	}
}

// A restart finds an answer, a late reply or a reminder and a completion
// that the last run handed the provider and never saw confirmed, with no
// queue row left: they go back to their owners, the completion due at once.
func TestTheBootSweepGivesBackWhatWasNeverConfirmed(t *testing.T) {
	t.Parallel()
	for _, tc := range owedWakeSeeds() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.app.startSessionFn = func(string) error { return nil }
			token := tc.seed(t, f)
			w := completedRemoteWatch(t, f.app, f.caller)
			queueOwnedAndPersonMessages(t, f, token, tc.late, w, "build passed")
			dispatchUnconfirmed(t, f)

			after := f.restart(t)
			after.app.restoreDurableFlushQueueAtBoot()

			requireGivenBack(t, after, token, tc.late, w)
			requireDueAtOnce(t, after.app, w)
		})
	}
}

// A completion a session end gave back waits on the thread request wake
// retry's ladder before the watcher delivers it again, and after the last
// attempt the watch gives it up with the reason and keeps the receipt. A
// Stop's give-back is due at once and counts nothing.
func TestACompletionASessionEndGaveBackWaitsOnTheLadder(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	w := completedRemoteWatch(t, f.app, f.caller)
	if err := f.app.queueAgentNotice(f.app.remoteCompletionNotice(w, remoteCompletionOutput{Tail: "done"})); err != nil {
		t.Fatalf("queueAgentNotice: %v", err)
	}
	f.app.waitAgentNoticeWork()
	dispatchUnconfirmed(t, f)
	sendID := remoteCompletionSendID(w)
	watch := func() store.RemoteWatch {
		t.Helper()
		saved, err := f.app.store.GetRemoteWatch(w.ComputerID, w.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	requireBooked := func(attempts int64, before time.Time) {
		t.Helper()
		saved := watch()
		earliest := before.Add(threadWakeRetryDelay(attempts)).UnixMilli()
		latest := time.Now().Add(threadWakeRetryDelay(attempts)).UnixMilli()
		if saved.Notification != "pending" || saved.WakeAttempts != attempts || saved.NextCheck < earliest || saved.NextCheck > latest {
			t.Fatalf("after %d session ends the watch is %q, due %d after %d; want pending, due in %v",
				attempts, saved.Notification, saved.NextCheck, saved.WakeAttempts, threadWakeRetryDelay(attempts))
		}
		due, err := f.app.store.ListRemoteWatches("", time.Now().UnixMilli(), 256)
		if err != nil || len(due) != 0 {
			t.Fatalf("the completion is due before its time: %+v %v", due, err)
		}
	}
	// The watcher queues a completion once it is due, observing the watch
	// first, which books its next poll.
	requeue := func() {
		t.Helper()
		if err := f.app.store.ObserveRemoteWatch(w.ComputerID, w.RequestID, w.Receipt, "", time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		item := store.FlushQueueItem{ID: uuid.NewString(), ThreadID: f.caller.ID, SendID: sendID, Message: "done", EnqueuedAt: time.Now().UnixMilli()}
		if err := f.app.store.QueueRemoteCompletion(w.ComputerID, w.RequestID, item); err != nil {
			t.Fatal(err)
		}
		if err := f.app.store.DeleteDispatchedFlushQueueItem(item.ID); err != nil {
			t.Fatal(err)
		}
	}

	// The first comes back through the session-death restore.
	before := time.Now()
	f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)
	requireBooked(1, before)

	for attempts := int64(2); attempts <= threadWakeRetryAttempts; attempts++ {
		requeue()
		before = time.Now()
		f.app.giveBackToOwners(f.caller.ID, []ownedMessage{{sendID: sendID, unconfirmed: true, ended: true}})
		requireBooked(attempts, before)
	}

	// A Stop is due at once and counts nothing.
	requeue()
	f.app.giveBackToOwners(f.caller.ID, []ownedMessage{{sendID: sendID, unconfirmed: true}})
	if saved := watch(); saved.Notification != "pending" || saved.WakeAttempts != threadWakeRetryAttempts || saved.NextCheck > time.Now().UnixMilli() {
		t.Fatalf("a Stop's give-back: %q due %d after %d", saved.Notification, saved.NextCheck, saved.WakeAttempts)
	}

	requeue()
	f.app.giveBackToOwners(f.caller.ID, []ownedMessage{{sendID: sendID, unconfirmed: true, ended: true}})
	saved := watch()
	if saved.Notification != "dismissed" || !strings.Contains(saved.WakeIssue, "9 times") || saved.Receipt.State != "succeeded" {
		t.Fatalf("after the ladder: %q issue %q receipt %+v; want dismissed with the reason and the receipt", saved.Notification, saved.WakeIssue, saved.Receipt)
	}
}

// The boot sweep of unconfirmed deliveries leaves a message whose queue row
// is still there to that row, which the restore gives back or, when that
// fails, keeps for the next boot: given back without its row, the owner
// would be delivered again with the row still queued.
func TestTheBootSweepOfUnconfirmedDeliveriesLeavesQueuedRows(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	token := owedWakeSeeds()[0].seed(t, f)
	w := completedRemoteWatch(t, f.app, f.caller)
	queueOwnedAndPersonMessages(t, f, token, false, w, "build passed")

	after := f.restart(t)
	after.app.giveBackUnconfirmedAtBoot()

	if row := after.request(t, token); row.DeliveredHow != store.ThreadWakeQueued {
		t.Fatalf("the answer with a queued row was given back: delivered as %q", row.DeliveredHow)
	}
	if saved, err := after.app.store.GetRemoteWatch(w.ComputerID, w.RequestID); err != nil || saved.Notification != "queued" {
		t.Fatalf("the completion with a queued row was given back: %+v %v", saved, err)
	}
	if durable, _ := queuedSendIDs(t, after.app, f.caller.ID); len(durable) != 3 {
		t.Fatalf("queued rows %q, want all three kept", durable)
	}
}

// queueRequestFromAnotherComputer accepts a request a thread on another
// computer made of the caller and queues its message there, as
// dispatchRequest queues one for a busy thread. It returns the token and
// the message.
func queueRequestFromAnotherComputer(t *testing.T, f *requestFixture) (string, string) {
	t.Helper()
	token := uuid.NewString()
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "device-desk", SourceComputerID: "c-desk", SourceComputerName: "desk",
		SourceThreadID: "t-lead", SourceThreadTitle: "Lead", Kind: store.ThreadRequestSend, TargetThreadID: f.caller.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	body := "Fix the nil check.\n\n---\nAgent request from thread \"Lead\" (t-lead, on desk).\nIt is not waiting."
	if err := f.app.threadTools().requeueRequest(token, f.caller.ID, body); err != nil {
		t.Fatal(err)
	}
	return token, body
}

// A session that ends before the agent took in a request Claude joined with
// the person's message gives the person their text alone and settles the
// request with why: nothing will deliver it now.
func TestASessionDeathSettlesAJoinedRequestAndGivesThePersonTheirText(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	if _, err := f.app.RegisterQueueItem(context.Background(), f.caller.ID, "my own words", SendMessageOptions{SendID: "person-send"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	token, _ := queueRequestFromAnotherComputer(t, f)
	dispatchUnconfirmed(t, f)
	waitUntil(t, 10*time.Second, func() bool { return f.receipt(t, token).State == store.ThreadReceiptRunning })

	f.app.restoreUnconfirmedQueueOnSessionDeath(f.caller.ID)

	requireDraft(t, f.app, f.caller.ID, "my own words")
	waitUntil(t, 10*time.Second, func() bool { return f.receipt(t, token).State == store.ThreadReceiptInterrupted })
	if receipt := f.receipt(t, token); string(receipt.Answer) != threadtools.InterruptSessionEndedUnread {
		t.Fatalf("receipt answer = %q, want the session's end before it read the request", receipt.Answer)
	}
	if rows := durableQueueRows(t, f.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("queue = %+v, want it empty", rows)
	}
}

// A request still queued at quit is not the person's to send: the boot
// restore leaves it out of the composer, and the boot sweep tells its
// sender the request was interrupted by the restart.
func TestTheBootRestoreLeavesAQueuedRequestOutOfTheComposer(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.startSessionFn = func(string) error { return nil }
	if _, err := f.app.RegisterQueueItem(context.Background(), f.caller.ID, "my own words", SendMessageOptions{SendID: "person-send"}); err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	token, _ := queueRequestFromAnotherComputer(t, f)

	after := f.restart(t)
	after.app.restoreDurableFlushQueueAtBoot()
	after.app.sweepThreadRequestsAtBoot()

	requireDraft(t, after.app, f.caller.ID, "my own words")
	if rows := durableQueueRows(t, after.app, f.caller.ID); len(rows) != 0 {
		t.Fatalf("queue = %+v, want it empty", rows)
	}
	if receipt := after.receipt(t, token); receipt.State != store.ThreadReceiptInterrupted {
		t.Fatalf("receipt = %q, want interrupted by the restart", receipt.State)
	}
}
