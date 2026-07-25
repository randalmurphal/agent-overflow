package main

import (
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"agent-overflow/internal/highlight"
)

// Highlight seed push: while an assistant_text row streams, the
// backend scans its accumulated text for fenced code blocks (the same
// flush cadence that feeds the live pathRefs enrichment), highlights
// them, and pushes span metadata on the remote-only `highlight:seed`
// channel. A remote client colors streaming code from the seed that
// rides the event stream instead of paying a WAN round trip per growth
// step; loopback clients never receive these frames (see
// transport/event_visibility.go) and keep the fast RPC path.
//
// Seeds carry NO text — per internal/highlight doctrine the server
// never re-ships content. Alignment is content-addressed: a cumulative
// per-line hash chain (frontend fnv1a parity) lets the client verify
// its own copy of the fence line by line. Divergence between this
// scanner and the frontend's marked pipeline (list-indented fences,
// exotic markdown) is therefore fail-safe: the seed simply never
// matches and the block uses the RPC path, today's behavior.
//
// Producer gating: no work happens unless a remote client is attached
// (Server.HasRemoteClient). Tunneled remotes (SSH local forward)
// arrive as loopback and are invisible to that probe — they keep the
// RPC path; a future explicit toggle can override if that becomes a
// real deployment shape.

const (
	// seedMaxScanBytes caps the accumulated text one tick scans.
	// Beyond it, seeding stops for the item (RPC path takes over);
	// matches the per-input parse cap so nothing past it could be
	// highlighted anyway.
	seedMaxScanBytes = 1 << 20 // 1 MB

	// seedMaxSourceBytes caps one fence's content in a seed. Larger
	// blocks re-parse too much per flush window and their span arrays
	// would sit in every connection's event ring; they fall back to
	// the RPC path, which allows far larger inputs.
	seedMaxSourceBytes = 128 << 10 // 128 KB

	// seedMaxStates bounds the per-item scanner state map. Streaming
	// items are few (one text row per streaming scope) and state is
	// dropped at settle; the cap only matters if settles are lost
	// (thread killed mid-stream), where new streams simply stop
	// seeding until existing entries settle out.
	seedMaxStates = 64

	// seedMaxEphemeralWorkers bounds workers for final-only ticks
	// (whole-block settles that never hit a flush window). They bypass
	// seedMaxStates by design — nothing registers — so without their
	// own cap a burst of settling code-bearing messages could stack
	// goroutines (each retaining its full text) behind the parse
	// semaphore. Excess settles drop their seed; the RPC path covers.
	seedMaxEphemeralWorkers = 8
)

// HighlightSeedEvent is the `highlight:seed` payload: spans plus the
// hash chain that lets the frontend verify its own copy of the fence.
// Final seeds (fence closed, or the item settled) also carry the
// frontend cache key so exact content can be adopted synchronously on
// later mounts.
type HighlightSeedEvent struct {
	ThreadID string `json:"threadId"`
	// ItemID scopes the frontend's live-seed retention per streaming
	// row: concurrent rows (subagent fan-out) can stream same-language
	// fences, and a shared (thread, lang) slot would let each tick
	// evict the other row's growing seed.
	ItemID string `json:"itemId"`
	// Lang is the first whitespace-delimited word of the fence info
	// string — the identity the frontend's span caches key by (the
	// host derives the same word from marked's full token.lang) — not
	// the resolved canonical language.
	Lang string `json:"lang"`
	// ContentKey is the frontend `contentKey(source)` string; set on
	// final seeds only.
	ContentKey string `json:"contentKey,omitempty"`
	// LineHashes is the cumulative per-line fnv1a chain over the fence
	// source (see highlight.FrontendLineHashes).
	LineHashes []uint32                `json:"lineHashes"`
	Lines      []highlight.EncodedLine `json:"lines"`
	Final      bool                    `json:"final"`
}

type highlightSeeder struct {
	mu     sync.Mutex
	states map[string]*seedState
	// ephemeralWorkers counts in-flight final-only-tick workers (see
	// seedMaxEphemeralWorkers). Registered states are bounded by
	// seedMaxStates; this bounds the unregistered rest.
	ephemeralWorkers atomic.Int32
}

// purgeThread drops every per-item scanner state for threadID. Called
// from session teardown (after the provider process is closed, so no
// flush tick can re-register): a thread killed mid-stream never
// delivers the final tick that would otherwise clear its entries, and
// stranded entries both consume the seedMaxStates budget and would
// hand a stale fencesDone watermark to a replacement stream reusing
// the same thread/item IDs (fork-and-revert restarts).
func (s *highlightSeeder) purgeThread(threadID string) {
	prefix := threadID + "|"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.states {
		if strings.HasPrefix(key, prefix) {
			delete(s.states, key)
		}
	}
}

// seedState is one streaming item's scanner state. fencesDone is only
// touched by the single busy worker goroutine (the busy flag hands off
// under mu, which orders the accesses); pending/hasPending/busy are
// mu-guarded.
type seedState struct {
	fencesDone int
	busy       bool
	pending    seedTick
	hasPending bool
}

type seedTick struct {
	threadID string
	itemID   string
	text     string
	final    bool
}

// observeAssistantTextStream is the triage router's streaming-text
// observer (wired in newTriageRouter). Flush-path calls can run on the
// provider read loop, so this only enqueues; parsing happens on a
// per-item worker goroutine that coalesces ticks (a slow parse skips
// intermediate texts and processes the newest).
func (a *App) observeAssistantTextStream(threadID, itemID, text string, final bool) {
	hasRemote := a.hasRemoteClient()
	s := &a.highlightSeeder
	key := threadID + "|" + itemID
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[key]
	if !hasRemote {
		// Nothing to push and nothing owed. Drop state on the final
		// tick so a remote client disconnecting mid-stream doesn't
		// strand the entry.
		if st != nil && final {
			delete(s.states, key)
		}
		return
	}
	ephemeral := false
	if st == nil {
		st = &seedState{}
		switch {
		case final:
			// A whole-block message can settle without ever hitting a
			// flush window — the final tick is its ONLY tick. Process
			// it on an ephemeral state; nothing registers, so there is
			// nothing to clean up. Bounded by its own worker cap.
			if s.ephemeralWorkers.Add(1) > seedMaxEphemeralWorkers {
				s.ephemeralWorkers.Add(-1)
				return
			}
			ephemeral = true
		case len(s.states) >= seedMaxStates:
			// Over the cap, new items skip seeding (RPC path covers).
			return
		default:
			if s.states == nil {
				s.states = make(map[string]*seedState)
			}
			s.states[key] = st
		}
	}
	st.pending = seedTick{threadID: threadID, itemID: itemID, text: text, final: final}
	st.hasPending = true
	if st.busy {
		return
	}
	st.busy = true
	if ephemeral {
		go func() {
			defer s.ephemeralWorkers.Add(-1)
			a.runHighlightSeedWorker(key, st)
		}()
		return
	}
	go a.runHighlightSeedWorker(key, st)
}

func (a *App) runHighlightSeedWorker(key string, st *seedState) {
	s := &a.highlightSeeder
	for {
		s.mu.Lock()
		if !st.hasPending {
			st.busy = false
			s.mu.Unlock()
			return
		}
		tick := st.pending
		st.hasPending = false
		if tick.final {
			// The settle tick is the item's last (post-settle flushes
			// find an already-drained buffer and never re-observe), so
			// the state can go now; the worker still processes the
			// final text below.
			if s.states[key] == st {
				delete(s.states, key)
			}
			st.busy = false
		}
		s.mu.Unlock()
		a.processHighlightSeedTick(st, tick)
		if tick.final {
			return
		}
	}
}

// processHighlightSeedTick scans one accumulated-text snapshot and
// pushes seeds for fences at or past the already-pushed watermark.
// Closed fences (and every fence on the final tick) push once as
// final and advance the watermark; the trailing open fence re-pushes
// its growing prefix each tick, parsed without cache insertion.
func (a *App) processHighlightSeedTick(st *seedState, tick seedTick) {
	// Re-probe: the tick may have queued behind a slow parse and the
	// last remote client may be gone by now — the scan and emit would
	// be pure waste (every remaining subscriber filters the channel).
	// Skipping does not advance the watermark; a reconnecting remote
	// resumes seeding from where it stopped.
	if len(tick.text) > seedMaxScanBytes || !a.hasRemoteClient() {
		return
	}
	fences := highlight.ScanFences(tick.text)
	if len(fences) < st.fencesDone {
		// The stream regressed below the watermark: the buffer was
		// replaced, not extended (retry, revert). Reseed from scratch —
		// the client's hash verification makes any overlap fail-safe.
		st.fencesDone = 0
	}
	for i := st.fencesDone; i < len(fences); i++ {
		fence := fences[i]
		finalFence := fence.Closed || tick.final
		if finalFence {
			st.fencesDone = i + 1
		}
		// Language-less fences render plain without a request on the
		// frontend; over-cap fences use the RPC path. Invalid UTF-8
		// must never seed: JSON transport maps each invalid byte to
		// U+FFFD, so the client's copy hashes IDENTICALLY while the
		// spans cover the original byte lengths — the one divergence
		// that would misalign colors instead of missing the cache.
		if fence.Lang == "" || len(fence.Source) > seedMaxSourceBytes ||
			!utf8.ValidString(fence.Source) {
			continue
		}
		lang := highlight.LangFromName(fence.Lang)
		var res highlight.Result
		if finalFence {
			// Final content warms the shared cache too: a later
			// HighlightCode RPC for the same block is a lookup.
			res = a.highlightCache().Code(lang, fence.Source)
		} else {
			res = a.highlightCache().CodeTransient(lang, fence.Source)
		}
		if res.Incomplete {
			// Transient degradation (parse timeout) never seeds: there
			// are no spans worth adopting, and an incomplete "exact"
			// match would cancel the client's own RPC and pin the
			// degradation. The RPC path owns incomplete retries.
			continue
		}
		evt := HighlightSeedEvent{
			ThreadID:   tick.threadID,
			ItemID:     tick.itemID,
			Lang:       fence.Lang,
			LineHashes: highlight.FrontendLineHashes(fence.Source),
			Lines:      res.Lines,
			Final:      finalFence,
		}
		if finalFence {
			evt.ContentKey = highlight.FrontendContentKey(fence.Source)
		}
		a.emit("highlight:seed", evt)
	}
}

// hasRemoteClient reports whether the transport currently has a
// non-loopback WebSocket connection attached.
func (a *App) hasRemoteClient() bool {
	if a.remoteClientProbeFn != nil {
		return a.remoteClientProbeFn()
	}
	s := a.transportServer.Load()
	return s != nil && s.HasRemoteClient()
}
