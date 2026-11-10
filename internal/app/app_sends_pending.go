package app

import (
	"maps"
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

// readSendsPending answers for threadID across the whole handoff chain:
// triage's queue and claim, the dispatcher's batches, and the pending sends
// awaiting their echo. Dispatch bookkeeping is held across the triage read,
// as in queueSnapshotForThread, so a message moving between the two is
// seen on one side.
func (a *App) readSendsPending(threadID string) bool {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
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
// Lock order: publishMu -> flushDispatch.mu -> triage.
func (a *App) publishSendsPending(threadID string) SendsPendingAnswer {
	s := &a.sendsPending
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	pending := a.readSendsPending(threadID)
	if _, was := s.pending[threadID]; was != pending {
		if pending {
			if s.pending == nil {
				s.pending = make(map[string]struct{})
			}
			s.pending[threadID] = struct{}{}
		} else {
			delete(s.pending, threadID)
		}
		a.emit(eventchan.ProviderSendsPending, SendsPendingEvent{ThreadID: threadID, Pending: pending})
	}
	return SendsPendingAnswer{Pending: pending, Sequence: a.eventSequence(eventchan.ProviderSendsPending)}
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
