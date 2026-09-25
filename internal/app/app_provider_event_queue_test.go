package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/testutil"
)

// waitProviderEvents is the observation point for tests that call a session
// event handler directly: the handler only queues the event, and this returns
// once every event queued for threadID so far has been handled.
func waitProviderEvents(t testing.TB, app *App, threadID string) {
	t.Helper()
	if err := app.providerEvents.drain(threadID); err != nil {
		t.Fatalf("wait for provider events: %v", err)
	}
}

// seqEvent is a queue test event whose Content carries seq, padded to size
// bytes when size is larger.
func seqEvent(seq, size int) provider.ProviderEvent {
	content := strconv.Itoa(seq)
	if size > len(content) {
		content += strings.Repeat("x", size-len(content))
	}
	return provider.ProviderEvent{Kind: provider.EventTextDelta, Content: content}
}

func eventSeq(evt provider.ProviderEvent) int {
	seq, err := strconv.Atoi(strings.TrimRight(evt.Content, "x"))
	if err != nil {
		return -1
	}
	return seq
}

// seqRecorder records the sequence numbers a queue handled and whether two
// handlers for its thread ever overlapped.
type seqRecorder struct {
	mu      sync.Mutex
	seqs    []int
	active  atomic.Int32
	overlap atomic.Bool
}

func (r *seqRecorder) record(seq int) {
	if r.active.Add(1) != 1 {
		r.overlap.Store(true)
	}
	r.mu.Lock()
	r.seqs = append(r.seqs, seq)
	r.mu.Unlock()
	r.active.Add(-1)
}

func (r *seqRecorder) assertInOrder(t *testing.T, label string, want int) {
	t.Helper()
	if r.overlap.Load() {
		t.Errorf("%s: two handlers ran at once", label)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seqs) != want {
		t.Fatalf("%s: handled %d events, want %d", label, len(r.seqs), want)
	}
	for i, seq := range r.seqs {
		if seq != i {
			t.Fatalf("%s: event %d handled at position %d", label, seq, i)
		}
	}
}

// waitForQueueWaiter reports whether a producer or a drain parks on the
// thread's queue within the deadline. The queue's change channel exists only
// while one is parked, and nothing closes it while the worker is blocked.
func waitForQueueWaiter(qs *providerEventQueues, threadID string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if q := qs.lookup(threadID); q != nil {
			q.mu.Lock()
			waiting := q.changed != nil
			q.mu.Unlock()
			if waiting {
				return true
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// enqueueWithin fails the test instead of hanging when an enqueue that must
// be admitted blocks.
func enqueueWithin(t *testing.T, qs *providerEventQueues, threadID string, evt provider.ProviderEvent, handle func(provider.ProviderEvent)) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		qs.enqueue(threadID, evt, handle)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("enqueue of %d bytes blocked below the queue bound", providerEventSize(evt))
	}
}

func TestProviderEventQueueKeepsEachThreadInOrderUnderLoad(t *testing.T) {
	const (
		threads   = 8
		perThread = 5000
	)
	var qs providerEventQueues
	recorders := make([]*seqRecorder, threads)
	var producers sync.WaitGroup
	for i := range threads {
		rec := &seqRecorder{seqs: make([]int, 0, perThread)}
		recorders[i] = rec
		threadID := fmt.Sprintf("thread-load-%d", i)
		slow := i%2 == 1
		handle := func(evt provider.ProviderEvent) {
			seq := eventSeq(evt)
			rec.record(seq)
			// Stands in for a session's "disconnected": the queue retires
			// once idle and the next event starts a fresh one, which must
			// neither overlap nor reorder with the old worker.
			if seq%1000 == 999 {
				qs.retireWhenIdle(threadID)
			}
			// Slow consumers fill their queue, so their producer blocks.
			if slow && seq%16 == 0 {
				time.Sleep(50 * time.Microsecond)
			}
		}
		producers.Add(1)
		go func() {
			defer producers.Done()
			for seq := range perThread {
				qs.enqueue(threadID, seqEvent(seq, 0), handle)
				// Fast threads pause now and then, so their worker runs
				// dry, exits, and is restarted by the next event.
				if !slow && seq%250 == 249 {
					if err := qs.drain(threadID); err != nil {
						t.Errorf("%s: drain: %v", threadID, err)
						return
					}
				}
			}
		}()
	}
	producers.Wait()
	if err := qs.drainAll(); err != nil {
		t.Fatalf("drainAll: %v", err)
	}
	for i, rec := range recorders {
		rec.assertInOrder(t, fmt.Sprintf("thread-load-%d", i), perThread)
	}
}

func TestProviderEventQueueBlocksProducerAtItsBoundThenResumes(t *testing.T) {
	ones := make([]int, providerEventQueueMaxEvents)
	for i := range ones {
		ones[i] = 1
	}
	cases := []struct {
		name string
		// first is handled until the test releases it; its bytes still
		// count against the bound while it is.
		first    int
		admitted []int
		blocked  int
	}{
		{name: "event count", first: 1, admitted: ones, blocked: 1},
		{name: "content bytes", first: 5 << 20, admitted: []int{3 << 20}, blocked: 1},
		{name: "single event larger than the byte bound", first: providerEventQueueMaxBytes + 1, blocked: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const threadID = "thread-backpressure"
			var qs providerEventQueues
			rec := &seqRecorder{}
			started := make(chan struct{})
			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseOnce)
			handle := func(evt provider.ProviderEvent) {
				seq := eventSeq(evt)
				rec.record(seq)
				if seq == 0 {
					close(started)
					<-release
				}
			}

			enqueueWithin(t, &qs, threadID, seqEvent(0, tc.first), handle)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("worker never started")
			}
			for i, size := range tc.admitted {
				enqueueWithin(t, &qs, threadID, seqEvent(i+1, size), handle)
			}

			blockedSeq := len(tc.admitted) + 1
			resumed := make(chan struct{})
			go func() {
				qs.enqueue(threadID, seqEvent(blockedSeq, tc.blocked), handle)
				close(resumed)
			}()
			if !waitForQueueWaiter(&qs, threadID) {
				t.Fatal("producer was admitted past the queue bound")
			}
			select {
			case <-resumed:
				t.Fatal("enqueue returned while the queue was at its bound")
			default:
			}

			releaseOnce()
			select {
			case <-resumed:
			case <-time.After(5 * time.Second):
				t.Fatal("producer did not resume after the worker made room")
			}
			if err := qs.drain(threadID); err != nil {
				t.Fatalf("drain: %v", err)
			}
			rec.assertInOrder(t, tc.name, blockedSeq+1)
		})
	}
}

// A drain whose worker stalls mid-queue abandons only the wait: the events
// it was waiting for are still handled, in order, behind the ones already
// running, and events read after it keep their place.
func TestProviderEventDrainStallLeavesTheQueueIntact(t *testing.T) {
	const threadID = "thread-drain-timeout"
	qs := providerEventQueues{drainStall: 50 * time.Millisecond}
	rec := &seqRecorder{}
	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	handle := func(evt provider.ProviderEvent) {
		seq := eventSeq(evt)
		if seq == 0 {
			close(started)
			<-release
		}
		rec.record(seq)
	}
	for seq := range 10 {
		enqueueWithin(t, &qs, threadID, seqEvent(seq, 0), handle)
	}
	<-started

	begin := time.Now()
	err := qs.drain(threadID)
	elapsed := time.Since(begin)
	if err == nil {
		t.Fatal("drain returned nil while the worker was blocked")
	}
	if !strings.Contains(err.Error(), "10 provider event(s) for thread "+threadID) {
		t.Fatalf("drain error = %q, want the pending count and thread", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("drain took %s past its 50ms bound", elapsed)
	}

	for seq := 10; seq < 20; seq++ {
		enqueueWithin(t, &qs, threadID, seqEvent(seq, 0), handle)
	}
	releaseOnce()
	qs.drainStall = 5 * time.Second
	if err := qs.drain(threadID); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	rec.assertInOrder(t, threadID, 20)
}

// A drain waits for the events queued when it began, not for a stream that
// keeps arriving behind them.
func TestProviderEventDrainWaitsOnlyForEventsQueuedBeforeIt(t *testing.T) {
	const threadID = "thread-drain-target"
	var qs providerEventQueues
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseLater := make(chan struct{})
	releaseLaterOnce := sync.OnceFunc(func() { close(releaseLater) })
	t.Cleanup(releaseLaterOnce)
	handle := func(evt provider.ProviderEvent) {
		if eventSeq(evt) == 0 {
			close(firstStarted)
			<-releaseFirst
			return
		}
		<-releaseLater
	}
	enqueueWithin(t, &qs, threadID, seqEvent(0, 0), handle)
	<-firstStarted

	drained := make(chan error, 1)
	go func() { drained <- qs.drain(threadID) }()
	if !waitForQueueWaiter(&qs, threadID) {
		t.Fatal("drain never waited")
	}
	enqueueWithin(t, &qs, threadID, seqEvent(1, 0), handle)
	close(releaseFirst)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain waited for an event queued after it began")
	}
}

// Retirement is what keeps a thread without a session from holding a queue:
// once the session's "disconnected" is handled and nothing is pending, the
// registry forgets the thread, and a later session starts a fresh queue.
func TestProviderEventQueueRetiresAfterDisconnect(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-queue-retire")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	handler := app.sessionEventHandler(thread.ID, "retire-token", string(provider.Claude))
	handler(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)
	if app.providerEvents.lookup(thread.ID) == nil {
		t.Fatal("a live session's queue retired before its disconnect")
	}

	handler(provider.ProviderEvent{Kind: provider.EventSessionStatus, ThreadID: thread.ID, Content: "disconnected", Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)
	waitForCondition(t, "queue retires after disconnect", func() bool {
		return app.providerEvents.lookup(thread.ID) == nil
	})

	var observed atomic.Int32
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(string, provider.ProviderEvent) { observed.Add(1) })
	t.Cleanup(unsubscribe)
	next := app.sessionEventHandler(thread.ID, "retire-token-2", string(provider.Claude))
	next(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)
	if observed.Load() != 1 {
		t.Fatalf("event after retirement handled %d times, want 1", observed.Load())
	}
}

// The worker retires its queue after it finds it empty and stops, without
// holding the lock across both. A producer that queues an event in between
// keeps the queue: retiring it would start a second worker for the thread.
func TestProviderEventQueueRetireKeepsAQueueWithWork(t *testing.T) {
	const threadID = "thread-retire-race"
	var qs providerEventQueues
	rec := &seqRecorder{}
	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	handle := func(evt provider.ProviderEvent) {
		seq := eventSeq(evt)
		if seq == 0 {
			close(started)
			<-release
		}
		rec.record(seq)
	}
	enqueueWithin(t, &qs, threadID, seqEvent(0, 0), handle)
	<-started
	enqueueWithin(t, &qs, threadID, seqEvent(1, 0), handle)

	q := qs.lookup(threadID)
	q.mu.Lock()
	q.retireWhenIdle = true
	q.mu.Unlock()
	qs.retireIfIdle(q)
	if qs.lookup(threadID) != q || q.retired {
		t.Fatal("a queue with pending events was retired")
	}

	releaseOnce()
	if err := qs.drain(threadID); err != nil {
		t.Fatalf("drain: %v", err)
	}
	rec.assertInOrder(t, threadID, 2)
}

// A handler error is logged with its thread and event kind, and the worker
// goes on to the next event.
func TestProviderEventHandlerErrorIsLoggedAndTheWorkerContinues(t *testing.T) {
	app := newTestAppWithTriage(t)
	thread := testThread("thread-handler-error")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	logs := testutil.CaptureLogOutput(t)
	var observed []provider.EventKind
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(_ string, evt provider.ProviderEvent) {
		observed = append(observed, evt.Kind)
	})
	t.Cleanup(unsubscribe)

	handler := app.sessionEventHandler(thread.ID, "error-token", string(provider.Claude))
	handler(provider.ProviderEvent{Kind: "not-a-kind", ThreadID: thread.ID, Timestamp: time.Now()})
	handler(provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: thread.ID, Timestamp: time.Now()})
	waitProviderEvents(t, app, thread.ID)

	if want := "triage: thread " + thread.ID + ": not-a-kind event: "; !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q, want a line starting %q", logs.String(), want)
	}
	if want := []provider.EventKind{"not-a-kind", provider.EventTurnStart}; !slices.Equal(observed, want) {
		t.Fatalf("handled %v, want %v", observed, want)
	}
}

// Events the provider emitted before a stop were read and must be persisted:
// the stop waits for them before CleanupThread marks the thread stopped,
// after which triage drops whatever reaches it.
func TestStopSessionPersistsEventsReadBeforeTheStop(t *testing.T) {
	app := newTestAppWithTriage(t)
	thread := testThread("thread-stop-drain")
	thread.Provider = string(provider.Claude)
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	started := make(chan struct{})
	var startedOnce sync.Once
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(_ string, evt provider.ProviderEvent) {
		if evt.Content == "first" {
			startedOnce.Do(func() { close(started) })
			<-release
		}
	})
	t.Cleanup(unsubscribe)

	handler := app.sessionEventHandler(thread.ID, "stop-drain-token", string(provider.Claude))
	for _, summary := range []string{"first", "second"} {
		handler(provider.ProviderEvent{
			Kind:      provider.EventError,
			ThreadID:  thread.ID,
			Content:   summary,
			Meta:      json.RawMessage(`{"fatal":false}`),
			Timestamp: time.Now(),
		})
	}
	<-started

	stopped := make(chan error, 1)
	go func() { stopped <- app.StopSession(thread.ID) }()
	// Release the worker once the stop is waiting on it, so the stop's own
	// drain is what orders "second" ahead of CleanupThread.
	go func() {
		waitForQueueWaiter(&app.providerEvents, thread.ID)
		releaseOnce()
	}()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("StopSession() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StopSession did not return")
	}

	items, err := app.store.ListItems(thread.ID)
	if err != nil {
		t.Fatalf("ListItems() error = %v", err)
	}
	var summaries []string
	for _, item := range items {
		summaries = append(summaries, item.Summary)
	}
	for _, want := range []string{"first", "second"} {
		if !slices.Contains(summaries, want) {
			t.Fatalf("items = %q, want %q persisted before the stop", summaries, want)
		}
	}
}

// A stop whose drain stalls logs it and still completes.
func TestStopSessionLogsADrainThatStalls(t *testing.T) {
	app := newTestAppWithTriage(t)
	app.providerEvents.drainStall = 50 * time.Millisecond
	thread := testThread("thread-stop-drain-bound")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	logs := testutil.CaptureLogOutput(t)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	started := make(chan struct{})
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(string, provider.ProviderEvent) {
		close(started)
		<-release
	})
	t.Cleanup(func() {
		releaseOnce()
		waitProviderEvents(t, app, thread.ID)
		unsubscribe()
	})

	app.sessionEventHandler(thread.ID, "bound-token", string(provider.Claude))(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: thread.ID, Timestamp: time.Now(),
	})
	<-started

	stopped := make(chan error, 1)
	go func() { stopped <- app.StopSession(thread.ID) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("StopSession() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopSession waited past its drain bound")
	}
	want := "app: stop session: 1 provider event(s) for thread " + thread.ID + " not handled: none handled for 50ms"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q, want %q", logs.String(), want)
	}
}

// Shutdown handles every event already read before it closes the store.
func TestShutdownHandlesProviderEventsAlreadyRead(t *testing.T) {
	app := newTestAppWithStore(t)
	const (
		threadID = "thread-shutdown-drain"
		events   = 10
	)
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var handled atomic.Int32
	unsubscribe := app.subscribeThreadTurnObserver(threadID, func(_ string, evt provider.ProviderEvent) {
		if evt.Content == "0" {
			<-release
		}
		handled.Add(1)
	})
	t.Cleanup(unsubscribe)
	handler := app.sessionEventHandler(threadID, "shutdown-drain-token", string(provider.Claude))
	for i := range events {
		handler(provider.ProviderEvent{Kind: provider.EventNotification, ThreadID: threadID, Content: strconv.Itoa(i)})
	}

	atDrain, atClose := int32(-1), int32(-1)
	var drainErr error
	app.shutdownStepFn = func(step string, err error) {
		switch step {
		case "close provider sessions":
			// Let the worker go once shutdown is waiting on it, so the
			// drain step is what holds the store open.
			go func() {
				waitForQueueWaiter(&app.providerEvents, threadID)
				releaseOnce()
			}()
		case "drain provider events":
			atDrain, drainErr = handled.Load(), err
		case "close store":
			atClose = handled.Load()
		}
	}
	if err := app.ServiceShutdown(); err != nil {
		t.Fatalf("ServiceShutdown() error = %v", err)
	}
	if drainErr != nil {
		t.Fatalf("drain provider events: %v", drainErr)
	}
	if atDrain != events || atClose != events {
		t.Fatalf("handled %d events at the drain step and %d at store close, want %d at both", atDrain, atClose, events)
	}
}

// A shutdown whose drain stalls reports it in its error, which the
// process logs, and still closes the store.
func TestShutdownReportsADrainThatStalls(t *testing.T) {
	app := newTestAppWithStore(t)
	app.providerEvents.drainStall = 50 * time.Millisecond
	const threadID = "thread-shutdown-drain-bound"
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	started := make(chan struct{})
	finished := make(chan struct{})
	unsubscribe := app.subscribeThreadTurnObserver(threadID, func(string, provider.ProviderEvent) {
		close(started)
		<-release
		close(finished)
	})
	t.Cleanup(func() {
		releaseOnce()
		<-finished
		unsubscribe()
	})
	app.sessionEventHandler(threadID, "bound-token", string(provider.Claude))(provider.ProviderEvent{
		Kind: provider.EventNotification, ThreadID: threadID,
	})
	<-started

	var closedStore bool
	app.shutdownStepFn = func(step string, _ error) {
		if step == "close store" {
			closedStore = true
		}
	}
	err := app.ServiceShutdown()
	if err == nil || !strings.Contains(err.Error(), "drain provider events") || !strings.Contains(err.Error(), threadID) {
		t.Fatalf("ServiceShutdown() error = %v, want the drain step naming %s", err, threadID)
	}
	if !closedStore {
		t.Fatal("shutdown stopped at the drain instead of finishing")
	}
}

// Closing a session leaves its final events (its own "disconnected" among
// them) queued; the close handles them before returning, so a stop or a
// replacement start never races the old session's teardown.
func TestStopSessionHandlesTheSessionsFinalEvents(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-final-events")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	initSeen := make(chan struct{})
	var initOnce sync.Once
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var disconnectHandled atomic.Bool
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(_ string, evt provider.ProviderEvent) {
		switch {
		case evt.Kind == provider.EventInit:
			initOnce.Do(func() { close(initSeen) })
		case evt.Kind == provider.EventSessionStatus && evt.Content == "disconnected":
			<-release
			disconnectHandled.Store(true)
		}
	})
	t.Cleanup(unsubscribe)

	const token = "final-events-token"
	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Binary: writeSilentClaudeBinary(t), WorkDir: thread.WorkspacePath},
		app.sessionEventHandler(thread.ID, token, string(provider.Claude)),
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	app.sessionManager().put(thread.ID, session{Provider: string(provider.Claude), Token: token, Claude: sess})
	select {
	case <-initSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("init never reached the event handler")
	}
	waitProviderEvents(t, app, thread.ID)

	stopped := make(chan error, 1)
	go func() { stopped <- app.StopSession(thread.ID) }()
	if !waitForQueueWaiter(&app.providerEvents, thread.ID) {
		t.Fatal("StopSession never waited for the session's final events")
	}
	select {
	case <-stopped:
		t.Fatal("StopSession returned before its session's disconnect was handled")
	default:
	}
	releaseOnce()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("StopSession() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StopSession did not return")
	}
	if !disconnectHandled.Load() {
		t.Fatal("StopSession returned before its session's disconnect was handled")
	}
}

// Codex reports its thread in an EventInit emitted before NewSession returns.
// The start handles it before returning, so a caller acting on the started
// session (a send, a fork reading the session ref) finds it handled.
func TestCodexStartHandlesItsInitBeforeReturning(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("codex-start-init")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	app.ensureTriageRouter()
	codexBinary := writeThreadCostCodex(t, `{"jsonrpc":"2.0","id":%s,"result":{}}`, filepath.Join(t.TempDir(), "usage-read.jsonl"))
	if _, err := app.settings.Update(map[string]any{"codexBinaryPath": codexBinary}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	var initHandled atomic.Bool
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(_ string, evt provider.ProviderEvent) {
		if evt.Kind == provider.EventInit {
			<-release
			initHandled.Store(true)
		}
	})
	t.Cleanup(func() {
		releaseOnce()
		app.codexThreadService().Close()
		if err := app.StopSession(thread.ID); err != nil {
			t.Errorf("StopSession() error = %v", err)
		}
		unsubscribe()
	})

	type startResult struct {
		err         error
		initHandled bool
	}
	started := make(chan startResult, 1)
	go func() {
		err := app.StartSession(thread.ID)
		started <- startResult{err: err, initHandled: initHandled.Load()}
	}()
	if !waitForQueueWaiter(&app.providerEvents, thread.ID) {
		t.Fatal("StartSession never waited for the events its provider emitted while starting")
	}
	select {
	case result := <-started:
		t.Fatalf("StartSession returned (err=%v) before its init was handled", result.err)
	default:
	}
	releaseOnce()
	var result startResult
	select {
	case result = <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("StartSession did not return")
	}
	if result.err != nil {
		t.Fatalf("StartSession() error = %v", result.err)
	}
	if !result.initHandled {
		t.Fatal("StartSession returned before its init was handled")
	}
	row, err := app.store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("GetThread() error = %v", err)
	}
	if row.SessionRef != threadCostProviderThread {
		t.Fatalf("SessionRef = %q when StartSession returned, want %q", row.SessionRef, threadCostProviderThread)
	}
}

// A Claude read loop answers control traffic itself. With event handling
// on the thread's worker, a handler blocked on one event does not hold the
// control_response that Interrupt waits for.
func TestClaudeInterruptAnswersWhileEventHandlingIsBlocked(t *testing.T) {
	app := newTestAppWithStore(t)
	thread := testThread("thread-blocked-handler-interrupt")
	thread.Provider = string(provider.Claude)
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	blocked := make(chan struct{})
	var blockedOnce sync.Once
	unsubscribe := app.subscribeThreadTurnObserver(thread.ID, func(_ string, evt provider.ProviderEvent) {
		if evt.Kind != provider.EventInit {
			return
		}
		blockedOnce.Do(func() { close(blocked) })
		<-release
	})
	t.Cleanup(unsubscribe)

	sess, err := claude.NewSession(
		context.Background(),
		thread.ID,
		claude.Config{Binary: writeSilentClaudeBinary(t), WorkDir: thread.WorkspacePath},
		app.sessionEventHandler(thread.ID, "blocked-handler-token", string(provider.Claude)),
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() {
		releaseOnce()
		if err := sess.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
		waitProviderEvents(t, app, thread.ID)
	})
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("init never reached the event handler")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	begin := time.Now()
	err = sess.Interrupt(ctx)
	elapsed := time.Since(begin)
	if err != nil {
		t.Fatalf("Interrupt() error = %v after %s: its control_response waited behind the blocked handler", err, elapsed)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Interrupt() took %s, want tens of milliseconds", elapsed)
	}
	t.Logf("Interrupt answered in %s with the event handler blocked", elapsed)
}
