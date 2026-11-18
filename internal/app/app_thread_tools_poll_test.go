package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"github.com/google/uuid"
)

// seedRemoteRequest writes a source row for a request answered on another
// computer, which is the only kind the poller visits.
func (f *requestFixture) seedRemoteRequest(t *testing.T, computerID string) string {
	t.Helper()
	token := uuid.NewString()
	pairing, err := f.app.currentPairing(computerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token:            token,
		CallerThreadID:   f.caller.ID,
		Kind:             store.ThreadRequestSend,
		State:            store.ThreadRequestUnconfirmed,
		TargetComputerID: computerID,
		Pairing:          pairing,
		TargetThreadID:   uuid.NewString(),
	}); err != nil {
		t.Fatalf("insert remote request: %v", err)
	}
	return token
}

// settleRemote settles one seeded row the way a destination's receipt does.
func (f *requestFixture) settleRemote(t *testing.T, token string, settledAt, expiresAt time.Time) {
	t.Helper()
	settled, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State:      store.ThreadRequestFinished,
		Answer:     []byte("the build is green"),
		AnswerKind: store.ThreadAnswerFinal,
		SettledAt:  settledAt.UnixMilli(),
		ExpiresAt:  expiresAt.UnixMilli(),
	})
	if err != nil || !settled {
		t.Fatalf("settle %s: settled=%v err=%v", token, settled, err)
	}
}

// scheduledDelay is how far out one rescheduling decision put a row: the
// delay it chose lies in [min, max]. The decision reads the clock somewhere
// inside the call, so time the call spends waiting on the store widens the
// window instead of shifting it.
type scheduledDelay struct {
	polled   bool
	min, max time.Duration
}

// is reports whether the decision chose want.
func (d scheduledDelay) is(want time.Duration) bool {
	return d.polled && d.min <= want && want <= d.max
}

func (d scheduledDelay) String() string {
	if !d.polled {
		return "retired"
	}
	return fmt.Sprintf("[%s, %s]", d.min, d.max)
}

// scheduleDelay runs one poll's rescheduling decision and reports how far
// out it put the row.
func (f *requestFixture) scheduleDelay(t *testing.T, token string) scheduledDelay {
	t.Helper()
	current := f.request(t, token)
	before := time.Now()
	f.app.scheduleNextThreadPoll(current)
	after := time.Now()
	row := f.request(t, token)
	if !row.Polling {
		return scheduledDelay{}
	}
	// next_check is the decision's clock plus its delay, truncated to the
	// millisecond.
	next := time.UnixMilli(row.NextCheck)
	return scheduledDelay{
		polled: true,
		min:    next.Sub(after) - time.Millisecond,
		max:    next.Sub(before) + time.Millisecond,
	}
}

// A settled request is polled for one thing only, a late thread_reply, and
// the destination holds one for a day. Until this bound existed every
// settled remote request cost a call every five seconds for the thirty days
// of its retention. The cadence now doubles towards its ceiling and the row
// leaves the poll for good once there is nothing left to ask for.
func TestSettledRemotePollBacksOffThenRetires(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	computer := uuid.NewString()

	t.Run("an open request keeps its caller's cadence", func(t *testing.T) {
		token := f.seedRemoteRequest(t, computer)
		for range 3 {
			if delay := f.scheduleDelay(t, token); !delay.is(threadPollNormalDelay) {
				t.Fatalf("open request rescheduled in %s, want %s", delay, threadPollNormalDelay)
			}
		}
		if !f.request(t, token).Polling {
			t.Fatal("an open request left the poll")
		}
	})

	t.Run("a settled request backs off towards the ceiling", func(t *testing.T) {
		token := f.seedRemoteRequest(t, computer)
		now := time.Now()
		f.settleRemote(t, token, now, now.Add(threadLateReplyWindow))

		previous := threadPollNormalDelay
		for pass := range 4 {
			delay := f.scheduleDelay(t, token)
			if !delay.polled || delay.min <= previous {
				t.Fatalf("pass %d rescheduled in %s, want longer than the previous %s", pass, delay, previous)
			}
			if delay.min > threadPollLateCap {
				t.Fatalf("pass %d rescheduled in %s, past the %s ceiling", pass, delay, threadPollLateCap)
			}
			previous = delay.max
		}
		// The ceiling holds however long the destination stays quiet.
		for range 20 {
			f.app.scheduleNextThreadPoll(f.request(t, token))
		}
		if delay := f.scheduleDelay(t, token); !delay.is(threadPollLateCap) {
			t.Fatalf("settled cadence = %s, want the %s ceiling", delay, threadPollLateCap)
		}
	})

	t.Run("the late reply window ends the poll", func(t *testing.T) {
		token := f.seedRemoteRequest(t, computer)
		now := time.Now()
		f.settleRemote(t, token, now.Add(-48*time.Hour), now.Add(-time.Minute))

		f.app.scheduleNextThreadPoll(f.request(t, token))
		if f.request(t, token).Polling {
			t.Fatal("a request whose answer hold ran out is still polled")
		}
		// Retirement is a column the due query excludes, so the row is gone
		// from the poll across a restart rather than only for this process.
		after := f.restart(t)
		polls, err := after.app.store.DueThreadRequestPolls(time.Now().Add(time.Hour).UnixMilli(), 64)
		if err != nil {
			t.Fatalf("due polls: %v", err)
		}
		for _, poll := range polls {
			if poll.Token == token {
				t.Fatal("a retired request came back with the boot pass")
			}
		}
	})

	t.Run("a collected late reply ends the poll", func(t *testing.T) {
		token := f.seedRemoteRequest(t, computer)
		now := time.Now()
		f.settleRemote(t, token, now, now.Add(threadLateReplyWindow))
		if delay := f.scheduleDelay(t, token); !delay.polled {
			t.Fatal("a request still awaiting a late reply was retired")
		}
		stored, err := f.app.store.StoreThreadRequestLateReply(token, []byte("one more thing"), now.UnixMilli(), nil, false)
		if err != nil || !stored {
			t.Fatalf("store late reply: stored=%v err=%v", stored, err)
		}
		f.app.scheduleNextThreadPoll(f.request(t, token))
		if f.request(t, token).Polling {
			t.Fatal("a request whose late reply arrived is still polled")
		}
	})
}

// A forwarded call names a thread on the computer it came from, so the
// ledger it would read here is not that thread's. The token and listing
// branches of thread_status are refused rather than answered with whatever
// local rows the sender's thread id happens to match.
func TestForwardedCallCannotReadThisComputersRequestLedger(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	adapter := f.adapter()
	caller := f.callerIdentity()
	forwarded := threadtools.WithForwarded(t.Context())

	if _, err := adapter.RequestStates(forwarded, caller, threadtools.StatusCall{Tokens: []string{uuid.NewString()}}); !isThreadToolCode(err, threadtools.CodeInvalidRequest) {
		t.Fatalf("forwarded token read = %v, want a refusal", err)
	}
	if _, err := adapter.ListRequests(forwarded, caller, threadtools.ListCall{}); !isThreadToolCode(err, threadtools.CodeInvalidRequest) {
		t.Fatalf("forwarded listing = %v, want a refusal", err)
	}
	if _, err := adapter.ExportAnswer(forwarded, caller, uuid.NewString()); !isThreadToolCode(err, threadtools.CodeInvalidRequest) {
		t.Fatalf("forwarded export = %v, want a refusal", err)
	}
	// The thread_ids branch is the one that is legitimately forwarded: it
	// reads threads on this computer, not the caller's ledger.
	if _, err := adapter.RequestStates(forwarded, caller, threadtools.StatusCall{ThreadIDs: []string{f.caller.ID}}); err != nil {
		t.Fatalf("forwarded thread watch = %v, want it answered", err)
	}
	// The same calls from a local thread are the ordinary ones.
	if _, err := adapter.ListRequests(t.Context(), caller, threadtools.ListCall{}); err != nil {
		t.Fatalf("local listing = %v, want it answered", err)
	}
}

// Every id a forwarded request writes into a receipt, and renders into the
// trusted "Agent request" footer of a user message, is one this app mints.
// A peer that sends something else is refused rather than trusted.
func TestForwardedRequestRefusesIdsThisAppDidNotMint(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	owner := uuid.NewString()
	good := threadtools.Caller{ThreadID: uuid.NewString(), ComputerID: uuid.NewString()}

	for _, sample := range []struct {
		name string
		call ThreadPeerCall
	}{
		{"a token that is not a minted id", ThreadPeerCall{Tool: "thread_send", Token: "../../etc/passwd", Source: good}},
		{"no token at all", ThreadPeerCall{Tool: "thread_send", Token: "", Source: good}},
		{"a source thread that is not a minted id", ThreadPeerCall{Tool: "thread_send", Token: uuid.NewString(),
			Source: threadtools.Caller{ThreadID: "the other one", ComputerID: good.ComputerID}}},
		{"a source computer that is not a minted id", ThreadPeerCall{Tool: "thread_send", Token: uuid.NewString(),
			Source: threadtools.Caller{ThreadID: good.ThreadID, ComputerID: "laptop"}}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			_, err := f.app.runThreadPeerRequest(t.Context(), owner, sample.call)
			if !isThreadToolCode(err, threadtools.CodeInvalidRequest) {
				t.Fatalf("refusal = %v, want %s", err, threadtools.CodeInvalidRequest)
			}
			rows, listErr := f.app.store.ListThreadRequestsByCaller(f.caller.ID, 64, 0)
			if listErr != nil {
				t.Fatalf("list: %v", listErr)
			}
			if len(rows) != 0 {
				t.Fatalf("a refused forwarded call left rows behind: %+v", rows)
			}
		})
	}
}

// A title another computer chose is rendered into this computer's message
// footer, so its length and its line count are this computer's business.
func TestForwardedTitleIsClippedToThisComputersBound(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("e", threadtools.MaxTitleRunes+50)
	if got := threadPeerTitle(long); len([]rune(got)) != threadtools.MaxTitleRunes {
		t.Fatalf("clipped title is %d runes, want %d", len([]rune(got)), threadtools.MaxTitleRunes)
	}
	if got := threadPeerTitle("first line\nsecond line\r\n\tthird"); got != "first line second line third" {
		t.Fatalf("collapsed title = %q", got)
	}
	// A multi-byte title is clipped on runes, never in the middle of one.
	wide := strings.Repeat("é", threadtools.MaxTitleRunes+10)
	clipped := threadPeerTitle(wide)
	if len([]rune(clipped)) != threadtools.MaxTitleRunes || !strings.HasPrefix(wide, clipped) {
		t.Fatalf("multi-byte title clipped to %q", clipped)
	}
}

// isThreadToolCode reports whether an error is the public refusal carrying
// one of thread tools' own codes.
func isThreadToolCode(err error, code string) bool {
	if err == nil {
		return false
	}
	got, _, public := errorsx.PublicDetails(err)
	return public && got == code
}

// A wake the caller thread cannot take was retried every thirty seconds for
// the life of the row. Nothing about the caller's state changes because a
// delivery was attempted again, so the retry now backs off, ends, and leaves
// the reason on the row where thread_status reads it.
func TestUndeliverableWakeBacksOffAndIsAbandoned(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	token := f.seedRemoteRequest(t, uuid.NewString())
	if _, err := f.app.store.SetThreadRequestNotify(token, true); err != nil {
		t.Fatalf("arm the wake: %v", err)
	}
	now := time.Now()
	f.settleRemote(t, token, now, now.Add(threadLateReplyWindow))
	// An outgoing move fences the caller thread: it cannot take a message
	// until the transfer settles, which no retry of this wake affects.
	digest := sha256.Sum256([]byte(f.caller.ID))
	if _, err := f.app.store.CreateThreadTransfer(store.ThreadTransfer{
		ID: uuid.NewString(), ThreadID: f.caller.ID, PeerBackendID: uuid.NewString(),
		Kind: "move", Direction: "outgoing", ActivationHash: hex.EncodeToString(digest[:]),
		PrivateState: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("fence the caller thread: %v", err)
	}

	var delays []time.Duration
	for pass := range threadWakeRetryAttempts {
		at := now.Add(time.Duration(pass) * time.Hour)
		f.app.retryUndeliveredThreadWakes(at)
		row := f.request(t, token)
		if row.WakeAttempts != int64(pass)+1 {
			t.Fatalf("pass %d left %d attempts", pass, row.WakeAttempts)
		}
		delays = append(delays, time.UnixMilli(row.WakeNextCheck).Sub(at))
	}
	// The booked moment is stored in milliseconds, so compare within a tick.
	if (delays[0] - threadWakeRetryBase).Abs() > time.Second {
		t.Fatalf("first retry booked at %s, want %s", delays[0], threadWakeRetryBase)
	}
	for i := 1; i < len(delays); i++ {
		if delays[i] < delays[i-1] || delays[i] > threadWakeRetryCap+time.Second {
			t.Fatalf("retry delays did not back off within the ceiling: %v", delays)
		}
	}
	if (delays[len(delays)-1] - threadWakeRetryCap).Abs() > time.Second {
		t.Fatalf("last retry booked at %s, want the %s ceiling", delays[len(delays)-1], threadWakeRetryCap)
	}

	// Every attempt is spent. The next pass gives up rather than retrying
	// for the thirty days the row is kept.
	waitUntil(t, 10*time.Second, func() bool {
		return strings.Contains(f.request(t, token).WakeIssue, "deliver")
	})
	f.app.retryUndeliveredThreadWakes(now.Add(threadWakeRetryAttempts * time.Hour))
	row := f.request(t, token)
	if row.Notify {
		t.Fatal("an abandoned wake is still armed")
	}
	if row.WakeIssue == "" {
		t.Fatal("an abandoned wake recorded no reason")
	}
	// The answer itself is not lost with the message.
	if !strings.Contains(string(row.Answer), "build is green") {
		t.Fatalf("abandoning the wake lost the answer: %+v", row)
	}
	// thread_status returns the answer with the reason it never arrived,
	// once the last detached delivery has failed.
	waitUntil(t, 10*time.Second, func() bool {
		unlock := f.app.threadRequestSettleLock(token)
		defer unlock()
		return !f.app.threadWakeInFlight(token, false)
	})
	answer, err := f.app.threadToolsServer().Call(t.Context(), f.callerIdentity(), "thread_status",
		json.RawMessage(`{"tokens":["`+token+`"]}`))
	if err != nil {
		t.Fatalf("thread_status: %v", err)
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Requests []struct {
			Answer    string `json:"answer"`
			WakeIssue string `json:"wake_issue"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(encoded, &status); err != nil {
		t.Fatalf("decode thread_status reply %s: %v", encoded, err)
	}
	if len(status.Requests) != 1 || status.Requests[0].WakeIssue != row.WakeIssue ||
		!strings.Contains(status.Requests[0].Answer, "build is green") {
		t.Fatalf("thread_status = %s, want the answer and the wake issue %q", encoded, row.WakeIssue)
	}
	// And the row is out of the undelivered set for good.
	rows, err := f.app.store.UndeliveredThreadRequestWakes(now.Add(30*24*time.Hour).UnixMilli(), 64)
	if err != nil {
		t.Fatalf("undelivered wakes: %v", err)
	}
	for _, left := range rows {
		if left.Token == token {
			t.Fatal("an abandoned wake stayed in the retry set")
		}
	}
}

// A thread on another computer has no row here to read its name from, so a
// wake rendered after a restart named nothing at all. The name its own
// computer reported is recorded with the target.
func TestRemoteTargetTitleSurvivesARestart(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	computer := uuid.NewString()
	token := f.seedRemoteRequest(t, computer)
	target := f.request(t, token).TargetThreadID
	if _, err := f.app.store.SetThreadRequestTarget(token, computer, target, "Release checklist", ""); err != nil {
		t.Fatalf("record the target: %v", err)
	}

	after := f.restart(t)
	origin := soloWakeOrigin(t, after.app, after.request(t, token))
	if origin == nil || origin.Title != "Release checklist" || origin.ThreadID != target {
		t.Fatalf("wake origin after a restart = %+v", origin)
	}
}

// Archiving the caller disarms notify for its open requests, so only a
// request re-armed by a later wait or thread_status can still owe an
// archived thread a message. When one does, the answer arriving is exactly
// the reason to bring the thread back: a wake queued behind a row the
// sidebar does not show would sit unread.
func TestWakeUnarchivesTheCallerItIsFor(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")
	f.holdCallerTurn(t)
	token := f.seedRemoteRequest(t, uuid.NewString())
	now := time.Now()
	f.settleRemote(t, token, now, now.Add(threadLateReplyWindow))
	if err := archiveThread(f.app, f.caller.ID); err != nil {
		t.Fatalf("archive the caller: %v", err)
	}
	// The archive disarmed the request. A later wait or thread_status on it
	// arms it again, and that is the wake that brings the thread back.
	if armed, err := f.app.store.SetThreadRequestNotify(token, true); err != nil || !armed {
		t.Fatalf("re-arm the request: armed=%v err=%v", armed, err)
	}

	f.app.deliverThreadWake(token, false)

	thread, err := f.app.store.GetThread(f.caller.ID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Archived {
		t.Fatal("the answer was queued behind an archived row")
	}
	wake := awaitQueuedWake(t, f.app, f.caller.ID, threadWakeSendID(token))
	if !strings.Contains(wake.Message, "build is green") {
		t.Fatalf("wake message = %q", wake.Message)
	}
}

// The sweep sleeps on the ledger's own schedule. A poll in flight owns the
// rows it read, so their due moment is left out of that schedule, but the
// reminders and wake retries beside them are not: a reminder due during a
// slow poll is served on its own clock, not the poll's timeout.
func TestSweepScheduleKeepsRemindersWhileAPollIsInFlight(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	remote := f.seedRemoteRequest(t, uuid.NewString())
	if err := f.app.store.RescheduleThreadRequest(remote, time.Now().Add(time.Minute).UnixMilli(), 0, ""); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	// The moments are far enough apart that a loaded host's pauses between
	// the steps do not change which one each delay is.
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: uuid.NewString(), CallerThreadID: f.caller.ID, Kind: store.ThreadRequestRemind,
		State: store.ThreadRequestAccepted, DueAt: time.Now().Add(30 * time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}
	lastExpiry := time.Now()

	if delay := f.app.threadRequestSweepDelay(lastExpiry); delay > 30*time.Second {
		t.Fatalf("delay = %v, want the reminder's thirty seconds", delay)
	}
	if !f.app.beginThreadRequestPoll() {
		t.Fatal("the poll slot was taken")
	}
	defer f.app.endThreadRequestPoll()
	if delay := f.app.threadRequestSweepDelay(lastExpiry); delay > 30*time.Second {
		t.Fatalf("delay with a poll in flight = %v, want the reminder still served", delay)
	}
	if err := f.app.store.RescheduleThreadRequest(remote, time.Now().Add(time.Second).UnixMilli(), 0, ""); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	if delay := f.app.threadRequestSweepDelay(lastExpiry); delay < 10*time.Second {
		t.Fatalf("delay = %v, want the in-flight poll's rows left out", delay)
	}
}
