package triage

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A settled block that never streamed looks its row up after its own
// thread's settles, and no other thread's settle holds it.
func TestSnapshotBlockWaitsOnlyForItsThreadsSettles(t *testing.T) {
	router, st, _ := newTestRouter(t)
	createTestThread(t, st, "t1")
	createTestThread(t, st, "t2")
	snapshot := func(threadID, itemID string) <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- router.Handle(provider.ProviderEvent{
				Kind: provider.EventContentBlockStop, ThreadID: threadID, ItemID: itemID,
				Content: "settled " + itemID, ContentPresent: true,
				Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now(),
			})
		}()
		return done
	}

	release := make(chan struct{})
	router.goSettle("t2", func() { <-release })
	select {
	case err := <-snapshot("t1", "a1"):
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("t1's snapshot block waited on t2's settle")
	}
	held := snapshot("t2", "a2")
	select {
	case err := <-held:
		t.Fatalf("t2's snapshot block ran before t2's settle finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	router.WaitForPendingSettles()

	for threadID, want := range map[string]string{"t1": "settled a1", "t2": "settled a2"} {
		items, err := st.ListItems(threadID)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Summary != want || items[0].Status != statusCompleted {
			t.Errorf("%s rows = %+v, want one completed %q", threadID, items, want)
		}
	}
	router.settleMu.Lock()
	defer router.settleMu.Unlock()
	if len(router.settles) != 0 {
		t.Errorf("settle counts left after every settle finished: %v", router.settles)
	}
}

// A Stop or a fatal error that lands while a block stop's settle is
// between its read of the row and its write flips the turn's rows after
// that settle: the block's text stays the completed answer, and its
// stream ends once.
func TestErrorFlipWaitsForInFlightBlockSettle(t *testing.T) {
	for _, end := range []struct {
		name string
		run  func(router *Router) error
	}{
		{"user stop", func(router *Router) error {
			_, err := markUserInterruptForTest(router, "t1")
			return err
		}},
		{"fatal error", func(router *Router) error {
			return router.Handle(provider.ProviderEvent{
				Kind: provider.EventError, ThreadID: "t1", Content: "provider failed",
				Meta: json.RawMessage(`{"fatal":true,"expect_turn_complete":true}`), Timestamp: time.Now(),
			})
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			hold := newBlockSettleHold(t, router)
			createTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 0)
			hold.stopBlock(t, "partial answer")
			hold.runPast(t, func() error { return end.run(router) })
			router.WaitForPendingSettles()

			items, err := st.ListItems("t1")
			if err != nil {
				t.Fatal(err)
			}
			var answers []string
			for _, item := range items {
				if item.Kind == itemKindAssistantText {
					answers = append(answers, item.Status+" "+item.Summary)
				}
			}
			if len(answers) != 1 || answers[0] != statusCompleted+" partial answer" {
				t.Errorf("assistant rows = %q, want the completed block text", answers)
			}
			if finals := hold.finalTicks(); len(finals) != 1 || finals[0] != "partial answer" {
				t.Errorf("final observer ticks = %q, want one with the block text", finals)
			}
		})
	}
}

// A Stop from an app goroutine can flip a row after a settle read it and
// before the settle writes. The settle then leaves the row as the flip
// wrote it: a block stop's settle, the turn end's, which also completes
// the turn without an error, and a content-bearing stop's for a row whose
// stream the router no longer holds.
func TestSettleLeavesARowAStopFlippedMeanwhile(t *testing.T) {
	t.Run("block stop", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		hold := newBlockSettleHold(t, router)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		hold.stopBlock(t, "partial answer")
		flipHeldAnswer(t, st, hold)
		router.WaitForPendingSettles()
	})
	t.Run("turn end", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		hold := newBlockSettleHold(t, router)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		handleOrFatal(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", Content: "partial answer", Timestamp: time.Now()})
		done := make(chan error, 1)
		go func() {
			done <- router.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
			})
		}()
		select {
		case <-hold.held:
		case <-time.After(5 * time.Second):
			t.Fatal("the turn end's settle never reached its write")
		}
		flipHeldAnswer(t, st, hold)
		if err := <-done; err != nil {
			t.Fatalf("turn complete after the flip: %v", err)
		}
	})
	t.Run("content stop of a forgotten stream", func(t *testing.T) {
		router, st, _ := newTestRouter(t)
		hold := newBlockSettleHold(t, router)
		createTestThread(t, st, "t1")
		seedOpenTurn(t, router, st, "t1", 0)
		handleOrFatal(t, router, provider.ProviderEvent{Kind: provider.EventTextDelta, ThreadID: "t1", ItemID: "msg-1", Content: "partial answer", Timestamp: time.Now()})
		forgetTurnStreams(router, "t1", 0)
		done := make(chan error, 1)
		go func() { done <- router.Handle(contentBlockStop("msg-1", "text", "partial answer and the rest")) }()
		select {
		case <-hold.held:
		case <-time.After(5 * time.Second):
			t.Fatal("the content stop's settle never reached its write")
		}
		flipHeldAnswer(t, st, hold)
		if err := <-done; err != nil {
			t.Fatalf("content stop after the flip: %v", err)
		}
	})
}

// flipHeldAnswer flips t1's streaming answer as a Stop does while its
// settle is held, releases the settle, and checks the settle left the
// flipped row.
func flipHeldAnswer(t *testing.T, st *store.Store, hold *blockSettleHold) {
	t.Helper()
	items, err := st.ListItems("t1")
	if err != nil {
		t.Fatal(err)
	}
	var row store.Item
	for _, item := range items {
		if item.Kind == itemKindAssistantText {
			row = item
		}
	}
	if row.Status != statusStreaming {
		t.Fatalf("assistant row = %+v, want it streaming while the settle is held", row)
	}
	stopped := stoppedSummary(row.Summary)
	if _, changed, err := st.ErrorActiveItemIfRevision("t1", row.ID, row.Rev, stopped, time.Now().UnixMilli(), nil); err != nil || !changed {
		t.Fatalf("flip: changed=%v err=%v", changed, err)
	}
	hold.release()
	hold.router.WaitForPendingSettles()

	got, _, err := st.GetThreadItem("t1", row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusErrored || got.Summary != stopped {
		t.Fatalf("assistant row = %s %q, want the flip's %s %q", got.Status, got.Summary, statusErrored, stopped)
	}
}

// A row settle writes the row only while it is still streaming. Its
// summarise runs between its read of the row and its write, so a flip
// there lands where a Stop from another goroutine can.
func TestRowSettleLeavesARowFlippedAfterItsRead(t *testing.T) {
	for _, kind := range []struct {
		name   string
		delta  provider.EventKind
		settle func(router *Router, itemID string, summarise func(string) string) error
	}{
		{"text", provider.EventTextDelta, func(router *Router, itemID string, summarise func(string) string) error {
			return router.settleStreamingTextRow("t1", itemID, statusErrored, summarise, "", false, nil)
		}},
		{"thinking", provider.EventThinking, func(router *Router, itemID string, summarise func(string) string) error {
			return router.settleStreamingThinkingRow("t1", itemID, statusErrored, summarise, "", false)
		}},
	} {
		t.Run(kind.name, func(t *testing.T) {
			router, st, _ := newTestRouter(t)
			createTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 0)
			handleOrFatal(t, router, provider.ProviderEvent{Kind: kind.delta, ThreadID: "t1", Content: "partial answer", Timestamp: time.Now()})
			items, err := st.ListItems("t1")
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].Status != statusStreaming {
				t.Fatalf("rows = %+v, want one streaming row", items)
			}
			id := items[0].ID
			stopped := stoppedSummary(items[0].Summary)

			err = kind.settle(router, id, func(summary string) string {
				row := mustItem(t, st, "t1", id)
				if _, changed, err := st.ErrorActiveItemIfRevision("t1", id, row.Rev, stopped, time.Now().UnixMilli(), nil); err != nil || !changed {
					t.Fatalf("flip: changed=%v err=%v", changed, err)
				}
				return interruptedSummary(summary)
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := mustItem(t, st, "t1", id); got.Status != statusErrored || got.Summary != stopped {
				t.Fatalf("row = %s %q, want the flip's %s %q", got.Status, got.Summary, statusErrored, stopped)
			}
		})
	}
}

// A content-bearing stop for a streaming row whose stream the router no
// longer holds settles the row as the stream's own stop would: the row
// and its payload take the stop's content, and the client gets the same
// settle patch.
func TestContentStopSettlesARowItsForgottenStreamLeftStreaming(t *testing.T) {
	const content = "partial answer and the rest"
	for _, kind := range []struct {
		blockType string
		delta     provider.EventKind
	}{
		{"text", provider.EventTextDelta},
		{"thinking", provider.EventThinking},
	} {
		t.Run(kind.blockType, func(t *testing.T) {
			router, st, emissions := newTestRouter(t)
			createTestThread(t, st, "t1")
			seedOpenTurn(t, router, st, "t1", 0)
			handleOrFatal(t, router, provider.ProviderEvent{Kind: kind.delta, ThreadID: "t1", ItemID: "blk-1", Content: "partial answer", Timestamp: time.Now()})
			forgetTurnStreams(router, "t1", 0)
			items, err := st.ListItems("t1")
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].Status != statusStreaming {
				t.Fatalf("rows = %+v, want one streaming row", items)
			}
			id := items[0].ID

			emissions.reset()
			handleOrFatal(t, router, contentBlockStop("blk-1", kind.blockType, content))

			got := mustItem(t, st, "t1", id)
			if got.Status != statusCompleted || got.Summary != content {
				t.Fatalf("row = %s %q, want completed %q", got.Status, got.Summary, content)
			}
			data, err := st.GetPayloadData("t1", got.PayloadID)
			if err != nil || string(data) != content {
				t.Fatalf("payload = %q (err %v), want %q", data, err, content)
			}
			events := itemStreamEventsFor(t, emissions, id)
			if len(events) != 1 || events[0].Action != itemStreamActionPatch {
				t.Fatalf("row events = %+v, want one settle patch", events)
			}
			if patch := events[0].Patch; patch.Status == nil || *patch.Status != statusCompleted || patch.Summary == nil || *patch.Summary != content {
				t.Fatalf("settle patch = %+v, want completed %q", patch.ItemPatchFields, content)
			}
		})
	}
}

// forgetTurnStreams forgets turn turnIndex's open streams as a turn
// boundary does, leaving their rows streaming.
func forgetTurnStreams(router *Router, threadID string, turnIndex int) {
	router.mu.Lock()
	defer router.mu.Unlock()
	router.dropTurnStreamsLocked(threadID, router.threadStateIfPresent(threadID), turnIndex, false, nil)
}

// contentBlockStop is a block stop on t1 that carries the block's content.
func contentBlockStop(itemID, blockType, content string) provider.ProviderEvent {
	return provider.ProviderEvent{
		Kind: provider.EventContentBlockStop, ThreadID: "t1", ItemID: itemID,
		Content: content, ContentPresent: true,
		Meta: json.RawMessage(`{"blockType":"` + blockType + `"}`), Timestamp: time.Now(),
	}
}

// A turn's result that lands while a block stop's settle is still
// writing is announced after that settle: a client that reads the turn
// on provider:turn_completed finds no streaming row.
func TestTurnCompleteWaitsForInFlightBlockSettle(t *testing.T) {
	router, probe := newAnnouncementRouter(t, "t1")
	hold := newBlockSettleHold(t, router)
	handleOrFatal(t, router, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: "t1", TurnIndex: 0, Timestamp: time.Now()})
	hold.stopBlock(t, "answer")
	hold.runPast(t, func() error {
		return router.Handle(provider.ProviderEvent{
			Kind: provider.EventTurnComplete, ThreadID: "t1", TurnComplete: normalTurnCompleteMeta(), Timestamp: time.Now(),
		})
	})

	got := probe.snapshot(t)
	if len(got) != 1 || len(got[0].streamingItems) != 0 {
		t.Errorf("turn_completed announcements = %+v, want one with no streaming row", got)
	}
	if finals := hold.finalTicks(); len(finals) != 1 || finals[0] != "answer" {
		t.Errorf("final observer ticks = %q, want one with the block text", finals)
	}
}

// blockSettleHold holds a router's first text-block settle between its
// read of the row and its write (at the observer's final tick) until
// released, and records the text of every final tick.
type blockSettleHold struct {
	router  *Router
	held    chan struct{}
	release func()
	mu      sync.Mutex
	finals  []string
}

func newBlockSettleHold(t *testing.T, router *Router) *blockSettleHold {
	released := make(chan struct{})
	hold := &blockSettleHold{
		router:  router,
		held:    make(chan struct{}),
		release: sync.OnceFunc(func() { close(released) }),
	}
	router.SetAssistantTextObservers(nil, func(_, _, text string) {
		hold.mu.Lock()
		first := len(hold.finals) == 0
		hold.finals = append(hold.finals, text)
		hold.mu.Unlock()
		if first {
			close(hold.held)
			<-released
		}
	})
	t.Cleanup(router.WaitForPendingSettles)
	t.Cleanup(hold.release)
	return hold
}

// stopBlock streams text on t1 and stops its block, and returns once the
// block's settle is held.
func (h *blockSettleHold) stopBlock(t *testing.T, text string) {
	t.Helper()
	for _, evt := range []provider.ProviderEvent{
		{Kind: provider.EventTextDelta, ThreadID: "t1", Content: text, Timestamp: time.Now()},
		{Kind: provider.EventContentBlockStop, ThreadID: "t1", Meta: json.RawMessage(`{"blockType":"text"}`), Timestamp: time.Now()},
	} {
		if err := h.router.Handle(evt); err != nil {
			t.Fatalf("handle %s: %v", evt.Kind, err)
		}
	}
	select {
	case <-h.held:
	case <-time.After(5 * time.Second):
		t.Fatal("the block's settle never reached its write")
	}
}

// runPast runs boundary while the settle is held, fails if boundary
// returns before the settle is released, and returns once both are done.
func (h *blockSettleHold) runPast(t *testing.T, boundary func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- boundary() }()
	var err error
	select {
	case err = <-done:
		t.Error("the boundary returned while a block settle was in flight")
		h.release()
	case <-time.After(100 * time.Millisecond):
		h.release()
		err = <-done
	}
	if err != nil {
		t.Fatal(err)
	}
}

func (h *blockSettleHold) finalTicks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.finals...)
}
