package app

import (
	"maps"
	"slices"
	"sync"

	"agent-overflow/internal/eventchan"
)

// SendsPendingEvent is the provider:sends_pending payload: whether a thread
// holds a user message the provider has not yet echoed, queued behind the
// running turn or flushed and awaiting its echo. A pane learns this from
// its send queue; every other client reads it here, because the queue
// frames carry message text (threads:operate) and the echo that empties
// the queue is only delivered to clients watching the thread.
type SendsPendingEvent struct {
	ThreadID string `json:"threadId"`
	Pending  bool   `json:"pending"`
	// FlushedThrough is the newest provider:queue_flushed sequence when
	// the answer was read. An answer that nothing is pending says every
	// message the thread flushed up to that sequence was echoed or taken
	// back, in whatever order the two channels' frames arrive.
	FlushedThrough uint64 `json:"flushedThrough"`
}

// SendsPendingAnswer is a thread's sends-pending answer as of the
// provider:sends_pending frame numbered Sequence: every frame up to it is
// reflected, none after it.
type SendsPendingAnswer struct {
	Pending  bool   `json:"pending"`
	Sequence uint64 `json:"sequence"`
}

// RegisteredQueueItem is RegisterQueueItem's answer: the queued item and
// the thread's sends-pending answer once it is queued. The reply can be
// written ahead of frames emitted before it, so the caller orders the
// answer against them by Sequence.
type RegisteredQueueItem struct {
	QueuedItem
	SendsPending SendsPendingAnswer `json:"sendsPending"`
}

// appSendsPendingState publishes each thread's sends-pending answer when it
// changes. Triage reports a possible change under its router lock and the
// flush dispatcher under its own, so neither can read the answer there
// (the read takes both, dispatch bookkeeping first). They mark the thread
// dirty instead, and one worker goroutine at a time reads and publishes.
type appSendsPendingState struct {
	// mu guards dirty, running and stopped. It is a leaf: markers hold
	// the router or dispatch lock while taking it.
	mu      sync.Mutex
	dirty   map[string]struct{}
	running bool
	stopped bool
	wg      sync.WaitGroup
	// publishMu guards pending, every thread last published as pending,
	// which ListThreadLiveActivity reports. Held across the emit so the set
	// never runs ahead of the frames, and so publishers emit in the order
	// they read.
	publishMu sync.Mutex
	pending   map[string]struct{}
}

// markSendsPendingDirty records that threadID's answer may have changed
// and starts the worker when none is running. Never blocks on anything
// but the leaf lock.
func (a *App) markSendsPendingDirty(threadID string) {
	if threadID == "" {
		return
	}
	s := &a.sendsPending
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if s.dirty == nil {
		s.dirty = make(map[string]struct{})
	}
	s.dirty[threadID] = struct{}{}
	if s.running {
		return
	}
	s.running = true
	s.wg.Add(1)
	go a.runSendsPendingWorker()
}

func (a *App) runSendsPendingWorker() {
	s := &a.sendsPending
	defer s.wg.Done()
	for {
		s.mu.Lock()
		if len(s.dirty) == 0 || s.stopped {
			s.running = false
			s.mu.Unlock()
			return
		}
		batch := make([]string, 0, len(s.dirty))
		for threadID := range s.dirty {
			batch = append(batch, threadID)
		}
		clear(s.dirty)
		s.mu.Unlock()
		for _, threadID := range batch {
			a.publishSendsPending(threadID)
		}
	}
}

// readSendsPendingLocked answers for threadID across the whole handoff chain:
// triage's queue and claim, the dispatcher's batches, and the pending sends
// awaiting their echo. Dispatch bookkeeping is held across the triage read,
// as in queueSnapshotForThread, so a message moving between the two is
// seen on one side. The caller holds flushDispatch.mu.
func (a *App) readSendsPendingLocked(threadID string) bool {
	if len(a.pendingFlushDispatchItemsLocked(threadID)) > 0 {
		return true
	}
	return a.triage != nil && a.triage.SendsPending(threadID)
}

// publishSendsPending reads threadID's answer, publishes it when it
// changed, and returns it with the sequence of the newest frame on the
// channel. The read happens under publishMu so two publishers cannot emit
// their answers in the opposite order to the one they read them in, and
// the sequence is read there too, after the emit, so it names this answer.
// Dispatch bookkeeping is held across the read: queue_flushed is emitted
// under it, so the queue_flushed sequence read beside the answer names
// exactly the flushes the answer saw. An answer that nothing is pending is
// also published when a flush since the last one is not covered yet, so
// every queue_flushed is followed by a frame whose FlushedThrough reaches
// it, even when the echo arrived before pending was ever published.
// Lock order: publishMu -> flushDispatch.mu -> triage.
func (a *App) publishSendsPending(threadID string) SendsPendingAnswer {
	s := &a.sendsPending
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	a.flushDispatch.mu.Lock()
	pending := a.readSendsPendingLocked(threadID)
	flushedThrough := a.eventSequence(eventchan.ProviderQueueFlushed)
	_, owesSettle := a.flushDispatch.flushUnsettled[threadID]
	if !pending {
		delete(a.flushDispatch.flushUnsettled, threadID)
	}
	a.flushDispatch.mu.Unlock()
	if _, was := s.pending[threadID]; was != pending || (!pending && owesSettle) {
		if pending {
			if s.pending == nil {
				s.pending = make(map[string]struct{})
			}
			s.pending[threadID] = struct{}{}
		} else {
			delete(s.pending, threadID)
		}
		a.emit(eventchan.ProviderSendsPending, SendsPendingEvent{ThreadID: threadID, Pending: pending, FlushedThrough: flushedThrough})
	}
	return SendsPendingAnswer{Pending: pending, Sequence: a.eventSequence(eventchan.ProviderSendsPending)}
}

// FlushSettlement is which flushes the provider:sends_pending frames have
// settled, as of FlushedThrough on provider:queue_flushed: every message a
// thread flushed up to that sequence was echoed or taken back, except for
// the threads in UnsettledThreadIDs, whose frame is still to come.
type FlushSettlement struct {
	FlushedThrough     uint64   `json:"flushedThrough"`
	UnsettledThreadIDs []string `json:"unsettledThreadIds"`
}

// ListUnsettledFlushes is the snapshot leg of FlushedThrough on
// provider:sends_pending. A frame that reads nothing pending is published
// once and not restated, so a client that connects, or loses frames on that
// channel, reads which threads still hold a flushed message here.
//
// threads:operate, as provider:queue_flushed: it serves only a client that
// holds the flushed messages.
//
//ao:scope threads:operate
//ao:route all
func (a *App) ListUnsettledFlushes() (FlushSettlement, error) {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	return FlushSettlement{
		FlushedThrough:     a.eventSequence(eventchan.ProviderQueueFlushed),
		UnsettledThreadIDs: slices.Sorted(maps.Keys(a.flushDispatch.flushUnsettled)),
	}, nil
}

// sendsPendingThreads returns every thread last published as pending. A
// change not yet published is a frame still to come.
func (a *App) sendsPendingThreads() map[string]struct{} {
	s := &a.sendsPending
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return maps.Clone(s.pending)
}

// stopSendsPending stops publication and joins the worker. Marks arriving
// afterwards are dropped: shutdown has stopped the producers that matter
// and the clients reconcile on their next connection.
func (a *App) stopSendsPending() {
	s := &a.sendsPending
	s.mu.Lock()
	s.stopped = true
	s.dirty = nil
	s.mu.Unlock()
	s.wg.Wait()
}
