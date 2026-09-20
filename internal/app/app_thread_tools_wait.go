package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"agent-overflow/internal/keyedlock"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// The parking half of the agent thread tools: one registry of tool calls
// waiting on something to happen elsewhere, and the per-token lock that makes
// a settlement and a timeout mutually exclusive.
//
// The registry holds both kinds of parked call this app has, because they
// want the same thing of it: a tool call parked on another thread's request
// and one parked on a remote command both have to be found by what they are
// waiting for (the delivery decision asks whether anyone is parked, since a
// reply that returns the answer IS the delivery) and ended by the interrupt
// of the turn that made them.
//
// A registration carries several KEYS, because `thread_status` waits on a
// list of tokens or threads and returns on the first of them, and a channel
// rather than only a cancel func, because the wait has to know WHICH key
// woke it in order to report `woke_on`. A remote command's wait uses one key
// and reads neither, which costs it nothing.

// threadRequestState is the in-process half of the request ledger.
type threadRequestState struct {
	mu   sync.Mutex
	next uint64
	// waits is every parked call, by registration id.
	waits map[uint64]threadRequestWait
	// running maps a target thread to the receipt tokens whose turn a turn
	// end on that thread could settle. It is the settlement observer's
	// gate: a thread with no entry here costs one map read per turn end
	// and no query. Written only where a receipt enters and leaves
	// `running`, so it cannot claim a receipt the store does not have.
	running map[string]map[string]struct{}
	// settleLocks serializes everything that can settle or deliver one
	// token, so a settlement and a timing-out wait never both deliver the
	// answer and never neither.
	settleOnce  sync.Once
	settleLocks *keyedlock.Registry
	// observerOnce guards the one global turn observer that settles
	// receipts. It is a process-wide subscription, not a per-thread one:
	// a request's target is any thread on this computer.
	observerOnce sync.Once
	// nudge asks the unattended sweep for a pass now. Nil until the sweep
	// is started, which is what makes a nudge from a test with no sweep a
	// no-op rather than a leak.
	nudge chan struct{}
	// polling is the single poll slot. A pass whose rows are still in
	// flight has not rescheduled them, so a second pass would ask the same
	// destination about the same tokens.
	polling bool
	// deliveries is how many wakes the sweep is handing over right now,
	// bounded by threadWakeDeliveryFanOut.
	deliveries int
	// blocked records which remote requests their own computer last
	// reported as waiting on a person. It is not state: the record is the
	// source row, this is the live reading beside it, and after a restart
	// it is empty until the next poll fills it in.
	blocked map[string]bool
}

// threadRequestWait is one parked tool call.
type threadRequestWait struct {
	// threadID is the CALLER's thread, so an interrupt of that thread ends
	// the call it was blocking.
	threadID string
	// remote marks a call parked on a remote command rather than on a
	// thread request. The two are ended by different operations, so the
	// registry keeps them apart.
	remote bool
	// keys are the tokens (and `thread:<id>` watches, and one remote job)
	// this call is parked on. Any of them ends it.
	keys   map[string]struct{}
	cancel context.CancelCauseFunc
	// woke carries the key that ended the wait. Buffered so a broadcaster
	// never blocks on a waiter that has already been woken by another key.
	woke chan string
}

// errThreadRequestWaitInterrupted marks a wait this app ended because the
// caller's turn was interrupted, as opposed to one the clock ended. The work
// itself keeps running; only the call stops waiting for it.
var errThreadRequestWaitInterrupted = errors.New("thread request wait interrupted")

func (a *App) threadRequestSettleLock(token string) func() {
	a.threadRequests.settleOnce.Do(func() {
		a.threadRequests.settleLocks = keyedlock.New()
	})
	return a.threadRequests.settleLocks.Lock(token)
}

// beginRequestWait registers one parked call on every key it waits for and
// returns the context it parks under, the channel the settlement arrives on,
// and the release.
//
// The release runs AFTER the reply is written, never before: between the
// terminal observation and the response the collector must still see the
// wait as active, or it queues a wake for an answer the model is about to
// read in the reply.
func (a *App) beginRequestWait(ctx context.Context, threadID string, keys []string) (context.Context, <-chan string, func()) {
	return a.beginWait(ctx, threadRequestWait{threadID: threadID}, keys)
}

// beginRemoteWait parks one tool call on a remote command. The returned end
// function releases the registration; callers invoke it after the reply is
// written, not before, so the completion watcher cannot slip a duplicate in
// between the terminal observation and the response.
func (a *App) beginRemoteWait(ctx context.Context, threadID, computerID, requestID string) (context.Context, func()) {
	waitCtx, _, end := a.beginWait(ctx, threadRequestWait{threadID: threadID, remote: true},
		[]string{remoteJobKey(computerID, requestID)})
	return waitCtx, end
}

// remoteJobKey names one remote command in the wait registry.
func remoteJobKey(computerID, requestID string) string {
	return "remote-job:" + computerID + ":" + requestID
}

// remoteWaitActive reports whether a tool call is parked on one remote
// command. While it is, the completion watcher leaves that job alone: the
// reply the wait produces is the delivery, and a completion message on top
// of it would hand the agent the same result twice.
func (a *App) remoteWaitActive(computerID, requestID string) bool {
	return a.requestWaitActive(remoteJobKey(computerID, requestID))
}

func (a *App) beginWait(ctx context.Context, wait threadRequestWait, keys []string) (context.Context, <-chan string, func()) {
	waitCtx, cancel := context.WithCancelCause(ctx)
	wait.keys = make(map[string]struct{}, len(keys))
	wait.cancel = cancel
	wait.woke = make(chan string, 1)
	for _, key := range keys {
		wait.keys[key] = struct{}{}
	}
	a.threadRequests.mu.Lock()
	if a.threadRequests.waits == nil {
		a.threadRequests.waits = make(map[uint64]threadRequestWait)
	}
	a.threadRequests.next++
	id := a.threadRequests.next
	a.threadRequests.waits[id] = wait
	a.threadRequests.mu.Unlock()

	return waitCtx, wait.woke, func() {
		a.threadRequests.mu.Lock()
		delete(a.threadRequests.waits, id)
		a.threadRequests.mu.Unlock()
		cancel(nil)
	}
}

// requestWaitActive reports whether a call is parked on this key. The
// collector checks it under the key's settle lock before queuing a wake, and
// the remote completion watcher checks it before queuing a completion: the
// reply the wait produces IS the delivery.
func (a *App) requestWaitActive(key string) bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	for _, wait := range a.threadRequests.waits {
		if _, ok := wait.keys[key]; ok {
			return true
		}
	}
	return false
}

// wakeRequestWaits tells every call parked on this key that it has something
// to report. The send is non-blocking: a waiter already holding a wake needs
// no second one, and the broadcaster runs on a settlement path that must not
// block on a slow reader.
func (a *App) wakeRequestWaits(key string) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	for _, wait := range a.threadRequests.waits {
		if _, ok := wait.keys[key]; !ok {
			continue
		}
		select {
		case wait.woke <- key:
		default:
		}
	}
}

// cancelThreadRequestWaits ends every call the given thread has parked on a
// thread request. The requests keep running; only the calls stop waiting.
func (a *App) cancelThreadRequestWaits(threadID string) {
	a.cancelWaits(threadID, false)
}

// cancelRemoteWaits ends every call the given thread has parked on a remote
// command. The commands keep running; only the tool calls stop waiting for
// them, and the parked call returns a backgrounded receipt at once.
//
// The two cancels are separate because the operations behind them are:
// revoking a thread's remote access ends its remote waits and nothing else,
// and cancelling its requests ends its request waits and nothing else.
func (a *App) cancelRemoteWaits(threadID string) {
	a.cancelWaits(threadID, true)
}

func (a *App) cancelWaits(threadID string, remote bool) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	for _, wait := range a.threadRequests.waits {
		if wait.threadID == threadID && wait.remote == remote {
			wait.cancel(errThreadRequestWaitInterrupted)
		}
	}
}

// noteReceiptRunning and forgetReceiptRunning maintain the settlement
// observer's gate. Both are idempotent.
func (a *App) noteReceiptRunning(threadID, token string) {
	if threadID == "" || token == "" {
		return
	}
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	if a.threadRequests.running == nil {
		a.threadRequests.running = make(map[string]map[string]struct{})
	}
	tokens := a.threadRequests.running[threadID]
	if tokens == nil {
		tokens = make(map[string]struct{})
		a.threadRequests.running[threadID] = tokens
	}
	tokens[token] = struct{}{}
}

func (a *App) forgetReceiptRunning(threadID, token string) {
	if threadID == "" {
		return
	}
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	tokens := a.threadRequests.running[threadID]
	if tokens == nil {
		return
	}
	delete(tokens, token)
	if len(tokens) == 0 {
		delete(a.threadRequests.running, threadID)
	}
}

// runningReceiptTokens returns the tokens a turn end on this thread could
// settle, and false when there are none.
func (a *App) runningReceiptTokens(threadID string) ([]string, bool) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	tokens := a.threadRequests.running[threadID]
	if len(tokens) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(tokens))
	for token := range tokens {
		out = append(out, token)
	}
	return out, true
}

// waitRequestOutcome is what one parked wait ended with.
type waitRequestOutcome struct {
	// WokeOn names the key that ended the wait, empty on a timeout or an
	// interrupt.
	WokeOn string
	// TimedOut is true when the clock ended it. An interrupt is neither
	// woken nor timed out: the caller's turn is going away and the reply
	// says the work continues.
	TimedOut bool
}

// waitRequest parks until one of the keys settles, the caller's turn is
// interrupted, or the time runs out.
//
// It does NOT decide what a settlement is: the caller passes `check`, which
// runs once at entry and again on every wake, and returns the key it is
// willing to end on. That is what lets `thread_status` end on a settlement
// past a revision, on a thread coming to rest, and on a target that became
// blocked on a person, without this function knowing any of the three.
func (a *App) waitRequest(
	ctx context.Context, threadID string, keys []string, seconds int,
	check func() (string, bool, error),
) (waitRequestOutcome, error) {
	woke, done, err := check()
	if err != nil || done {
		return waitRequestOutcome{WokeOn: woke}, err
	}
	if seconds <= 0 {
		return waitRequestOutcome{TimedOut: true}, nil
	}
	waitCtx, wake, end := a.beginRequestWait(ctx, threadID, keys)
	defer end()

	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	// A target's live state changes without any settlement: a turn
	// starting, an approval appearing. A settlement arrives on the wake
	// channel, so this covers only what no settlement produces, and it
	// backs off, because a person answering an approval is the fastest
	// thing it is watching for.
	watch := time.NewTimer(threadRequestWatchInterval)
	defer watch.Stop()
	period := threadRequestWatchInterval
	for {
		select {
		case <-waitCtx.Done():
			// An interrupt of the caller's turn and a cancelled tool call
			// are the same answer: stop waiting, report the work as still
			// running. The request itself is untouched.
			if woke, done, err := check(); err != nil || done {
				return waitRequestOutcome{WokeOn: woke}, err
			}
			return waitRequestOutcome{}, nil
		case <-timer.C:
			if woke, done, err := check(); err != nil || done {
				return waitRequestOutcome{WokeOn: woke}, err
			}
			return waitRequestOutcome{TimedOut: true}, nil
		case <-wake:
		case <-watch.C:
			if period < threadRequestWatchCap {
				period *= 2
			}
			watch.Reset(period)
		}
		if woke, done, err := check(); err != nil || done {
			return waitRequestOutcome{WokeOn: woke}, err
		}
	}
}

// threadRequestWatchInterval is how soon a parked wait first re-reads the
// live state it is watching, and threadRequestWatchCap is the longest it
// waits between those reads.
const (
	threadRequestWatchInterval = time.Second
	threadRequestWatchCap      = 5 * time.Second
)

// threadRequestSettled reports whether a source row has reached a state that
// ends a wait.
func threadRequestSettled(row store.ThreadRequest) bool {
	switch row.State {
	case store.ThreadRequestUnconfirmed, store.ThreadRequestAccepted, store.ThreadRequestRunning:
		return false
	default:
		return true
	}
}

// noteRemoteRequestBlocked records whether a destination last reported its
// target as waiting on a person. It is the only live reading a remote
// request has: everything else the destination says about it is written to
// the row. forgetRemoteRequestBlocked drops it once the request settles,
// after which nothing is blocked on anyone.
//
// A target that has just become blocked ends the waits parked on it, the way
// a settlement does: the reading changed, and the wait's own clock is slower
// than the round trip that carried it.
func (a *App) noteRemoteRequestBlocked(token string, blocked bool) {
	if token == "" {
		return
	}
	a.threadRequests.mu.Lock()
	changed := blocked && !a.threadRequests.blocked[token]
	if a.threadRequests.blocked == nil {
		a.threadRequests.blocked = make(map[string]bool)
	}
	a.threadRequests.blocked[token] = blocked
	a.threadRequests.mu.Unlock()
	if changed {
		a.wakeRequestWaits(token)
	}
}

func (a *App) forgetRemoteRequestBlocked(token string) {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	delete(a.threadRequests.blocked, token)
}

func (a *App) remoteRequestBlocked(token string) bool {
	a.threadRequests.mu.Lock()
	defer a.threadRequests.mu.Unlock()
	return a.threadRequests.blocked[token]
}

// threadRequestBlocked reports whether a request's target is waiting on a
// person right now. A blocked target ends a wait without settling the
// request: the person owns the answer, and the agent is told to leave it to
// them.
//
// A local target is read from the live router. A remote one is read from
// what its own computer reported in the last poll, which is the same
// property observed by the only process that can see it; there is no second
// state model, because neither reading is stored.
func (a *App) threadRequestBlocked(row store.ThreadRequest) bool {
	if row.TargetThreadID == "" || threadRequestSettled(row) {
		return false
	}
	if row.TargetComputerID != "" {
		return a.remoteRequestBlocked(row.Token)
	}
	live, err := a.threadToolsAdapter().LiveState(context.Background(), row.TargetThreadID)
	if err != nil {
		return false
	}
	return live.Blocked()
}

// threadRequestOutcome maps a source row plus what ended the wait onto the
// outcome the tools render. It is the one place the four outcomes are
// decided, so an ack and a status row cannot disagree about the same request.
func threadRequestOutcome(row store.ThreadRequest, blocked bool) string {
	switch {
	case threadRequestSettled(row):
		return threadtools.OutcomeSettled
	case row.State == store.ThreadRequestUnconfirmed:
		return threadtools.OutcomeUnconfirmed
	case blocked:
		return threadtools.OutcomeBlocked
	default:
		return threadtools.OutcomeBackgrounded
	}
}
