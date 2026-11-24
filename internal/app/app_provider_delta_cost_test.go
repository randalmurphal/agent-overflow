package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/store/storetest"
	"agent-overflow/internal/triage"
)

// BenchmarkStreamedDeltaCost measures what a reply costs between a
// provider's emit and SQLite when it arrives as many small deltas rather
// than as the same bytes in one: the thread's provider event queue, the
// router, and the store, per provider shape. Each op is one turn on a new
// thread (turn start, the deltas, turn completion), so "split" minus
// "whole" divided by the extra deltas is what one more delta costs.
//
// Reported per op: chunk-writes, the payload chunk rows the stream's
// flushes appended (one SQLite transaction each), and live-deltas, the
// item delta events triage emitted for the transport. Not measured:
// sessionEventHandler's work around the queue (rate-limit attribution,
// activity records, the session-current check), the transport's fan-out,
// and the frontend.
func BenchmarkStreamedDeltaCost(b *testing.B) {
	shapes := []struct {
		name     string
		provider provider.ProviderKind
		kind     provider.EventKind
		sizes    []int
		// claudeBlocks wraps the deltas in content block start and stop,
		// as Claude's stream does; itemID gives them a provider item id, as
		// Codex's do; turnID gives the turn's events a provider turn id, as
		// ACP and Codex do.
		claudeBlocks, itemID, turnID bool
	}{
		// The Cursor ACP spike's measured nano turn: 101 thinking chunks,
		// 436 bytes (docs/specs/cursor-provider.md).
		{name: "acp-thinking-101x436B", provider: provider.Agy, kind: provider.EventThinking, sizes: spreadBytes(101, 436), turnID: true},
		{name: "acp-text-2000x4B", provider: provider.Agy, kind: provider.EventTextDelta, sizes: spreadBytes(2000, 8000), turnID: true},
		{name: "claude-text-2000x4B", provider: provider.Claude, kind: provider.EventTextDelta, sizes: spreadBytes(2000, 8000), claudeBlocks: true},
		{name: "codex-text-2000x4B", provider: provider.Codex, kind: provider.EventTextDelta, sizes: spreadBytes(2000, 8000), itemID: true, turnID: true},
	}
	for _, shape := range shapes {
		total := 0
		for _, n := range shape.sizes {
			total += n
		}
		for _, split := range []bool{true, false} {
			sizes := shape.sizes
			name := fmt.Sprintf("%s/split", shape.name)
			if !split {
				sizes = []int{total}
				name = fmt.Sprintf("%s/whole", shape.name)
			}
			b.Run(name, func(b *testing.B) {
				rig := newDeltaCostRig(b)
				deltas := make([]string, len(sizes))
				for i, n := range sizes {
					deltas[i] = strings.Repeat("a", n)
				}
				var chunkWrites, liveDeltas int64
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					threadID := fmt.Sprintf("delta-cost-%d", i)
					rig.createThread(b, threadID, shape.provider)
					startDeltas := rig.liveDeltas.Load()
					turnID := ""
					if shape.turnID {
						turnID = "turn-" + threadID
					}
					events := make([]provider.ProviderEvent, 0, len(deltas)+4)
					now := time.Now()
					events = append(events, provider.ProviderEvent{Kind: provider.EventTurnStart, ThreadID: threadID, TurnID: turnID, Timestamp: now})
					blockType := "text"
					if shape.kind == provider.EventThinking {
						blockType = "thinking"
					}
					blockMeta, err := json.Marshal(map[string]any{"index": 0, "blockType": blockType, provider.MetaAPIMessageIDKey: "msg-" + threadID})
					if err != nil {
						b.Fatal(err)
					}
					if shape.claudeBlocks {
						events = append(events, provider.ProviderEvent{Kind: provider.EventContentBlockStart, ThreadID: threadID, Meta: blockMeta, Timestamp: now})
					}
					for _, delta := range deltas {
						evt := provider.ProviderEvent{Kind: shape.kind, ThreadID: threadID, TurnID: turnID, Content: delta, Role: "assistant", Timestamp: now}
						if shape.itemID {
							evt.ItemID = "item-" + threadID
						}
						events = append(events, evt)
					}
					if shape.claudeBlocks {
						events = append(events, provider.ProviderEvent{Kind: provider.EventContentBlockStop, ThreadID: threadID, Meta: blockMeta, Timestamp: now})
					}
					events = append(events, provider.ProviderEvent{
						Kind: provider.EventTurnComplete, ThreadID: threadID, TurnID: turnID, Timestamp: now,
						TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
					})
					b.StartTimer()

					for _, evt := range events {
						rig.queues.enqueue(threadID, evt, rig.handle)
					}
					if err := rig.queues.drain(threadID); err != nil {
						b.Fatalf("drain: %v", err)
					}
					rig.router.WaitForPendingSettles()

					b.StopTimer()
					if err := rig.handleErr.Load(); err != nil {
						b.Fatalf("handle: %v", *err)
					}
					chunkWrites += rig.chunkRows(b, threadID)
					liveDeltas += rig.liveDeltas.Load() - startDeltas
					rig.queues.retireWhenIdle(threadID)
					rig.router.CleanupThread(threadID)
					b.StartTimer()
				}
				b.StopTimer()
				b.ReportMetric(float64(len(deltas)), "deltas/op")
				b.ReportMetric(float64(chunkWrites)/float64(b.N), "chunk-writes/op")
				b.ReportMetric(float64(liveDeltas)/float64(b.N), "live-deltas/op")
			})
		}
	}
}

// spreadBytes splits total bytes into n sizes that differ by at most one.
func spreadBytes(n, total int) []int {
	sizes := make([]int, n)
	for i := range sizes {
		sizes[i] = total / n
		if i < total%n {
			sizes[i]++
		}
	}
	return sizes
}

type deltaCostRig struct {
	store      *store.Store
	reader     *sql.DB
	router     *triage.Router
	queues     *providerEventQueues
	liveDeltas atomic.Int64
	handleErr  atomic.Pointer[error]
	handle     func(provider.ProviderEvent)
}

func newDeltaCostRig(b *testing.B) *deltaCostRig {
	b.Helper()
	path := storetest.ClonePath(b)
	st, err := store.New(path)
	if err != nil {
		b.Fatalf("store.New: %v", err)
	}
	b.Cleanup(func() { _ = st.Close() })
	reader, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		b.Fatalf("open reader: %v", err)
	}
	b.Cleanup(func() { _ = reader.Close() })
	now := time.Now().UnixMilli()
	if _, err := st.CreateProject(store.Project{ID: defaultTestProjectID, Path: "/tmp/workspace", Name: "Delta cost", CreatedAt: now, UpdatedAt: now}); err != nil {
		b.Fatalf("CreateProject: %v", err)
	}
	rig := &deltaCostRig{store: st, reader: reader, queues: &providerEventQueues{}}
	rig.router = triage.NewRouter(st, func(ch eventchan.Channel, payload any) {
		if ch != eventchan.ProviderItemEvent {
			return
		}
		if evt, ok := payload.(triage.ItemStreamEvent); ok && evt.Action == "delta" {
			rig.liveDeltas.Add(1)
		}
	})
	rig.handle = func(evt provider.ProviderEvent) {
		if err := rig.router.Handle(evt); err != nil {
			rig.handleErr.CompareAndSwap(nil, &err)
		}
	}
	return rig
}

func (r *deltaCostRig) createThread(b *testing.B, id string, kind provider.ProviderKind) {
	b.Helper()
	now := time.Now().UnixMilli()
	if err := r.store.CreateThread(store.Thread{
		ID: id, ProjectID: defaultTestProjectID, Title: "Delta cost", Provider: string(kind),
		WorkspacePath: "/tmp/workspace", Mode: "chat", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		b.Fatalf("CreateThread: %v", err)
	}
}

func (r *deltaCostRig) chunkRows(b *testing.B, threadID string) int64 {
	b.Helper()
	var n int64
	if err := r.reader.QueryRow(`SELECT COUNT(*) FROM payload_chunks WHERE thread_id = ?`, threadID).Scan(&n); err != nil {
		b.Fatalf("count payload chunks: %v", err)
	}
	return n
}
