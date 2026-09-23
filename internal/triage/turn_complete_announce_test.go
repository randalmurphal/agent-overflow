package triage

import (
	"errors"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
)

// turnAnnouncement is the durable state a client would read at the moment
// `provider:turn_completed` reaches it.
type turnAnnouncement struct {
	activeTurn     bool
	streamingItems []string
	afterDispatch  bool
}

// announcementProbe snapshots durable turn state inside the emit callback,
// which is the earliest point any client can react to the event.
type announcementProbe struct {
	mu            sync.Mutex
	st            *store.Store
	threadID      string
	dispatched    bool
	announcements []turnAnnouncement
	readErr       error
}

func (p *announcementProbe) emit(channel eventchan.Channel, _ any) {
	if channel != eventchan.ProviderTurnCompleted {
		return
	}
	_, active, err := p.st.GetActiveTurn(p.threadID)
	items, listErr := p.st.ListItems(p.threadID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil || listErr != nil {
		p.readErr = errors.Join(err, listErr)
		return
	}
	var streaming []string
	for _, item := range items {
		if item.Status == statusStreaming {
			streaming = append(streaming, item.ID)
		}
	}
	p.announcements = append(p.announcements, turnAnnouncement{
		activeTurn: active, streamingItems: streaming, afterDispatch: p.dispatched,
	})
}

func (p *announcementProbe) dispatch(string, []QueuedFlushItem) {
	p.mu.Lock()
	p.dispatched = true
	p.mu.Unlock()
}

func (p *announcementProbe) snapshot(t *testing.T) []turnAnnouncement {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readErr != nil {
		t.Fatalf("read turn state at announcement: %v", p.readErr)
	}
	return append([]turnAnnouncement(nil), p.announcements...)
}

func newAnnouncementRouter(t *testing.T, threadID string) (*Router, *announcementProbe) {
	t.Helper()
	st := storetest.Clone(t)
	probe := &announcementProbe{st: st, threadID: threadID}
	router := NewRouter(st, probe.emit)
	t.Cleanup(router.flushAllUsage)
	t.Cleanup(router.DrainWireItemRefresh)
	router.SetFlushDispatcher(probe.dispatch)
	createTestThread(t, st, threadID)
	return router, probe
}

func handleOrFatal(t *testing.T, router *Router, evt provider.ProviderEvent) {
	t.Helper()
	if err := router.Handle(evt); err != nil {
		t.Fatalf("handle %s: %v", evt.Kind, err)
	}
}

// A client that reacts to `provider:turn_completed` by reading turn state
// (send admission, the workspace lock, revert eligibility) must find the
// turn settled, and the queue boundary must not start the next turn before
// the previous one is announced.
func TestTurnCompletedIsAnnouncedAfterSettlement(t *testing.T) {
	router, probe := newAnnouncementRouter(t, "t1")
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	})
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "still streaming", Timestamp: time.Now(),
	})
	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "next"))

	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	})

	got := probe.snapshot(t)
	if len(got) != 1 {
		t.Fatalf("turn_completed announcements = %d, want 1", len(got))
	}
	if got[0].activeTurn {
		t.Fatal("turn_completed announced while the turns row was still open")
	}
	if len(got[0].streamingItems) != 0 {
		t.Fatalf("turn_completed announced with streaming items %v", got[0].streamingItems)
	}
	if got[0].afterDispatch {
		t.Fatal("queued message was dispatched before turn_completed was announced")
	}
	probe.mu.Lock()
	dispatched := probe.dispatched
	probe.mu.Unlock()
	if !dispatched {
		t.Fatal("queued message was not dispatched at the turn boundary")
	}
}

// A later round on an already-settled logical turn (Claude answering a
// background task notification) settles its own streaming rows on the
// late-fold path; its announcement must follow that settlement too.
func TestReRoundTurnCompletedIsAnnouncedAfterSettlement(t *testing.T) {
	router, probe := newAnnouncementRouter(t, "t1")
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now(),
	})
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	})

	setOpenRoundForTest(router, "t1", "round-2")
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventTextDelta, ThreadID: "t1", Content: "task finished", Timestamp: time.Now(),
	})
	handleOrFatal(t, router, provider.ProviderEvent{
		Kind: provider.EventTurnComplete, ThreadID: "t1",
		TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
	})

	got := probe.snapshot(t)
	if len(got) != 2 {
		t.Fatalf("turn_completed announcements = %d, want one per round", len(got))
	}
	if len(got[1].streamingItems) != 0 {
		t.Fatalf("re-round turn_completed announced with streaming items %v", got[1].streamingItems)
	}
}
