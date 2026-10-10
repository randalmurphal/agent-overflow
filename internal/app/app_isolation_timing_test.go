package app

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestIsolationTimingReachesThreadPollAndTransferJobs: the intervals a
// harness boot shortens through ConfigureIsolation are the ones the request
// poller schedules with and the transfer scheduler retries with. A parked
// call never polls slower than the shortened cadence.
func TestIsolationTimingReachesThreadPollAndTransferJobs(t *testing.T) {
	t.Parallel()
	const poll = 300 * time.Millisecond
	const retry = 40 * time.Millisecond
	f := newRequestFixture(t)
	const prRetry = 25 * time.Millisecond
	const ciLive, ciFollow = 70 * time.Millisecond, 35 * time.Millisecond
	ConfigureIsolation(f.app, IsolationConfig{
		ThreadRequestPoll: poll, TransferPendingRetry: retry,
		PRUpdateRetryBase: prRetry, PRCILiveInterval: ciLive, PRCIFollowInterval: ciFollow,
	})
	if got := f.app.prUpdateRetryBase(); got != prRetry {
		t.Fatalf("pr update retry base = %s, want %s", got, prRetry)
	}
	if got := f.app.prCILiveInterval(); got != ciLive {
		t.Fatalf("pr ci live interval = %s, want %s", got, ciLive)
	}
	if got := f.app.prCIFollowInterval(); got != ciFollow {
		t.Fatalf("pr ci follow interval = %s, want %s", got, ciFollow)
	}
	computer := uuid.NewString()

	open := f.seedRemoteRequest(t, computer)
	if delay := f.scheduleDelay(t, open); delay <= 0 || delay > poll+200*time.Millisecond {
		t.Fatalf("open request rescheduled in %s, want about %s", delay, poll)
	}
	_, _, end := f.app.beginWait(context.Background(), threadRequestWait{}, []string{open})
	if delay := f.app.threadPollDelay(open); delay != poll {
		t.Fatalf("parked request cadence = %s, want the shortened %s", delay, poll)
	}
	end()

	settled := f.seedRemoteRequest(t, computer)
	now := time.Now()
	f.settleRemote(t, settled, now, now.Add(threadLateReplyWindow))
	if delay := f.scheduleDelay(t, settled); delay <= poll || delay > 2*poll+200*time.Millisecond {
		t.Fatalf("settled request rescheduled in %s, want about %s", delay, 2*poll)
	}

	if err := f.app.startThreadTransfers(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.app.transfers.close)
	if got := f.app.transfers.live.Load().jobs.PendingRetry(); got != retry {
		t.Fatalf("transfer pending retry = %s, want %s", got, retry)
	}
}
